package files

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/chunkstore"
	"gitea.lan/matthew/fog/internal/jobs"
	fogstore "gitea.lan/matthew/fog/internal/store"
)

func storeManager(t *testing.T) (*Manager, *chunkstore.Store, string) {
	t.Helper()
	m := newManager(t)
	backups := t.TempDir()
	m.BackupRoot = func() string { return backups }
	cs, err := chunkstore.Open(filepath.Join(backups, "fog-store"), chunkstore.Options{
		IndexPath: filepath.Join(t.TempDir(), "ix.db"), Create: true,
		Params: chunkstore.Params{Min: 2 << 10, Avg: 8 << 10, Max: 32 << 10}, PackSize: 64 << 10})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	m.ChunkStore = func() (*chunkstore.Store, error) { return cs, nil }
	return m, cs, backups
}

func bigContent(seed byte, n int) string {
	b := make([]byte, n)
	x := uint32(seed) + 1
	for i := range b {
		x = x*1664525 + 1013904223
		b[i] = byte(x >> 24)
	}
	return string(b)
}

// Drive snapshots land in the store: excluded folders left out, unchanged
// files carried over unread, and a big file that changes a little costs the
// little.
func TestStoreSnapshotsAreIncremental(t *testing.T) {
	m, cs, backups := storeManager(t)
	ctx := context.Background()
	big := bigContent(1, 600<<10)
	write(t, m, "docs/a.txt", "aaaa")
	write(t, m, "media/big.bin", big)
	write(t, m, "skip/secret.txt", "s")
	if err := m.SetExcluded("skip", true); err != nil {
		t.Fatal(err)
	}
	p := jobs.NewProgress()
	first, err := m.RunSnapshot(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if first.Mode != ModeStore || first.StoreSnapshot == "" || first.Files != 2 || first.Bytes != int64(4+len(big)) {
		t.Fatalf("first = %+v", first)
	}
	if _, err := os.Stat(filepath.Join(backups, "files")); err == nil {
		t.Fatal("a hardlink tree was written beside the store")
	}

	time.Sleep(20 * time.Millisecond)
	edited := []byte(big)
	copy(edited[300<<10:], "a small edit")
	write(t, m, "media/big.bin", string(edited))
	second, err := m.RunSnapshot(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if second.NewBytes*4 > first.NewBytes {
		t.Fatalf("second night stored %d, first %d: a small edit should cost a sliver", second.NewBytes, first.NewBytes)
	}
	snaps, _ := m.ListSnapshots()
	if len(snaps) != 2 {
		t.Fatalf("snapshots %d", len(snaps))
	}

	// Browse the first: the old content is there.
	l, err := m.SnapshotList(first.ID, "", ListOpts{})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, n := range l.Entries {
		names = append(names, n.Name)
	}
	if len(names) != 2 || names[0] != "docs" || names[1] != "media" {
		t.Fatalf("root listing %v", names)
	}
	f, node, err := m.SnapshotOpenFile(first.ID, "media/big.bin")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(f)
	f.Close()
	if string(got) != big || node.Size != int64(len(big)) {
		t.Fatal("first snapshot's file reads back wrong")
	}
	if _, err := m.SnapshotList(first.ID, "skip", ListOpts{}); err == nil {
		t.Fatal("excluded folder is in the snapshot")
	}
	_ = cs
}

// Restoring from a store snapshot puts a deleted file back.
func TestStoreSnapshotRestore(t *testing.T) {
	m, _, _ := storeManager(t)
	ctx := context.Background()
	write(t, m, "docs/a.txt", "precious")
	write(t, m, "docs/sub/b.txt", "also")
	row, err := m.RunSnapshot(ctx, jobs.NewProgress())
	if err != nil {
		t.Fatal(err)
	}
	os.RemoveAll(filepath.Join(m.Root(), "docs"))
	job, err := m.RestoreSnapshot(row.ID, []string{"docs"}, "", false, ConflictRename)
	if err != nil {
		t.Fatal(err)
	}
	if done := waitJob(t, m, job.ID); done.State == "failed" {
		t.Fatalf("restore failed: %s", done.Error)
	}
	for rel, want := range map[string]string{"docs/a.txt": "precious", "docs/sub/b.txt": "also"} {
		got, err := os.ReadFile(filepath.Join(m.Root(), rel))
		if err != nil || string(got) != want {
			t.Fatalf("%s: %q %v", rel, got, err)
		}
	}
}

// The weekly check reads a sample back and marks the snapshot verified;
// damage is reported, not ignored.
func TestStoreSnapshotVerify(t *testing.T) {
	m, cs, _ := storeManager(t)
	write(t, m, "a.txt", bigContent(2, 100<<10))
	row, err := m.RunSnapshot(context.Background(), jobs.NewProgress())
	if err != nil {
		t.Fatal(err)
	}
	m.VerifySnapshots(context.Background(), time.Now())
	if r, _ := m.Store.FileSnapshotByID(row.ID); r.VerifiedAt == "" {
		t.Fatal("not verified")
	}
	// Damage every pack, then verify again a week later.
	st, _ := cs.Stats()
	_ = st
	filepath.Walk(filepath.Join(cs.Dir(), "packs"), func(p string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() {
			f, _ := os.OpenFile(p, os.O_RDWR, 0)
			f.WriteAt(bytes.Repeat([]byte{0xaa}, 512), 16)
			f.Close()
		}
		return nil
	})
	m.Store.DB().Exec(`UPDATE file_snapshots SET verified_at = ''`)
	m.VerifySnapshots(context.Background(), time.Now())
	if r, _ := m.Store.FileSnapshotByID(row.ID); r.VerifiedAt != "" {
		t.Fatal("damaged snapshot marked verified")
	}
}

// Hardlink snapshots from before the store are brought into it and their
// trees removed; history carries over.
func TestImportClassicSnapshots(t *testing.T) {
	m, cs, backups := storeManager(t)
	ctx := context.Background()
	// Two classic nights, the way the old code wrote them.
	store := m.ChunkStore
	m.ChunkStore = nil
	write(t, m, "a.txt", "night one")
	write(t, m, "keep.txt", "unchanged")
	one, err := m.RunSnapshot(ctx, jobs.NewProgress())
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	write(t, m, "a.txt", "night two")
	two, err := m.RunSnapshot(ctx, jobs.NewProgress())
	if err != nil {
		t.Fatal(err)
	}
	m.ChunkStore = store
	// Tonight's snapshot goes to the store and imports the old ones.
	if _, err := m.RunSnapshot(ctx, jobs.NewProgress()); err != nil {
		t.Fatal(err)
	}
	for _, old := range []int64{one.ID, two.ID} {
		r, err := m.Store.FileSnapshotByID(old)
		if err != nil || r.Mode != ModeStore || r.StoreSnapshot == "" {
			t.Fatalf("not imported: %+v %v", r, err)
		}
		if _, err := os.Stat(r.Path); err == nil {
			t.Fatalf("imported tree %s still on disk", r.Path)
		}
	}
	f, _, err := m.SnapshotOpenFile(one.ID, "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(f)
	f.Close()
	if string(got) != "night one" {
		t.Fatalf("imported night one reads %q", got)
	}
	if _, err := os.Stat(filepath.Join(backups, "files")); err == nil {
		t.Fatal("the old snapshots folder is still there")
	}
	_, se, _ := m.storeEntries(cs, mustRow(t, m, two.ID))
	if kids := se.sortedKids(""); len(kids) != 2 {
		t.Fatalf("night two holds %v", kids)
	}
}

func mustRow(t *testing.T, m *Manager, id int64) *fogstore.FileSnapshot {
	t.Helper()
	r, err := m.Store.FileSnapshotByID(id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// Importing an SMB mirror keeps what it had set aside as deleted, until it
// is 30 days old.
func TestMirrorImportKeepsDeletions(t *testing.T) {
	m, _, backups := storeManager(t)
	ctx := context.Background()
	mirror := filepath.Join(backups, "files", "mirror")
	stamp := time.Now().UTC().Format("20060102-150405")
	os.MkdirAll(filepath.Join(mirror, ".deleted", stamp), 0o755)
	os.WriteFile(filepath.Join(mirror, ".deleted", stamp, "oops.txt"), []byte("deleted yesterday"), 0o644)
	os.WriteFile(filepath.Join(mirror, "here.txt"), []byte("still here"), 0o644)
	os.WriteFile(filepath.Join(mirror, ".fog-snapshot.json"), []byte(`{"format":"fog-files-snapshot/1","files":[]}`), 0o644)
	if _, err := m.Store.CreateFileSnapshot(mirror, "mirror", 1, 10, 10, 0); err != nil {
		t.Fatal(err)
	}
	write(t, m, "here.txt", "still here")
	if _, err := m.RunSnapshot(ctx, jobs.NewProgress()); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(backups, "files", keptDeletedDir, stamp, "oops.txt"))
	if err != nil || string(got) != "deleted yesterday" {
		t.Fatalf("set-aside deletion lost: %q %v", got, err)
	}
	if _, err := os.Stat(mirror); err == nil {
		t.Fatal("mirror tree not removed after import")
	}
}
