package offsite

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/objstore"
	"gitea.lan/matthew/fog/internal/objstore/fakes3"
	"gitea.lan/matthew/fog/internal/store"
)

// A backup directory with one delegated app (two backups), one legacy
// app (one backup) and a kit, the way fog leaves them on disk.
func plantBackups(t *testing.T, h *harness) (root string, ids map[string]int64) {
	t.Helper()
	root = t.TempDir()
	h.m.BackupRoot = func() string { return root }
	ids = map[string]int64{}
	mk := func(app, stamp string, files map[string][]byte) string {
		dir := filepath.Join(root, app, stamp)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		for name, data := range files {
			os.WriteFile(filepath.Join(dir, name), data, 0o600)
		}
		return dir
	}
	kits := filepath.Join(root, "fog-recovery")
	os.MkdirAll(kits, 0o700)
	os.WriteFile(filepath.Join(kits, "fog-recovery-kit-20260827-010000.enc"), []byte("FOGKIT1\x00sealed kit bytes"), 0o600)

	hub, _ := h.st.CreateApp("hub")
	h.st.CreateRevision(hub.ID, `{"source":{"type":"image","image":"hub:latest"},"backup":{"enabled":true,"mode":"delegated"}}`, "", "test")
	artifact := []byte("artifact tar bytes")
	sum := sha256.Sum256(artifact)
	for _, stamp := range []string{"20260826-010314", "20260827-010314"} {
		dir := mk("hub", stamp, map[string][]byte{"artifact.tar": artifact, "manifest.json": []byte(`{"format":"fog-backup/1","app":"hub"}`)})
		id, err := h.st.CreateArtifactBackup(hub.ID, "scheduled", dir, 100, "fog-backup/1", hex.EncodeToString(sum[:]), `{"format":"fog-backup/1","app":"hub"}`)
		if err != nil {
			t.Fatal(err)
		}
		ids["hub/"+stamp] = id
	}
	legacy, _ := h.st.CreateApp("paperless")
	h.st.CreateRevision(legacy.ID, `{"source":{"type":"image","image":"p:latest"},"volumes":[{"name":"data","mount":"/data"}],"backup":{"enabled":true}}`, "", "test")
	dir := mk("paperless", "20260827-010500", map[string][]byte{"meta.json": []byte(`{"app":"paperless","volumes":["data"]}`), "volume-data.tar.gz": bytes.Repeat([]byte("z"), 3000), "db-paperless.dump": []byte("pgdump")})
	id, _ := h.st.CreateBackup(legacy.ID, "scheduled", dir, 3100)
	ids["paperless/20260827-010500"] = id

	// Both apps have been installed for a week. A just-installed app is
	// inside the local alarm's first-backup grace, which would make every
	// "is this app locally stale?" answer here the wrong one.
	week := time.Now().Add(-7 * 24 * time.Hour).UTC().Format(time.RFC3339)
	h.st.DB().Exec(`UPDATE apps SET created_at = ?`, week)
	h.st.DB().Exec(`UPDATE app_revisions SET created_at = ?`, week)
	return root, ids
}

func runSync(t *testing.T, h *harness, name string) error {
	t.Helper()
	job, err := h.m.StartSync(name)
	if err != nil {
		t.Fatal(err)
	}
	done, _ := h.m.jobs().Wait(context.Background(), job.ID)
	if done.State == "failed" {
		return &syncErr{done.Error}
	}
	return nil
}

type syncErr struct{ msg string }

func (e *syncErr) Error() string { return e.msg }

func TestSyncCopiesEverythingAndWritesTheIndex(t *testing.T) {
	fake := fakes3.New("fog-test")
	fake.Lock = fakes3.LockConfig{Enabled: true}
	h := newHarness(t, fake)
	h.st.SetSetting("backup_hour_utc", "1")
	_, ids := plantBackups(t, h)
	target, _, err := h.m.AddTarget(context.Background(), h.spec("cloud"), "key-1", "secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := runSync(t, h, "cloud"); err != nil {
		t.Fatalf("sync: %v", err)
	}
	ctx := context.Background()
	r, err := h.m.Reader(ctx, target)
	if err != nil {
		t.Fatal(err)
	}

	// The kit is in the locker byte for byte.
	kits, _ := r.Kits(ctx)
	if len(kits) != 1 || kits[0].Stamp != "20260827-010000" {
		t.Fatalf("kits: %+v", kits)
	}
	var kit bytes.Buffer
	r.FetchKit(ctx, kits[0].Name, &kit)
	if kit.String() != "FOGKIT1\x00sealed kit bytes" {
		t.Fatalf("kit bytes: %q", kit.String())
	}
	// fog's record of the kit — its digest, sealed into the index and kept
	// locally — is what recovery and verify check the bucket against.
	ksum := sha256.Sum256(kit.Bytes())
	if ix, err := r.Index(ctx); err != nil || ix.LatestKit == nil || ix.LatestKit.Name != kits[0].Name || ix.LatestKit.SHA256 != hex.EncodeToString(ksum[:]) {
		t.Fatalf("index must record the kit fog uploaded with its digest: %+v %v", ix, err)
	}
	if rec, ok := h.m.latestKit(target); !ok || rec.Name != kits[0].Name || rec.SHA256 != hex.EncodeToString(ksum[:]) {
		t.Fatalf("local latest-kit record: %+v %v", rec, ok)
	}

	// Every backup has a sealed snapshot and a row; the snapshot opens to
	// the directory's files and the row names them.
	rows, err := r.AppRows(ctx, "hub")
	if err != nil || len(rows) != 2 || rows[0].Stamp != "20260827-010314" || rows[0].Mode != "delegated" || rows[0].Identity != "hub" {
		t.Fatalf("hub rows: %+v %v", rows, err)
	}
	if rows[0].SourceBackupID != ids["hub/20260827-010314"] || strings.Join(rows[0].Files, ",") != "artifact.tar,manifest.json" {
		t.Fatalf("row detail: %+v", rows[0])
	}
	rc, err := r.Open(ctx, r.Layout.AppSnapshot("hub", "20260827-010314"))
	if err != nil {
		t.Fatal(err)
	}
	tarBytes, _ := io.ReadAll(rc)
	rc.Close()
	sum := sha256.Sum256(tarBytes)
	if hex.EncodeToString(sum[:]) != rows[0].SnapshotSHA256 {
		t.Fatal("row digest is not the snapshot's")
	}
	tr := tar.NewReader(bytes.NewReader(tarBytes))
	var names []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, hdr.Name)
		if hdr.Name == "artifact.tar" {
			data, _ := io.ReadAll(tr)
			if string(data) != "artifact tar bytes" {
				t.Fatal("artifact bytes differ")
			}
		}
	}
	if strings.Join(names, ",") != "artifact.tar,manifest.json" {
		t.Fatalf("tar entries: %v", names)
	}
	legacy, _ := r.AppRows(ctx, "paperless")
	if len(legacy) != 1 || legacy[0].Mode != "volumes" || len(legacy[0].Files) != 3 {
		t.Fatalf("legacy row: %+v", legacy)
	}

	// The index rolls it all up, without manifests.
	ix, err := r.Index(ctx)
	if err != nil || len(ix.Kits) != 1 || len(ix.Apps["hub"]) != 2 || len(ix.Apps["paperless"]) != 1 || ix.Apps["hub"][0].Manifest != "" {
		t.Fatalf("index: %+v %v", ix, err)
	}

	// Copy rows say stored with a lock (grade A), and the target says
	// synced; the fleet status column says copied.
	copies, _ := h.st.ListOffsiteCopies(target.ID, "")
	if len(copies) != 4 {
		t.Fatalf("copies: %d", len(copies))
	}
	for _, c := range copies {
		if c.State != "stored" || c.RetainUntil == "" {
			t.Fatalf("copy %+v", c)
		}
	}
	tgt, _ := h.st.OffsiteTargetByName("cloud")
	if tgt.LastSyncAt == "" || tgt.LastError != "" {
		t.Fatalf("target sync state: %+v", tgt)
	}
	info, _ := r.Client.Head(ctx, r.Layout.AppSnapshot("hub", "20260827-010314"))
	if info.LockMode != "COMPLIANCE" {
		t.Fatalf("grade A uploads must be locked: %+v", info)
	}
	// The index is a cache, so it carries no lock even on a grade A target.
	var indexKeys []string
	r.Client.List(ctx, r.Layout.IndexPrefix(), func(o objstore.Object) error {
		indexKeys = append(indexKeys, o.Key)
		return nil
	})
	if len(indexKeys) != 1 {
		t.Fatalf("index objects: %v", indexKeys)
	}
	if info, err := r.Client.Head(ctx, indexKeys[0]); err != nil || info.LockMode != "" {
		t.Fatalf("the index must not be locked: %+v %v", info, err)
	}
	// A second sync copies nothing new, and with nothing moved it doesn't
	// rewrite the index either.
	puts := fake.Calls("put") + fake.Calls("complete-multipart")
	if err := runSync(t, h, "cloud"); err != nil {
		t.Fatal(err)
	}
	if fake.Calls("put")+fake.Calls("complete-multipart") != puts {
		t.Fatalf("second sync wrote: %d → %d", puts, fake.Calls("put")+fake.Calls("complete-multipart"))
	}
	// No staleness while everything is fresh.
	notices, _ := h.st.ActiveNotices()
	for _, n := range notices {
		if strings.HasPrefix(n.Kind, "offsite-") {
			t.Fatalf("unexpected notice %+v", n)
		}
	}
}

// Housekeeping — the index, retention, the blob sweep, lock extension —
// runs when a copy moved or the policy changed, and otherwise once a day,
// not on every 15-minute tick.
func TestSyncHousekeepsOnlyWhenSomethingMoved(t *testing.T) {
	fake := fakes3.New("fog-test")
	fake.Lock = fakes3.LockConfig{Enabled: true}
	h := newHarness(t, fake)
	plantBackups(t, h)
	now := time.Now()
	h.m.Now = func() time.Time { return now }
	target, _, err := h.m.AddTarget(context.Background(), h.spec("cloud"), "key-1", "secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := runSync(t, h, "cloud"); err != nil {
		t.Fatal(err)
	}
	writes := func() int { return fake.Calls("put") + fake.Calls("put-retention") }
	sync := func(step string, wantWrites int) {
		t.Helper()
		before := writes()
		if err := runSync(t, h, "cloud"); err != nil {
			t.Fatalf("%s: %v", step, err)
		}
		if got := writes() - before; got != wantWrites {
			t.Fatalf("%s: %d writes, want %d", step, got, wantWrites)
		}
	}
	sync("nothing moved", 0)

	// A retention change is housekeeping's business: a new index.
	h.st.SetSetting(SettingKeepRecent, "29")
	sync("policy changed", 1)
	sync("policy settled", 0)

	// A copy hidden from outside the sync (the console) moves the rows.
	copies, _ := h.st.ListOffsiteCopiesSlim(target.ID, store.OffsiteKindApp, "stored")
	h.st.SetOffsiteCopyState(copies[0].ID, "hidden")
	sync("a copy hidden", 1)

	// A lock about to lapse waits for the daily pass, not the next tick.
	soon := now.Add(24 * time.Hour).UTC().Format(time.RFC3339)
	copies, _ = h.st.ListOffsiteCopiesSlim(target.ID, store.OffsiteKindKit, "stored")
	h.st.SetOffsiteCopyRetention(copies[0].ID, soon)
	sync("lock lapsing, same day", 0)

	// A day on, both passes run: a fresh index and the re-lock.
	now = now.Add(25 * time.Hour)
	sync("a day later", 2)
	sync("and then quiet", 0)

	// The record lives in the store, so a restarted fog (a fresh Manager)
	// doesn't redo it on its first tick.
	if hk := h.m.readHousekeeping("cloud"); hk.Fingerprint == "" || !hk.At.Equal(now) || !hk.LocksAt.Equal(now) {
		t.Fatalf("housekeeping record: %+v", hk)
	}
}

// The staleness check runs after a pass that landed a copy, and otherwise
// hourly.
func TestSyncAllChecksStalenessHourlyWhenQuiet(t *testing.T) {
	fake := fakes3.New("fog-test")
	h := newHarness(t, fake)
	plantBackups(t, h)
	now := time.Now()
	h.m.Now = func() time.Time { return now }
	if _, _, err := h.m.AddTarget(context.Background(), h.spec("cloud"), "key-1", "secret"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	h.m.syncAll(ctx) // copies land
	if !h.m.stalenessAt.Equal(now) {
		t.Fatal("a pass that landed copies checks staleness")
	}
	checked := now
	now = now.Add(10 * time.Minute)
	h.m.syncAll(ctx)
	if !h.m.stalenessAt.Equal(checked) {
		t.Fatal("a quiet pass within the hour skips the check")
	}
	now = now.Add(time.Hour)
	h.m.syncAll(ctx)
	if !h.m.stalenessAt.Equal(now) {
		t.Fatal("an hour on, the check runs again")
	}
}

func TestSyncRetentionHidesWhatThePolicyDrops(t *testing.T) {
	fake := fakes3.New("fog-test")
	h := newHarness(t, fake)
	plantBackups(t, h)
	h.st.SetSetting(SettingKeepRecent, "1")
	h.st.SetSetting(SettingKeepMonthly, "0")
	h.st.SetSetting(SettingKeepYearly, "0")
	target, _, err := h.m.AddTarget(context.Background(), h.spec("cloud"), "key-1", "secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := runSync(t, h, "cloud"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r, _ := h.m.Reader(ctx, target)
	rows, _ := r.AppRows(ctx, "hub")
	if len(rows) != 1 || rows[0].Stamp != "20260827-010314" {
		t.Fatalf("after retention: %+v", rows)
	}
	// Hidden, not destroyed: the version is still there under the marker.
	old := r.Layout.AppSnapshot("hub", "20260826-010314")
	if _, err := r.Client.Head(ctx, old); err == nil {
		t.Fatal("old snapshot should be hidden")
	}
	if vs := fake.Versions(old); len(vs) != 2 || !strings.HasPrefix(vs[1], "marker:") {
		t.Fatalf("versions: %v", vs)
	}
	copies, _ := h.st.ListOffsiteCopies(target.ID, store.OffsiteKindApp)
	hidden := 0
	for _, c := range copies {
		if c.State == "hidden" {
			hidden++
		}
	}
	if hidden != 1 {
		t.Fatalf("hidden copies: %d", hidden)
	}
	// The legacy app's only backup and the only kit are untouched.
	if _, err := r.Client.Head(ctx, r.Layout.AppSnapshot("paperless", "20260827-010500")); err != nil {
		t.Fatal("a series of one must not be hidden")
	}
}

func TestSyncSkipsCorruptArtifactsAndReportsFailure(t *testing.T) {
	fake := fakes3.New("fog-test")
	h := newHarness(t, fake)
	root, _ := plantBackups(t, h)
	// Corrupt the newest hub artifact on disk.
	os.WriteFile(filepath.Join(root, "hub", "20260827-010314", "artifact.tar"), []byte("rotten"), 0o600)
	if _, _, err := h.m.AddTarget(context.Background(), h.spec("cloud"), "key-1", "secret"); err != nil {
		t.Fatal(err)
	}
	err := runSync(t, h, "cloud")
	if err == nil || !strings.Contains(err.Error(), "changed on disk") {
		t.Fatalf("expected the corrupt artifact to fail the sync: %v", err)
	}
	notices, _ := h.st.ActiveNotices()
	kinds := map[string]bool{}
	for _, n := range notices {
		kinds[n.Kind] = true
	}
	if !kinds[NoticeCorrupt] || !kinds[NoticeSyncFailed] {
		t.Fatalf("notices: %v", kinds)
	}
	tgt, _ := h.st.OffsiteTargetByName("cloud")
	if tgt.LastError == "" || tgt.LastSyncAt != "" {
		t.Fatalf("target must record the failure: %+v", tgt)
	}
	// The intact backups still went.
	target, _ := h.st.OffsiteTargetByName("cloud")
	r, _ := h.m.Reader(context.Background(), target)
	rows, _ := r.AppRows(context.Background(), "hub")
	if len(rows) != 1 || rows[0].Stamp != "20260826-010314" {
		t.Fatalf("intact rows: %+v", rows)
	}
}

func TestSyncResumesAnInterruptedUpload(t *testing.T) {
	smallParts(t)
	fake := fakes3.New("fog-test")
	h := newHarness(t, fake)
	root, _ := plantBackups(t, h)
	big := bytes.Repeat([]byte("big artifact "), int(PartSize)/4)
	sum := sha256.Sum256(big)
	dir := filepath.Join(root, "hub", "20260827-020000")
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "artifact.tar"), big, 0o600)
	os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"format":"fog-backup/1","app":"hub"}`), 0o600)
	hub, _ := h.st.AppByName("hub")
	bigID, _ := h.st.CreateArtifactBackup(hub.ID, "manual", dir, int64(len(big)), "fog-backup/1", hex.EncodeToString(sum[:]), "{}")
	target, _, err := h.m.AddTarget(context.Background(), h.spec("cloud"), "key-1", "secret")
	if err != nil {
		t.Fatal(err)
	}
	fake.FailOp("upload-part", 2)
	if err := runSync(t, h, "cloud"); err == nil {
		t.Fatal("expected the interrupted sync to fail")
	}
	c, err := h.st.OffsiteCopy(target.ID, store.OffsiteKindApp, strconv.FormatInt(bigID, 10))
	if err != nil || c.State != "uploading" || c.UploadID == "" {
		t.Fatalf("copy row after interruption: %+v %v", c, err)
	}
	fake.FailOp("", 0)
	before := fake.Calls("upload-part")
	if err := runSync(t, h, "cloud"); err != nil {
		t.Fatalf("resumed sync: %v", err)
	}
	if fake.Calls("upload-part")-before >= 4 {
		t.Fatalf("resume re-sent every part (%d)", fake.Calls("upload-part")-before)
	}
	c, _ = h.st.OffsiteCopy(target.ID, store.OffsiteKindApp, strconv.FormatInt(bigID, 10))
	if c.State != "stored" || c.UploadID != "" {
		t.Fatalf("copy row after resume: %+v", c)
	}
	r, _ := h.m.Reader(context.Background(), target)
	rc, err := r.Open(context.Background(), r.Layout.AppSnapshot("hub", "20260827-020000"))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Contains(data, big[:1000]) {
		t.Fatal("resumed snapshot does not contain the artifact")
	}
}

func TestStalenessKeysOnAbsenceOfCopies(t *testing.T) {
	fake := fakes3.New("fog-test")
	h := newHarness(t, fake)
	h.st.SetSetting("backup_hour_utc", "1")
	plantBackups(t, h)
	target, _, err := h.m.AddTarget(context.Background(), h.spec("cloud"), "key-1", "secret")
	if err != nil {
		t.Fatal(err)
	}
	// The target is brand new: grace, no alarm even with nothing copied.
	h.m.checkStaleness()
	if n, _ := h.st.ActiveNotices(); len(n) != 0 {
		t.Fatalf("grace period: %+v", n)
	}
	// Three days old, still nothing copied: the apps' local backups are
	// current (planted just now) but no copy has landed → offsite-stale
	// for each; the kit (expected once a passphrase and hour exist) too.
	old := time.Now().Add(-72 * time.Hour).UTC().Format(time.RFC3339)
	if _, err := h.st.DB().Exec(`UPDATE offsite_targets SET created_at = ?`, old); err != nil {
		t.Fatal(err)
	}
	h.st.SetSetting("recovery_kit_passphrase", "passphrase 12345")
	h.m.checkStaleness()
	kinds := map[string]int{}
	notices, _ := h.st.ActiveNotices()
	for _, n := range notices {
		kinds[n.Kind+":"+n.App]++
	}
	if kinds[NoticeKitStale+":"] != 1 || kinds[NoticeStale+":hub"] != 1 || kinds[NoticeStale+":paperless"] != 1 {
		t.Fatalf("expected kit + hub + paperless alarms: %v", kinds)
	}
	// An app whose local backups are themselves stale is the local
	// alarm's; offsite stays quiet for it.
	if _, err := h.st.DB().Exec(`UPDATE backups SET created_at = ? WHERE app_id = (SELECT id FROM apps WHERE name = 'paperless')`, old); err != nil {
		t.Fatal(err)
	}
	h.m.checkStaleness()
	notices, _ = h.st.ActiveNotices()
	for _, n := range notices {
		if n.Kind == NoticeStale && n.App == "paperless" {
			t.Fatal("offsite-stale should defer to the local alarm for a locally stale app")
		}
	}
	// Copies landing clear the alarms.
	stamp := time.Now().UTC().Format("20060102-150405")
	h.st.UpsertOffsiteCopy(store.OffsiteCopy{TargetID: target.ID, Kind: store.OffsiteKindApp, Subject: "999", Key: LayoutOf(target).AppSnapshot("hub", stamp), State: "stored"})
	h.st.UpsertOffsiteCopy(store.OffsiteCopy{TargetID: target.ID, Kind: store.OffsiteKindKit, Subject: "fog-recovery-kit-" + stamp + ".enc", Key: "k", State: "stored"})
	h.m.checkStaleness()
	notices, _ = h.st.ActiveNotices()
	for _, n := range notices {
		if (n.Kind == NoticeStale && n.App == "hub") || n.Kind == NoticeKitStale {
			t.Fatalf("notice should have cleared: %+v", n)
		}
	}
	_ = objstore.ErrNotFound
}

// A hidden copy's row goes once the provider has had time to expire the
// object — and only if nothing local would send it back up.
func TestPruneHiddenForgetsOnlyExpiredCopiesOfWhatIsGone(t *testing.T) {
	fake := fakes3.New("fog-test")
	h := newHarness(t, fake)
	_, ids := plantBackups(t, h)
	target, _, err := h.m.AddTarget(context.Background(), h.spec("cloud"), "key-1", "secret")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	h.m.Now = func() time.Time { return now }
	stamp := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339) }
	long := -60 * 24 * time.Hour // well past the hidden-file expiry
	hub := strconv.FormatInt(ids["hub/20260826-010314"], 10)
	rows := []struct {
		kind, subject, state, hiddenAt, retainUntil string
		pruned                                      bool
	}{
		{store.OffsiteKindBlob, "blob-old", "hidden", stamp(long), "", true},
		{store.OffsiteKindBlob, "blob-recent", "hidden", stamp(-2 * 24 * time.Hour), "", false},
		{store.OffsiteKindBlob, "blob-locked", "hidden", stamp(long), stamp(24 * time.Hour), false},
		{store.OffsiteKindBlob, "blob-stored", "stored", "", "", false},
		{store.OffsiteKindApp, hub, "hidden", stamp(long), "", false}, // the backup is still here
		{store.OffsiteKindApp, "99999", "hidden", stamp(long), "", true},
		{store.OffsiteKindKit, "fog-recovery-kit-20260827-010000.enc", "hidden", stamp(long), "", false}, // still on disk
		{store.OffsiteKindKit, "fog-recovery-kit-20250101-010000.enc", "hidden", stamp(long), "", true},
		{store.OffsiteKindFilesSnapshot, "99999", "hidden", stamp(long), "", true},
	}
	for _, r := range rows {
		if err := h.st.UpsertOffsiteCopy(store.OffsiteCopy{TargetID: target.ID, Kind: r.kind, Subject: r.subject, Key: "k/" + r.subject,
			State: r.state, HiddenAt: r.hiddenAt, RetainUntil: r.retainUntil}); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.m.pruneHidden(target, ReadPolicy(h.st)); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		_, err := h.st.OffsiteCopy(target.ID, r.kind, r.subject)
		if gone := errors.Is(err, store.ErrNotFound); gone != r.pruned {
			t.Errorf("%s %s: pruned = %v, want %v", r.kind, r.subject, gone, r.pruned)
		}
	}
}

// The index reads a Files snapshot's line from its summary alone; a row
// without one is left out rather than costing a manifest read.
func TestFilesSummaryReadsTheSummary(t *testing.T) {
	h := newHarness(t, fakes3.New("fog-test"))
	target, _, err := h.m.AddTarget(context.Background(), h.spec("cloud"), "key-1", "secret")
	if err != nil {
		t.Fatal(err)
	}
	want := IndexFiles{Stamp: "20260101-010000", CreatedAt: "2026-01-01T01:00:00Z", Files: 2, Bytes: 7}
	h.st.UpsertOffsiteCopy(store.OffsiteCopy{TargetID: target.ID, Kind: store.OffsiteKindFilesSnapshot, Subject: "1", Key: "k1", State: "stored",
		Summary: `{"stamp":"20260101-010000","created_at":"2026-01-01T01:00:00Z","files":2,"bytes":7}`})
	h.st.UpsertOffsiteCopy(store.OffsiteCopy{TargetID: target.ID, Kind: store.OffsiteKindFilesSnapshot, Subject: "2", Key: "k2", State: "stored",
		Doc: `{"stamp":"20260101-010000","created_at":"2026-01-01T01:00:00Z","files":[{},{}],"bytes":7,"count":2}`})
	slim, err := h.st.ListOffsiteCopiesSlim(target.ID, store.OffsiteKindFilesSnapshot)
	if err != nil || len(slim) != 2 {
		t.Fatal(slim, err)
	}
	for _, c := range slim {
		got, ok := filesSummary(&c)
		if c.Subject == "2" {
			if ok {
				t.Errorf("a row without a summary was listed: %+v", got)
			}
			continue
		}
		if !ok || got != want {
			t.Errorf("snapshot %s: %+v %v, want %+v", c.Subject, got, ok, want)
		}
	}
}

// A box rebuilt after losing its disk adopts its old locker. Its own copy
// records are empty, and an index built from them alone hid every backup
// the old box had left there — jen's restore wizard came up with nothing
// to restore. The index lists what the locker holds, whoever wrote it.
func TestAnAdoptedLockersBackupsStayInTheIndex(t *testing.T) {
	fake := fakes3.New("fog-test")
	fake.Lock = fakes3.LockConfig{Enabled: true}
	old := newHarness(t, fake)
	old.st.SetSetting("backup_hour_utc", "1")
	plantBackups(t, old)
	if _, _, err := old.m.AddTarget(context.Background(), old.spec("cloud"), "key-1", "secret"); err != nil {
		t.Fatal(err)
	}
	if err := runSync(t, old, "cloud"); err != nil {
		t.Fatalf("old box sync: %v", err)
	}

	rebuilt := newHarness(t, fake)
	empty := t.TempDir()
	rebuilt.m.BackupRoot = func() string { return empty }
	target, _, err := rebuilt.m.AddTarget(context.Background(), rebuilt.spec("cloud"), "key-1", "secret")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r, err := rebuilt.m.Reader(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	for pass := 1; pass <= 2; pass++ { // the second pass builds on the first's index
		if err := runSync(t, rebuilt, "cloud"); err != nil {
			t.Fatalf("rebuilt box sync %d: %v", pass, err)
		}
		ix, err := r.Index(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if ix.Fog != "house" || len(ix.Apps["hub"]) != 2 || len(ix.Apps["paperless"]) != 1 || len(ix.Kits) != 1 {
			t.Fatalf("pass %d index: apps %v kits %v", pass, ix.Apps, ix.Kits)
		}
		if ix.Apps["hub"][0].Stamp != "20260827-010314" || ix.Apps["hub"][0].Manifest != "" {
			t.Fatalf("pass %d hub rows: %+v", pass, ix.Apps["hub"])
		}
	}
	rebuilt.m.forgetIndex("cloud")
	rows, err := rebuilt.m.AppRows(ctx, "hub")
	if err != nil || len(rows) != 2 {
		t.Fatalf("restore wizard rows: %+v %v", rows, err)
	}
}
