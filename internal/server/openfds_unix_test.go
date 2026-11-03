//go:build darwin || linux

package server

import (
	"os"
	"strconv"
	"syscall"
	"testing"
)

// openFDsOn counts this process's descriptors open on the file at path,
// matched by device and inode so a handle survives the file being renamed.
func openFDsOn(t *testing.T, path string) int {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	want := info.Sys().(*syscall.Stat_t)
	ents, err := os.ReadDir("/dev/fd")
	if err != nil {
		t.Skip("no /dev/fd:", err)
	}
	n := 0
	for _, e := range ents {
		fd, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		var st syscall.Stat_t
		if syscall.Fstat(fd, &st) == nil && st.Dev == want.Dev && st.Ino == want.Ino {
			n++
		}
	}
	return n
}
