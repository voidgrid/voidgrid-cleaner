package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/voidgrid/voidgrid-cleaner/internal/clients/httpjson"
	"github.com/voidgrid/voidgrid-cleaner/internal/clients/plex"
	"github.com/voidgrid/voidgrid-cleaner/internal/storage"
)

// fixData is what a finding carries for its fix.
type fixData struct {
	RatingKey  string  `json:"rating_key,omitempty"`
	Section    string  `json:"section,omitempty"`
	Library    string  `json:"library,omitempty"`
	Type       string  `json:"type,omitempty"` // "movie" or "show"
	Agent      string  `json:"agent,omitempty"`
	WantID     string  `json:"want_id,omitempty"`
	ArrTitle   string  `json:"arr_title,omitempty"`
	ArrYear    int     `json:"arr_year,omitempty"`
	App        string  `json:"app,omitempty"` // "radarr" or "sonarr"
	ArrID      int64   `json:"arr_id,omitempty"`
	FileID     int64   `json:"file_id,omitempty"`
	OldName    string  `json:"old_name,omitempty"`
	NewName    string  `json:"new_name,omitempty"`
	Folder     string  `json:"folder,omitempty"`
	Root       string  `json:"root,omitempty"`
	MediaID    int64   `json:"media_id,omitempty"`
	Monitored  bool    `json:"monitored,omitempty"`
	HasFile    bool    `json:"has_file,omitempty"`
	Episodes   []int64 `json:"episodes,omitempty"`
	DownloadID string  `json:"download_id,omitempty"`
	QueueIDs   []int64 `json:"queue_ids,omitempty"`
	// Blocked is a download that won't finish without someone acting.
	Blocked bool `json:"blocked,omitempty"`
	// Resolution is what Plex measured, as Plex writes it.
	Resolution string `json:"resolution,omitempty"`
}

func encode(d fixData) string {
	b, err := json.Marshal(d)
	if err != nil {
		return "{}" // can't happen: plain strings and ints
	}
	return string(b)
}

const localPrefix = "plex:local:"

// Fixable reports whether the doctor can fix a finding.
func Fixable(kind, subject string) bool {
	return kind == KindWrongMatch || kind == KindQuestionable || kind == KindUntracked || kind == KindNotAccepted ||
		kind == KindMissing || kind == KindStuck || (kind == KindHealth && strings.HasPrefix(subject, removedPrefix)) || kind == KindReleaseTitle || kind == KindNaming ||
		kind == KindStaleRequest || kind == KindEmptyFolder || kind == KindStrayFile ||
		(kind == KindSettings && (strings.HasPrefix(subject, localPrefix) || subject == emptyFoldersSubject))
}

// NeedsChoice reports whether a kind's fix waits for the admin to pick an option.
func NeedsChoice(kind string) bool {
	return kind == KindWrongMatch || kind == KindQuestionable || kind == KindUntracked || kind == KindNotAccepted || kind == KindMissing || kind == KindHealth || kind == KindStuck
}

func jsonDecode(s string, v any) error { return json.Unmarshal([]byte(s), v) }

// refreshChoice keeps the match and re-reads the item's metadata.
const refreshChoice = "refresh"

// Choice is one option a fix offers.
type Choice struct {
	Value, Label string
	Default      bool // selected to start with
}

// Preview says exactly what a fix would do, before anyone approves it.
type Preview struct {
	Finding storage.Finding
	Lines   []string
	Choices []Choice // if set, the admin picks one
	// HasDefault says a choice starts selected, not "leave it as it is".
	HasDefault bool
	CanFix     bool
}

func plexAgent(itemType string) string {
	if itemType == "show" {
		return "tv.plex.agents.series"
	}
	return "tv.plex.agents.movie"
}

// Preview builds a finding's preview. It reads from Plex and changes nothing.
func (d *Doctor) Preview(ctx context.Context, f storage.Finding) (Preview, error) {
	p := Preview{Finding: f}
	var data fixData
	if err := json.Unmarshal([]byte(f.Data), &data); err != nil {
		return p, err
	}
	if isArrFix(f) {
		return p, d.previewArr(ctx, &p, data)
	}
	if isDiskFix(f) {
		d.previewDisk(ctx, &p, data)
		return p, nil
	}
	if f.Kind == KindStuck {
		return p, d.previewStuck(ctx, &p, data)
	}
	if f.Kind == KindSettings {
		p.CanFix = true
		p.Lines = []string{
			"Turn off Prefer local metadata on the " + data.Library + " library.",
			"Refresh all of its metadata, so titles come from Plex's own data.",
			"Titles you've locked in Plex stay as they are. Plex does this in the background, and it can take a while.",
		}
		return p, nil
	}
	item, err := d.Plex.Item(ctx, data.RatingKey)
	if err != nil {
		return p, err
	}
	if f.Kind == KindReleaseTitle {
		p.CanFix = true
		p.Lines = []string{"Refresh this item's metadata in Plex, so its title comes from Plex's own data. Its match doesn't change."}
		return p, nil
	}
	// The right match with the wrong name needs a refresh. Rematching can't help,
	// and listing other films would hide that the match is already right.
	if data.WantID != "" && item.ExternalID(data.Agent) == data.WantID {
		p.CanFix = true
		p.Choices = []Choice{{Value: refreshChoice, Label: "keep the match and refresh its metadata"}}
		p.Lines = []string{
			fmt.Sprintf("Plex has the right match: it's %s (%d), the same as Radarr or Sonarr. Only what's displayed differs: %s (%d).", data.ArrTitle, data.ArrYear, item.Title, item.Year),
			"If the difference is fine, dismiss it instead.",
		}
		return p, nil
	}
	choices, err := d.candidates(ctx, data, item)
	if err != nil {
		return p, err
	}
	p.Lines = []string{fmt.Sprintf("Plex now: %s (%d). Radarr or Sonarr: %s (%d).", item.Title, item.Year, data.ArrTitle, data.ArrYear)}
	if len(choices) == 0 {
		p.Lines = append(p.Lines, "Plex offers nothing to match this to, or it's already matched to what Radarr or Sonarr has. If that's right, dismiss it.")
		return p, nil
	}
	p.Choices, p.CanFix = choices, true
	p.Lines = append(p.Lines, "Rematch it in Plex. The file doesn't change. Plays stay with the item.")
	return p, nil
}

// candidates lists what an item could be rematched to, the arr's ID first.
// What it's matched to now is left out, since that wouldn't change anything.
func (d *Doctor) candidates(ctx context.Context, data fixData, item plex.Item) ([]Choice, error) {
	var out []Choice
	seen := map[string]bool{item.PlexGUID: true}
	add := func(title string, year int) error {
		ms, err := d.Plex.Matches(ctx, data.RatingKey, title, year, plexAgent(data.Type))
		if err != nil {
			return err
		}
		for _, m := range ms {
			if m.GUID != "" && !seen[m.GUID] {
				seen[m.GUID] = true
				out = append(out, Choice{Value: m.GUID + "|" + m.Name, Label: fmt.Sprintf("%s (%d)", m.Name, m.Year)})
			}
		}
		return nil
	}
	if data.Agent != "" && data.WantID != "" {
		if err := add(data.Agent+"-"+data.WantID, 0); err != nil {
			return nil, err
		}
	}
	if data.ArrTitle != "" {
		if err := add(data.ArrTitle, data.ArrYear); err != nil {
			return nil, err
		}
	}
	return out[:min(len(out), 6)], nil
}

// verifyTries, with Doctor.FixWait between them, covers Plex taking a few
// seconds to apply a match.
const verifyTries = 10

// apply runs one fix and proves it took. The returned text says what happened.
func (d *Doctor) apply(ctx context.Context, f storage.Finding, choice string) (string, error) {
	var data fixData
	if err := json.Unmarshal([]byte(f.Data), &data); err != nil {
		return "", err
	}
	if isArrFix(f) {
		return d.applyArr(ctx, f, data, choice)
	}
	if isDiskFix(f) {
		return d.applyDisk(ctx, f, data)
	}
	if f.Kind == KindStuck {
		return d.applyStuck(ctx, data, choice)
	}
	if f.Kind == KindSettings {
		return d.applyLocalMetadata(ctx, data)
	}
	if f.Kind == KindReleaseTitle || choice == refreshChoice {
		if err := d.Plex.RefreshItem(ctx, data.RatingKey); err != nil {
			return "", err
		}
		return "refresh started; the next check shows if the title changed", nil
	}
	guid, name, ok := strings.Cut(choice, "|")
	if !ok || !strings.HasPrefix(guid, "plex://") {
		return "", errors.New("no match chosen")
	}
	if err := d.Plex.Match(ctx, data.RatingKey, guid, name); err != nil {
		return "", err
	}
	for try := 0; ; try++ {
		item, err := d.Plex.Item(ctx, data.RatingKey)
		if err == nil && item.PlexGUID == guid {
			return fmt.Sprintf("now matched to %s", name), nil
		}
		if try == verifyTries {
			return "", fmt.Errorf("plex accepted the match, but the item still isn't %s", name)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(d.FixWait):
		}
	}
}

func (d *Doctor) applyLocalMetadata(ctx context.Context, data fixData) (string, error) {
	if err := d.Plex.SetPreferLocalMetadata(ctx, data.Section, false); err != nil {
		return "", err
	}
	on, err := d.Plex.PreferLocalMetadata(ctx, data.Section)
	if err != nil {
		return "", err
	}
	if on {
		return "", errors.New("plex still has Prefer local metadata on")
	}
	if err := d.Plex.RefreshMetadata(ctx, data.Section); err != nil {
		return "", err
	}
	return "Prefer local metadata is off and a refresh has started", nil
}

// FixLoop runs approved fixes until ctx ends.
func (d *Doctor) FixLoop(ctx context.Context, every time.Duration) {
	if err := d.DB.FailInterruptedFixes(ctx); err != nil {
		log.Printf("doctor: interrupted fixes: %v", err)
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if d.RunFixes(ctx) > 0 {
				d.Run(ctx) // findings catch up with what changed
			}
		}
	}
}

// RunFixes works through the queue and returns how many it ran.
func (d *Doctor) RunFixes(ctx context.Context) int {
	n := 0
	for ctx.Err() == nil {
		job, found, err := d.DB.ClaimFix(ctx)
		if err != nil {
			log.Printf("doctor: claim fix: %v", err)
			return n
		}
		if !found {
			return n
		}
		n++
		d.runFix(ctx, job)
	}
	return n
}

func (d *Doctor) runFix(ctx context.Context, job storage.FixJob) {
	state, detail := d.fixOutcome(d.recording(ctx, job.Title), job)
	ctx = context.WithoutCancel(ctx) // the outcome is recorded even if shutdown stopped the fix
	log.Printf("doctor: fix %s: %s: %s", job.Title, state, detail)
	d.DB.Audit(ctx, job.Who, "doctor fix "+state, job.Title, detail)
	if err := d.DB.FinishFix(ctx, job.ID, state, detail); err != nil {
		log.Printf("doctor: finish fix: %v", err)
	}
}

func (d *Doctor) fixOutcome(ctx context.Context, job storage.FixJob) (state, detail string) {
	f, found, err := d.DB.Finding(ctx, job.FindingID)
	switch {
	case err != nil:
		return "failed", err.Error()
	case !found || f.State != "open":
		return "skipped", "the finding is no longer open"
	}
	var data fixData
	if err := json.Unmarshal([]byte(f.Data), &data); err != nil {
		return "failed", err.Error()
	}
	if data.RatingKey != "" {
		busy, err := d.DB.HasActiveDeletion(ctx, data.RatingKey)
		if err != nil {
			return "failed", err.Error()
		}
		if busy {
			return "skipped", "it's being deleted"
		}
	}
	if data.ArrID != 0 {
		busy, err := d.DB.HasActiveArrDeletion(ctx, data.App == appSonarr, data.ArrID)
		if err != nil {
			return "failed", err.Error()
		}
		if busy {
			return "skipped", "it's being deleted"
		}
	}
	detail, err = d.apply(ctx, f, job.Choice)
	if err != nil {
		return "failed", err.Error()
	}
	return "done", detail
}

// recording sends each call on ctx to the audit log at the audit log level.
func (d *Doctor) recording(ctx context.Context, item string) context.Context {
	if level, err := d.DB.LogLevel(ctx); err == nil && level == storage.LogNormal {
		return ctx
	}
	return httpjson.WithRecorder(ctx, func(c httpjson.Call) { d.DB.Audit(ctx, "doctor", "api call", item, c.String()) })
}
