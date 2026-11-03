package files

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"gitea.lan/matthew/fog/internal/jobs"
)

// A snapshot of many small files asks for the backup disk's free space a
// handful of times, not once per file: each ask is a statfs that, on a
// NAS, costs a goroutine and a timer.
func TestSnapshotChecksSpaceInStrides(t *testing.T) {
	m := newManager(t)
	backups := t.TempDir()
	m.BackupRoot = func() string { return backups }
	for i := range 40 {
		write(t, m, fmt.Sprintf("docs/%02d.txt", i), "some bytes")
	}
	var calls atomic.Int64
	prev := diskSpace
	diskSpace = func(string) (int64, int64, bool) { calls.Add(1); return 1 << 40, 2 << 40, true }
	t.Cleanup(func() { diskSpace = prev })
	if _, err := m.RunSnapshot(context.Background(), jobs.NewProgress()); err != nil {
		t.Fatal(err)
	}
	// One for the preflight, one when copying starts.
	if n := calls.Load(); n > 2 {
		t.Errorf("free space checked %d times for 40 files", n)
	}
}

// The disk filling up after the preflight (another writer on it) still
// stops the copy at the floor, with the notice, and leaves no partial tree.
func TestSnapshotStopsWhenTheDiskFillsMidRun(t *testing.T) {
	m := newManager(t)
	backups := t.TempDir()
	m.BackupRoot = func() string { return backups }
	write(t, m, "docs/a.txt", strings.Repeat("a", 4096))
	var calls atomic.Int64
	prev := diskSpace
	diskSpace = func(string) (int64, int64, bool) {
		if calls.Add(1) == 1 {
			return 50 << 30, 100 << 30, true // the preflight: plenty
		}
		return 5 << 30, 100 << 30, true // then: exactly at the floor
	}
	t.Cleanup(func() { diskSpace = prev })
	if _, err := m.RunSnapshot(context.Background(), jobs.NewProgress()); !errors.Is(err, ErrBackupDiskFull) {
		t.Fatalf("err = %v", err)
	}
	entries, _ := os.ReadDir(filepath.Join(backups, "files"))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".partial") {
			t.Fatalf("a partial snapshot was left: %s", e.Name())
		}
	}
	notices, _ := m.Store.ActiveNotices()
	found := false
	for _, n := range notices {
		found = found || n.Kind == NoticeFilesBackupNoSpace
	}
	if !found {
		t.Error("no disk-full notice")
	}
}

// The gauge counts copies down from what it last read, reads again after
// a stride or when the next file would not fit, and refuses only when a
// fresh read agrees.
func TestSpaceGauge(t *testing.T) {
	floor := freeFloor(100 << 30) // 5 GiB on a 100 GiB disk
	avail := floor + 10<<30
	reads := 0
	prev := diskSpace
	diskSpace = func(string) (int64, int64, bool) { reads++; return avail, 100 << 30, true }
	t.Cleanup(func() { diskSpace = prev })
	reserve := func(g *spaceGauge, n int, size int64) {
		t.Helper()
		for range n {
			if err := g.reserve(size); err != nil {
				t.Fatal(err)
			}
		}
	}
	g := &spaceGauge{dir: t.TempDir()}
	reserve(g, 200, 1<<20)
	if reads != 1 {
		t.Errorf("200 MiB in 1 MiB files: %d reads, want 1", reads)
	}
	// Past the stride it looks again, room or not.
	reserve(g, 100, 1<<20)
	if reads != 2 {
		t.Errorf("after the stride: %d reads, want 2", reads)
	}

	// A file bigger than the room last seen makes it look again, and the
	// disk freed meanwhile lets it through.
	reads, avail = 0, floor+300<<20
	g = &spaceGauge{dir: t.TempDir()}
	reserve(g, 1, 200<<20)
	avail = floor + 2<<30
	if err := g.reserve(1 << 30); err != nil {
		t.Fatalf("a file that fits after a re-read: %v", err)
	}
	if reads != 2 {
		t.Errorf("a file past the room: %d reads, want 2", reads)
	}
	if err := g.reserve(4 << 30); !errors.Is(err, ErrBackupDiskFull) {
		t.Fatalf("a file that cannot fit: %v", err)
	}
}
