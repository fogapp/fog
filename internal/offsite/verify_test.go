package offsite

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gitea.lan/matthew/fog/internal/objstore"
	"gitea.lan/matthew/fog/internal/objstore/fakes3"
	"gitea.lan/matthew/fog/internal/store"
)

// The weekly verify fetches the newest copy of each series back from the
// bucket, checks it against its row, test-restores a delegated artifact
// through the injected hook, samples Files blobs — and only then stamps
// verified_at. A blob that no longer hashes to the manifest is a notice.
func TestVerifyStampsCopiesAndCatchesRot(t *testing.T) {
	t.Parallel()

	fake := fakes3.New("fog-test")
	h := newHarness(t, fake)
	root, ids := plantBackups(t, h)
	plantFilesSnapshot(t, h, root, "20260827-011500", map[string][]byte{"a.txt": []byte("alpha"), "b.txt": []byte("beta")})
	target, _, err := h.m.AddTarget(context.Background(), h.spec("cloud"), "key-1", "secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := runSync(t, h, "cloud"); err != nil {
		t.Fatal(err)
	}
	var tested []string
	h.m.TestRestore = func(_ context.Context, app, artifact, manifest string) error {
		if _, err := os.Stat(artifact); err != nil {
			return err
		}
		if !strings.Contains(manifest, "fog-backup/1") {
			t.Errorf("manifest not passed: %q", manifest)
		}
		tested = append(tested, app+":"+filepath.Base(filepath.Dir(artifact)))
		return nil
	}
	ctx := context.Background()
	if err := h.m.verifyTarget(ctx, target); err != nil {
		t.Fatalf("verify: %v", err)
	}
	// hub's newest (delegated) was test-restored; paperless (volumes) was
	// digest-checked only; the Files snapshot was sampled.
	if len(tested) != 1 || !strings.HasPrefix(tested[0], "hub:") {
		t.Fatalf("test-restored: %v", tested)
	}
	for _, sub := range []string{strconv.FormatInt(ids["hub/20260827-010314"], 10), strconv.FormatInt(ids["paperless/20260827-010500"], 10)} {
		c, _ := h.st.OffsiteCopy(target.ID, store.OffsiteKindApp, sub)
		if c.VerifiedAt == "" {
			t.Fatalf("copy %s not verified", sub)
		}
	}
	older, _ := h.st.OffsiteCopy(target.ID, store.OffsiteKindApp, strconv.FormatInt(ids["hub/20260826-010314"], 10))
	if older.VerifiedAt != "" {
		t.Fatal("only the newest of a series is verified")
	}
	snaps, _ := h.st.ListOffsiteCopies(target.ID, store.OffsiteKindFilesSnapshot)
	if len(snaps) != 1 || snaps[0].VerifiedAt == "" {
		t.Fatalf("files snapshot: %+v", snaps)
	}
	if st := offsiteStateOf(h.st, store.OffsiteKindApp, strconv.FormatInt(ids["hub/20260827-010314"], 10)); st != "verified" {
		t.Fatalf("state: %q", st)
	}
	// Not due again for a week.
	tested = nil
	if err := h.m.verifyTarget(ctx, target); err != nil || len(tested) != 0 {
		t.Fatalf("re-verify too soon: %v %v", err, tested)
	}

	// Rot: overwrite a blob in the bucket with sealed garbage. Force the
	// copies due again and expect the Files verify to fail with a notice.
	r, _ := h.m.Reader(ctx, target)
	blob, _ := r.Keys.BlobIDForDigest(sha256hex([]byte("alpha")))
	garbage, _ := r.Keys.SealBytes(r.Layout.Name(r.Layout.Blob(blob)), []byte("not alpha"))
	r.Client.PutBytes(ctx, r.Layout.Blob(blob), garbage, objstore.PutOptions{})
	h.st.DB().Exec(`UPDATE offsite_copies SET verified_at = NULL`)
	err = h.m.verifyTarget(ctx, target)
	if err == nil || !strings.Contains(err.Error(), "files") {
		t.Fatalf("expected the files verify to fail: %v", err)
	}
	notices, _ := h.st.ActiveNotices()
	found := false
	for _, n := range notices {
		if n.Kind == NoticeVerifyFailed && n.App == "files" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no verify-failed notice: %+v", notices)
	}
	// The app copies still verified fine.
	c, _ := h.st.OffsiteCopy(target.ID, store.OffsiteKindApp, strconv.FormatInt(ids["hub/20260827-010314"], 10))
	if c.VerifiedAt == "" {
		t.Fatal("app copies should verify independently of the files failure")
	}
	// A test-restore that fails is a failed verify for that app.
	h.m.TestRestore = func(context.Context, string, string, string) error { return errorsNew("pg_restore exploded") }
	h.st.DB().Exec(`UPDATE offsite_copies SET verified_at = NULL`)
	if err := h.m.verifyTarget(ctx, target); err == nil || !strings.Contains(err.Error(), "pg_restore exploded") {
		t.Fatalf("test-restore failure: %v", err)
	}
	_ = bytes.MinRead
}

type strErr string

func (e strErr) Error() string { return string(e) }

func errorsNew(s string) error { return strErr(s) }

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
