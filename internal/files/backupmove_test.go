package files

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/backup"
	"gitea.lan/matthew/fog/internal/jobs"
)

// withBackups wires m to a backup manager over the same store, the way
// fogd does, and returns it.
func withBackups(t *testing.T, m *Manager) *backup.Manager {
	t.Helper()
	bk := &backup.Manager{DataDir: m.DataDir, Store: m.Store}
	m.BackupRoot = bk.Root
	m.HoldBackupDir = bk.HoldDir
	m.LockBackupDir = bk.LockDir
	return bk
}

func put(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Moving the backup directory carries an app snapshot, two Drive snapshots
// and a kit there, re-points every record at the new place, and leaves
// the setting naming it; moving back to the default clears the setting.
func TestBackupMove(t *testing.T) {
	m := newManager(t)
	bk := withBackups(t, m)
	cur := m.DefaultBackupDir()

	app, err := m.Store.CreateApp("sonarr")
	if err != nil {
		t.Fatal(err)
	}
	snap := filepath.Join(cur, "sonarr", "20261027T030000Z")
	put(t, filepath.Join(snap, "artifact.tar"), "artifact")
	bid, err := m.Store.CreateBackup(app.ID, "scheduled", snap, 8)
	if err != nil {
		t.Fatal(err)
	}
	drive := filepath.Join(cur, backup.FilesDirName, "20261027T031500Z")
	put(t, filepath.Join(drive, "a.txt"), "a")
	fid, err := m.Store.CreateFileSnapshot(drive, "links", 1, 1, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(cur, backup.KitsDirName, "kit.enc"), "kit")

	to := filepath.Join(t.TempDir(), "nas", "backups")
	job, err := m.MoveBackups(to, "move")
	if err != nil {
		t.Fatal(err)
	}
	if j := waitJob(t, m, job.ID); j.State != "done" {
		t.Fatalf("job = %+v", j)
	}
	if bk.Root() != to {
		t.Errorf("backup root = %q, want %q", bk.Root(), to)
	}
	if b, err := m.Store.BackupByID(bid); err != nil || b.Path != filepath.Join(to, "sonarr", "20261027T030000Z") {
		t.Errorf("backup row: %v %+v", err, b)
	}
	if f, err := m.Store.FileSnapshotByID(fid); err != nil || f.Path != filepath.Join(to, backup.FilesDirName, "20261027T031500Z") {
		t.Errorf("files snapshot row: %v %+v", err, f)
	}
	for _, rel := range []string{"sonarr/20261027T030000Z/artifact.tar", "files/20261027T031500Z/a.txt", "fog-recovery/kit.enc"} {
		if _, err := os.Stat(filepath.Join(to, rel)); err != nil {
			t.Errorf("%s did not move: %v", rel, err)
		}
	}
	if _, err := os.Stat(cur); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("old directory still there: %v", err)
	}
	p := m.BackupPlacement()
	if p.Location != to || p.Default || !p.Bound {
		t.Errorf("placement = %+v", p)
	}

	job, err = m.MoveBackups("", "move")
	if err != nil {
		t.Fatal(err)
	}
	if j := waitJob(t, m, job.ID); j.State != "done" {
		t.Fatalf("job back = %+v", j)
	}
	if v, _ := m.Store.GetSetting(backup.SettingBackupDir); v != "" {
		t.Errorf("setting after moving back = %q, want empty", v)
	}
	if b, _ := m.Store.BackupByID(bid); b.Path != snap {
		t.Errorf("backup row after moving back = %q, want %q", b.Path, snap)
	}
	if !m.BackupPlacement().Default {
		t.Error("placement does not say default after moving back")
	}
}

// Adopting re-points only the records whose snapshot is at the new place
// and leaves the old directory alone.
func TestBackupAdopt(t *testing.T) {
	m := newManager(t)
	withBackups(t, m)
	cur := m.DefaultBackupDir()
	app, _ := m.Store.CreateApp("sonarr")
	copied := filepath.Join(cur, "sonarr", "20261026T030000Z")
	left := filepath.Join(cur, "sonarr", "20261027T030000Z")
	put(t, filepath.Join(copied, "artifact.tar"), "old")
	put(t, filepath.Join(left, "artifact.tar"), "new")
	copiedID, _ := m.Store.CreateBackup(app.ID, "scheduled", copied, 3)
	leftID, _ := m.Store.CreateBackup(app.ID, "scheduled", left, 3)

	to := filepath.Join(t.TempDir(), "nas")
	put(t, filepath.Join(to, "sonarr", "20261026T030000Z", "artifact.tar"), "old")

	if _, err := m.MoveBackups(to, "move"); err == nil {
		t.Error("a move into a folder with backups in it was allowed")
	}
	job, err := m.MoveBackups(to, "adopt")
	if err != nil {
		t.Fatal(err)
	}
	if j := waitJob(t, m, job.ID); j.State != "done" {
		t.Fatalf("job = %+v", j)
	}
	if b, _ := m.Store.BackupByID(copiedID); b.Path != filepath.Join(to, "sonarr", "20261026T030000Z") {
		t.Errorf("copied backup row = %q", b.Path)
	}
	if b, _ := m.Store.BackupByID(leftID); b.Path != left {
		t.Errorf("backup not at the new place was re-pointed: %q", b.Path)
	}
	if _, err := os.Stat(filepath.Join(left, "artifact.tar")); err != nil {
		t.Errorf("adopt touched the old place: %v", err)
	}
}

// The new place may not overlap the current one, Drive, or fog's own
// trees.
func TestBackupMoveRefusals(t *testing.T) {
	m := newManager(t)
	withBackups(t, m)
	for name, to := range map[string]string{
		"relative":       "backups",
		"inside current": filepath.Join(m.DefaultBackupDir(), "nested"),
		"inside Drive":   filepath.Join(m.Root(), "Backups"),
		"fog's own tree": filepath.Join(m.DataDir, "postgres", "b"),
		"the data dir":   filepath.Dir(m.DataDir),
	} {
		if _, err := m.MoveBackups(to, "move"); err == nil {
			t.Errorf("%s: %s was allowed", name, to)
		}
	}
	if _, err := m.MoveBackups("", "move"); err == nil {
		t.Error("moving to where backups already are was allowed")
	}
}

// A move waits for work holding the directory, and work that comes while
// it waits or runs is turned away rather than queued behind it.
func TestBackupMoveWaitsForHolders(t *testing.T) {
	m := newManager(t)
	bk := withBackups(t, m)
	release, err := bk.HoldDir()
	if err != nil {
		t.Fatal(err)
	}
	job, err := m.MoveBackups(filepath.Join(t.TempDir(), "nas"), "move")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !bk.Moving() {
		if time.Now().After(deadline) {
			t.Fatal("the move never started waiting")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := bk.HoldDir(); !errors.Is(err, backup.ErrDirMoving) {
		t.Errorf("hold while a move waits: %v", err)
	}
	if m.BackupPlacement().Bound {
		t.Error("placement reads available while the move runs")
	}
	if _, err := m.StartSnapshot("test"); err == nil {
		t.Error("a Drive snapshot started beside the move")
	}
	if j, _ := m.Jobs.Get(job.ID); j.State != "running" {
		t.Fatalf("the move finished while the directory was held: %+v", j)
	}
	release()
	if j := waitJob(t, m, job.ID); j.State != "done" {
		t.Fatalf("job = %+v", j)
	}
	if bk.Moving() {
		t.Error("still moving after the job")
	}
	r, err := bk.HoldDir()
	if err != nil {
		t.Fatalf("hold after the move: %v", err)
	}
	r()
}

// A copy across disks keeps hard links: a Drive snapshot's unchanged
// files are links to the one before, and a copy that broke them would
// cost the whole Drive once per night kept.
func TestCopyTreeKeepsHardLinks(t *testing.T) {
	m := newManager(t)
	from := filepath.Join(t.TempDir(), "from")
	put(t, filepath.Join(from, "s1", "big.bin"), "same bytes")
	if err := os.MkdirAll(filepath.Join(from, "s2"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(from, "s1", "big.bin"), filepath.Join(from, "s2", "big.bin")); err != nil {
		t.Skip("no hard links here:", err)
	}
	to := filepath.Join(t.TempDir(), "to")
	if err := os.MkdirAll(to, 0o755); err != nil {
		t.Fatal(err)
	}
	job := m.Jobs.Start("copy", "copy", func(ctx context.Context, p *jobs.Progress) error {
		return copyTree(ctx, from, to, "", p)
	})
	if j := waitJob(t, m, job.ID); j.State != "done" {
		t.Fatalf("job = %+v", j)
	}
	a, err1 := os.Stat(filepath.Join(to, "s1", "big.bin"))
	b, err2 := os.Stat(filepath.Join(to, "s2", "big.bin"))
	if err1 != nil || err2 != nil {
		t.Fatal(err1, err2)
	}
	if !os.SameFile(a, b) {
		t.Error("the copy broke the hard link into two files")
	}
}

// Across disks the move copies, verifies, re-points the records and only
// then removes the old copy — the Drive snapshots' hard links intact.
func TestBackupMoveAcrossDisks(t *testing.T) {
	m := newManager(t)
	bk := withBackups(t, m)
	cur := m.DefaultBackupDir()
	to := filepath.Join(t.TempDir(), "nas")
	old := deviceOf
	t.Cleanup(func() { deviceOf = old })
	deviceOf = func(path string) (uint64, error) {
		if IsUnder(path, to) {
			return 2, nil
		}
		return 1, nil
	}

	s1 := filepath.Join(cur, backup.FilesDirName, "s1")
	s2 := filepath.Join(cur, backup.FilesDirName, "s2")
	put(t, filepath.Join(s1, "film.mkv"), "film")
	if err := os.MkdirAll(s2, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(s1, "film.mkv"), filepath.Join(s2, "film.mkv")); err != nil {
		t.Skip("no hard links here:", err)
	}
	id, _ := m.Store.CreateFileSnapshot(s2, "links", 1, 4, 0, 0)

	job, err := m.MoveBackups(to, "move")
	if err != nil {
		t.Fatal(err)
	}
	if j := waitJob(t, m, job.ID); j.State != "done" {
		t.Fatalf("job = %+v", j)
	}
	if bk.Root() != to {
		t.Errorf("backup root = %q", bk.Root())
	}
	if f, _ := m.Store.FileSnapshotByID(id); f.Path != filepath.Join(to, backup.FilesDirName, "s2") {
		t.Errorf("snapshot row = %q", f.Path)
	}
	a, err1 := os.Stat(filepath.Join(to, backup.FilesDirName, "s1", "film.mkv"))
	b, err2 := os.Stat(filepath.Join(to, backup.FilesDirName, "s2", "film.mkv"))
	if err1 != nil || err2 != nil || !os.SameFile(a, b) {
		t.Errorf("hard link not kept: %v %v", err1, err2)
	}
	if _, err := os.Stat(cur); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("old copy not removed: %v", err)
	}
}
