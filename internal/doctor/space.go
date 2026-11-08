package doctor

import (
	"context"
	"io/fs"
	"path/filepath"
	"slices"

	"github.com/voidgrid/voidgrid-cleaner/internal/clients/arr"
	"github.com/voidgrid/voidgrid-cleaner/internal/storage"
)

// Disk is one disk holding a library.
type Disk struct {
	Path  string
	Used  int
	Range storage.SpaceRange
	Free  int64
	// Downloads are download folders on the disk the app can see, such as
	// torrents still seeding.
	Downloads []Usage
	// DownloadsOnly is a disk holding download folders and no library, so
	// deletions can't free it and it has no range.
	DownloadsOnly bool
}

// Usage is how much a folder holds.
type Usage struct {
	Path  string
	Bytes int64
}

// Space reports each disk holding a library and the space-based items in the
// order the space check would take them first.
func (d *Doctor) Space(ctx context.Context) ([]Disk, []storage.SpaceItem, error) {
	ranges, err := d.DB.SpaceRanges(ctx)
	if err != nil {
		return nil, nil, err
	}
	paths, err := d.DB.LibraryPaths(ctx)
	if err != nil {
		return nil, nil, err
	}
	var all []arr.DiskSpace
	for _, a := range []ArrAPI{d.Radarr, d.Sonarr} {
		ds, err := a.DiskSpace(ctx)
		if err != nil {
			return nil, nil, err
		}
		all = append(all, ds...)
	}
	var disks []Disk
	var seen []arr.DiskSpace
	for _, p := range paths {
		if !p.Media {
			continue
		}
		ds, ok := arr.DiskFor(all, p.Path)
		if !ok || slices.ContainsFunc(seen, func(o arr.DiskSpace) bool { return arr.SameFS(o, ds) }) {
			continue
		}
		seen = append(seen, ds)
		ds = arr.Canonical(all, ds) // the name the space check and the settings use
		disk := Disk{Path: ds.Path, Used: int((ds.TotalSpace - ds.FreeSpace) * 100 / ds.TotalSpace), Range: ranges.For(ds.Path), Free: ds.FreeSpace}
		for _, o := range paths {
			if od, ok := arr.DiskFor(all, o.Path); ok && !o.Media && o.Visible && arr.SameFS(od, ds) {
				disk.Downloads = append(disk.Downloads, Usage{o.Path, folderSize(storage.Local(paths, o.Path))})
			}
		}
		disks = append(disks, disk)
	}
	// Download folders elsewhere are reported on their own disk.
	for _, o := range paths {
		od, ok := arr.DiskFor(all, o.Path)
		if !ok || o.Media || !o.Visible || slices.ContainsFunc(seen, func(s arr.DiskSpace) bool { return arr.SameFS(s, od) }) {
			continue
		}
		od = arr.Canonical(all, od)
		i := slices.IndexFunc(disks, func(d Disk) bool { return d.DownloadsOnly && d.Path == od.Path })
		if i < 0 {
			disks = append(disks, Disk{Path: od.Path, Used: int((od.TotalSpace - od.FreeSpace) * 100 / od.TotalSpace), Free: od.FreeSpace, DownloadsOnly: true})
			i = len(disks) - 1
		}
		disks[i].Downloads = append(disks[i].Downloads, Usage{o.Path, folderSize(storage.Local(paths, o.Path))})
	}
	items, err := d.DB.SpaceItems(ctx, d.DryRun)
	if err != nil {
		return nil, nil, err
	}
	var queue []storage.SpaceItem
	for _, it := range items {
		if it.Ticked && !it.Active {
			queue = append(queue, it)
		}
	}
	return disks, queue, nil
}

// folderSize adds up the files under dir, skipping what it can't read.
// shortcut: walks on every call (the torrents folder is small); cache it if pages slow down.
func folderSize(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, e fs.DirEntry, err error) error {
		if err == nil && e.Type().IsRegular() {
			if info, err := e.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}
