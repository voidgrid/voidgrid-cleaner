package doctor

import (
	"context"
	"slices"

	"github.com/voidgrid/voidgrid-cleaner/internal/clients/arr"
	"github.com/voidgrid/voidgrid-cleaner/internal/storage"
)

// Disk is one disk holding a library.
type Disk struct {
	Path        string
	Used, Limit int
	Free        int64
}

// Space reports each disk holding a library and the space-based items in the
// order the space check would take them.
func (d *Doctor) Space(ctx context.Context) ([]Disk, []storage.SpaceItem, error) {
	limit, err := d.DB.SpaceLimit(ctx)
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
		disks = append(disks, Disk{Path: ds.Path, Used: int((ds.TotalSpace - ds.FreeSpace) * 100 / ds.TotalSpace), Limit: limit, Free: ds.FreeSpace})
	}
	items, err := d.DB.SpaceItems(ctx, d.DryRun)
	if err != nil {
		return nil, nil, err
	}
	var queue []storage.SpaceItem
	for _, it := range items {
		if !it.Active {
			queue = append(queue, it)
		}
	}
	return disks, queue, nil
}
