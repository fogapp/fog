package offsite

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"testing"
)

func init() {
	// The KDF's cost is the point in production and a nuisance in tests.
	DefaultKDF = KDFParams{Time: 1, Memory: 8 * 1024, Threads: 1}
}

type fakeVault struct{ key byte }

func (v fakeVault) Encrypt(p string) (string, error) {
	b := []byte(p)
	for i := range b {
		b[i] ^= v.key
	}
	return "vault:" + string(b), nil
}

func (v fakeVault) Decrypt(c string) (string, error) {
	if len(c) < 6 || c[:6] != "vault:" {
		return "", errors.New("not mine")
	}
	b := []byte(c[6:])
	for i := range b {
		b[i] ^= v.key
	}
	return string(b), nil
}

func testKeyring(t *testing.T) *Keyring {
	t.Helper()
	_, kr, err := CreateLocker("fog-test", "correct horse battery staple", fakeVault{7})
	if err != nil {
		t.Fatal(err)
	}
	return kr
}

func roundTrip(t *testing.T, kr *Keyring, name string, plain []byte) []byte {
	t.Helper()
	var sealed bytes.Buffer
	w, err := kr.Seal(&sealed, name, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Write in awkward chunk sizes so frame boundaries fall mid-write.
	for i := 0; i < len(plain); i += 1000 {
		end := i + 1000
		if end > len(plain) {
			end = len(plain)
		}
		if _, err := w.Write(plain[i:end]); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if got, want := int64(sealed.Len()), SealedSize(int64(len(plain))); got != want {
		t.Fatalf("sealed size %d, SealedSize says %d", got, want)
	}
	if n, ok := PlainSize(int64(sealed.Len())); !ok || n != int64(len(plain)) {
		t.Fatalf("PlainSize(%d) = %d,%v want %d", sealed.Len(), n, ok, len(plain))
	}
	r, err := kr.Open(bytes.NewReader(sealed.Bytes()), name)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("round trip mismatch: %d bytes vs %d", len(got), len(plain))
	}
	return sealed.Bytes()
}

func TestSealRoundTripSizes(t *testing.T) {
	t.Parallel()

	kr := testKeyring(t)
	for _, n := range []int{0, 1, 100, FrameSize - 1, FrameSize, FrameSize + 1, 2 * FrameSize, 3*FrameSize + 12345} {
		plain := make([]byte, n)
		rand.Read(plain)
		roundTrip(t, kr, "apps/hub/x.snapshot", plain)
	}
}

func TestSealDetectsTruncationAndTamper(t *testing.T) {
	t.Parallel()

	kr := testKeyring(t)
	plain := make([]byte, 2*FrameSize+100)
	rand.Read(plain)
	sealed := roundTrip(t, kr, "n", plain)

	open := func(b []byte) error {
		r, err := kr.Open(bytes.NewReader(b), "n")
		if err != nil {
			return err
		}
		_, err = io.ReadAll(r)
		return err
	}
	// Cut inside the last frame: the final tag fails.
	if err := open(sealed[:len(sealed)-10]); err == nil {
		t.Fatal("truncated tail must not open")
	}
	// Cut exactly at a frame boundary: no last frame was seen.
	cut := headerSize + FrameSize + tagSize
	if err := open(sealed[:cut]); !errors.Is(err, ErrTruncated) && !errors.Is(err, ErrBadKey) {
		t.Fatalf("frame-boundary cut: %v", err)
	}
	// A frame moved out of order (swap frames 1 and 2) fails on the counter.
	swapped := append([]byte(nil), sealed...)
	f1 := swapped[headerSize : headerSize+FrameSize+tagSize]
	f2 := swapped[headerSize+FrameSize+tagSize : headerSize+2*(FrameSize+tagSize)]
	tmp := append([]byte(nil), f1...)
	copy(f1, f2)
	copy(f2, tmp)
	if err := open(swapped); !errors.Is(err, ErrBadKey) {
		t.Fatalf("reordered frames: %v", err)
	}
	// A flipped bit fails.
	flipped := append([]byte(nil), sealed...)
	flipped[headerSize+5] ^= 1
	if err := open(flipped); !errors.Is(err, ErrBadKey) {
		t.Fatalf("flipped bit: %v", err)
	}
	// Not a sealed object at all.
	if _, err := kr.Open(bytes.NewReader([]byte("hello")), "n"); !errors.Is(err, ErrNotSealed) {
		t.Fatalf("plain bytes: %v", err)
	}
}

// The name is in the additional data: an object served under a different
// key does not open, so a swapped artifact cannot be passed off.
func TestSealBindsName(t *testing.T) {
	t.Parallel()

	kr := testKeyring(t)
	sealed, _ := kr.SealBytes("apps/hub/a.snapshot", []byte("hub data"))
	if _, err := kr.OpenBytes("apps/feed/a.snapshot", sealed, 1<<10); !errors.Is(err, ErrBadKey) {
		t.Fatalf("wrong name must not open: %v", err)
	}
	got, err := kr.OpenBytes("apps/hub/a.snapshot", sealed, 1<<10)
	if err != nil || string(got) != "hub data" {
		t.Fatalf("right name: %q %v", got, err)
	}
	if _, err := kr.OpenBytes("apps/hub/a.snapshot", sealed, 3); err == nil {
		t.Fatal("limit must be enforced")
	}
}

// Re-sealing the same source with the same nonce yields identical bytes —
// what lets a resumed multipart upload keep its finished parts.
func TestSealIsDeterministicForANonce(t *testing.T) {
	t.Parallel()

	kr := testKeyring(t)
	nonce, _ := NewNonce()
	plain := make([]byte, FrameSize+7)
	rand.Read(plain)
	seal := func() []byte {
		var b bytes.Buffer
		w, _ := kr.Seal(&b, "x", nonce)
		w.Write(plain)
		w.Close()
		return b.Bytes()
	}
	if !bytes.Equal(seal(), seal()) {
		t.Fatal("same nonce, different ciphertext")
	}
	var b bytes.Buffer
	w, _ := kr.Seal(&b, "x", nil)
	w.Write(plain)
	w.Close()
	if bytes.Equal(b.Bytes(), seal()) {
		t.Fatal("fresh nonce must differ")
	}
}

func TestPlainSizeRejectsImpossibleLengths(t *testing.T) {
	t.Parallel()

	if _, ok := PlainSize(int64(headerSize) + 3); ok {
		t.Fatal("a body shorter than a tag is not a sealed object")
	}
	if _, ok := PlainSize(int64(headerSize) + int64(FrameSize+tagSize) + 5); ok {
		t.Fatal("a trailing fragment shorter than a tag is impossible")
	}
}
