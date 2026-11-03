package offsite

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"gitea.lan/matthew/fog/internal/objstore"
	"gitea.lan/matthew/fog/internal/objstore/fakes3"
)

// flakyBucket is testBucket behind a front that, while armed, swallows a
// whole upload body and then answers 503 — a part that went out on the
// wire and must go again.
func flakyBucket(t *testing.T, failPuts int32) (*Reader, *fakes3.Server, *atomic.Int32) {
	t.Helper()
	r, _, fake := testBucket(t)
	var fails atomic.Int32
	fails.Store(failPuts)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPut && fails.Load() > 0 {
			fails.Add(-1)
			io.Copy(io.Discard, req.Body)
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, `<Error><Code>SlowDown</Code></Error>`)
			return
		}
		fake.ServeHTTP(w, req)
	}))
	t.Cleanup(srv.Close)
	client, err := objstore.New(objstore.Config{Endpoint: srv.URL, Region: "x", Bucket: "fog-test",
		AccessKey: "k", SecretKey: "s", PathStyle: true, HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return &Reader{Client: client, Layout: r.Layout, Keys: r.Keys}, fake, &fails
}

// A part re-sent after a failure reports its bytes once: progress ends at
// the object's size, not the size plus whatever the failed attempt sent.
func TestUploadProgressIsNotDoubleCountedOnRetry(t *testing.T) {
	smallParts(t)
	for _, n := range []int{5000, int(PartSize)*2 + 77} {
		r, _, fails := flakyBucket(t, 1)
		plain := make([]byte, n)
		rand.Read(plain)
		path := filepath.Join(t.TempDir(), "f")
		os.WriteFile(path, plain, 0o600)
		var progressed int64
		up := &Uploader{Client: r.Client, Keys: r.Keys, Spool: t.TempDir(), Progress: func(n int64) { progressed += n }}
		var state UploadState
		key := r.Layout.Blob("retry")
		res, err := up.Upload(context.Background(), key, r.Layout.Name(key), FileSource(path), &state, func(*UploadState) error { return nil })
		if err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		if fails.Load() != 0 {
			t.Fatalf("n=%d: the injected failure never fired", n)
		}
		if progressed != res.Bytes {
			t.Fatalf("n=%d: progress %d for a %d-byte object", n, progressed, res.Bytes)
		}
	}
}

// Parts bigger than the in-memory stage spill to the spool, still hashed
// as they are written (the bucket checks both digests), and the spool is
// empty afterwards.
func TestUploadSpillsBigPartsToTheSpool(t *testing.T) {
	smallParts(t)
	orig := memPartMax
	memPartMax = 64 << 10
	t.Cleanup(func() { memPartMax = orig })
	r, _, _ := testBucket(t)
	plain := make([]byte, int(PartSize)*2+300_000)
	rand.Read(plain)
	path := filepath.Join(t.TempDir(), "f")
	os.WriteFile(path, plain, 0o600)
	up := &Uploader{Client: r.Client, Keys: r.Keys, Spool: t.TempDir()}
	var state UploadState
	key := r.Layout.Blob("spill")
	if _, err := up.Upload(context.Background(), key, r.Layout.Name(key), FileSource(path), &state, func(*UploadState) error { return nil }); err != nil {
		t.Fatal(err)
	}
	rc, err := r.Open(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("round trip: %v", err)
	}
	if ents, _ := os.ReadDir(up.Spool); len(ents) != 0 {
		t.Fatalf("spool not cleaned: %v", ents)
	}
}

func fileSHA(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// A file uploaded as it is goes up from the file itself: nothing is
// staged in the spool, single PUT or multipart.
func TestRawFileUploadSendsSectionsWithoutSpooling(t *testing.T) {
	smallParts(t)
	r, _, fake := testBucket(t)
	for _, n := range []int{0, 1000, int(PartSize) * 2, int(PartSize)*3 + 9} {
		data := make([]byte, n)
		rand.Read(data)
		path := filepath.Join(t.TempDir(), "kit")
		os.WriteFile(path, data, 0o600)
		spool := filepath.Join(t.TempDir(), "spool")
		up := &Uploader{Client: r.Client, Spool: spool}
		var state UploadState
		key := r.Layout.Kit("k.enc")
		res, err := up.Upload(context.Background(), key, "kits/k.enc", FileSource(path), &state, func(*UploadState) error { return nil })
		if err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		if res.Bytes != int64(n) || res.PlainBytes != int64(n) || res.SHA256 != fileSHA(data) {
			t.Fatalf("n=%d: %+v", n, res)
		}
		got, _, err := r.Client.GetBytes(context.Background(), key, 1<<30)
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("n=%d: bytes changed: %v", n, err)
		}
		if _, err := os.Stat(spool); !os.IsNotExist(err) {
			t.Fatalf("n=%d: the spool was used", n)
		}
	}
	if fake.Calls("create-multipart") != 2 {
		t.Fatalf("multipart uploads: %d", fake.Calls("create-multipart"))
	}
}
