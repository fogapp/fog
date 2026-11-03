package objstore

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"testing"

	"gitea.lan/matthew/fog/internal/objstore/fakes3"
)

// countedBody is a DigestedBody that counts the bytes read out of it.
type countedBody struct {
	*bytes.Reader
	d    Digests
	read int
}

func (b *countedBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	b.read += n
	return n, err
}

func (b *countedBody) Digests() Digests { return b.d }

func digestsOf(data []byte) Digests {
	s, m := sha256.Sum256(data), md5.Sum(data)
	return Digests{SHA256: hex.EncodeToString(s[:]), MD5: base64.StdEncoding.EncodeToString(m[:])}
}

func TestDigestedBodyIsReadOnce(t *testing.T) {
	fake := fakes3.New("fog-test")
	c := testClient(t, fake)
	ctx := context.Background()
	data := bytes.Repeat([]byte("sealed "), 4096)

	body := &countedBody{Reader: bytes.NewReader(data), d: digestsOf(data)}
	if _, err := c.Put(ctx, "k", body, int64(len(data)), PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if body.read != len(data) {
		t.Fatalf("body read %d bytes for a %d-byte put; the hashing pass was not skipped", body.read, len(data))
	}
	rc, _, err := c.Get(ctx, "k", "")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, data) {
		t.Fatal("round trip mismatch")
	}

	// Digests that lie are refused by the bucket, not stored.
	bad := &countedBody{Reader: bytes.NewReader(data), d: digestsOf([]byte("something else"))}
	if _, err := c.Put(ctx, "k2", bad, int64(len(data)), PutOptions{}); err == nil {
		t.Fatal("a body with the wrong digests was accepted")
	}
}
