package offsite

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitea.lan/matthew/fog/internal/objstore"
	"gitea.lan/matthew/fog/internal/objstore/fakes3"
)

// Leaving a provider: the locker is copied object by object into a
// second target, verified as it goes, under the same id — so the new
// bucket opens with the same passphrase, lists the same history, and the
// next sync to it uploads nothing.
func TestCopyLockerToAnotherTarget(t *testing.T) {
	smallParts(t)
	src := fakes3.New("fog-test")
	h := newHarness(t, src)
	root, _ := plantBackups(t, h)
	plantFilesSnapshot(t, h, root, "20260827-011500", map[string][]byte{"a.txt": []byte("alpha"), "big.bin": bytes.Repeat([]byte("b"), int(PartSize)+10)})
	ctx := context.Background()
	if _, _, err := h.m.AddTarget(ctx, h.spec("cloud"), "key-1", "secret"); err != nil {
		t.Fatal(err)
	}
	if err := runSync(t, h, "cloud"); err != nil {
		t.Fatal(err)
	}
	source, _ := h.st.OffsiteTargetByName("cloud")

	// A second bucket at a second provider, with Object Lock.
	dstFake := fakes3.New("fog-other")
	dstFake.AccessKey = "key-2"
	dstFake.Lock = fakes3.LockConfig{Enabled: true}
	dstSrv := httptest.NewServer(dstFake)
	defer dstSrv.Close()
	spec := TargetSpec{Name: "other", Provider: "s3", Endpoint: dstSrv.URL, Region: "eu", Bucket: "fog-other", Prefix: "new/home", PathStyle: true}
	dest, _, err := h.m.AddTarget(ctx, spec, "key-2", "s")
	if err != nil {
		t.Fatal(err)
	}
	if dest.LockerID == source.LockerID {
		t.Fatal("a fresh target gets its own locker")
	}
	job, err := h.m.StartCopy("cloud", "other")
	if err != nil {
		t.Fatal(err)
	}
	done, _ := h.m.jobs().Wait(ctx, job.ID)
	if done.State != "done" {
		t.Fatalf("copy: %+v", done)
	}
	dest, _ = h.st.OffsiteTargetByName("other")
	if dest.LockerID != source.LockerID {
		t.Fatalf("destination should now be a home of the source locker: %s vs %s", dest.LockerID, source.LockerID)
	}
	// Exactly one locker is visible under the destination prefix.
	dc, _ := h.m.Client(dest)
	ids, _ := Discover(ctx, dc, "new/home")
	if len(ids) != 1 || ids[0] != source.LockerID {
		t.Fatalf("lockers at destination: %v", ids)
	}
	// It opens with the passphrase alone and lists the same history.
	r, lk, err := OpenForRecovery(ctx, spec, "key-2", "s", "passphrase 12345", nil)
	if err != nil || lk.ID != source.LockerID {
		t.Fatalf("open destination: %v", err)
	}
	pv, _ := Preview(ctx, r, lk)
	if strings.Join(pv.Apps, ",") != "hub,paperless" || len(pv.Kits) != 1 || len(pv.Files) != 1 {
		t.Fatalf("preview: %+v", pv)
	}
	var kitBuf bytes.Buffer
	r.FetchKit(ctx, pv.Kits[0].Name, &kitBuf)
	if kitBuf.String() != "FOGKIT1\x00sealed kit bytes" {
		t.Fatal("kit bytes differ")
	}
	if _, _, err := r.RestoreFiles(ctx, "20260827-011500", t.TempDir(), nil); err != nil {
		t.Fatalf("files from the copy: %v", err)
	}
	// Copy rows mirror the source, locked at the destination's grade.
	srcRows, _ := h.st.ListOffsiteCopies(source.ID, "")
	dstRows, _ := h.st.ListOffsiteCopies(dest.ID, "")
	if len(dstRows) != len(srcRows) {
		t.Fatalf("copies: %d vs %d", len(dstRows), len(srcRows))
	}
	for _, c := range dstRows {
		if c.State != "stored" || c.RetainUntil == "" || !strings.HasPrefix(c.Key, "new/home/fog-locker/1/"+source.LockerID+"/") {
			t.Fatalf("dest copy: %+v", c)
		}
	}
	// The next sync to the destination has nothing to upload but the index.
	puts := dstFake.Calls("put") + dstFake.Calls("complete-multipart")
	if err := runSync(t, h, "other"); err != nil {
		t.Fatalf("sync after copy: %v", err)
	}
	if got := dstFake.Calls("put") + dstFake.Calls("complete-multipart"); got != puts+1 {
		t.Fatalf("sync after copy re-uploaded: %d → %d", puts, got)
	}
	// A copy into a target that holds a locker of its own, with data in
	// it, is refused; a copy between two homes of the same locker is a
	// resume and allowed.
	thirdFake := fakes3.New("fog-third")
	thirdFake.AccessKey = "key-3"
	thirdSrv := httptest.NewServer(thirdFake)
	defer thirdSrv.Close()
	if _, _, err := h.m.AddTarget(ctx, TargetSpec{Name: "third", Provider: "s3", Endpoint: thirdSrv.URL, Bucket: "fog-third", PathStyle: true}, "key-3", "s"); err != nil {
		t.Fatal(err)
	}
	if err := runSync(t, h, "third"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.m.StartCopy("cloud", "third"); err == nil || !strings.Contains(err.Error(), "already holds a locker") {
		t.Fatalf("copy over a populated locker of a different id must be refused: %v", err)
	}
	if job, err := h.m.StartCopy("other", "cloud"); err != nil {
		t.Fatalf("copy between homes of one locker: %v", err)
	} else if done, _ := h.m.jobs().Wait(ctx, job.ID); done.State != "done" {
		t.Fatalf("resume copy: %+v", done)
	}
	// Retire the old home: the loop skips it, readers still work.
	if err := h.m.Retire("cloud"); err != nil {
		t.Fatal(err)
	}
	if tgt, _ := h.st.OffsiteTargetByName("cloud"); tgt.Role != "retired" {
		t.Fatal("not retired")
	}
	if _, err := h.m.StartSync("cloud"); err != nil {
		t.Fatal(err)
	}
	rows, err := h.m.AppRows(ctx, "hub")
	if err != nil || len(rows) != 6 { // two stamps × three targets, the retired one included
		t.Fatalf("rows across targets: %d %v", len(rows), err)
	}
	if err := h.m.Activate("cloud"); err != nil {
		t.Fatal(err)
	}
	_ = objstore.ErrNotFound
}

// An app backup is two objects under one copy row — the snapshot and its
// sealed row — and the copy carries both: the destination lists the rows
// by itself, and an app snapshot lost locally fetches back from it.
func TestCopyLockerCarriesAppRows(t *testing.T) {
	src := fakes3.New("fog-test")
	h := newHarness(t, src)
	root, ids := plantBackups(t, h)
	ctx := context.Background()
	if _, _, err := h.m.AddTarget(ctx, h.spec("cloud"), "key-1", "secret"); err != nil {
		t.Fatal(err)
	}
	if err := runSync(t, h, "cloud"); err != nil {
		t.Fatal(err)
	}
	dstFake := fakes3.New("fog-other")
	dstFake.AccessKey = "key-2"
	dstSrv := httptest.NewServer(dstFake)
	defer dstSrv.Close()
	spec := TargetSpec{Name: "other", Provider: "s3", Endpoint: dstSrv.URL, Bucket: "fog-other", Prefix: "moved", PathStyle: true}
	if _, _, err := h.m.AddTarget(ctx, spec, "key-2", "s"); err != nil {
		t.Fatal(err)
	}
	job, err := h.m.StartCopy("cloud", "other")
	if err != nil {
		t.Fatal(err)
	}
	if done, _ := h.m.jobs().Wait(ctx, job.ID); done.State != "done" {
		t.Fatalf("copy: %+v", done)
	}

	// The rows themselves are at the destination, by listing.
	dest, _ := h.st.OffsiteTargetByName("other")
	r, err := h.m.Reader(ctx, dest)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := r.AppRows(ctx, "hub")
	if err != nil || len(rows) != 2 || rows[0].Stamp != "20260827-010314" {
		t.Fatalf("hub rows at the destination: %+v %v", rows, err)
	}

	// Lose a snapshot locally and fetch it back from the destination.
	os.RemoveAll(filepath.Join(root, "hub", "20260827-010314"))
	h.st.DeleteBackup(ids["hub/20260827-010314"])
	id, err := h.m.Fetch(ctx, nil, "other", "hub", "20260827-010314")
	if err != nil {
		t.Fatalf("fetch from the destination: %v", err)
	}
	b, _ := h.st.BackupByID(id)
	if got, err := os.ReadFile(filepath.Join(b.Path, "artifact.tar")); err != nil || string(got) != "artifact tar bytes" {
		t.Fatalf("artifact: %q %v", got, err)
	}
}

// A source object that no longer authenticates is not copied — and is
// named — rather than carried to the new home as garbage.
func TestCopyRefusesTamperedObjects(t *testing.T) {
	src := fakes3.New("fog-test")
	h := newHarness(t, src)
	plantBackups(t, h)
	ctx := context.Background()
	if _, _, err := h.m.AddTarget(ctx, h.spec("cloud"), "key-1", "secret"); err != nil {
		t.Fatal(err)
	}
	if err := runSync(t, h, "cloud"); err != nil {
		t.Fatal(err)
	}
	source, _ := h.st.OffsiteTargetByName("cloud")
	r, _ := h.m.Reader(ctx, source)
	key := r.Layout.AppSnapshot("hub", "20260827-010314")
	r.Client.PutBytes(ctx, key, []byte("FOGLKR1\x01\x00\x00\x00\x01garbage garbage garbage garbage garbage"), objstore.PutOptions{})

	dstFake := fakes3.New("fog-other")
	dstFake.AccessKey = "key-2"
	dstSrv := httptest.NewServer(dstFake)
	defer dstSrv.Close()
	spec := TargetSpec{Name: "other", Provider: "s3", Endpoint: dstSrv.URL, Bucket: "fog-other", PathStyle: true}
	if _, _, err := h.m.AddTarget(ctx, spec, "key-2", "s"); err != nil {
		t.Fatal(err)
	}
	job, _ := h.m.StartCopy("cloud", "other")
	done, _ := h.m.jobs().Wait(ctx, job.ID)
	if done.State != "failed" || !strings.Contains(done.Error, "20260827-010314") || !strings.Contains(done.Error, "authenticate") {
		t.Fatalf("expected the tampered object to be named: %+v", done)
	}
	// Everything else did land, and a rerun only retries the bad one.
	dest, _ := h.st.OffsiteTargetByName("other")
	rows, _ := h.st.ListOffsiteCopies(dest.ID, "")
	if len(rows) != 3 {
		t.Fatalf("copied despite the failure: %d", len(rows))
	}
}
