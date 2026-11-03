package objstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/objstore/fakes3"
)

func testClient(t *testing.T, fake *fakes3.Server) *Client {
	t.Helper()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	fake.AccessKey = "0001234567890000000000001"
	c, err := New(Config{Endpoint: srv.URL, Region: "us-west-004", Bucket: fake.Bucket,
		AccessKey: fake.AccessKey, SecretKey: "K001secret", PathStyle: true, HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestPutGetHeadRoundTrip(t *testing.T) {
	fake := fakes3.New("fog-test")
	c := testClient(t, fake)
	ctx := context.Background()

	// A key with a space, a colon and non-ASCII: the escaping the
	// signature uses must be the escaping the wire carries, or the fake
	// (like B2) answers 403 for the mismatch.
	key := "fog-locker/1/abc/files/blobs/ab/photo 2026:été.jpg"
	body := bytes.Repeat([]byte("locker "), 1000)
	etag, err := c.Put(ctx, key, bytes.NewReader(body), int64(len(body)),
		PutOptions{ContentType: "application/octet-stream", Metadata: map[string]string{"fog-kind": "blob"}})
	if err != nil {
		t.Fatal(err)
	}
	if etag == "" {
		t.Fatal("no etag")
	}
	info, err := c.Head(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size != int64(len(body)) || info.ETag != etag || info.ContentType != "application/octet-stream" || info.Metadata["fog-kind"] != "blob" {
		t.Fatalf("head: %+v", info)
	}
	rc, _, err := c.Get(ctx, key, "")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, body) {
		t.Fatal("get returned different bytes")
	}
	rc, info, err = c.Get(ctx, key, "bytes=0-6")
	if err != nil {
		t.Fatal(err)
	}
	got, _ = io.ReadAll(rc)
	rc.Close()
	if string(got) != "locker " || info.Size != 7 {
		t.Fatalf("ranged get: %q size %d", got, info.Size)
	}

	if _, err := c.Head(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("head missing: %v", err)
	}
	if _, _, err := c.GetBytes(ctx, key, 10); err == nil {
		t.Fatal("GetBytes over the limit must refuse")
	}
	data, _, err := c.GetBytes(ctx, key, int64(len(body)))
	if err != nil || !bytes.Equal(data, body) {
		t.Fatalf("GetBytes: %v", err)
	}
}

func TestListPaginationAndPrefixes(t *testing.T) {
	fake := fakes3.New("fog-test")
	fake.PageSize = 2 // force the client through continuation tokens
	c := testClient(t, fake)
	ctx := context.Background()
	for _, k := range []string{"L/apps/feed/a", "L/apps/feed/b", "L/apps/hub/a", "L/kits/k1", "L/kits/k2", "other/x"} {
		if _, err := c.PutBytes(ctx, k, []byte(k), PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	var keys []string
	if err := c.List(ctx, "L/", func(o Object) error { keys = append(keys, o.Key); return nil }); err != nil {
		t.Fatal(err)
	}
	if strings.Join(keys, ",") != "L/apps/feed/a,L/apps/feed/b,L/apps/hub/a,L/kits/k1,L/kits/k2" {
		t.Fatalf("list: %v", keys)
	}
	if fake.Calls("list") < 3 {
		t.Fatalf("expected paged listing, got %d list calls", fake.Calls("list"))
	}
	apps, err := c.ListPrefixes(ctx, "L/apps/")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(apps, ",") != "feed,hub" {
		t.Fatalf("prefixes: %v", apps)
	}
	// A hidden object drops out of listings.
	if err := c.Delete(ctx, "L/kits/k1"); err != nil {
		t.Fatal(err)
	}
	keys = nil
	c.List(ctx, "L/kits/", func(o Object) error { keys = append(keys, o.Key); return nil })
	if strings.Join(keys, ",") != "L/kits/k2" {
		t.Fatalf("after hide: %v", keys)
	}
}

// The property the whole offsite design rests on: on a versioned bucket a
// delete by name hides, and only a version delete — which a no-delete key
// cannot make — destroys.
func TestDeleteHidesAndVersionDeleteNeedsTheRight(t *testing.T) {
	fake := fakes3.New("fog-test")
	c := testClient(t, fake)
	ctx := context.Background()
	if _, err := c.PutBytes(ctx, "k", []byte("v1"), PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Head(ctx, "k"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("hidden object must read as absent, got %v", err)
	}
	// Deleting again is idempotent from the caller's view.
	if err := c.Delete(ctx, "k"); err != nil {
		t.Fatalf("second delete: %v", err)
	}
	var versions []Version
	if err := c.ListVersions(ctx, "k", func(v Version) error { versions = append(versions, v); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(versions) != 2 || !versions[0].DeleteMarker || !versions[0].IsLatest || versions[1].DeleteMarker || versions[1].Size != 2 {
		t.Fatalf("versions: %+v", versions)
	}
	dataVersion := versions[1].VersionID
	if err := c.DeleteVersion(ctx, "k", dataVersion); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("a key without deleteFiles must be refused, got %v", err)
	}
	// The operator unhides in the console; the data was there all along.
	fake.Unhide("k")
	if data, _, err := c.GetBytes(ctx, "k", 100); err != nil || string(data) != "v1" {
		t.Fatalf("after unhide: %v %q", err, data)
	}
	fake.CanDeleteVersions = true
	if err := c.DeleteVersion(ctx, "k", dataVersion); err != nil {
		t.Fatalf("with the right: %v", err)
	}
	if _, err := c.Head(ctx, "k"); !errors.Is(err, ErrNotFound) {
		t.Fatal("version delete must destroy")
	}
}

func TestObjectLock(t *testing.T) {
	fake := fakes3.New("fog-test")
	fake.Lock = fakes3.LockConfig{Enabled: true, Mode: "COMPLIANCE", Days: 30}
	fake.CanDeleteVersions = true
	c := testClient(t, fake)
	ctx := context.Background()

	cfg, err := c.ObjectLock(ctx)
	if err != nil || !cfg.Enabled || cfg.Mode != "COMPLIANCE" || cfg.Days != 30 {
		t.Fatalf("lock config: %+v %v", cfg, err)
	}
	if v, err := c.Versioning(ctx); err != nil || v != "Enabled" {
		t.Fatalf("versioning: %q %v", v, err)
	}

	until := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	if _, err := c.PutBytes(ctx, "locked", []byte("x"), PutOptions{LockMode: "COMPLIANCE", RetainUntil: until}); err != nil {
		t.Fatal(err)
	}
	info, err := c.Head(ctx, "locked")
	if err != nil || info.LockMode != "COMPLIANCE" || !info.RetainUntil.Equal(until) {
		t.Fatalf("head lock: %+v %v", info, err)
	}
	// Even a key WITH deleteFiles cannot destroy a locked version.
	if err := c.DeleteVersion(ctx, "locked", info.VersionID); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("locked version delete: %v", err)
	}
	// Hiding is still allowed — retention is reversible tidy-up.
	if err := c.Delete(ctx, "locked"); err != nil {
		t.Fatalf("hide locked: %v", err)
	}
	// Extension is fine; shortening compliance retention is not.
	later := until.Add(24 * time.Hour)
	if err := c.PutRetention(ctx, "locked", info.VersionID, "COMPLIANCE", later); err != nil {
		t.Fatalf("extend: %v", err)
	}
	if err := c.PutRetention(ctx, "locked", info.VersionID, "COMPLIANCE", until); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("shorten must be refused: %v", err)
	}
	// The bucket default applies to plain uploads.
	if _, err := c.PutBytes(ctx, "plain", []byte("y"), PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if info, err := c.Head(ctx, "plain"); err != nil || info.LockMode != "COMPLIANCE" || info.RetainUntil.Before(time.Now().Add(29*24*time.Hour)) {
		t.Fatalf("default lock: %+v %v", info, err)
	}
}

func TestObjectLockAbsentIsAFact(t *testing.T) {
	fake := fakes3.New("fog-test")
	c := testClient(t, fake)
	cfg, err := c.ObjectLock(context.Background())
	if err != nil || cfg.Enabled {
		t.Fatalf("no lock: %+v %v", cfg, err)
	}
	// Lock headers on an unlocked bucket are a request error, not a retry.
	_, err = c.PutBytes(context.Background(), "k", []byte("x"), PutOptions{LockMode: "COMPLIANCE", RetainUntil: time.Now().Add(time.Hour)})
	var se *Error
	if !errors.As(err, &se) || se.Status != 400 {
		t.Fatalf("expected 400, got %v", err)
	}
	if fake.Calls("put") != 1 {
		t.Fatalf("a 4xx must not be retried, saw %d puts", fake.Calls("put"))
	}
}

func TestMultipart(t *testing.T) {
	fake := fakes3.New("fog-test")
	c := testClient(t, fake)
	ctx := context.Background()
	id, err := c.CreateMultipart(ctx, "big", PutOptions{ContentType: "application/x-fog-locker"})
	if err != nil {
		t.Fatal(err)
	}
	chunks := [][]byte{bytes.Repeat([]byte("a"), 10), bytes.Repeat([]byte("b"), 10), []byte("tail")}
	var parts []Part
	for i, ch := range chunks {
		p, err := c.UploadPart(ctx, "big", id, i+1, bytes.NewReader(ch), int64(len(ch)))
		if err != nil {
			t.Fatal(err)
		}
		parts = append(parts, p)
	}
	listed, err := c.ListParts(ctx, "big", id)
	if err != nil || len(listed) != 3 || listed[2].Size != 4 || listed[0].ETag != parts[0].ETag {
		t.Fatalf("list parts: %+v %v", listed, err)
	}
	// Complete out of order: the client sorts.
	etag, err := c.CompleteMultipart(ctx, "big", id, []Part{parts[2], parts[0], parts[1]})
	if err != nil || !strings.HasSuffix(etag, "-3") {
		t.Fatalf("complete: %q %v", etag, err)
	}
	data, info, err := c.GetBytes(ctx, "big", 100)
	if err != nil || string(data) != strings.Repeat("a", 10)+strings.Repeat("b", 10)+"tail" || info.ContentType != "application/x-fog-locker" {
		t.Fatalf("assembled: %q %+v %v", data, info, err)
	}
	// Abort of an unknown upload is not a failure to the caller.
	if err := c.AbortMultipart(ctx, "big", "gone"); err != nil {
		t.Fatal(err)
	}
	id2, _ := c.CreateMultipart(ctx, "other", PutOptions{})
	if err := c.AbortMultipart(ctx, "other", id2); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListParts(ctx, "other", id2); !errors.Is(err, ErrNotFound) {
		t.Fatalf("aborted upload should be gone: %v", err)
	}
}

func TestRetriesThrottlingButNotClientErrors(t *testing.T) {
	fake := fakes3.New("fog-test")
	c := testClient(t, fake)
	ctx := context.Background()
	fake.FailNext(2)
	body := []byte("resend me")
	if _, err := c.Put(ctx, "k", bytes.NewReader(body), int64(len(body)), PutOptions{}); err != nil {
		t.Fatalf("put through two 503s: %v", err)
	}
	if data, _, _ := c.GetBytes(ctx, "k", 100); string(data) != "resend me" {
		t.Fatalf("the retried body must be re-read from the start, got %q", data)
	}
	// Exhausted retries surface the last error.
	fake.FailNext(10)
	_, err := c.Head(ctx, "k")
	var se *Error
	if !errors.As(err, &se) || se.Status != 503 {
		t.Fatalf("expected the 503 after retries, got %v", err)
	}
	fake.FailNext(0)
	// A cancelled context stops the retry loop promptly.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	fake.FailNext(3)
	if _, err := c.Head(cctx, "k"); err == nil {
		t.Fatal("cancelled context must fail")
	}
}

func TestBadCredentialIsDenied(t *testing.T) {
	fake := fakes3.New("fog-test")
	c := testClient(t, fake)
	c.cfg.AccessKey = "someone-else"
	_, err := c.Head(context.Background(), "k")
	if !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("expected access denied, got %v", err)
	}
}

func TestNewValidates(t *testing.T) {
	if _, err := New(Config{Endpoint: "not a url", Bucket: "b", AccessKey: "a", SecretKey: "s"}); err == nil {
		t.Fatal("bad endpoint accepted")
	}
	if _, err := New(Config{Endpoint: "https://x", Bucket: "b"}); err == nil {
		t.Fatal("missing credentials accepted")
	}
	c, err := New(Config{Endpoint: "https://s3.example.com/", Bucket: "b", AccessKey: "a", SecretKey: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if u := c.urlFor("a b", nil); u.String() != "https://b.s3.example.com/a%20b" {
		t.Fatalf("virtual-hosted url: %s", u)
	}
	c.cfg.PathStyle = true
	if u := c.urlFor("", nil); u.String() != "https://s3.example.com/b" {
		t.Fatalf("path-style bucket url: %s", u)
	}
}
