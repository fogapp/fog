package offsite

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"gitea.lan/matthew/fog/internal/objstore/fakes3"
	"gitea.lan/matthew/fog/internal/store"
)

// plantFilesSnapshot writes a hardlink-style snapshot directory the way
// files.RunSnapshot does — the tree plus .fog-snapshot.json — and records
// it. Returns the row id.
func plantFilesSnapshot(t *testing.T, h *harness, root, stamp string, files map[string][]byte) int64 {
	t.Helper()
	dir := filepath.Join(root, "files", stamp)
	type entry struct {
		Path   string `json:"path"`
		Size   int64  `json:"size"`
		Mtime  int64  `json:"mtime"`
		SHA256 string `json:"sha256"`
	}
	mf := struct {
		Format    string   `json:"format"`
		CreatedAt string   `json:"created_at"`
		Mode      string   `json:"mode"`
		Root      string   `json:"root"`
		Files     []entry  `json:"files"`
		Excluded  []string `json:"excluded,omitempty"`
		Bytes     int64    `json:"bytes"`
	}{Format: "fog-files-snapshot/1", CreatedAt: "2026-08-27T01:15:00Z", Mode: "links", Root: "/srv/files"}
	var total int64
	for rel, data := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		mf.Files = append(mf.Files, entry{Path: rel, Size: int64(len(data)), Mtime: 1700000000000000000, SHA256: hex.EncodeToString(sum[:])})
		total += int64(len(data))
	}
	mf.Bytes = total
	raw, _ := json.Marshal(mf)
	if err := os.WriteFile(filepath.Join(dir, ".fog-snapshot.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := h.st.CreateFileSnapshot(dir, "links", int64(len(files)), total, total, 0)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestSyncFilesDedupsBlobsAndRestores(t *testing.T) {
	t.Parallel()

	fake := fakes3.New("fog-test")
	h := newHarness(t, fake)
	root, _ := plantBackups(t, h)
	photo := bytes.Repeat([]byte("jpeg"), 50000)
	id1 := plantFilesSnapshot(t, h, root, "20260826-011500", map[string][]byte{
		"Photos/a.jpg": photo, "notes.txt": []byte("v1"), "Photos/dup.jpg": photo,
	})
	target, _, err := h.m.AddTarget(context.Background(), h.spec("cloud"), "key-1", "secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := runSync(t, h, "cloud"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// Two distinct contents → two blobs, though three files.
	blobs, _ := h.st.ListOffsiteCopies(target.ID, store.OffsiteKindBlob)
	if len(blobs) != 2 {
		t.Fatalf("blobs: %d", len(blobs))
	}
	if c, err := h.st.OffsiteCopy(target.ID, store.OffsiteKindFilesSnapshot, strconv.FormatInt(id1, 10)); err != nil || c.State != "stored" || c.Doc == "" {
		t.Fatalf("snapshot copy: %+v %v", c, err)
	}
	r, _ := h.m.Reader(ctx, target)
	stamps, _ := r.FilesSnapshots(ctx)
	if len(stamps) != 1 || stamps[0] != "20260826-011500" {
		t.Fatalf("stamps: %v", stamps)
	}
	out := t.TempDir()
	n, total, err := r.RestoreFiles(ctx, "20260826-011500", out, nil)
	if err != nil || n != 3 || total != int64(2*len(photo)+2) {
		t.Fatalf("restore: %d %d %v", n, total, err)
	}
	if got, _ := os.ReadFile(filepath.Join(out, "Photos", "dup.jpg")); !bytes.Equal(got, photo) {
		t.Fatal("restored photo differs")
	}
	if got, _ := os.ReadFile(filepath.Join(out, "notes.txt")); string(got) != "v1" {
		t.Fatal("restored notes differ")
	}
	ix, _ := r.Index(ctx)
	if len(ix.Files) != 1 || ix.Files[0].Files != 3 {
		t.Fatalf("index files: %+v", ix.Files)
	}

	// Night two: the photo is unchanged (no re-upload), notes changed (one
	// new blob), and the status column says copied.
	uploads := fake.Calls("put")
	id2 := plantFilesSnapshot(t, h, root, "20260827-011500", map[string][]byte{
		"Photos/a.jpg": photo, "notes.txt": []byte("v2"),
	})
	if err := runSync(t, h, "cloud"); err != nil {
		t.Fatal(err)
	}
	blobs, _ = h.st.ListOffsiteCopies(target.ID, store.OffsiteKindBlob)
	if len(blobs) != 3 {
		t.Fatalf("blobs after night two: %d", len(blobs))
	}
	// One blob + one manifest + one index = three puts.
	if fake.Calls("put")-uploads != 3 {
		t.Fatalf("night two puts: %d", fake.Calls("put")-uploads)
	}
	if st := offsiteStateOf(h.st, store.OffsiteKindFilesSnapshot, strconv.FormatInt(id2, 10)); st != "stored" {
		t.Fatalf("status: %q", st)
	}

	// Retention to one recent snapshot: night one's manifest is hidden and
	// the blob only it named ("v1") is swept; the shared photo stays.
	h.st.SetSetting(SettingKeepRecent, "1")
	h.st.SetSetting(SettingKeepMonthly, "0")
	h.st.SetSetting(SettingKeepYearly, "0")
	if err := runSync(t, h, "cloud"); err != nil {
		t.Fatal(err)
	}
	stamps, _ = r.FilesSnapshots(ctx)
	if len(stamps) != 1 || stamps[0] != "20260827-011500" {
		t.Fatalf("after retention: %v", stamps)
	}
	blobs, _ = h.st.ListOffsiteCopies(target.ID, store.OffsiteKindBlob)
	hidden, stored := 0, 0
	for _, c := range blobs {
		switch c.State {
		case "hidden":
			hidden++
		case "stored":
			stored++
		}
	}
	if hidden != 1 || stored != 2 {
		t.Fatalf("sweep: hidden %d stored %d", hidden, stored)
	}
	if _, _, err := r.RestoreFiles(ctx, "20260827-011500", t.TempDir(), nil); err != nil {
		t.Fatalf("the retained snapshot must still restore: %v", err)
	}
	// A snapshot that needs a swept blob again simply re-uploads it.
	plantFilesSnapshot(t, h, root, "20260828-011500", map[string][]byte{"notes.txt": []byte("v1")})
	if err := runSync(t, h, "cloud"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.RestoreFiles(ctx, "20260828-011500", t.TempDir(), nil); err != nil {
		t.Fatalf("re-uploaded blob: %v", err)
	}
}

// A Drive seed that dies part way (here: the bucket stops taking puts
// after seven) keeps the blobs that landed, uploaded in parallel and
// recorded in batches; the next run sends only the rest, and every blob
// ends up written exactly once.
func TestSyncFilesResumesAParallelSeed(t *testing.T) {
	t.Parallel()

	fake := fakes3.New("fog-test")
	h := newHarness(t, fake)
	root, _ := plantBackups(t, h)
	target, _, err := h.m.AddTarget(context.Background(), h.spec("cloud"), "key-1", "secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := runSync(t, h, "cloud"); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	for i := range 20 {
		files["f"+strconv.Itoa(i)+".txt"] = bytes.Repeat([]byte{byte('a' + i)}, 1000+i)
	}
	plantFilesSnapshot(t, h, root, "20260826-011500", files)

	fake.FailOp("put", 7)
	if err := runSync(t, h, "cloud"); err == nil {
		t.Fatal("the seed should fail once the bucket refuses puts")
	}
	fake.FailOp("", 0)
	first, _ := h.st.ListOffsiteCopies(target.ID, store.OffsiteKindBlob, "stored")
	if len(first) == 0 || len(first) >= len(files) {
		t.Fatalf("after the failed seed: %d of %d blobs recorded", len(first), len(files))
	}

	if err := runSync(t, h, "cloud"); err != nil {
		t.Fatal(err)
	}
	blobs, _ := h.st.ListOffsiteCopies(target.ID, store.OffsiteKindBlob, "stored")
	if len(blobs) != len(files) {
		t.Fatalf("blobs stored: %d of %d", len(blobs), len(files))
	}
	for _, c := range blobs {
		if v := fake.Versions(c.Key); len(v) != 1 {
			t.Fatalf("blob %s written %d times", c.Key, len(v))
		}
	}
	r, _ := h.m.Reader(context.Background(), target)
	out := t.TempDir()
	if n, _, err := r.RestoreFiles(context.Background(), "20260826-011500", out, nil); err != nil || n != int64(len(files)) {
		t.Fatalf("restore: %d %v", n, err)
	}
	if got, _ := os.ReadFile(filepath.Join(out, "f3.txt")); !bytes.Equal(got, files["f3.txt"]) {
		t.Fatal("restored file differs")
	}
}
