//go:build linux || darwin

package metrics

import "syscall"

// disk reports used and total bytes for the filesystem holding path, matching
// the statfs figures /api/system already exposes.
func disk(path string) (used, total uint64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0
	}
	bsize := uint64(st.Bsize)
	total = uint64(st.Blocks) * bsize
	free := uint64(st.Bavail) * bsize
	if free > total {
		return 0, total
	}
	return total - free, total
}
