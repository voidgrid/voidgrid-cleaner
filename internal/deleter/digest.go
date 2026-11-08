package deleter

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/voidgrid/voidgrid-cleaner/internal/storage"
)

// discordLimit is Discord's message size, less room for "and N more".
const discordLimit = 1900

// digest posts the day's summary once a day, after the digest time. A post
// that fails is tried again after notifyRetry; the day is only marked sent
// once both posts that should go have gone.
func (w *Worker) digest(ctx context.Context) error {
	if w.Notify == nil || w.Now().Before(w.notifyAfter) {
		return nil
	}
	at, err := w.DB.DigestTime(ctx)
	if err != nil {
		return err
	}
	now := w.Now()
	today := now.Format("2006-01-02")
	due, err := time.ParseInLocation("2006-01-02 15:04", today+" "+at, now.Location())
	if err != nil || now.Before(due) {
		return err
	}
	if sent, err := w.DB.DigestSent(ctx); err != nil || sent == today {
		return err
	}
	data, err := w.DB.DigestData(ctx, now.Add(-24*time.Hour), now, w.DryRun)
	if err != nil {
		return err
	}
	main := mainDigest(data)
	admin := w.adminDigest(ctx, data)
	if main != "" {
		if err := w.Notify.Post(ctx, false, main); err != nil {
			w.notifyAfter = now.Add(notifyRetry)
			return err
		}
	}
	switch {
	case admin == "":
	case !w.AdminChannel:
		log.Printf("deleter: no admin channel set, so the tech digest wasn't posted")
	default:
		if err := w.Notify.Post(ctx, true, admin); err != nil {
			w.notifyAfter = now.Add(notifyRetry)
			return err
		}
	}
	return w.DB.SetDigestSent(ctx, today)
}

// section writes a heading and lines, cut short to fit what's left.
func section(b *strings.Builder, heading string, lines []string) {
	if len(lines) == 0 {
		return
	}
	b.WriteString("**" + heading + "**\n")
	for i, l := range lines {
		if b.Len()+len(l) > discordLimit {
			fmt.Fprintf(b, "and %d more\n", len(lines)-i)
			return
		}
		b.WriteString(l + "\n")
	}
}

func gigabytes(n int64) string { return fmt.Sprintf("%.1f GB", float64(n)/1e9) }

// mainDigest is for everyone in the channel: only what's about to go, since
// that's the one thing anyone can still act on. What went and what's waiting
// are on the status page. No mentions, no buttons, never episodes by name.
func mainDigest(d storage.Digest) string {
	var soon []string
	for _, x := range d.Upcoming {
		line := fmt.Sprintf("%s <t:%d:R>", x.Title, x.DeleteAfter)
		if x.Episodes > 0 {
			line = fmt.Sprintf("%s, watched episodes (%d) <t:%d:R>", x.Title, x.Episodes, x.DeleteAfter)
		}
		soon = append(soon, line)
	}
	var b strings.Builder
	section(&b, "Going in the next day, unless someone presses Keep", soon)
	return strings.TrimSpace(b.String())
}

// adminDigest is for tech alerts. Empty when nothing needs a look.
func (w *Worker) adminDigest(ctx context.Context, d storage.Digest) string {
	var lines []string
	if d.Failed > 0 {
		lines = append(lines, fmt.Sprintf("%d deletions failed in the last day; see the status page.", d.Failed))
	}
	if d.Held > 0 {
		lines = append(lines, fmt.Sprintf("%d deletions are being held back (someone watching, or a check that couldn't run).", d.Held))
	}
	for _, s := range d.SyncErrors {
		lines = append(lines, "Sync of "+s.Source+" is failing: "+s.LastError)
	}
	if d.NewFindings > 0 {
		lines = append(lines, fmt.Sprintf("Doctor: %d new, %d open.", d.NewFindings, d.OpenFindings))
	}
	if d.PendingAsks > 0 {
		lines = append(lines, fmt.Sprintf("%d requests waiting on the admin page.", d.PendingAsks))
	}
	if d.UnlinkedDisc+d.UnlinkedPlex > 0 {
		lines = append(lines, fmt.Sprintf("Unlinked: %d Discord users, %d Plex accounts.", d.UnlinkedDisc, d.UnlinkedPlex))
	}
	disks, over := w.diskLines(ctx)
	// Disk levels alone aren't news; they ride along with something that is.
	if len(lines) == 0 && !over {
		return ""
	}
	if d.SpaceQueue > 0 {
		disks = append(disks, fmt.Sprintf("%d space-based items waiting for a disk to fill.", d.SpaceQueue))
	}
	var b strings.Builder
	section(&b, "Needs a look", lines)
	section(&b, "Disks", disks)
	return strings.TrimSpace(b.String())
}

// diskLines reports each disk holding a library, once per filesystem, with
// any download folders on it. over says one has reached the start of the
// space range.
func (w *Worker) diskLines(ctx context.Context) (lines []string, over bool) {
	if w.Disks == nil {
		return nil, false
	}
	disks, _, err := w.Disks.Space(ctx)
	if err != nil {
		return []string{"couldn't read disk space: " + err.Error()}, false
	}
	for _, d := range disks {
		if d.DownloadsOnly {
			lines = append(lines, fmt.Sprintf("%s: %d%% used, %s free (downloads only)", d.Path, d.Used, gigabytes(d.Free)))
		} else {
			over = over || d.Used >= d.Range.Start
			lines = append(lines, fmt.Sprintf("%s: %d%% used, %s free (deletions start at %d%%, clearing to %d%%)",
				d.Path, d.Used, gigabytes(d.Free), d.Range.Start, d.Range.Floor))
		}
		for _, u := range d.Downloads {
			lines = append(lines, fmt.Sprintf("%s on that disk holds %s.", u.Path, gigabytes(u.Bytes)))
		}
	}
	return lines, over
}
