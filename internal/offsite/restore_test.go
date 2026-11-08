package offsite

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitea.lan/matthew/fog/internal/objstore"
	"gitea.lan/matthew/fog/internal/objstore/fakes3"
)

// A backup pruned locally (or lost with the NAS) comes back from the
// locker as the row it was — same stamp, same creation time, same digest
// — into the backup directory, where the ordinary restore door finds it.
func TestFetchRecreatesALocalBackup(t *testing.T) {
	t.Parallel()

	fake := fakes3.New("fog-test")
	h := newHarness(t, fake)
	root, ids := plantBackups(t, h)
	if _, _, err := h.m.AddTarget(context.Background(), h.spec("cloud"), "key-1", "secret"); err != nil {
		t.Fatal(err)
	}
	if err := runSync(t, h, "cloud"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	hub, _ := h.st.AppByName("hub")
	original, _ := h.st.BackupByID(ids["hub/20260827-010314"])
	// Lose it locally.
	os.RemoveAll(filepath.Join(root, "hub", "20260827-010314"))
	h.st.DeleteBackup(original.ID)

	rows, err := h.m.AppRows(ctx, "hub")
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows: %+v %v", rows, err)
	}
	if rows[0].Stamp != "20260827-010314" || rows[0].Local || rows[1].Stamp != "20260826-010314" || !rows[1].Local || rows[1].BackupID == 0 {
		t.Fatalf("locality: %+v", rows)
	}
	id, err := h.m.Fetch(ctx, nil, "cloud", "hub", "20260827-010314")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	b, err := h.st.BackupByID(id)
	if err != nil {
		t.Fatal(err)
	}
	if b.AppID != hub.ID || b.CreatedAt != original.CreatedAt || b.SHA256 != original.SHA256 || b.Format != "fog-backup/1" || b.Kind != "scheduled" || b.Manifest == "" {
		t.Fatalf("recreated row: %+v vs %+v", b, original)
	}
	got, err := os.ReadFile(filepath.Join(b.Path, "artifact.tar"))
	if err != nil || string(got) != "artifact tar bytes" {
		t.Fatalf("artifact: %q %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(b.Path, "manifest.json")); err != nil {
		t.Fatal("manifest.json not unpacked")
	}
	// Fetching what is already local is a no-op that answers the row.
	again, err := h.m.Fetch(ctx, nil, "cloud", "hub", "20260827-010314")
	if err != nil || again != id {
		t.Fatalf("second fetch: %d %v", again, err)
	}
	rows, _ = h.m.AppRows(ctx, "hub")
	if !rows[0].Local || rows[0].BackupID != id {
		t.Fatalf("after fetch: %+v", rows[0])
	}
	// A legacy snapshot comes back the same way, meta.json and all.
	os.RemoveAll(filepath.Join(root, "paperless", "20260827-010500"))
	h.st.DeleteBackup(ids["paperless/20260827-010500"])
	lid, err := h.m.Fetch(ctx, nil, "cloud", "paperless", "20260827-010500")
	if err != nil {
		t.Fatalf("legacy fetch: %v", err)
	}
	lb, _ := h.st.BackupByID(lid)
	if lb.Format != "" || lb.SizeBytes == 0 {
		t.Fatalf("legacy row: %+v", lb)
	}
	for _, name := range []string{"meta.json", "volume-data.tar.gz", "db-paperless.dump"} {
		if _, err := os.Stat(filepath.Join(lb.Path, name)); err != nil {
			t.Fatalf("legacy file %s missing", name)
		}
	}
	// An app this fog does not have cannot be fetched into.
	if _, err := h.m.Fetch(ctx, nil, "cloud", "nothere", "20260827-010314"); err == nil || !strings.Contains(err.Error(), "install it first") {
		t.Fatalf("unknown app: %v", err)
	}
}

// Fetch unpacks as the object streams in, so it must hold everything back
// until the final frame authenticates: an object whose last frame was
// tampered with leaves no snapshot directory, no staging, and no row.
func TestFetchOfATamperedLastFrameLeavesNothing(t *testing.T) {
	t.Parallel()

	fake := fakes3.New("fog-test")
	h := newHarness(t, fake)
	root, ids := plantBackups(t, h)
	target, _, err := h.m.AddTarget(context.Background(), h.spec("cloud"), "key-1", "secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := runSync(t, h, "cloud"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dest := filepath.Join(root, "hub", "20260827-010314")
	os.RemoveAll(dest)
	h.st.DeleteBackup(ids["hub/20260827-010314"])

	r, err := h.m.Reader(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	key := r.Layout.AppSnapshot("hub", "20260827-010314")
	sealed, _, err := r.Client.GetBytes(ctx, key, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	sealed[len(sealed)-1] ^= 0x01 // the last frame's tag
	if _, err := r.Client.PutBytes(ctx, key, sealed, objstore.PutOptions{}); err != nil {
		t.Fatal(err)
	}

	if _, err := h.m.Fetch(ctx, nil, "cloud", "hub", "20260827-010314"); !errors.Is(err, ErrBadKey) {
		t.Fatalf("fetch of a tampered object: %v", err)
	}
	for _, p := range []string{dest, dest + ".fetching"} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s left behind", p)
		}
	}
	if rows, _ := h.m.AppRows(ctx, "hub"); rows[0].Local {
		t.Fatalf("a row was recorded: %+v", rows[0])
	}
}

func TestFilesSnapshotsListAndRestoreJob(t *testing.T) {
	t.Parallel()

	fake := fakes3.New("fog-test")
	h := newHarness(t, fake)
	root, _ := plantBackups(t, h)
	id := plantFilesSnapshot(t, h, root, "20260827-011500", map[string][]byte{"a.txt": []byte("alpha"), "d/b.txt": []byte("beta")})
	if _, _, err := h.m.AddTarget(context.Background(), h.spec("cloud"), "key-1", "secret"); err != nil {
		t.Fatal(err)
	}
	if err := runSync(t, h, "cloud"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	list, err := h.m.FilesSnapshots(ctx)
	if err != nil || len(list) != 1 || !list[0].Local || list[0].Files != 2 {
		t.Fatalf("list: %+v %v", list, err)
	}
	os.RemoveAll(filepath.Join(root, "files", "20260827-011500"))
	h.st.DeleteFileSnapshot(id)
	list, _ = h.m.FilesSnapshots(ctx)
	if list[0].Local {
		t.Fatal("removed snapshot must read as offsite-only")
	}
	filesRoot := t.TempDir()
	h.m.FilesRoot = func() string { return filesRoot }
	kicked := false
	h.m.FilesRestored = func() { kicked = true }
	if _, err := h.m.StartFilesRestore("cloud", "20260827-011500", "../escape"); err == nil {
		t.Fatal("escape destination accepted")
	}
	job, err := h.m.StartFilesRestore("cloud", "20260827-011500", "Restored")
	if err != nil {
		t.Fatal(err)
	}
	done, _ := h.m.jobs().Wait(ctx, job.ID)
	if done.State != "done" || !kicked {
		t.Fatalf("job: %+v kicked=%v", done, kicked)
	}
	if got, _ := os.ReadFile(filepath.Join(filesRoot, "Restored", "d", "b.txt")); string(got) != "beta" {
		t.Fatalf("restored file: %q", got)
	}
}

// Recover everything: every app this fog knows gets its newest snapshot
// fetched and pushed through the door that fits — a deployed app's
// normal restore, a delegated app's staged first boot, a volume app's
// deploy-then-restore — and Files lands in the root.
func TestRecoverEverything(t *testing.T) {
	t.Parallel()

	fake := fakes3.New("fog-test")
	h := newHarness(t, fake)
	root, ids := plantBackups(t, h)
	plantFilesSnapshot(t, h, root, "20260827-011500", map[string][]byte{"a.txt": []byte("alpha")})
	if _, _, err := h.m.AddTarget(context.Background(), h.spec("cloud"), "key-1", "secret"); err != nil {
		t.Fatal(err)
	}
	if err := runSync(t, h, "cloud"); err != nil {
		t.Fatal(err)
	}
	// The box "forgets" its backups: the kit brought the app rows back,
	// but the snapshot directories are gone.
	for _, id := range ids {
		b, _ := h.st.BackupByID(id)
		os.RemoveAll(b.Path)
		h.st.DeleteBackup(id)
	}
	var restored []int64
	var staged, deployed []string
	h.m.RestoreApp = func(_ context.Context, id int64) error { restored = append(restored, id); return nil }
	h.m.Deployed = func(name string) bool { return false }
	h.m.DeployRestored = func(_ context.Context, name, artifact string) error {
		if _, err := os.Stat(artifact); err != nil {
			return err
		}
		staged = append(staged, name)
		return nil
	}
	h.m.Deploy = func(_ context.Context, name string) error { deployed = append(deployed, name); return nil }
	filesRoot := t.TempDir()
	h.m.FilesRoot = func() string { return filesRoot }

	job, err := h.m.StartRecover("cloud", false)
	if err != nil {
		t.Fatal(err)
	}
	done, _ := h.m.jobs().Wait(context.Background(), job.ID)
	if done.State != "done" {
		t.Fatalf("recover: %+v", done)
	}
	res, ok := done.Result.(RecoverResult)
	if !ok || len(res.Apps) != 2 || res.Files == nil || res.Files.Status != "restored" {
		t.Fatalf("result: %+v", done.Result)
	}
	for _, step := range res.Apps {
		if step.Status != "restored" {
			t.Fatalf("step: %+v", step)
		}
	}
	// hub is delegated and undeployed → staged for first boot; paperless
	// is volumes and undeployed → deployed, then restored.
	if strings.Join(staged, ",") != "hub" || strings.Join(deployed, ",") != "paperless" || len(restored) != 1 {
		t.Fatalf("doors: staged %v deployed %v restored %v", staged, deployed, restored)
	}
	if got, _ := os.ReadFile(filepath.Join(filesRoot, "a.txt")); string(got) != "alpha" {
		t.Fatal("files not restored")
	}
	// The newest snapshot per app was the one fetched.
	hub, _ := h.st.AppByName("hub")
	list, _ := h.st.ListBackups(hub.ID, 10)
	if len(list) != 1 || filepath.Base(list[0].Path) != "20260827-010314" {
		t.Fatalf("fetched: %+v", list)
	}
	// Deployed apps take the normal door.
	h.m.Deployed = func(string) bool { return true }
	restored, staged, deployed = nil, nil, nil
	job, _ = h.m.StartRecover("cloud", false)
	done, _ = h.m.jobs().Wait(context.Background(), job.ID)
	if done.State != "done" || len(restored) != 2 || len(staged) != 0 || len(deployed) != 0 {
		t.Fatalf("deployed doors: %+v restored %v", done, restored)
	}
}

// The fresh-machine door: credentials and passphrase, nothing else.
func TestOpenForRecoveryAndPreview(t *testing.T) {
	t.Parallel()

	fake := fakes3.New("fog-test")
	h := newHarness(t, fake)
	plantBackups(t, h)
	if _, _, err := h.m.AddTarget(context.Background(), h.spec("cloud"), "key-1", "secret"); err != nil {
		t.Fatal(err)
	}
	if err := runSync(t, h, "cloud"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	spec := h.spec("x")
	fresh := &Manager{HTTPClient: h.m.HTTPClient}
	_ = fresh
	// OpenForRecovery builds its own client; point it at the fake through
	// the package-level default transport by using the test server's URL.
	r, lk, err := OpenForRecovery(ctx, spec, "key-1", "secret", "passphrase 12345", nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if lk.Name != "house" {
		t.Fatalf("locker: %+v", lk)
	}
	pv, err := Preview(ctx, r, lk)
	if err != nil || len(pv.Kits) != 1 || strings.Join(pv.Apps, ",") != "hub,paperless" {
		t.Fatalf("preview: %+v %v", pv, err)
	}
	var kitBuf bytes.Buffer
	name := pv.Kits[0].Name
	if _, err := r.FetchKit(ctx, name, &kitBuf); err != nil || name != "fog-recovery-kit-20260827-010000.enc" || !bytes.Equal(kitBuf.Bytes(), []byte("FOGKIT1\x00sealed kit bytes")) {
		t.Fatalf("kit: %q %v", name, err)
	}
	if _, _, err := OpenForRecovery(ctx, spec, "key-1", "secret", "wrong passphrase", nil); !errors.Is(err, ErrWrongPassphrase) {
		t.Fatalf("wrong passphrase: %v", err)
	}
	spec.Prefix = "nowhere"
	if _, _, err := OpenForRecovery(ctx, spec, "key-1", "secret", "passphrase 12345", nil); err == nil || !strings.Contains(err.Error(), "no locker") {
		t.Fatalf("empty prefix: %v", err)
	}
}
