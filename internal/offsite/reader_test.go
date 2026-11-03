package offsite

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitea.lan/matthew/fog/internal/objstore"
	"gitea.lan/matthew/fog/internal/objstore/fakes3"
)

// testBucket stands up a fake bucket with a fresh locker in it and
// returns a reader plus the raw client for planting objects.
func testBucket(t *testing.T) (*Reader, *objstore.Client, *fakes3.Server) {
	t.Helper()
	fake := fakes3.New("fog-test")
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	client, err := objstore.New(objstore.Config{Endpoint: srv.URL, Region: "x", Bucket: "fog-test",
		AccessKey: "k", SecretKey: "s", PathStyle: true, HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	lk, keys, err := CreateLocker("house", "passphrase 12345", nil)
	if err != nil {
		t.Fatal(err)
	}
	layout := Layout{Prefix: "backups", ID: lk.ID}
	raw, _ := lk.Marshal()
	if _, err := client.PutBytes(context.Background(), layout.LockerJSON(), raw, objstore.PutOptions{ContentType: "application/json"}); err != nil {
		t.Fatal(err)
	}
	return &Reader{Client: client, Layout: layout, Keys: keys}, client, fake
}

func plant(t *testing.T, r *Reader, key string, plain []byte) {
	t.Helper()
	sealed, err := r.Keys.SealBytes(r.Layout.Name(key), plain)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Client.PutBytes(context.Background(), key, sealed, objstore.PutOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverAndOpenFromNothing(t *testing.T) {
	r, client, _ := testBucket(t)
	ctx := context.Background()
	ids, err := Discover(ctx, client, "backups")
	if err != nil || len(ids) != 1 || ids[0] != r.Layout.ID {
		t.Fatalf("discover: %v %v", ids, err)
	}
	lk, err := FetchLocker(ctx, client, r.Layout)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := lk.OpenWithPassphrase("passphrase 12345")
	if err != nil {
		t.Fatal(err)
	}
	// A kit is stored as-is; the reader hands it back byte for byte.
	client.PutBytes(ctx, r.Layout.Kit("fog-recovery-kit-20260827-010000.enc"), []byte("FOGKIT1\x00sealed"), objstore.PutOptions{})
	client.PutBytes(ctx, r.Layout.Kit("fog-recovery-kit-20260826-010000.enc"), []byte("older"), objstore.PutOptions{})
	fr := &Reader{Client: client, Layout: r.Layout, Keys: fresh}
	kits, err := fr.Kits(ctx)
	if err != nil || len(kits) != 2 || kits[0].Stamp != "20260827-010000" {
		t.Fatalf("kits: %+v %v", kits, err)
	}
	var buf bytes.Buffer
	if _, err := fr.FetchKit(ctx, kits[0].Name, &buf); err != nil || buf.String() != "FOGKIT1\x00sealed" {
		t.Fatalf("fetch kit: %q %v", buf.String(), err)
	}
	if _, err := fr.Index(ctx); !errors.Is(err, ErrNoIndex) {
		t.Fatalf("no index yet: %v", err)
	}
}

// A planted junk object under index/ (one that sorts newest but does not
// open or parse) must not break reading the locker: Index skips it and falls
// back to the newest valid index.
func TestIndexSkipsJunkAndFallsBack(t *testing.T) {
	r, client, _ := testBucket(t)
	ctx := context.Background()
	good := Index{Format: Format, LockerID: r.Layout.ID, Fog: "house", WrittenAt: "2026-08-27T01:00:00Z"}
	raw, _ := json.Marshal(good)
	plant(t, r, r.Layout.Index("20260827-010000"), raw)
	// Junk that sorts newest: not sealed, not even a document.
	client.PutBytes(ctx, r.Layout.IndexPrefix()+"20260828-010000.json", []byte("not a sealed index"), objstore.PutOptions{})
	client.PutBytes(ctx, r.Layout.IndexPrefix()+"zzzz", []byte("junk"), objstore.PutOptions{})

	ix, err := r.Index(ctx)
	if err != nil || ix.WrittenAt != "2026-08-27T01:00:00Z" {
		t.Fatalf("Index must fall back to the newest valid one: %+v %v", ix, err)
	}
	// With only junk, it is ErrNoIndex, not a hard error.
	r2, client2, _ := testBucket(t)
	client2.PutBytes(ctx, r2.Layout.IndexPrefix()+"zzzz", []byte("junk"), objstore.PutOptions{})
	if _, err := r2.Index(ctx); !errors.Is(err, ErrNoIndex) {
		t.Fatalf("only junk indexes must read as ErrNoIndex, got %v", err)
	}
}

// A kit object far larger than MaxKitBytes must be refused, not buffered into
// an OOM.
func TestFetchKitCapsSize(t *testing.T) {
	r, client, _ := testBucket(t)
	ctx := context.Background()
	defer func(old int64) { MaxKitBytes = old }(MaxKitBytes)
	MaxKitBytes = 1024
	client.PutBytes(ctx, r.Layout.Kit("fog-recovery-kit-20260827-010000.enc"), bytes.Repeat([]byte("x"), 4096), objstore.PutOptions{})
	if _, err := r.FetchKit(ctx, "fog-recovery-kit-20260827-010000.enc", &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "cap") {
		t.Fatalf("an oversize kit must be refused: %v", err)
	}
}

// One unreadable blob must not strand a whole Drive restore: the other files
// land and the error names what failed.
func TestRestoreFilesContinuesPastABadBlob(t *testing.T) {
	r, _, _ := testBucket(t)
	ctx := context.Background()
	good := []byte("good file contents")
	gsha := sha256.Sum256(good)
	ghex := hex.EncodeToString(gsha[:])
	gblob, _ := r.Keys.BlobIDForDigest(ghex)
	plant(t, r, r.Layout.Blob(gblob), good) // a real, openable blob

	msha := sha256.Sum256([]byte("missing"))
	mhex := hex.EncodeToString(msha[:])
	mblob, _ := r.Keys.BlobIDForDigest(mhex) // never planted → unreadable

	snap := FilesSnapshot{Format: Format, Stamp: "20260101-010000", Files: []FileEntry{
		{Path: "a.txt", Size: int64(len(good)), SHA256: ghex, Blob: gblob},
		{Path: "b.txt", Size: 7, SHA256: mhex, Blob: mblob},
	}}
	raw, _ := json.Marshal(snap)
	plant(t, r, r.Layout.FilesSnapshot("20260101-010000"), raw)

	out := t.TempDir()
	n, _, err := r.RestoreFiles(ctx, "20260101-010000", out, nil)
	if err == nil || !strings.Contains(err.Error(), "could not be restored") {
		t.Fatalf("a missing blob must be reported, not swallowed: %v", err)
	}
	if n != 1 {
		t.Fatalf("the good file must still restore, got n=%d", n)
	}
	if got, _ := os.ReadFile(filepath.Join(out, "a.txt")); string(got) != string(good) {
		t.Fatalf("the readable file did not land: %q", got)
	}
}

func TestRowsIndexAndFetch(t *testing.T) {
	r, _, _ := testBucket(t)
	ctx := context.Background()
	row := AppRow{Format: Format, App: "hub", Stamp: "20260827-010314", Kind: "scheduled", Mode: "delegated", SizeBytes: 3}
	raw, _ := json.Marshal(row)
	plant(t, r, r.Layout.AppRow("hub", "20260827-010314"), raw)
	row.Stamp = "20260826-010314"
	raw, _ = json.Marshal(row)
	plant(t, r, r.Layout.AppRow("hub", "20260826-010314"), raw)
	plant(t, r, r.Layout.AppSnapshot("hub", "20260827-010314"), []byte("tar"))

	apps, _ := r.Apps(ctx)
	if strings.Join(apps, ",") != "hub" {
		t.Fatalf("apps: %v", apps)
	}
	rows, err := r.AppRows(ctx, "hub")
	if err != nil || len(rows) != 2 || rows[0].Stamp != "20260827-010314" {
		t.Fatalf("rows: %+v %v", rows, err)
	}
	sum, n, err := r.Hash(ctx, r.Layout.AppSnapshot("hub", "20260827-010314"))
	if err != nil || n != 3 {
		t.Fatalf("hash: %d %v", n, err)
	}
	want := sha256.Sum256([]byte("tar"))
	if sum != hex.EncodeToString(want[:]) {
		t.Fatal("digest of the plaintext, not the ciphertext")
	}
	dst := filepath.Join(t.TempDir(), "sub", "hub.tar")
	if fsum, n, err := r.FetchTo(ctx, r.Layout.AppSnapshot("hub", "20260827-010314"), dst); err != nil || n != 3 || fsum != sum {
		t.Fatalf("fetch: %d %v", n, err)
	}
	if got, _ := os.ReadFile(dst); string(got) != "tar" {
		t.Fatalf("file: %q", got)
	}
	// An index, once written, is what ls reads.
	ix := Index{Format: Format, LockerID: r.Layout.ID, Apps: map[string][]AppRow{"hub": rows}}
	raw, _ = json.Marshal(ix)
	plant(t, r, r.Layout.Index("20260827-012000"), raw)
	got, err := r.Index(ctx)
	if err != nil || len(got.Apps["hub"]) != 2 {
		t.Fatalf("index: %+v %v", got, err)
	}
	// A tampered object fails to open rather than yielding bytes.
	r.Client.PutBytes(ctx, r.Layout.AppSnapshot("hub", "bad"), []byte("FOGLKR1\x01\x00\x00\x00\x01garbage garbage garbage garbage garbage"), objstore.PutOptions{})
	if _, _, err := r.Hash(ctx, r.Layout.AppSnapshot("hub", "bad")); err == nil {
		t.Fatal("tampered object fetched")
	}
}

func TestRestoreFiles(t *testing.T) {
	r, _, _ := testBucket(t)
	ctx := context.Background()
	files := map[string][]byte{
		"Photos/2026/a.jpg": bytes.Repeat([]byte("jpeg"), 40000), // spans frames
		"notes.txt":         []byte("hello"),
		"Photos/2026/b.jpg": bytes.Repeat([]byte("jpeg"), 40000), // same content → same blob
	}
	var snap FilesSnapshot
	snap.Format, snap.Stamp = Format, "20260827-011500"
	for path, data := range files {
		id, _, _ := r.Keys.BlobID(bytes.NewReader(data))
		sum := sha256.Sum256(data)
		snap.Files = append(snap.Files, FileEntry{Path: path, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:]), Blob: id, Mtime: 1700000000000000000})
		plant(t, r, r.Layout.Blob(id), data)
	}
	raw, _ := json.Marshal(snap)
	plant(t, r, r.Layout.FilesSnapshot(snap.Stamp), raw)

	dir := t.TempDir()
	var seen []string
	n, total, err := r.RestoreFiles(ctx, snap.Stamp, dir, func(p string) { seen = append(seen, p) })
	if err != nil || n != 3 || total != 320005 || len(seen) != 3 {
		t.Fatalf("restore: %d %d %v %v", n, total, err, seen)
	}
	for path, data := range files {
		got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(path)))
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("%s: %v", path, err)
		}
	}
	// Re-running restores nothing that already matches.
	seen = nil
	if _, _, err := r.RestoreFiles(ctx, snap.Stamp, dir, func(p string) { seen = append(seen, p) }); err != nil || len(seen) != 0 {
		t.Fatalf("rerun: %v %v", err, seen)
	}
	// A manifest that escapes the directory is refused.
	snap.Files = append(snap.Files, FileEntry{Path: "../escape", Size: 1, Blob: "0000"})
	raw, _ = json.Marshal(snap)
	plant(t, r, r.Layout.FilesSnapshot("20260827-020000"), raw)
	if _, _, err := r.RestoreFiles(ctx, "20260827-020000", dir, nil); err == nil || !strings.Contains(err.Error(), "unsafe path") {
		t.Fatalf("escape: %v", err)
	}
}

// A Drive file named at the 255-byte limit restores from a locker: its
// staging name is fixed-length, not its name plus a suffix that would be
// too long to create.
func TestRestoreFilesWithANameAtTheLimit(t *testing.T) {
	r, _, _ := testBucket(t)
	ctx := context.Background()
	body := []byte("long name")
	sum := sha256.Sum256(body)
	hexsum := hex.EncodeToString(sum[:])
	blob, _ := r.Keys.BlobIDForDigest(hexsum)
	plant(t, r, r.Layout.Blob(blob), body)
	long := "d/" + strings.Repeat("n", 251) + ".txt"
	snap := FilesSnapshot{Format: Format, Stamp: "20260101-010000", Files: []FileEntry{{Path: long, Size: int64(len(body)), SHA256: hexsum, Blob: blob}}}
	raw, _ := json.Marshal(snap)
	plant(t, r, r.Layout.FilesSnapshot("20260101-010000"), raw)
	out := t.TempDir()
	if _, _, err := r.RestoreFiles(ctx, "20260101-010000", out, nil); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(out, filepath.FromSlash(long))); string(got) != "long name" {
		t.Fatalf("restored: %q", got)
	}
}
