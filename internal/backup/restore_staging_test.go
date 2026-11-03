package backup

import (
	"context"
	"golang.org/x/sys/unix"
	"os"
	"strings"
	"testing"
)

// A share that refuses chown (CIFS, root-squashed NFS) used to fail every
// restore it held, though the app could read and delete the staged files
// perfectly well. The chown failing must defer to a write as the owner.
func TestStagingAccessDefersToTheOwnersWrite(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root's chown succeeds; the refusal is what this exercises")
	}
	swap := canWriteAs
	t.Cleanup(func() { canWriteAs = swap })
	dir := t.TempDir()
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unix.Close(fd) })

	canWriteAs = func(context.Context, string, int, int) (bool, string, error) { return true, "", nil }
	if err := ensureStagingAccess(fd, dir, "65534:65534"); err != nil {
		t.Fatalf("owner can write, chown refused: want staged, got %v", err)
	}

	canWriteAs = func(context.Context, string, int, int) (bool, string, error) { return false, "Permission denied", nil }
	err = ensureStagingAccess(fd, dir, "65534:65534")
	if err == nil || !strings.Contains(err.Error(), "Permission denied") {
		t.Fatalf("owner cannot write: want a refusal naming why, got %v", err)
	}
}
