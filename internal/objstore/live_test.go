package objstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// The live check against a real provider. Skipped unless the environment
// names a bucket; the fake proves the client's logic, this proves the
// wire — signatures, escaping, the shape of each response — against the
// service that will actually hold the backups.
//
//	FOG_S3_TEST_ENDPOINT=https://s3.us-west-004.backblazeb2.com \
//	FOG_S3_TEST_REGION=us-west-004 FOG_S3_TEST_BUCKET=fog-test \
//	FOG_S3_TEST_KEY_ID=… FOG_S3_TEST_SECRET=… go test ./internal/objstore -run Live -v
//
// Everything is written under a random prefix and hidden afterwards.
// Hidden versions stay until the bucket's lifecycle rule expires them (or
// forever without one) — the test tries a version delete and accepts a
// refusal, which is the answer a no-delete key gives.
func TestLiveProvider(t *testing.T) {
	endpoint := os.Getenv("FOG_S3_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("FOG_S3_TEST_ENDPOINT not set")
	}
	c, err := New(Config{
		Endpoint:  endpoint,
		Region:    os.Getenv("FOG_S3_TEST_REGION"),
		Bucket:    os.Getenv("FOG_S3_TEST_BUCKET"),
		AccessKey: os.Getenv("FOG_S3_TEST_KEY_ID"),
		SecretKey: os.Getenv("FOG_S3_TEST_SECRET"),
		PathStyle: os.Getenv("FOG_S3_TEST_VHOST") == "",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var rnd [6]byte
	rand.Read(rnd[:])
	prefix := "fog-objstore-test/" + hex.EncodeToString(rnd[:]) + "/"
	t.Logf("prefix %s", prefix)
	var written []string
	cleanup := func() {
		for _, k := range written {
			if err := c.Delete(ctx, k); err != nil {
				t.Logf("cleanup hide %s: %v", k, err)
			}
		}
		c.ListVersions(ctx, prefix, func(v Version) error {
			if !v.DeleteMarker {
				if err := c.DeleteVersion(ctx, v.Key, v.VersionID); err != nil && !errors.Is(err, ErrAccessDenied) {
					t.Logf("cleanup version %s: %v", v.Key, err)
				}
			}
			return nil
		})
	}
	defer cleanup()
	put := func(key string, data []byte, opts PutOptions) {
		t.Helper()
		if _, err := c.Put(ctx, prefix+key, bytes.NewReader(data), int64(len(data)), opts); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
		written = append(written, prefix+key)
	}

	if v, err := c.Versioning(ctx); err != nil {
		t.Fatalf("versioning: %v", err)
	} else {
		t.Logf("versioning: %q", v)
	}
	if cfg, err := c.ObjectLock(ctx); err != nil {
		t.Fatalf("object lock: %v", err)
	} else {
		t.Logf("object lock: %+v", cfg)
	}

	// Escaping on the wire: space, colon, plus, unicode.
	odd := "apps/hub/2026 08:27+été.json"
	put(odd, []byte(`{"hello":"locker"}`), PutOptions{ContentType: "application/json", Metadata: map[string]string{"fog-kind": "row"}})
	info, err := c.Head(ctx, prefix+odd)
	if err != nil || info.Size != 18 || info.Metadata["fog-kind"] != "row" {
		t.Fatalf("head odd key: %+v %v", info, err)
	}
	data, _, err := c.GetBytes(ctx, prefix+odd, 1<<10)
	if err != nil || string(data) != `{"hello":"locker"}` {
		t.Fatalf("get odd key: %q %v", data, err)
	}
	rc, _, err := c.Get(ctx, prefix+odd, "bytes=2-6")
	if err != nil {
		t.Fatal(err)
	}
	part := make([]byte, 5)
	rc.Read(part)
	rc.Close()
	if string(part) != "hello" {
		t.Fatalf("range: %q", part)
	}

	// Listing, prefixes, pagination through a modest set.
	for i := 0; i < 12; i++ {
		put("kits/kit-"+strings.Repeat("0", 2-len(string(rune('0'+i%10))))+string(rune('a'+i)), []byte("k"), PutOptions{})
	}
	n := 0
	if err := c.List(ctx, prefix+"kits/", func(Object) error { n++; return nil }); err != nil || n != 12 {
		t.Fatalf("list: %d %v", n, err)
	}
	tops, err := c.ListPrefixes(ctx, prefix)
	if err != nil || strings.Join(tops, ",") != "apps,kits" {
		t.Fatalf("prefixes: %v %v", tops, err)
	}

	// Multipart: two 5 MiB parts and a short tail.
	big := prefix + "files/blobs/aa/bigblob"
	id, err := c.CreateMultipart(ctx, big, PutOptions{ContentType: "application/octet-stream"})
	if err != nil {
		t.Fatalf("create multipart: %v", err)
	}
	chunk := bytes.Repeat([]byte{0xAB}, 5<<20)
	var parts []Part
	for i, ch := range [][]byte{chunk, chunk, []byte("tail")} {
		p, err := c.UploadPart(ctx, big, id, i+1, bytes.NewReader(ch), int64(len(ch)))
		if err != nil {
			c.AbortMultipart(ctx, big, id)
			t.Fatalf("part %d: %v", i+1, err)
		}
		parts = append(parts, p)
	}
	if listed, err := c.ListParts(ctx, big, id); err != nil || len(listed) != 3 {
		t.Fatalf("list parts: %v %v", listed, err)
	}
	if _, err := c.CompleteMultipart(ctx, big, id, parts); err != nil {
		t.Fatalf("complete: %v", err)
	}
	written = append(written, big)
	if info, err := c.Head(ctx, big); err != nil || info.Size != int64(2*len(chunk)+4) {
		t.Fatalf("assembled: %+v %v", info, err)
	}

	// Hide, and prove the credential's reach.
	if err := c.Delete(ctx, prefix+odd); err != nil {
		t.Fatalf("hide: %v", err)
	}
	if _, err := c.Head(ctx, prefix+odd); !errors.Is(err, ErrNotFound) {
		t.Fatalf("hidden must be absent: %v", err)
	}
	var dataVersion string
	c.ListVersions(ctx, prefix+odd, func(v Version) error {
		if !v.DeleteMarker {
			dataVersion = v.VersionID
		}
		return nil
	})
	if dataVersion == "" {
		t.Fatal("hidden object's data version not listed")
	}
	switch err := c.DeleteVersion(ctx, prefix+odd, dataVersion); {
	case err == nil:
		t.Log("credential CAN delete versions (grade C or below)")
	case errors.Is(err, ErrAccessDenied):
		t.Log("credential cannot delete versions (append-only) — as recommended")
	default:
		t.Fatalf("delete version: %v", err)
	}
}
