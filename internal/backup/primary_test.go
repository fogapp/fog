package backup

import (
	"bytes"
	"compress/gzip"
	"context"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"encoding/json"

	"gitea.lan/matthew/fog/internal/chunkstore"
	"gitea.lan/matthew/fog/internal/store"
)

// A legacy backup whose directory is gone restores from the store: the
// same volume contents land, the old ones are replaced.
func TestLegacyRestoreFromStore(t *testing.T) {
	dockerLog := fakeDocker(t, "0")
	m, st := legacyBackupManager(t)
	t.Cleanup(m.CloseChunkStore)
	m.startSeam = func(context.Context, string) error { return nil }
	ctx := context.Background()
	id, err := m.BackupApp(ctx, "demo", "manual")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := st.BackupByID(id)
	os.RemoveAll(b.Path) // as retirement would, once graduated
	vol := filepath.Join(m.Mat.DataPath("demo"), "data")
	os.WriteFile(filepath.Join(vol, "state.txt"), []byte("changed since"), 0o644)

	if err := m.RestoreApp(ctx, id); err != nil {
		log, _ := os.ReadFile(dockerLog)
		t.Fatalf("restore from the store: %v (docker: %s)", err, log)
	}
	got, _ := os.ReadFile(filepath.Join(vol, "state.txt"))
	if string(got) != "live" {
		t.Fatalf("restored volume holds %q", got)
	}
	// And the scratch copy it was rebuilt into is gone.
	cs, _ := m.ChunkStore()
	if ents, _ := os.ReadDir(filepath.Join(cs.Dir(), "tmp")); len(ents) != 0 {
		t.Fatalf("scratch left behind: %v", ents)
	}
}

// Directories are retired once the store holds them, the newest kept as
// an anchor until the app has proven its store copies GraduationPasses
// times; after that no directory stays.
func TestRetirementKeepsAnAnchorUntilGraduation(t *testing.T) {
	fakeDocker(t, "0")
	m, st := legacyBackupManager(t)
	t.Cleanup(m.CloseChunkStore)
	ctx := context.Background()
	dirs := func() int {
		ents, _ := os.ReadDir(filepath.Join(m.baseDir(), "demo"))
		return len(ents)
	}
	for i := 1; i < GraduationPasses; i++ {
		if _, err := m.BackupApp(ctx, "demo", "manual"); err != nil {
			t.Fatal(err)
		}
		if n := dirs(); n != 1 {
			t.Fatalf("after %d backups, %d directories; want just the anchor", i, n)
		}
	}
	if _, err := m.BackupApp(ctx, "demo", "manual"); err != nil {
		t.Fatal(err)
	}
	if n := dirs(); n != 0 {
		t.Fatalf("graduated app still has %d directories", n)
	}
	list, _ := st.ListBackups(1, 10)
	for i := range list {
		if !HasFiles(&list[i]) {
			t.Fatalf("backup %d unreadable", list[i].ID)
		}
	}
}

// A backup the store cannot read back keeps its directory: nothing is
// retired on an unproven copy.
func TestRetirementSkipsUnverified(t *testing.T) {
	m, st := legacyBackupManager(t)
	t.Cleanup(m.CloseChunkStore)
	app, _ := st.AppByName("demo")
	m.passes(app.Name)
	for i := 0; i < GraduationPasses; i++ {
		m.notePass(app.Name)
	}
	dir := filepath.Join(m.baseDir(), "demo", "20261101-030000")
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "meta.json"), []byte(`{}`), 0o600)
	id, _ := st.CreateBackup(app.ID, "scheduled", dir, 1)
	st.SetBackupStore(id, "0123456789abcdef0123456789abcdef", "", "read back wrong")
	m.retireClassic("demo")
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("retired a directory whose store copy failed")
	}
}

// Backups taken before the store are brought into it at night, and their
// directories go once they read back.
func TestImportClassic(t *testing.T) {
	m, st := legacyBackupManager(t)
	t.Cleanup(m.CloseChunkStore)
	app, _ := st.AppByName("demo")
	for i := 0; i < GraduationPasses; i++ {
		m.notePass(app.Name)
	}
	var ids []int64
	for _, stamp := range []string{"20261101-030000", "20261102-030000"} {
		dir := filepath.Join(m.baseDir(), "demo", stamp)
		os.MkdirAll(dir, 0o700)
		os.WriteFile(filepath.Join(dir, "meta.json"), []byte(`{"app":"demo","volumes":[]}`), 0o600)
		var gz bytes.Buffer
		zw := gzip.NewWriter(&gz)
		zw.Write([]byte("old tar " + stamp))
		zw.Close()
		os.WriteFile(filepath.Join(dir, "volume-data.tar.gz"), gz.Bytes(), 0o600)
		id, _ := st.CreateBackup(app.ID, "scheduled", dir, 1)
		ids = append(ids, id)
	}
	// A third whose archive is not whole: refused, and its directory kept.
	bad := filepath.Join(m.baseDir(), "demo", "20261103-030000")
	os.MkdirAll(bad, 0o700)
	os.WriteFile(filepath.Join(bad, "meta.json"), []byte(`{"app":"demo","volumes":[]}`), 0o600)
	os.WriteFile(filepath.Join(bad, "volume-data.tar.gz"), []byte("not gzip"), 0o600)
	badID, _ := st.CreateBackup(app.ID, "scheduled", bad, 1)

	m.importClassic(context.Background(), time.Hour)
	for _, id := range ids {
		b, _ := st.BackupByID(id)
		if b.StoreSnapshot == "" || b.StoreVerifiedAt == "" {
			t.Fatalf("backup %d not imported: %+v", id, b)
		}
		if hasClassic(b) {
			t.Fatalf("backup %d kept its directory after import", id)
		}
		// Unpacked on the way in, as the store's own volume backups are,
		// so one night's archive can share chunks with the next.
		r, _, err := m.OpenBackupFile(b, "volume-data.tar")
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(r)
		if !strings.HasPrefix(string(got), "old tar ") {
			t.Fatalf("imported archive reads %q", got)
		}
	}
	b, _ := st.BackupByID(badID)
	if b.StoreError == "" || !hasClassic(b) {
		t.Fatalf("a damaged archive must not be imported, nor its directory retired: %+v", b)
	}
}

type plainCipher struct{}

func (plainCipher) Encrypt(s string) (string, error) { return "x" + s, nil }
func (plainCipher) Decrypt(s string) (string, error) { return strings.TrimPrefix(s, "x"), nil }

// Turning encryption on rewrites the store sealed and points every row at
// the copies; turning it off again rewrites it plain. Content survives both.
func TestConvertStore(t *testing.T) {
	fakeDocker(t, "0")
	m, st := legacyBackupManager(t)
	m.Vault = plainCipher{}
	t.Cleanup(m.CloseChunkStore)
	ctx := context.Background()
	id, err := m.BackupApp(ctx, "demo", "manual")
	if err != nil {
		t.Fatal(err)
	}
	before, _ := st.BackupByID(id)
	want := readAll(t, m, before, "volume-data.tar")

	for _, on := range []string{"on", "off"} {
		st.SetSetting(SettingStoreEncrypt, on)
		if err := m.convertStore(ctx); err != nil {
			t.Fatal(err)
		}
		cs, err := m.ChunkStore()
		if err != nil {
			t.Fatal(err)
		}
		if cs.Encrypted() != (on == "on") {
			t.Fatalf("setting %s: store encrypted=%t", on, cs.Encrypted())
		}
		b, _ := st.BackupByID(id)
		if b.StoreSnapshot == before.StoreSnapshot {
			t.Fatal("row not pointed at the rewritten snapshot")
		}
		os.RemoveAll(b.Path)
		if got := readAll(t, m, b, "volume-data.tar"); got != want {
			t.Fatal("content changed across the rewrite")
		}
		before = b
		for _, leftover := range []string{nextStoreDir, oldStoreDir} {
			if _, err := os.Stat(filepath.Join(m.baseDir(), leftover)); err == nil {
				t.Fatalf("%s left behind", leftover)
			}
		}
	}
	if s := m.Summary(); s.Encrypted || s.EncryptPending || s.UsedBytes == 0 || s.OldestAt == "" {
		t.Fatalf("summary %+v", s)
	}
}

func readAll(t *testing.T, m *Manager, b *store.Backup, name string) string {
	t.Helper()
	r, _, err := m.OpenBackupFile(b, name)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(got)
	return hex.EncodeToString(sum[:])
}

// A crash between the two renames of a conversion leaves no store at the
// usual place; the next open finishes the job from the marked copy.
func TestConversionRecoversAfterCrash(t *testing.T) {
	m, _ := legacyBackupManager(t)
	t.Cleanup(m.CloseChunkStore)
	cs, err := m.ChunkStore()
	if err != nil {
		t.Fatal(err)
	}
	w, _ := cs.Begin("app/demo")
	w.AddBytes("f", 0o600, []byte("kept"))
	snap, _ := w.Commit("20261104-030000", s0, nil)
	m.CloseChunkStore()
	root := m.baseDir()
	os.Rename(filepath.Join(root, StoreDirName), filepath.Join(root, nextStoreDir))
	os.WriteFile(filepath.Join(root, nextStoreDir, convertedMark), nil, 0o600)

	cs, err = m.ChunkStore()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cs.Snapshot(snap.ID); err != nil {
		t.Fatalf("store not recovered: %v", err)
	}
}

// A nearly full disk thins the oldest backups across apps, never an app's
// newest three, and says so.
func TestGuardSpaceThinsOldest(t *testing.T) {
	m, st := legacyBackupManager(t)
	t.Cleanup(m.CloseChunkStore)
	var told string
	app, _ := st.AppByName("demo")
	var ids []int64
	for i := 0; i < 6; i++ {
		id, _ := st.CreateArtifactBackupAt(app.ID, "scheduled", filepath.Join(m.baseDir(), "demo", "x"+string(rune('a'+i))), 1, "", "", "",
			s0.AddDate(0, 0, i).Format("2006-01-02T15:04:05Z"))
		ids = append(ids, id)
	}
	full := true
	swapVar(t, &diskUseSeam, func(string) (uint64, uint64, bool) {
		if full {
			return 95, 100, true
		}
		return 50, 100, true
	})
	cs, _ := m.ChunkStore()
	// The backups hold most of what is used, and each round frees space
	// until the disk has room again.
	w, _ := cs.Begin("app/demo")
	w.AddBytes("f", 0o600, randomBytes(1<<20))
	w.Commit("20261101-030000", s0, nil)
	rounds := 0
	swapVar(t, &diskUseSeam, func(string) (uint64, uint64, bool) {
		rounds++
		if full && rounds > 2 {
			full = false
		}
		if full {
			return 95 << 16, 100 << 16, true
		}
		return 50 << 16, 100 << 16, true
	})
	m.Notify = func(title, _ string, _ bool) { told = title }
	m.guardSpace(context.Background(), cs)
	left, _ := st.ListBackups(app.ID, 10)
	if len(left) < keepNewest || len(left) == 6 {
		t.Fatalf("%d backups left", len(left))
	}
	for _, b := range left[:keepNewest] {
		if b.ID != ids[len(ids)-1] && b.ID != ids[len(ids)-2] && b.ID != ids[len(ids)-3] {
			t.Fatalf("a newest backup was thinned: kept %d", b.ID)
		}
	}
	if !strings.Contains(told, "nearly full") {
		t.Fatalf("not told: %q", told)
	}
}

// A disk that is full of something other than the backups keeps every
// backup; and when collection cannot run, thinning stops rather than
// deleting history it cannot turn into space.
func TestGuardSpaceKeepsBackupsItCannotHelpWith(t *testing.T) {
	m, st := legacyBackupManager(t)
	t.Cleanup(m.CloseChunkStore)
	m.Notify = func(string, string, bool) {}
	app, _ := st.AppByName("demo")
	for i := 0; i < 8; i++ {
		st.CreateArtifactBackupAt(app.ID, "scheduled", filepath.Join(m.baseDir(), "demo", "y"+string(rune('a'+i))), 1, "", "", "",
			s0.AddDate(0, 0, i).Format("2006-01-02T15:04:05Z"))
	}
	swapVar(t, &diskUseSeam, func(string) (uint64, uint64, bool) { return 95 << 30, 100 << 30, true })
	cs, _ := m.ChunkStore()
	m.guardSpace(context.Background(), cs) // the store holds next to nothing of 95 GiB
	if left, _ := st.ListBackups(app.ID, 20); len(left) != 8 {
		t.Fatalf("thinned for space the backups never held: %d left", len(left))
	}

	// Now the backups fill the disk, but collection refuses to run.
	w, _ := cs.Begin("app/demo")
	w.AddBytes("f", 0o600, randomBytes(1<<20))
	w.Commit("20261101-030000", s0, nil)
	swapVar(t, &diskUseSeam, func(string) (uint64, uint64, bool) { return 95 << 16, 100 << 16, true })
	os.WriteFile(filepath.Join(cs.Dir(), "snapshots", "0123456789abcdef0123456789abcdef.json"), []byte("{bad"), 0o600)
	m.guardSpace(context.Background(), cs)
	if left, _ := st.ListBackups(app.ID, 20); len(left) != 8 {
		t.Fatalf("a store that cannot collect frees nothing, so nothing is thinned: %d left", len(left))
	}
}

// storeBackup records a backup of app held in the store as one file of n
// random bytes, created at.
func storeBackupOf(t *testing.T, m *Manager, st *store.Store, cs *chunkstore.Store, app *store.App, stamp string, at time.Time, n int) int64 {
	t.Helper()
	w, err := cs.Begin(Series(app.Name))
	if err != nil {
		t.Fatal(err)
	}
	w.AddBytes("artifact.tar", 0o600, randomBytes(n))
	snap, err := w.Commit(stamp, at, nil)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := st.CreateArtifactBackupAt(app.ID, "scheduled", filepath.Join(m.baseDir(), app.Name, stamp), int64(n), "", "", "", at.Format(time.RFC3339))
	st.SetBackupStore(id, snap.ID, "", "")
	return id
}

// diskFollowingStore is a 100-unit disk whose use is what the store holds
// plus other of everything else: thinning and collection show in it as
// they would on a real disk.
func diskFollowingStore(t *testing.T, cs *chunkstore.Store, other, total uint64) {
	t.Helper()
	swapVar(t, &diskUseSeam, func(string) (uint64, uint64, bool) {
		st, err := cs.Stats()
		if err != nil {
			return 0, 0, false
		}
		return other + uint64(st.Bytes), total, true
	})
}

// Backups deleted by hand leave their packs until a collection; a nearly
// full disk collects them straight away, before any history is thinned.
func TestGuardSpaceCollectsWhatIsAlreadyLetGo(t *testing.T) {
	m, st := legacyBackupManager(t)
	t.Cleanup(m.CloseChunkStore)
	m.Notify = func(string, string, bool) {}
	app, _ := st.AppByName("demo")
	cs, _ := m.ChunkStore()
	var ids []int64
	for i := 0; i < 6; i++ {
		ids = append(ids, storeBackupOf(t, m, st, cs, app, fmt.Sprintf("2026110%d-030000", i+1), s0.AddDate(0, 0, i), 256<<10))
	}
	// Five deleted by hand: rows and snapshots gone, packs still on disk.
	for _, id := range ids[:5] {
		b, _ := st.BackupByID(id)
		m.dropFromStore(b)
		st.DeleteBackup(id)
	}
	held, _ := cs.Stats()
	// The store is most of a disk that is 95% full.
	diskFollowingStore(t, cs, uint64(held.Bytes)/3, uint64(held.Bytes)*14/10)
	m.guardSpace(context.Background(), cs)
	if left, _ := st.ListBackups(app.ID, 10); len(left) != 1 {
		t.Fatalf("the backup left must stay: %d", len(left))
	}
	after, _ := cs.Stats()
	if after.Bytes*3 > held.Bytes {
		t.Fatalf("the deleted backups' space must come back: %d of %d bytes still held", after.Bytes, held.Bytes)
	}
}

// Thinning goes oldest first across apps, and the oldest may be small
// apps' backups that free next to nothing. It keeps going to the backups
// that hold the space instead of stopping after a round that freed little.
func TestGuardSpaceThinsPastRoundsThatFreeLittle(t *testing.T) {
	m, st := legacyBackupManager(t)
	t.Cleanup(m.CloseChunkStore)
	m.Notify = func(string, string, bool) {}
	small, _ := st.AppByName("demo")
	big, _ := st.CreateApp("signet")
	st.CreateRevision(big.ID, `{"source":{"type":"image","image":"signet:latest"}}`, "", "test")
	cs, _ := m.ChunkStore()
	// Twelve tiny backups, oldest of all, then six large ones.
	for i := 0; i < 12; i++ {
		storeBackupOf(t, m, st, cs, small, fmt.Sprintf("s%02d", i), s0.Add(time.Duration(i)*time.Minute), 512)
	}
	for i := 0; i < 6; i++ {
		storeBackupOf(t, m, st, cs, big, fmt.Sprintf("b%02d", i), s0.AddDate(0, 0, 1+i), 1<<20)
	}
	held, _ := cs.Stats()
	diskFollowingStore(t, cs, 0, uint64(held.Bytes)*100/95)
	m.guardSpace(context.Background(), cs)
	left, _ := st.ListBackups(big.ID, 10)
	if len(left) == 6 {
		t.Fatal("the large backups behind rounds of small ones were never reached")
	}
	if len(left) < keepNewest {
		t.Fatalf("an app's newest %d always stay: %d left", keepNewest, len(left))
	}
}

// The nightly upkeep runs once a day, on the first look after the backup
// hour — not whenever twenty hours have passed since it last ran, which let
// an afternoon run (an app with its own hour) push it past the night.
func TestMaintenanceDueAnchorsOnBackupHour(t *testing.T) {
	_, st := legacyBackupManager(t)
	at := func(s string) time.Time { v, _ := time.Parse(time.RFC3339, s); return v }
	if !maintenanceDue(st, at("2026-11-06T01:20:00Z"), 1) {
		t.Fatal("never run: due")
	}
	st.SetSetting(SettingStoreMaintainedAt, "2026-11-05T19:05:48Z")
	if !maintenanceDue(st, at("2026-11-06T01:20:00Z"), 1) {
		t.Fatal("ran yesterday afternoon: due after tonight's backups")
	}
	if maintenanceDue(st, at("2026-11-06T00:50:00Z"), 1) {
		t.Fatal("ran since yesterday's backup hour: not due before tonight's")
	}
	st.SetSetting(SettingStoreMaintainedAt, "2026-11-06T01:21:00Z")
	if maintenanceDue(st, at("2026-11-06T19:00:00Z"), 1) {
		t.Fatal("ran after tonight's backups: not due again today")
	}
}

// A store with a document that will not read is not rewritten: the
// rewrite would lose that backup.
func TestConvertRefusesUnreadableSnapshot(t *testing.T) {
	m, st := legacyBackupManager(t)
	m.Vault = plainCipher{}
	t.Cleanup(m.CloseChunkStore)
	cs, _ := m.ChunkStore()
	os.WriteFile(filepath.Join(cs.Dir(), "snapshots", "0123456789abcdef0123456789abcdef.json"), []byte("{bad"), 0o600)
	st.SetSetting(SettingStoreEncrypt, "on")
	if err := m.convertStore(context.Background()); err == nil {
		t.Fatal("rewrote a store holding an unreadable backup")
	}
	cs, _ = m.ChunkStore()
	if cs.Encrypted() {
		t.Fatal("store swapped despite the refusal")
	}
}

// A crash after the swap but before the rows moved: the next open points
// every row at its copy from the map kept in the new store, and only then
// lets the old store go.
func TestConversionRepointsAfterCrashAfterSwap(t *testing.T) {
	fakeDocker(t, "0")
	m, st := legacyBackupManager(t)
	m.Vault = plainCipher{}
	t.Cleanup(m.CloseChunkStore)
	ctx := context.Background()
	id, err := m.BackupApp(ctx, "demo", "manual")
	if err != nil {
		t.Fatal(err)
	}
	before, _ := st.BackupByID(id)
	// Hand-make the state a crash right after the renames would leave.
	old, _ := m.ChunkStore()
	snap, _ := old.Snapshot(before.StoreSnapshot)
	root := m.baseDir()
	ns, err := chunkstore.Open(filepath.Join(root, nextStoreDir), chunkstore.Options{IndexPath: m.indexPath() + ".next", Create: true})
	if err != nil {
		t.Fatal(err)
	}
	newID, err := copySnapshot(ctx, old, ns, snap)
	if err != nil {
		t.Fatal(err)
	}
	ns.Close()
	mb, _ := json.Marshal(map[string]string{before.StoreSnapshot: newID})
	os.WriteFile(filepath.Join(root, nextStoreDir, movedFile), mb, 0o600)
	os.WriteFile(filepath.Join(root, nextStoreDir, convertedMark), nil, 0o600)
	m.CloseChunkStore()
	os.Rename(filepath.Join(root, StoreDirName), filepath.Join(root, oldStoreDir))
	os.Rename(filepath.Join(root, nextStoreDir), filepath.Join(root, StoreDirName))
	os.Rename(m.indexPath()+".next", m.indexPath())

	if _, err := m.ChunkStore(); err != nil {
		t.Fatal(err)
	}
	after, _ := st.BackupByID(id)
	if after.StoreSnapshot != newID {
		t.Fatalf("row not repointed: %s, want %s", after.StoreSnapshot, newID)
	}
	if _, err := os.Stat(filepath.Join(root, oldStoreDir)); err == nil {
		t.Fatal("old store kept after the rows moved")
	}
	os.RemoveAll(after.Path)
	if got := readAll(t, m, after, "volume-data.tar"); got == "" {
		t.Fatal("backup unreadable after recovery")
	}
}

// A store created encrypted mints a key, and a recovery kit is due until
// one is written.
func TestNewStoreKeyMakesAKitDue(t *testing.T) {
	m, st := legacyBackupManager(t)
	m.Vault = plainCipher{}
	t.Cleanup(m.CloseChunkStore)
	st.SetSetting(SettingStoreEncrypt, "on")
	cs, err := m.ChunkStore()
	if err != nil {
		t.Fatal(err)
	}
	if !cs.Encrypted() || !m.kitIsDue() {
		t.Fatalf("encrypted %t, kit due %t", cs.Encrypted(), m.kitIsDue())
	}
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	crand.Read(b)
	return b
}

// A disk under the nearly-full line, with backups deleted by hand still
// held by the store and a backup failing for want of room: the hourly look
// collects them now instead of a day or two later, and thins nothing. With
// nothing failing it leaves the store to the nightly collection.
func TestGuardSpaceBetweenCollectsWhenABackupIsStarved(t *testing.T) {
	m, st := legacyBackupManager(t)
	t.Cleanup(m.CloseChunkStore)
	m.Notify = func(string, string, bool) {}
	app, _ := st.AppByName("demo")
	cs, _ := m.ChunkStore()
	var ids []int64
	for i := 0; i < 6; i++ {
		ids = append(ids, storeBackupOf(t, m, st, cs, app, fmt.Sprintf("2026110%d-030000", i+1), s0.AddDate(0, 0, i), 256<<10))
	}
	for _, id := range ids[:5] {
		b, _ := st.BackupByID(id)
		m.dropFromStore(b)
		st.DeleteBackup(id)
	}
	held, _ := cs.Stats()
	// 89%: under the line where guardSpace acts.
	diskFollowingStore(t, cs, 0, uint64(held.Bytes)*100/89)
	now := s0.AddDate(0, 0, 10)

	starved := false
	m.SpaceStarved = func() bool { return starved }
	m.guardSpaceBetween(context.Background(), now)
	if after, _ := cs.Stats(); after.Bytes != held.Bytes {
		t.Fatal("nothing failing for room: the store is left to the nightly collection")
	}

	starved = true
	m.guardSpaceBetween(context.Background(), now.Add(30*time.Minute))
	if after, _ := cs.Stats(); after.Bytes != held.Bytes {
		t.Fatal("the look is hourly")
	}
	m.guardSpaceBetween(context.Background(), now.Add(61*time.Minute))
	if after, _ := cs.Stats(); after.Bytes*3 > held.Bytes {
		t.Fatalf("a starved backup must get the deleted backups' space back: %d of %d bytes still held", after.Bytes, held.Bytes)
	}
	if left, _ := st.ListBackups(app.ID, 10); len(left) != 1 {
		t.Fatalf("collecting thins nothing: %d left", len(left))
	}
}
