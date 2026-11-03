package files

import (
	"os"
	"testing"
)

// TestMain gives every test a roomy disk, so snapshot tests don't fail on
// a developer machine that happens to be past the 5% free-space floor.
// Tests about the floor itself stub their own numbers.
func TestMain(m *testing.M) {
	diskSpace = func(string) (int64, int64, bool) { return 1 << 40, 2 << 40, true }
	os.Exit(m.Run())
}

func stubDiskSpace(t *testing.T, avail, total int64) {
	t.Helper()
	prev := diskSpace
	diskSpace = func(string) (int64, int64, bool) { return avail, total, true }
	t.Cleanup(func() { diskSpace = prev })
}
