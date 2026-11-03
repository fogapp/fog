package setup

import "syscall"

// duBytes reports how full the filesystem behind path is — the target root
// is its own mount, so this is exactly the extracted-so-far byte count.
func duBytes(path string) int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0
	}
	return int64(st.Blocks-st.Bfree) * int64(st.Bsize)
}
