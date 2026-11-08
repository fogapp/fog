package offsite

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/chunkstore"
	"gitea.lan/matthew/fog/internal/objstore/fakes3"
	"gitea.lan/matthew/fog/internal/store"
)

func noise(seed uint64, n int) []byte {
	r := rand.New(rand.NewPCG(seed, seed+1))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return b
}

type storeHarness struct {
	*harness
	root string
	cs   *chunkstore.Store
	app  *store.App
}

func newStoreHarness(t *testing.T) *storeHarness {
	t.Helper()
	h := newHarness(t, fakes3.New("fog-test"))
	root := t.TempDir()
	h.m.BackupRoot = func() string { return root }
	cs, err := chunkstore.Open(filepath.Join(root, "fog-store"), chunkstore.Options{
		IndexPath: filepath.Join(t.TempDir(), "ix.db"), Create: true,
		Params: chunkstore.Params{Min: 2 << 10, Avg: 8 << 10, Max: 32 << 10}, PackSize: 64 << 10})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	h.m.ChunkStore = func() (*chunkstore.Store, error) { return cs, nil }
	app, _ := h.st.CreateApp("hub")
	h.st.CreateRevision(app.ID, `{"source":{"type":"image","image":"hub:latest"},"backup":{"enabled":true,"mode":"delegated"}}`, "", "test")
	week := time.Now().Add(-7 * 24 * time.Hour).UTC().Format(time.RFC3339)
	h.st.DB().Exec(`UPDATE apps SET created_at = ?`, week)
	if _, _, err := h.m.AddTarget(context.Background(), h.spec("cloud"), "key-1", "secret"); err != nil {
		t.Fatal(err)
	}
	return &storeHarness{harness: h, root: root, cs: cs, app: app}
}

// backup records a store-only delegated backup of artifact.
func (h *storeHarness) backup(t *testing.T, stamp string, artifact []byte) *store.Backup {
	t.Helper()
	w, err := h.cs.Begin("app/hub")
	if err != nil {
		t.Fatal(err)
	}
	w.AddBytes("artifact.tar", 0o600, artifact)
	w.AddBytes("manifest.json", 0o600, []byte(`{"format":"fog-backup/2","app":"hub"}`))
	snap, err := w.Commit(stamp, time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(artifact)
	created := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	id, _ := h.st.CreateArtifactBackupAt(h.app.ID, "scheduled", filepath.Join(h.root, "hub", stamp), int64(len(artifact)),
		"fog-backup/1", hex.EncodeToString(sum[:]), `{"format":"fog-backup/2","app":"hub"}`, created)
	h.st.SetBackupStore(id, snap.ID, created, "")
	b, _ := h.st.BackupByID(id)
	return b
}

func packCopies(t *testing.T, st *store.Store) int {
	t.Helper()
	targets, _ := st.ListOffsiteTargets()
	rows, _ := st.ListOffsiteCopiesSlim(targets[0].ID, kindPack, store.OffsiteStored)
	return len(rows)
}

// A store-backed backup goes up as the store's packs plus a row; the next
// night uploads only the packs it does not share; and fetching it back
// rebuilds the very artifact fog verified.
func TestStoreBackedSyncFetchAndVerify(t *testing.T) {
	t.Parallel()

	h := newStoreHarness(t)
	ctx := context.Background()
	art1 := noise(1, 600<<10)
	h.backup(t, "20261103-030000", art1)
	if err := runSync(t, h.harness, "cloud"); err != nil {
		t.Fatal(err)
	}
	first := packCopies(t, h.st)
	if first == 0 {
		t.Fatal("no packs copied")
	}
	art2 := append([]byte{}, art1...)
	copy(art2[300<<10:], []byte("one night's change"))
	b2 := h.backup(t, "20261104-030000", art2)
	if err := runSync(t, h.harness, "cloud"); err != nil {
		t.Fatal(err)
	}
	if added := packCopies(t, h.st) - first; added == 0 || added*3 > first {
		t.Fatalf("second night copied %d packs on top of %d; want a few", added, first)
	}
	if state := offsiteStateOf(h.st, store.OffsiteKindApp, strconv.FormatInt(b2.ID, 10)); state != store.OffsiteStored {
		t.Fatalf("row state %q", state)
	}

	// Lose the backup locally and fetch it back.
	h.st.DeleteBackup(b2.ID)
	id, err := h.m.Fetch(ctx, nil, "cloud", "hub", "20261104-030000")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	got, _ := h.st.BackupByID(id)
	data, err := os.ReadFile(filepath.Join(got.Path, "artifact.tar"))
	if err != nil || !bytes.Equal(data, art2) {
		t.Fatalf("fetched artifact differs: %v", err)
	}
	if got.SHA256 != b2.SHA256 || got.CreatedAt != b2.CreatedAt {
		t.Fatalf("fetched row %+v", got)
	}

	// The offsite verifier rebuilds it from the bucket too.
	targets, _ := h.st.ListOffsiteTargets()
	r, err := h.m.Reader(ctx, &targets[0])
	if err != nil {
		t.Fatal(err)
	}
	copies, _ := h.st.ListOffsiteCopies(targets[0].ID, store.OffsiteKindApp, store.OffsiteStored)
	for i := range copies {
		if err := h.m.verifyAppCopy(ctx, r, &copies[i]); err != nil {
			t.Fatalf("verify %s: %v", copies[i].Key, err)
		}
	}
}

// Damage found in the local store is repaired from the locker's copy of
// the same pack.
func TestHealFromLocker(t *testing.T) {
	t.Parallel()

	h := newStoreHarness(t)
	ctx := context.Background()
	art := noise(2, 300<<10)
	b := h.backup(t, "20261104-030000", art)
	if err := runSync(t, h.harness, "cloud"); err != nil {
		t.Fatal(err)
	}
	snap, _ := h.cs.Snapshot(b.StoreSnapshot)
	e, _ := h.cs.Find(snap, "artifact.tar")
	pack, _, _ := h.cs.PackOf(e.Chunks[0])
	// Scribble over the pack's records.
	f, _ := os.OpenFile(h.cs.PackPath(pack), os.O_RDWR, 0)
	f.WriteAt(bytes.Repeat([]byte{0xee}, 2048), 16)
	f.Close()
	rep, err := h.cs.CheckPacks(ctx, 0)
	if err != nil || rep.OK() {
		t.Fatalf("damage not found: %+v %v", rep, err)
	}
	failed, err := h.m.HealChunks(ctx, h.cs, rep.BadChunks)
	if err != nil || len(failed) != 0 {
		t.Fatalf("heal: failed %v err %v", failed, err)
	}
	r, _ := h.cs.Open(e)
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil || !bytes.Equal(buf.Bytes(), art) {
		t.Fatalf("after heal: %v", err)
	}
}

// Packs no retained row uses are hidden; packs a retained row uses stay.
func TestSweepPacksHidesOnlyUnreferenced(t *testing.T) {
	t.Parallel()

	h := newStoreHarness(t)
	ctx := context.Background()
	h.backup(t, "20261103-030000", noise(3, 200<<10))
	if err := runSync(t, h.harness, "cloud"); err != nil {
		t.Fatal(err)
	}
	b := h.backup(t, "20261104-030000", noise(4, 200<<10))
	if err := runSync(t, h.harness, "cloud"); err != nil {
		t.Fatal(err)
	}
	targets, _ := h.st.ListOffsiteTargets()
	t0 := &targets[0]
	r, _ := h.m.Reader(ctx, t0)
	// Retention lets the older row go.
	rows, _ := h.st.ListOffsiteCopies(t0.ID, store.OffsiteKindApp, store.OffsiteStored)
	for _, c := range rows {
		if c.Subject != strconv.FormatInt(b.ID, 10) {
			h.st.SetOffsiteCopyState(c.ID, store.OffsiteHidden)
		}
	}
	stored, _ := h.st.ListOffsiteCopiesSlim(t0.ID, "", store.OffsiteStored)
	before := packCopies(t, h.st)
	if _, err := h.m.sweepPacks(ctx, t0, r, stored); err != nil {
		t.Fatal(err)
	}
	after := packCopies(t, h.st)
	if after >= before || after == 0 {
		t.Fatalf("packs %d → %d", before, after)
	}
	// The retained backup still rebuilds from what is left.
	var row AppRow
	if err := r.Document(ctx, r.Layout.AppRow("hub", "20261104-030000"), &row); err != nil {
		t.Fatal(err)
	}
	if err := FetchStoreSnapshot(ctx, r, &row, t.TempDir(), filepath.Join(t.TempDir(), "out"), nil); err != nil {
		t.Fatalf("retained backup after sweep: %v", err)
	}
}

// A Drive snapshot the store holds goes up as packs and comes back from
// the locker whole.
func TestStoreBackedDriveSnapshot(t *testing.T) {
	t.Parallel()

	h := newStoreHarness(t)
	ctx := context.Background()
	big := noise(5, 300<<10)
	w, _ := h.cs.Begin("drive")
	w.Add(chunkstore.Entry{Path: "docs", Type: chunkstore.TypeDir, Mode: 0o755}, nil)
	w.AddBytes("docs/big.bin", 0o644, big)
	snap, err := w.Commit("20261104-031500", time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.CreateStoreFileSnapshot(filepath.Join(h.root, "files", "20261104-031500"), snap.ID, "", 1, int64(len(big)), snap.New, 0); err != nil {
		t.Fatal(err)
	}
	if err := runSync(t, h.harness, "cloud"); err != nil {
		t.Fatal(err)
	}
	targets, _ := h.st.ListOffsiteTargets()
	r, err := h.m.Reader(ctx, &targets[0])
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "restored")
	n, _, err := r.RestoreFiles(ctx, "20261104-031500", out, nil)
	if err != nil || n != 1 {
		t.Fatalf("restore files: %d %v", n, err)
	}
	got, _ := os.ReadFile(filepath.Join(out, "docs", "big.bin"))
	if !bytes.Equal(got, big) {
		t.Fatal("restored Drive file differs")
	}
	copies, _ := h.st.ListOffsiteCopies(targets[0].ID, store.OffsiteKindFilesSnapshot, store.OffsiteStored)
	if len(copies) != 1 {
		t.Fatalf("files copies %d", len(copies))
	}
	if err := h.m.verifyFilesCopy(ctx, r, &copies[0]); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

// A pack the sweep hid is uploaded again when a backup needs its chunks
// once more, so no row is written naming an object the provider deletes.
func TestHiddenPackIsUploadedAgainWhenNeeded(t *testing.T) {
	t.Parallel()

	h := newStoreHarness(t)
	ctx := context.Background()
	art := noise(9, 200<<10)
	first := h.backup(t, "20261103-030000", art)
	if err := runSync(t, h.harness, "cloud"); err != nil {
		t.Fatal(err)
	}
	targets, _ := h.st.ListOffsiteTargets()
	t0 := &targets[0]
	r, _ := h.m.Reader(ctx, t0)
	// Retention drops the only row; the sweep hides its packs.
	rows, _ := h.st.ListOffsiteCopies(t0.ID, store.OffsiteKindApp, store.OffsiteStored)
	for _, c := range rows {
		r.Client.Delete(ctx, c.Key)
		h.st.SetOffsiteCopyState(c.ID, store.OffsiteHidden)
	}
	h.backup(t, "20261104-020000", noise(10, 10<<10)) // a newer row, so the sweep may hide
	if err := runSync(t, h.harness, "cloud"); err != nil {
		t.Fatal(err)
	}
	stored, _ := h.st.ListOffsiteCopiesSlim(t0.ID, "", store.OffsiteStored)
	if _, err := h.m.sweepPacks(ctx, t0, r, stored); err != nil {
		t.Fatal(err)
	}
	if hidden, _ := h.st.ListOffsiteCopiesSlim(t0.ID, kindPack, store.OffsiteHidden); len(hidden) == 0 {
		t.Fatal("the sweep hid nothing; the test proves nothing")
	}
	// The same content is backed up again: its packs must go back up.
	h.st.DeleteBackup(first.ID)
	again := h.backup(t, "20261105-030000", art)
	if err := runSync(t, h.harness, "cloud"); err != nil {
		t.Fatal(err)
	}
	h.st.DeleteBackup(again.ID)
	var row AppRow
	if err := r.Document(ctx, r.Layout.AppRow("hub", "20261105-030000"), &row); err != nil {
		t.Fatal(err)
	}
	if err := FetchStoreSnapshot(ctx, r, &row, t.TempDir(), filepath.Join(t.TempDir(), "out"), nil); err != nil {
		t.Fatalf("backup reusing swept packs is not restorable: %v", err)
	}
}

// A removed app's copies stay until they age out, but its spec — what says
// which extensions a scratch restore may recreate — went with it. Its copy
// is read back whole and checked, and not test-restored; an installed app's
// still is.
func TestVerifyRemovedAppReadsBackWithoutRestoring(t *testing.T) {
	t.Parallel()

	h := newStoreHarness(t)
	ctx := context.Background()
	h.backup(t, "20261103-030000", noise(3, 300<<10))
	if err := runSync(t, h.harness, "cloud"); err != nil {
		t.Fatal(err)
	}
	var restored []string
	h.m.TestRestore = func(_ context.Context, app, artifact, _ string) error {
		if _, err := os.Stat(artifact); err != nil {
			return err
		}
		restored = append(restored, app)
		return nil
	}
	targets, _ := h.st.ListOffsiteTargets()
	if err := h.m.verifyTarget(ctx, &targets[0]); err != nil || len(restored) != 1 {
		t.Fatalf("an installed app's copy is test-restored: %v %v", err, restored)
	}

	h.st.DB().Exec(`UPDATE offsite_copies SET verified_at = NULL`)
	if _, err := h.st.DB().Exec(`DELETE FROM apps WHERE name = 'hub'`); err != nil {
		t.Fatal(err)
	}
	restored = nil
	if err := h.m.verifyTarget(ctx, &targets[0]); err != nil {
		t.Fatalf("a removed app's copy must still verify: %v", err)
	}
	if len(restored) != 0 {
		t.Fatalf("a removed app's copy must not be test-restored: %v", restored)
	}
	copies, _ := h.st.ListOffsiteCopies(targets[0].ID, store.OffsiteKindApp, store.OffsiteStored)
	if len(copies) != 1 || copies[0].VerifiedAt == "" {
		t.Fatalf("the copy is verified: %+v", copies)
	}
}

// A check that would fill this server's disk is not run, and is not a
// failed copy: it raises its own notice, leaves the copy unverified, and
// clears once a check runs again.
func TestVerifyWithoutRoomIsNotAFailedCopy(t *testing.T) {
	t.Parallel()

	h := newStoreHarness(t)
	ctx := context.Background()
	h.backup(t, "20261103-030000", noise(4, 300<<10))
	if err := runSync(t, h.harness, "cloud"); err != nil {
		t.Fatal(err)
	}
	h.m.TestRestore = func(context.Context, string, string, string) error { return nil }
	h.m.DiskSpace = func(string) (int64, int64, bool) { return 1 << 20, 100 << 30, true }

	targets, _ := h.st.ListOffsiteTargets()
	if err := h.m.verifyTarget(ctx, &targets[0]); err == nil {
		t.Fatal("a check with no room must report it")
	}
	kinds := map[string]bool{}
	notices, _ := h.st.ActiveNotices()
	for _, n := range notices {
		kinds[n.Kind] = true
	}
	if !kinds[NoticeVerifyNoRoom] || kinds[NoticeVerifyFailed] {
		t.Fatalf("want the no-room notice and no failed-copy one: %v", kinds)
	}
	copies, _ := h.st.ListOffsiteCopies(targets[0].ID, store.OffsiteKindApp, store.OffsiteStored)
	if copies[0].VerifiedAt != "" {
		t.Fatal("an unchecked copy is not verified")
	}

	h.m.DiskSpace = plentyOfRoom
	if err := h.m.verifyTarget(ctx, &targets[0]); err != nil {
		t.Fatalf("with room: %v", err)
	}
	notices, _ = h.st.ActiveNotices()
	for _, n := range notices {
		if n.Kind == NoticeVerifyNoRoom {
			t.Fatal("the no-room notice clears once the check runs")
		}
	}
}
