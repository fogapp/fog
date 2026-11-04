package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/store"
)

// Every backup is copied into the store and read back; the row says so.
func TestBackupIsCopiedIntoStore(t *testing.T) {
	fakeDocker(t, "0")
	m, st := legacyBackupManager(t)
	t.Cleanup(m.CloseChunkStore)
	ctx := context.Background()

	id, err := m.BackupApp(ctx, "demo", "manual")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := st.BackupByID(id)
	if b.StoreSnapshot == "" || b.StoreVerifiedAt == "" || b.StoreError != "" {
		t.Fatalf("row after backup: snapshot %q verified %q error %q", b.StoreSnapshot, b.StoreVerifiedAt, b.StoreError)
	}
	cs, err := m.ChunkStore()
	if err != nil {
		t.Fatal(err)
	}
	snap, err := cs.Snapshot(b.StoreSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Series != "app/demo" || snap.Stamp != filepath.Base(b.Path) {
		t.Fatalf("snapshot %+v", snap)
	}
	var meta StoreMeta
	json.Unmarshal(snap.Meta, &meta)
	if meta.App != "demo" || meta.Mode != "volumes" || meta.Kind != "manual" {
		t.Fatalf("meta %+v", meta)
	}
	for _, name := range []string{"meta.json", "volume-data.tar"} {
		want, _ := os.ReadFile(filepath.Join(b.Path, name))
		e, err := cs.Find(snap, name)
		if err != nil {
			t.Fatal(err)
		}
		r, _ := cs.Open(e)
		got, err := io.ReadAll(r)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s from the store differs: %v", name, err)
		}
	}

	// Deleting the backup deletes its store snapshot too.
	if err := m.DeleteBackup(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.Snapshot(b.StoreSnapshot); err == nil {
		t.Fatal("store snapshot outlived its backup")
	}
}

// A delegated backup's store copy is checked against the digest fog
// verified at download; a store that hands back other bytes is reported.
func TestStoreCopyCheckedAgainstArtifactDigest(t *testing.T) {
	m, st := legacyBackupManager(t)
	t.Cleanup(m.CloseChunkStore)
	app, _ := st.AppByName("demo")
	dir := filepath.Join(m.baseDir(), "demo", "20261104-030000")
	os.MkdirAll(dir, 0o700)
	artifact := []byte("pretend this is a fog-backup tar")
	os.WriteFile(filepath.Join(dir, "artifact.tar"), artifact, 0o600)
	sum := sha256.Sum256(artifact)

	good, _ := st.CreateArtifactBackup(app.ID, "scheduled", dir, 1, ArtifactFormat, hex.EncodeToString(sum[:]), `{"format":"fog-backup/1","app":"demo"}`)
	m.storeBackup(context.Background(), "demo", good)
	if b, _ := st.BackupByID(good); b.StoreVerifiedAt == "" || b.StoreError != "" {
		t.Fatalf("matching artifact: %+v", b)
	}

	bad, _ := st.CreateArtifactBackup(app.ID, "scheduled", dir, 1, ArtifactFormat, strings.Repeat("0", 64), `{}`)
	m.storeBackup(context.Background(), "demo", bad)
	b, _ := st.BackupByID(bad)
	if b.StoreVerifiedAt != "" || !strings.Contains(b.StoreError, "not the") {
		t.Fatalf("mismatched artifact should be reported: %+v", b)
	}
}

// Removing an app removes its snapshots from the store and no others.
func TestRemoveAppBackupsClearsItsSeries(t *testing.T) {
	m, _ := legacyBackupManager(t)
	t.Cleanup(m.CloseChunkStore)
	cs, err := m.ChunkStore()
	if err != nil {
		t.Fatal(err)
	}
	put := func(series string) string {
		w, _ := cs.Begin(series)
		w.AddBytes("f", 0o600, []byte(series))
		s, err := w.Commit("20261104-030000", s0, nil)
		if err != nil {
			t.Fatal(err)
		}
		return s.ID
	}
	demo, other := put("app/demo"), put("app/other")
	if err := m.RemoveAppBackups("demo"); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.Snapshot(demo); err == nil {
		t.Fatal("removed app's snapshot survived")
	}
	if _, err := cs.Snapshot(other); err != nil {
		t.Fatalf("another app's snapshot went too: %v", err)
	}
}

var s0 = time.Date(2026, 11, 4, 3, 0, 0, 0, time.UTC)

// The store refuses to start on a NAS share that is away.
func TestChunkStoreRefusesAbsentShare(t *testing.T) {
	m, st := legacyBackupManager(t)
	if _, err := st.CreateNasShare(store.NasShare{
		Name: "backups", Proto: "nfs", Server: "nas.lan",
		Remote: "/volume1/backups", MountPoint: "/mnt/nas-test-nonexistent", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	st.SetSetting(SettingBackupDir, "/mnt/nas-test-nonexistent/backups")
	if _, err := m.ChunkStore(); err == nil {
		t.Fatal("opened a store on an unmounted share")
	}
	if _, err := os.Stat("/mnt/nas-test-nonexistent"); err == nil {
		t.Fatal("made a directory on the bare mountpoint")
	}
}
