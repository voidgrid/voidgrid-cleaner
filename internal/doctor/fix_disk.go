package doctor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/voidgrid/voidgrid-cleaner/internal/clients/arr"
	"github.com/voidgrid/voidgrid-cleaner/internal/clients/httpjson"
	"github.com/voidgrid/voidgrid-cleaner/internal/storage"
)

// leftoverFile is what Sonarr leaves in a folder when a series goes. A folder
// holding only this counts as empty.
const leftoverFile = ".plexmatch"

func isDiskFix(f storage.Finding) bool {
	return f.Kind == KindEmptyFolder || f.Kind == KindStrayFile || f.Kind == KindStaleRequest
}

func (d *Doctor) previewDisk(p *Preview, data fixData) {
	p.CanFix = true
	switch p.Finding.Kind {
	case KindStaleRequest:
		p.Lines = []string{"Remove " + p.Finding.Title + " and its requests from Seerr, so it can be requested again.", "Nothing changes in Radarr, Sonarr, Plex or on disk."}
	case KindEmptyFolder:
		p.Lines = []string{"Remove " + data.Folder + ". It holds no media.", "If anything else has appeared in it by then, it's left alone."}
	case KindStrayFile:
		p.Lines = []string{"Delete " + data.Folder + " for good. Radarr and Sonarr have no recycle bin here, so it can't be undone."}
		if info, err := os.Stat(data.Folder); err == nil {
			p.Lines = append(p.Lines, fmt.Sprintf("It's %.1f GB.", float64(info.Size())/1e9))
		}
		if d.DryRun {
			p.Lines = append(p.Lines, "This is a dry run, so it will only report what it would delete.")
		}
	}
}

func (d *Doctor) applyDisk(ctx context.Context, f storage.Finding, data fixData) (string, error) {
	switch f.Kind {
	case KindStaleRequest:
		return d.applyStale(ctx, data)
	case KindEmptyFolder:
		return d.applyEmptyFolder(ctx, data)
	}
	return d.applyStray(ctx, data)
}

func (d *Doctor) applyStale(ctx context.Context, data fixData) (string, error) {
	var se *httpjson.StatusError
	if err := d.Seerr.DeleteMedia(ctx, data.MediaID); err != nil && (!errors.As(err, &se) || se.Code != http.StatusNotFound) {
		return "", err
	}
	if err := d.DB.ForgetSeerrMedia(ctx, data.MediaID); err != nil {
		return "", err
	}
	return "removed from seerr", nil
}

// managedRoot finds the managed, verified, writable path holding p. Nothing on
// disk changes outside one, whatever a finding says.
func (d *Doctor) managedRoot(ctx context.Context, p string) (string, error) {
	paths, err := d.DB.LibraryPaths(ctx)
	if err != nil {
		return "", err
	}
	for _, lp := range paths {
		if lp.Managed && lp.Verified && lp.Writable && strings.HasPrefix(p, lp.Path+"/") {
			return lp.Path, nil
		}
	}
	return "", errors.New("that's not in a managed, verified path")
}

// removeUnder removes names from root, relative to it. os.Root refuses anything
// that would leave root, and Remove won't take a folder that isn't empty.
func removeUnder(root string, names ...string) error {
	r, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	for _, n := range names {
		rel, err := filepath.Rel(root, n)
		if err != nil {
			return err
		}
		if err := r.Remove(rel); err != nil {
			return err
		}
	}
	return nil
}

func (d *Doctor) applyEmptyFolder(ctx context.Context, data fixData) (string, error) {
	root, err := d.managedRoot(ctx, data.Folder)
	if err != nil {
		return "", err
	}
	// Checked again here: something may have claimed the folder since.
	var a ArrAPI = d.Radarr
	if data.App == appSonarr {
		a = d.Sonarr
	}
	roots, err := a.RootFolders(ctx)
	if err != nil {
		return "", err
	}
	if !slices.ContainsFunc(roots, func(r arr.RootFolder) bool { return unmappedIn(r, data.Folder) }) {
		return "", errors.New(data.App + " no longer lists this as untracked")
	}
	if folderKind(data.Folder) != "empty" {
		return "", errors.New("the folder isn't empty any more")
	}
	var targets []string
	if _, err := os.Lstat(filepath.Join(data.Folder, leftoverFile)); err == nil {
		targets = append(targets, filepath.Join(data.Folder, leftoverFile))
	}
	if err := removeUnder(root, append(targets, data.Folder)...); err != nil {
		return "", err
	}
	return "removed", nil
}

func unmappedIn(r arr.RootFolder, folder string) bool {
	for _, u := range r.UnmappedFolders {
		if u.Path == folder {
			return true
		}
	}
	return false
}

func (d *Doctor) applyStray(ctx context.Context, data fixData) (string, error) {
	root, err := d.managedRoot(ctx, data.Folder)
	if err != nil {
		return "", err
	}
	tracked, err := d.isTracked(ctx, data)
	if err != nil {
		return "", err
	}
	if tracked {
		return "", errors.New("it's tracked now, so it was left alone")
	}
	if d.DryRun {
		return "dry run: would have deleted " + path.Base(data.Folder), nil
	}
	if err := removeUnder(root, data.Folder); err != nil {
		return "", err
	}
	return "deleted " + path.Base(data.Folder), nil
}

// isTracked asks Radarr or Sonarr again, right before a delete.
func (d *Doctor) isTracked(ctx context.Context, data fixData) (bool, error) {
	if data.App == appSonarr {
		files, err := d.Sonarr.EpisodeFiles(ctx, data.ArrID)
		if err != nil {
			return false, err
		}
		for _, f := range files {
			if f.Path == data.Folder {
				return true, nil
			}
		}
		return false, nil
	}
	m, err := d.Radarr.Movie(ctx, data.ArrID)
	if err != nil {
		return false, err
	}
	return m.MovieFile != nil && path.Join(m.Path, m.MovieFile.RelativePath) == data.Folder, nil
}
