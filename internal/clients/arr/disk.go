package arr

import "strings"

// sameFSSlack allows for writes between the Radarr and Sonarr answers.
const sameFSSlack = 1 << 30

// DiskFor finds the disk holding path: the longest mount that contains it.
// "/" never counts: it's the reporting app's own container, not media, so a
// path outside every real mount has no known disk.
func DiskFor(disks []DiskSpace, path string) (DiskSpace, bool) {
	var best DiskSpace
	found := false
	for _, d := range disks {
		if d.Path == "/" || d.TotalSpace <= 0 || path == "" {
			continue
		}
		mount := strings.TrimSuffix(d.Path, "/") + "/"
		if strings.HasPrefix(path+"/", mount) && (!found || len(d.Path) > len(best.Path)) {
			best, found = d, true
		}
	}
	return best, found
}

// Canonical names a disk by the first entry in all on the same filesystem,
// so every caller gives one disk the same name however it was reached.
func Canonical(all []DiskSpace, d DiskSpace) DiskSpace {
	for _, o := range all {
		if o.Path != "/" && SameFS(o, d) {
			return o
		}
	}
	return d
}

// SameFS spots one filesystem seen through two mounts, such as a movies and a
// TV folder on one pool, so its use isn't counted once for each. Two matching
// disks would be taken as one, which only ever deletes less.
func SameFS(a, b DiskSpace) bool {
	diff := a.FreeSpace - b.FreeSpace
	return a.TotalSpace == b.TotalSpace && diff > -sameFSSlack && diff < sameFSSlack
}
