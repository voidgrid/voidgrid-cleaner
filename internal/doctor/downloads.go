package doctor

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path"
	"strings"
	"time"

	"github.com/voidgrid/voidgrid-cleaner/internal/clients/arr"
	"github.com/voidgrid/voidgrid-cleaner/internal/storage"
)

// KindStuck is a download Radarr or Sonarr can't finish on its own.
const KindStuck = "stuck_download"

// stuckFor is how long a download stays stuck before Discord hears about it,
// so a moment's hiccup while importing doesn't post anything.
const stuckFor = 15 * time.Minute

// What can be done about a stuck download, named as Radarr and Sonarr name them.
const (
	doImport         = "import"
	doRemoveDownload = "remove"
	doBlocklist      = "blocklist"
	doBlocklistAgain = "blocklist-search"
)

// DownloadChoices labels the actions, for Discord and the preview.
var DownloadChoices = []Choice{
	{Value: doRemoveDownload, Label: "Remove"},
	{Value: doBlocklist, Label: "Blocklist only"},
	{Value: doBlocklistAgain, Label: "Blocklist and search"},
}

// DownloadAlerter posts a stuck download to the tech alerts channel.
type DownloadAlerter interface {
	DownloadAlert(ctx context.Context, text string, findingID int64) error
}

func isStuck(q arr.QueueRecord) bool {
	return q.TrackedDownloadStatus == "warning" || q.TrackedDownloadStatus == "error" ||
		q.TrackedDownloadState == "importBlocked" || q.TrackedDownloadState == "failedPending" ||
		q.Status == "failed" || q.Status == "warning"
}

func itemName(q arr.QueueRecord) string {
	switch {
	case q.Movie != nil:
		return fmt.Sprintf("%s (%d)", q.Movie.Title, q.Movie.Year)
	case q.Series != nil && q.Episode != nil:
		return fmt.Sprintf("%s S%02dE%02d", q.Series.Title, q.Episode.SeasonNumber, q.Episode.EpisodeNumber)
	case q.Series != nil:
		return q.Series.Title
	}
	return q.Title
}

// QueueRow is a download for the downloads page.
type QueueRow struct {
	App, Item, Release, State, Reason, Client string
	Progress                                  int
	FindingID                                 int64 // set when it's stuck and can be fixed
}

func (d *Doctor) queues(ctx context.Context) (map[string][]arr.QueueRecord, error) {
	out := map[string][]arr.QueueRecord{}
	for app, a := range map[string]ArrAPI{appRadarr: d.Radarr, appSonarr: d.Sonarr} {
		q, err := a.Queue(ctx)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", app, err)
		}
		out[app] = q
	}
	return out, nil
}

// Downloads lists both queues, a row per download.
func (d *Doctor) Downloads(ctx context.Context) ([]QueueRow, error) {
	qs, err := d.queues(ctx)
	if err != nil {
		return nil, err
	}
	open, err := d.DB.Findings(ctx, "open")
	if err != nil {
		return nil, err
	}
	ids := map[string]int64{}
	for _, f := range open {
		if f.Kind == KindStuck {
			ids[f.Subject] = f.ID
		}
	}
	var rows []QueueRow
	for _, app := range []string{appRadarr, appSonarr} {
		seen := map[string]bool{}
		for _, q := range qs[app] {
			if seen[q.DownloadID] && q.DownloadID != "" {
				continue
			}
			seen[q.DownloadID] = true
			r := QueueRow{App: app, Item: itemName(q), Release: q.Title, State: q.TrackedDownloadState, Reason: q.Reason(), Client: q.DownloadClient}
			if r.State == "" {
				r.State = q.Status
			}
			if q.Size > 0 {
				r.Progress = int((q.Size - q.Sizeleft) * 100 / q.Size)
			}
			r.FindingID = ids[app+":"+q.DownloadID]
			rows = append(rows, r)
		}
	}
	return rows, nil
}

// CheckDownloads records what's stuck and tells the admin channel about
// anything stuck for a while. Either queue failing to answer changes nothing.
func (d *Doctor) CheckDownloads(ctx context.Context) error {
	qs, err := d.queues(ctx)
	if err != nil {
		return err
	}
	var found []storage.Finding
	for _, app := range []string{appRadarr, appSonarr} {
		groups := map[string][]arr.QueueRecord{}
		var order []string
		for _, q := range qs[app] {
			if !isStuck(q) {
				continue
			}
			key := q.DownloadID
			if key == "" {
				key = fmt.Sprint(q.ID)
			}
			if groups[key] == nil {
				order = append(order, key)
			}
			groups[key] = append(groups[key], q)
		}
		for _, key := range order {
			g := groups[key]
			var ids []int64
			for _, q := range g {
				ids = append(ids, q.ID)
			}
			name := itemName(g[0])
			if len(g) > 1 && g[0].Series != nil {
				name = fmt.Sprintf("%s (%d episodes)", g[0].Series.Title, len(g))
			}
			reason := g[0].Reason()
			if reason == "" {
				reason = "no reason given; state " + g[0].TrackedDownloadState
			}
			found = append(found, storage.Finding{
				Subject: app + ":" + key, Title: name, Severity: problem,
				Detail: "Stuck in " + app + ": " + reason + " Release: " + g[0].Title,
				Data:   encode(fixData{App: app, DownloadID: g[0].DownloadID, QueueIDs: ids, OldName: g[0].Title, NewName: reason}),
			})
		}
	}
	if err := d.DB.SaveFindings(ctx, KindStuck, found); err != nil {
		return err
	}
	return d.alertStuck(ctx)
}

func (d *Doctor) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d *Doctor) alertStuck(ctx context.Context) error {
	if d.Alerts == nil {
		return nil
	}
	open, err := d.DB.Findings(ctx, "open")
	if err != nil {
		return err
	}
	for _, f := range open {
		if f.Kind != KindStuck || f.Alerted || d.now().Sub(time.Unix(f.FirstSeen, 0)) < stuckFor {
			continue
		}
		var data fixData
		if err := jsonDecode(f.Data, &data); err != nil {
			return err
		}
		text := fmt.Sprintf("**Stuck in %s: %s**\n%s\nRelease: `%s`\nManual import is on the downloads page.", data.App, f.Title, data.NewName, data.OldName)
		if err := d.Alerts.DownloadAlert(ctx, text, f.ID); err != nil {
			return err
		}
		if err := d.DB.MarkFindingAlerted(ctx, f.ID); err != nil {
			return err
		}
	}
	return nil
}

// WatchDownloads checks the queues until ctx ends.
func (d *Doctor) WatchDownloads(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := d.CheckDownloads(ctx); err != nil {
				log.Printf("doctor: downloads: %v", err)
			}
		}
	}
}

func (d *Doctor) previewStuck(ctx context.Context, p *Preview, data fixData) error {
	p.CanFix = true
	p.Lines = []string{"Release: " + data.OldName, "Why it's stuck: " + data.NewName}
	files, err := d.arrFor(data.App).DownloadImports(ctx, data.DownloadID)
	if err != nil {
		return err
	}
	if importable, lines := importPlan(data.App, files); importable {
		p.Choices = append(p.Choices, Choice{Value: doImport, Label: "import as " + data.App + " identified it"})
		p.Lines = append(p.Lines, lines...)
	} else if len(files) > 0 {
		p.Lines = append(p.Lines, data.App+" can't tell what some files are, so importing has to be done in "+data.App+".")
	}
	p.Choices = append(p.Choices, DownloadChoices...)
	p.Lines = append(p.Lines, "Blocklist stops this release being grabbed again; blocklist and search then looks for another.")
	return nil
}

func (d *Doctor) arrFor(app string) ArrAPI {
	if app == appSonarr {
		return d.Sonarr
	}
	return d.Radarr
}

// importPlan says whether every file has something to import to, and lists them.
func importPlan(app string, files []arr.DownloadFile) (bool, []string) {
	var videos int
	var lines []string
	for _, f := range files {
		if !isVideoFile(f.Path) {
			continue
		}
		videos++
		if (app == appRadarr && f.Movie == nil) || (app == appSonarr && (f.Series == nil || len(f.Episodes) == 0)) {
			return false, nil
		}
		line := "Would import " + path.Base(f.Path)
		for _, r := range f.Rejections {
			line += "; " + app + "'s concern: " + r.Reason
		}
		lines = append(lines, line)
	}
	return videos > 0, lines
}

func (d *Doctor) applyStuck(ctx context.Context, data fixData, choice string) (string, error) {
	a := d.arrFor(data.App)
	var err error
	switch choice {
	case doRemoveDownload:
		err = a.RemoveQueue(ctx, data.QueueIDs, false, true)
	case doBlocklist:
		err = a.RemoveQueue(ctx, data.QueueIDs, true, true)
	case doBlocklistAgain:
		err = a.RemoveQueue(ctx, data.QueueIDs, true, false)
	case doImport:
		err = d.importDownload(ctx, a, data)
	default:
		return "", errors.New("nothing chosen")
	}
	if err != nil {
		return "", err
	}
	// Done means gone from the queue, not merely asked.
	for try := 0; ; try++ {
		q, err := a.Queue(ctx)
		if err != nil {
			return "", err
		}
		if !inQueue(q, data.DownloadID) {
			break
		}
		if try == verifyTries {
			return "", errors.New("it's still in the queue")
		}
		if err := d.sleep(ctx); err != nil {
			return "", err
		}
	}
	for _, c := range append([]Choice{{Value: doImport, Label: "imported"}}, DownloadChoices...) {
		if c.Value == choice {
			return strings.ToLower(c.Label) + ": done", nil
		}
	}
	return "done", nil
}

func inQueue(q []arr.QueueRecord, downloadID string) bool {
	for _, r := range q {
		if r.DownloadID == downloadID {
			return true
		}
	}
	return false
}

func (d *Doctor) importDownload(ctx context.Context, a ArrAPI, data fixData) error {
	files, err := a.DownloadImports(ctx, data.DownloadID)
	if err != nil {
		return err
	}
	if ok, _ := importPlan(data.App, files); !ok {
		return errors.New("not every file is identified any more")
	}
	var list []map[string]any
	for _, f := range files {
		if !isVideoFile(f.Path) {
			continue
		}
		item := map[string]any{
			"path": f.Path, "folderName": f.FolderName, "quality": f.Quality, "languages": f.Languages,
			"releaseGroup": f.ReleaseGroup, "downloadId": data.DownloadID,
		}
		if data.App == appSonarr {
			var eps []int64
			for _, e := range f.Episodes {
				eps = append(eps, e.ID)
			}
			item["seriesId"], item["episodeIds"] = f.Series.ID, eps
		} else {
			item["movieId"] = f.Movie.ID
		}
		list = append(list, item)
	}
	return d.runCommand(ctx, a, map[string]any{"name": "ManualImport", "importMode": "auto", "files": list})
}
