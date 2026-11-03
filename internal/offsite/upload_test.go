package offsite

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"gitea.lan/matthew/fog/internal/objstore"
)

func smallParts(t *testing.T) {
	t.Helper()
	orig := PartSize
	PartSize = 1 << 20
	t.Cleanup(func() { PartSize = orig })
}

func TestUploadSingleAndMultipartRoundTrip(t *testing.T) {
	smallParts(t)
	r, _, fake := testBucket(t)
	ctx := context.Background()
	up := &Uploader{Client: r.Client, Keys: r.Keys, Spool: t.TempDir()}
	// Sealing adds ~16 bytes per 64 KiB, so "fits in one part" is judged
	// on the sealed size: PartSize-1000 plaintext still goes up in one PUT.
	for _, n := range []int{0, 10, int(PartSize) - 1000, int(PartSize) * 2, int(PartSize)*2 + 5000} {
		plain := make([]byte, n)
		rand.Read(plain)
		path := filepath.Join(t.TempDir(), "f")
		os.WriteFile(path, plain, 0o600)
		key := r.Layout.Blob("aa" + hex.EncodeToString([]byte{byte(n)}))
		var state UploadState
		res, err := up.Upload(ctx, key, r.Layout.Name(key), FileSource(path), &state, func(*UploadState) error { return nil })
		if err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		want := sha256.Sum256(plain)
		if res.SHA256 != hex.EncodeToString(want[:]) || res.PlainBytes != int64(n) || res.Bytes != SealedSize(int64(n)) {
			t.Fatalf("n=%d: result %+v", n, res)
		}
		rc, err := r.Open(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(rc)
		rc.Close()
		if err != nil || !bytes.Equal(got, plain) {
			t.Fatalf("n=%d: round trip %v", n, err)
		}
		if state.UploadID != "" {
			t.Fatal("upload id must be cleared on completion")
		}
	}
	if fake.Calls("create-multipart") != 2 {
		t.Fatalf("expected two multipart uploads, got %d", fake.Calls("create-multipart"))
	}
	// The spool is clean.
	if ents, _ := os.ReadDir(up.Spool); len(ents) != 0 {
		t.Fatalf("spool not cleaned: %v", ents)
	}
}

// A crash mid-upload leaves the upload id and nonce; the next attempt
// re-seals identically, learns which parts landed, and sends the rest.
func TestUploadResumes(t *testing.T) {
	smallParts(t)
	r, _, fake := testBucket(t)
	ctx := context.Background()
	plain := make([]byte, int(PartSize)*3+123)
	rand.Read(plain)
	path := filepath.Join(t.TempDir(), "f")
	os.WriteFile(path, plain, 0o600)
	key := r.Layout.Blob("abcdef")
	var saved UploadState
	save := func(s *UploadState) error { saved = *s; return nil }
	// Progress counts sealed bytes, sent or skipped.
	var progressed int64
	up := &Uploader{Client: r.Client, Keys: r.Keys, Spool: t.TempDir(), Progress: func(n int64) { progressed += n }}

	// First attempt: part 1 lands, then the bucket goes away for good.
	partsBefore := fake.Calls("upload-part")
	state := UploadState{}
	fake.FailOp("upload-part", 1)
	_, err := up.Upload(ctx, key, r.Layout.Name(key), FileSource(path), &state, save)
	if err == nil {
		t.Fatal("expected the first attempt to fail")
	}
	if saved.UploadID == "" || saved.Nonce == nil {
		t.Fatalf("state must survive the failure: %+v", saved)
	}
	firstAttemptParts := fake.Calls("upload-part") - partsBefore

	// Second attempt resumes from the saved state.
	fake.FailOp("", 0)
	progressed = 0
	before := fake.Calls("upload-part")
	res, err := up.Upload(ctx, key, r.Layout.Name(key), FileSource(path), &saved, save)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	resumedParts := fake.Calls("upload-part") - before
	if resumedParts >= 4 || firstAttemptParts+resumedParts < 4 {
		t.Fatalf("resume should skip landed parts: first attempt sent %d, resume sent %d", firstAttemptParts, resumedParts)
	}
	if progressed != res.Bytes {
		t.Fatalf("progress %d, sealed %d", progressed, res.Bytes)
	}
	rc, _ := r.Open(ctx, key)
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatal("resumed object does not open to the source")
	}
	// A vanished multipart (provider expired it) restarts cleanly — and under
	// a fresh nonce, since the lost parts may have been observed and the
	// source could have changed, so the old nonce must not seal new data.
	staleNonce := append([]byte(nil), saved.Nonce...)
	state = UploadState{UploadID: "upload-gone", Nonce: staleNonce}
	if _, err := up.Upload(ctx, r.Layout.Blob("abcdef2"), "x", FileSource(path), &state, save); err != nil {
		t.Fatalf("restart after lost upload: %v", err)
	}
	if bytes.Equal(state.Nonce, staleNonce) {
		t.Fatal("a lost multipart must restart under a fresh nonce, not the saved one")
	}
}

// If the source changes between an interrupted upload and its resume, the
// parts already in the bucket were sealed under the saved nonce from the old
// bytes. Reusing that nonce for the new bytes would be a catastrophic
// nonce reuse and would record an object that does not decrypt. The resume
// must detect the change, start over under a fresh nonce, and land an object
// that opens to the new source.
func TestUploadRestartsUnderFreshNonceWhenSourceChanges(t *testing.T) {
	smallParts(t)
	r, _, fake := testBucket(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "f")
	v1 := make([]byte, int(PartSize)*3+123)
	rand.Read(v1)
	os.WriteFile(path, v1, 0o600)
	key := r.Layout.Blob("abcdef")
	var saved UploadState
	save := func(s *UploadState) error { saved = *s; return nil }
	up := &Uploader{Client: r.Client, Keys: r.Keys, Spool: t.TempDir()}

	// First attempt: part 1 lands, then every later part fails.
	fake.FailOp("upload-part", 1)
	state := UploadState{}
	if _, err := up.Upload(ctx, key, r.Layout.Name(key), FileSource(path), &state, save); err == nil {
		t.Fatal("expected the first attempt to fail after part 1")
	}
	if saved.UploadID == "" || saved.Nonce == nil {
		t.Fatalf("state must survive the failure: %+v", saved)
	}
	oldNonce := append([]byte(nil), saved.Nonce...)

	// The source changes under the resume (same length, different bytes).
	v2 := make([]byte, len(v1))
	rand.Read(v2)
	os.WriteFile(path, v2, 0o600)

	fake.FailOp("", 0)
	res, err := up.Upload(ctx, key, r.Layout.Name(key), FileSource(path), &saved, save)
	if err != nil {
		t.Fatalf("resume over a changed source: %v", err)
	}
	if saved.Nonce == nil || bytes.Equal(saved.Nonce, oldNonce) {
		t.Fatal("a changed source must be re-sealed under a fresh nonce")
	}
	want := sha256.Sum256(v2)
	if res.SHA256 != hex.EncodeToString(want[:]) {
		t.Fatal("result digest must be the new source's")
	}
	// And the object in the bucket opens to the new source, whole.
	rc, err := r.Open(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || !bytes.Equal(got, v2) {
		t.Fatalf("stored object does not open to the changed source: err %v, equal %v", err, bytes.Equal(got, v2))
	}
}

func TestDirSourceIsDeterministicAndFlat(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{}`), 0o644)
	os.WriteFile(filepath.Join(dir, "artifact.tar"), bytes.Repeat([]byte("x"), 5000), 0o644)
	os.Mkdir(filepath.Join(dir, "sub"), 0o755)
	src, names, err := DirSource(dir)
	if err != nil || len(names) != 2 || names[0] != "artifact.tar" {
		t.Fatalf("%v %v", names, err)
	}
	read := func() []byte {
		rc, _ := src.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		return b
	}
	a, b := read(), read()
	if !bytes.Equal(a, b) || len(a) == 0 {
		t.Fatal("two opens must yield identical tars")
	}
	// Uploading a raw source (kits) sends the bytes as they are.
	r, _, _ := testBucket(t)
	up := &Uploader{Client: r.Client, Spool: t.TempDir()}
	var state UploadState
	res, err := up.Upload(context.Background(), r.Layout.Kit("k.enc"), "kits/k.enc", src, &state, func(*UploadState) error { return nil })
	if err != nil || res.Bytes != int64(len(a)) {
		t.Fatalf("raw: %+v %v", res, err)
	}
	got, _, _ := r.Client.GetBytes(context.Background(), r.Layout.Kit("k.enc"), 1<<20)
	if !bytes.Equal(got, a) {
		t.Fatal("raw upload changed the bytes")
	}
}

func TestRateLimiter(t *testing.T) {
	if NewRateLimiter(0) != nil {
		t.Fatal("0 must mean no cap")
	}
	l := NewRateLimiter(8) // 1 MB/s, 1 MB burst
	ctx, cancel := context.WithCancel(context.Background())
	if err := l.Wait(ctx, 500_000); err != nil {
		t.Fatal(err)
	}
	cancel()
	// Past the burst, a cancelled context returns promptly instead of
	// sleeping for the shortfall.
	if err := l.Wait(ctx, 5_000_000); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait: %v", err)
	}
	_ = objstore.ErrNotFound
}
