package files

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/backup"
	"gitea.lan/matthew/fog/internal/jobs"
	"gitea.lan/matthew/fog/internal/store"
)

func newManager(t *testing.T) *Manager {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m := New(dir, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(m.Close)
	return m
}

func write(t *testing.T, m *Manager, rel, content string) {
	t.Helper()
	p := filepath.Join(m.Root(), rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCleanRel(t *testing.T) {
	cases := []struct {
		in, want string
		err      error
	}{
		{"", "", nil}, {"/", "", nil}, {".", "", nil}, {"a", "a", nil}, {"/a/b/", "a/b", nil},
		{"a/./b", "a/b", nil}, {"a/../b", "b", nil},
		{"..", "", ErrEscapes}, {"../x", "", ErrEscapes}, {"a/../..", "", ErrEscapes},
		{".fog", "", ErrReserved}, {".fog/uploads", "", ErrReserved}, {"a/.fog", "a/.fog", nil},
		{"Apps/.fog", "", ErrReserved}, {"Content/.fog/trash", "", ErrReserved}, {"Apps/x/.fog", "Apps/x/.fog", nil},
		{"a\x00b", "", ErrBadName}, {"\xff", "", ErrBadName},
		{string(bytes.Repeat([]byte("n"), 256)), "", ErrNameTooLong},
	}
	for _, c := range cases {
		got, err := CleanRel(c.in)
		if !errors.Is(err, c.err) || got != c.want {
			t.Errorf("CleanRel(%q) = %q, %v; want %q, %v", c.in, got, err, c.want, c.err)
		}
	}
}

// A listing reflects what is on disk, including files written behind the
// index's back, and the reserved directory never appears.
// apps.ValidateFilesRel is the spec-side twin of CleanRel: a Files folder
// a spec accepts is one Files would address unchanged, and vice versa.
func TestSpecFilesRelAgreesWithCleanRel(t *testing.T) {
	for _, c := range []string{
		"Apps/Books", "Apps/Media/Movies", "Photos", "a b/c", ".fog", ".fog/trash", "Apps/.fog", "Content/.fog/uploads", "Apps/x/.fog",
		"/Apps/Books", "Apps//Books", "Apps/../etc", "..", ".", "Apps/.", "Apps/Books/", "",
	} {
		clean, cerr := CleanRel(c)
		specOK := apps.ValidateFilesRel(c) == nil
		filesOK := cerr == nil && clean == c && clean != ""
		if specOK != filesOK {
			t.Errorf("%q: spec accepts=%v, files accepts unchanged=%v (clean=%q err=%v)", c, specOK, filesOK, clean, cerr)
		}
	}
	if AppsDir != apps.FilesAppsDir {
		t.Errorf("AppsDir %q != apps.FilesAppsDir %q", AppsDir, apps.FilesAppsDir)
	}
}

func TestListRefreshesFromDisk(t *testing.T) {
	m := newManager(t)
	write(t, m, "docs/readme.md", "hello")
	write(t, m, "photo.jpg", "not really")
	l, err := m.List("", ListOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Entries) != 2 || l.Entries[0].Name != "docs" || !l.Entries[0].IsDir() || l.Entries[1].Name != "photo.jpg" {
		t.Fatalf("entries = %+v", l.Entries)
	}
	if l.Entries[1].Mime != "image/jpeg" || l.Entries[1].Size != 10 {
		t.Errorf("photo row = %+v", l.Entries[1])
	}
	if _, err := os.Stat(filepath.Join(m.Root(), ReservedDir, "uploads")); err != nil {
		t.Errorf("reserved dir not created: %v", err)
	}

	// A file rewritten in place: same directory mtime, different bytes.
	write(t, m, "photo.jpg", "longer content now")
	l, _ = m.List("", ListOpts{})
	if l.Entries[1].Size != 18 {
		t.Errorf("rewritten file size = %d, want 18", l.Entries[1].Size)
	}
	// A file removed by another tool vanishes from the index.
	os.Remove(filepath.Join(m.Root(), "photo.jpg"))
	l, _ = m.List("", ListOpts{})
	if len(l.Entries) != 1 {
		t.Errorf("after external delete: %+v", l.Entries)
	}
}

func TestListPagination(t *testing.T) {
	m := newManager(t)
	for _, n := range []string{"b", "a", "c", "e", "d"} {
		write(t, m, n+".txt", n)
	}
	write(t, m, "zdir/x", "x")
	var names []string
	cursor := ""
	for {
		l, err := m.List("", ListOpts{Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range l.Entries {
			names = append(names, e.Name)
		}
		if l.Total != 6 {
			t.Errorf("total = %d", l.Total)
		}
		if l.Next == "" {
			break
		}
		cursor = l.Next
	}
	want := []string{"zdir", "a.txt", "b.txt", "c.txt", "d.txt", "e.txt"}
	if len(names) != len(want) {
		t.Fatalf("paged names = %v", names)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("paged names = %v, want %v", names, want)
		}
	}
	// Descending by size still leads with the directory.
	l, _ := m.List("", ListOpts{Sort: SortSize, Desc: true, Limit: 3})
	if l.Entries[0].Name != "zdir" {
		t.Errorf("size desc first = %s", l.Entries[0].Name)
	}
}

func TestWalkSizesAndSearch(t *testing.T) {
	m := newManager(t)
	write(t, m, "a/b/deep.bin", "0123456789")
	write(t, m, "a/top.txt", "12345")
	write(t, m, "Holiday Photos.zip", "zz")
	if err := m.Walk(context.Background()); err != nil {
		t.Fatal(err)
	}
	d, err := m.Stat("a")
	if err != nil {
		t.Fatal(err)
	}
	if d.Size != 15 || d.WalkedAt == "" {
		t.Errorf("a = size %d walked %q, want 15 and a time", d.Size, d.WalkedAt)
	}
	root, _ := m.Stat("")
	if root.Size != 17 {
		t.Errorf("root size = %d, want 17", root.Size)
	}
	hits, err := m.Search("photo", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Path != "Holiday Photos.zip" {
		t.Errorf("search photo = %+v", hits)
	}
	hits, _ = m.Search("de", "a", "file", 10)
	if len(hits) != 1 || hits[0].Path != "a/b/deep.bin" {
		t.Errorf("short prefix search = %+v", hits)
	}
	// Hashing picks the files up and duplicates become visible.
	write(t, m, "copy.bin", "0123456789")
	m.List("", ListOpts{})
	if _, err := m.hashBatch(context.Background(), 100); err != nil {
		t.Fatal(err)
	}
	d, _ = m.Stat("copy.bin")
	if d.SHA256 == "" || len(d.Duplicates) != 1 || d.Duplicates[0] != "a/b/deep.bin" {
		t.Errorf("copy.bin = sha %q dups %v", d.SHA256, d.Duplicates)
	}
}

// Every escape route a client could try is refused before the syscall
// layer, and a symlink pointing out of the root cannot be read through.
func TestNoEscape(t *testing.T) {
	m := newManager(t)
	outside := filepath.Join(t.TempDir(), "secret.txt")
	os.WriteFile(outside, []byte("secret"), 0o644)
	m.List("", ListOpts{}) // creates the root
	if err := os.Symlink(outside, filepath.Join(m.Root(), "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(m.Root(), "dirlink")); err != nil {
		t.Fatal(err)
	}
	l, _ := m.List("", ListOpts{})
	for _, e := range l.Entries {
		if e.Kind != "other" {
			t.Errorf("symlink %s listed as %s", e.Name, e.Kind)
		}
	}
	if _, _, err := m.OpenFile("link"); err == nil {
		t.Error("opened a symlink to outside the root")
	}
	if _, err := m.List("dirlink", ListOpts{}); err == nil {
		t.Error("listed through a symlink to outside the root")
	}
	if _, _, err := m.OpenFile("../fog.db"); !errors.Is(err, ErrEscapes) && !errors.Is(err, ErrNotFound) {
		t.Errorf("../fog.db: %v", err)
	}
}

func TestZipStreamsFolder(t *testing.T) {
	m := newManager(t)
	write(t, m, "Photos/2024/a.jpg", "jpeg")
	write(t, m, "Photos/note.txt", "note")
	var buf bytes.Buffer
	if err := m.WriteZip(context.Background(), &buf, []string{"Photos"}); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]uint16{}
	for _, f := range zr.File {
		got[f.Name] = f.Method
	}
	if got["Photos/2024/a.jpg"] != zip.Store || got["Photos/note.txt"] != zip.Deflate || got["Photos/"] != zip.Store {
		t.Errorf("zip entries = %v", got)
	}
}

func TestValidateRoot(t *testing.T) {
	m := newManager(t)
	bad := []string{"relative", "/", m.DataDir, filepath.Join(m.DataDir, "data", "x"), filepath.Dir(m.DataDir)}
	for _, b := range bad {
		if err := m.Validate(b); err == nil {
			t.Errorf("Validate(%q) accepted", b)
		}
	}
	good := filepath.Join(t.TempDir(), "files")
	if err := m.Validate(good); err != nil {
		t.Errorf("Validate(%q): %v", good, err)
	}
	if _, err := os.Stat(good); err != nil {
		t.Error("validate should create the directory")
	}
}

func TestRootMove(t *testing.T) {
	m := newManager(t)
	write(t, m, "keep/me.txt", "data")
	m.List("", ListOpts{})
	if err := m.PrepareRootChange(filepath.Join(t.TempDir(), "new")); !errors.Is(err, ErrRootHasContent) {
		t.Fatalf("PrepareRootChange with content: %v", err)
	}
	to := filepath.Join(t.TempDir(), "moved")
	job, err := m.MoveRoot(to, "move")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		j, _ := m.Jobs.Get(job.ID)
		if j.State != "running" {
			if j.State != "done" {
				t.Fatalf("job = %+v", j)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("job did not finish")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if m.Root() != to {
		t.Errorf("root = %s, want %s", m.Root(), to)
	}
	if b, err := os.ReadFile(filepath.Join(to, "keep", "me.txt")); err != nil || string(b) != "data" {
		t.Errorf("moved file: %v %q", err, b)
	}
	d, err := m.Stat("keep/me.txt")
	if err != nil || d.Size != 4 {
		t.Errorf("stat after move: %+v %v", d, err)
	}
}

func waitJob(t *testing.T, m *Manager, id string) jobs.Job {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		j, _ := m.Jobs.Get(id)
		if j.State != "running" {
			return j
		}
		if time.Now().After(deadline) {
			t.Fatal("job did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func names(l Listing) []string {
	var out []string
	for _, e := range l.Entries {
		out = append(out, e.Name)
	}
	return out
}

func TestMutations(t *testing.T) {
	m := newManager(t)
	write(t, m, "a/one.txt", "1")
	write(t, m, "a/sub/two.txt", "22")
	write(t, m, "b/keep.txt", "333")
	if err := m.Walk(context.Background()); err != nil {
		t.Fatal(err)
	}

	// mkdir, rename, and the index follows without a walk.
	if _, err := m.Mkdir("a/new folder"); err != nil {
		t.Fatal(err)
	}
	n, err := m.Rename("a/new folder", "renamed")
	if err != nil || n.Path != "a/renamed" {
		t.Fatalf("rename: %+v %v", n, err)
	}
	if _, err := m.Rename("a/one.txt", "keep.txt"); err != nil {
		t.Fatal(err)
	}
	// A conflicting rename says what is in the way.
	var c *ConflictError
	if _, err := m.Rename("a/sub", "keep.txt"); !errors.As(err, &c) || c.Existing.Name != "keep.txt" {
		t.Errorf("conflicting rename: %v", err)
	}

	// Move a directory: its subtree rows move with it, sizes follow.
	res, err := m.Move([]string{"a/sub"}, "b", ConflictAsk)
	if err != nil || len(res.Moved) != 1 || res.Moved[0] != "b/sub" {
		t.Fatalf("move: %+v %v", res, err)
	}
	if d, err := m.Stat("b/sub/two.txt"); err != nil || d.Size != 2 {
		t.Errorf("moved child: %+v %v", d, err)
	}
	if d, _ := m.Stat("b"); d.Size != 5 {
		t.Errorf("b size after move = %d, want 5", d.Size)
	}
	if d, _ := m.Stat("a"); d.Size != 1 {
		t.Errorf("a size after move = %d, want 1", d.Size)
	}
	// Moving onto a taken name asks, renames, or skips as told.
	if _, err := m.Move([]string{"a/keep.txt"}, "b", ConflictAsk); !errors.As(err, &c) {
		t.Errorf("move conflict: %v", err)
	}
	res, _ = m.Move([]string{"a/keep.txt"}, "b", ConflictRename)
	if len(res.Moved) != 1 || res.Moved[0] != "b/keep 2.txt" {
		t.Errorf("move rename = %+v", res)
	}
	if _, err := m.Move([]string{"b"}, "b/sub", ConflictAsk); err == nil {
		t.Error("moved a folder into itself")
	}

	// Copy is a job and lands with the subtree indexed.
	job, err := m.Copy([]string{"b/sub"}, "a", ConflictAsk)
	if err != nil {
		t.Fatal(err)
	}
	if j := waitJob(t, m, job.ID); j.State != "done" {
		t.Fatalf("copy job: %+v", j)
	}
	if d, err := m.Stat("a/sub/two.txt"); err != nil || d.Size != 2 {
		t.Errorf("copied child: %+v %v", d, err)
	}
	if d, _ := m.Stat("a"); d.Size != 2 {
		t.Errorf("a size after copy = %d, want 2", d.Size)
	}
	l, _ := m.List("b", ListOpts{})
	got := names(l)
	if len(got) != 3 || got[0] != "sub" || got[1] != "keep 2.txt" || got[2] != "keep.txt" {
		t.Errorf("b = %v", got)
	}
}

func TestTrashRoundTrip(t *testing.T) {
	m := newManager(t)
	write(t, m, "docs/a.txt", "aaa")
	write(t, m, "docs/b.txt", "bb")
	write(t, m, "loose.txt", "x")
	m.Walk(context.Background())
	items, err := m.Trash([]string{"docs", "loose.txt"})
	if err != nil || len(items) != 2 {
		t.Fatalf("trash: %+v %v", items, err)
	}
	if items[0].Size != 5 || items[0].Kind != "dir" {
		t.Errorf("trashed dir receipt = %+v", items[0])
	}
	l, _ := m.List("", ListOpts{})
	if len(l.Entries) != 0 || l.Dir.Size != 0 {
		t.Errorf("after trash: %v size %d", names(l), l.Dir.Size)
	}
	listed, _ := m.ListTrash()
	if len(listed) != 2 {
		t.Errorf("trash listing = %+v", listed)
	}
	// The receipt is on disk, readable by anyone.
	if _, err := os.Stat(filepath.Join(m.Root(), ReservedDir, "trash", items[0].ID+".json")); err != nil {
		t.Error("no receipt on disk")
	}
	// Something takes the old name; restore keeps both.
	write(t, m, "loose.txt", "new")
	restored, err := m.RestoreTrash([]string{items[1].ID, items[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	if restored[0] != "loose 2.txt" || restored[1] != "docs" {
		t.Errorf("restored = %v", restored)
	}
	if d, err := m.Stat("docs/a.txt"); err != nil || d.Size != 3 {
		t.Errorf("restored child: %+v %v", d, err)
	}
	if d, _ := m.Stat(""); d.Size != 9 {
		t.Errorf("root size after restore = %d, want 9", d.Size)
	}
	if listed, _ := m.ListTrash(); len(listed) != 0 {
		t.Errorf("trash not empty: %+v", listed)
	}
	// Purge for good, and the sweep respects retention.
	its, _ := m.Trash([]string{"loose 2.txt"})
	if err := m.PurgeTrash(its[0].ID); err != nil {
		t.Fatal(err)
	}
	if listed, _ := m.ListTrash(); len(listed) != 0 {
		t.Errorf("purge left %+v", listed)
	}
	if _, err := m.RestoreTrash([]string{its[0].ID}); !errors.Is(err, ErrNotFound) {
		t.Errorf("restore of purged: %v", err)
	}
}

// Deleting an app with its data trashes its own folder in Drive, and only
// that; an app that never had one deletes without complaint.
func TestTrashAppFolder(t *testing.T) {
	m := newManager(t)
	write(t, m, "Apps/Paperless/config/a.txt", "aaa")
	write(t, m, "Apps/Manor/b.txt", "bb")
	m.Walk(context.Background())
	if err := m.TrashAppFolder("paperless"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Stat("Apps/Paperless"); err == nil {
		t.Error("the app's folder is still in place")
	}
	if _, err := m.Stat("Apps/Manor/b.txt"); err != nil {
		t.Errorf("another app's folder went too: %v", err)
	}
	listed, _ := m.ListTrash()
	if len(listed) != 1 || listed[0].Path != "Apps/Paperless" {
		t.Errorf("trash = %+v, want just Apps/Paperless", listed)
	}
	if err := m.TrashAppFolder("ghost"); err != nil {
		t.Errorf("an app with no folder: %v", err)
	}
}

func TestMountedGuard(t *testing.T) {
	m := newManager(t)
	write(t, m, "media/movies/x.mkv", "m")
	m.MountedPaths = func() []MountedPath {
		return []MountedPath{{Path: filepath.Join(m.Root(), "media", "movies"), App: "jellyfin"}}
	}
	if _, err := m.Trash([]string{"media"}); !errors.Is(err, ErrMounted) {
		t.Errorf("trash of a mounted parent: %v", err)
	}
	if _, err := m.Move([]string{"media/movies"}, "", ConflictAsk); !errors.Is(err, ErrMounted) {
		t.Errorf("move of a mounted folder: %v", err)
	}
	// Inside the mounted folder is fine.
	if _, err := m.Trash([]string{"media/movies/x.mkv"}); err != nil {
		t.Errorf("trash inside mounted: %v", err)
	}
	l, _ := m.List("", ListOpts{})
	if len(l.Entries) != 1 || len(l.Entries[0].MountedBy) != 1 || l.Entries[0].MountedBy[0] != "jellyfin" {
		t.Errorf("mounted badge = %+v", l.Entries)
	}
}

// The Apps folder and what is in it are stamped with their roles, and a
// folder an app mounts says which app and at what container path — the
// facts the Drive page turns into "bookorbit sees this folder as /books".
func TestAppsFolderRolesAndMounts(t *testing.T) {
	m := newManager(t)
	if err := m.EnsureDir(AppsDir + "/Books"); err != nil {
		t.Fatal(err)
	}
	write(t, m, AppsDir+"/Books/Fiction/a.epub", "x")
	m.MountedPaths = func() []MountedPath {
		return []MountedPath{{Path: filepath.Join(m.Root(), AppsDir, "Books"), App: "bookorbit", Mount: "/books"}}
	}
	root, err := m.List("", ListOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(root.Entries) != 1 || root.Entries[0].Role != RoleApps {
		t.Fatalf("Apps folder role = %+v", root.Entries)
	}
	// Apps itself is mounted "by" bookorbit (a folder inside it is), but no
	// app sees Apps itself at a container path.
	if got := root.Entries[0]; len(got.MountedBy) != 1 || len(got.Mounts) != 0 {
		t.Errorf("Apps: mountedBy=%v mounts=%v", got.MountedBy, got.Mounts)
	}
	apps, err := m.List(AppsDir, ListOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if apps.Dir.Role != RoleApps {
		t.Errorf("listing Apps: dir role = %q", apps.Dir.Role)
	}
	books := apps.Entries[0]
	if books.Role != RoleAppFolder || len(books.Mounts) != 1 || books.Mounts[0].App != "bookorbit" || books.Mounts[0].Mount != "/books" {
		t.Errorf("Books = %+v", books)
	}
	d, err := m.Stat(AppsDir + "/Books")
	if err != nil {
		t.Fatal(err)
	}
	if d.Role != RoleAppFolder || len(d.Mounts) != 1 {
		t.Errorf("stat Books = %+v", d.Node)
	}
	// Inside the library is the operator's to arrange: no role, no mount,
	// and moving a subfolder is allowed.
	inner, _ := m.List(AppsDir+"/Books", ListOpts{})
	if len(inner.Entries) != 1 || inner.Entries[0].Role != "" || len(inner.Entries[0].MountedBy) != 0 {
		t.Errorf("inside the library = %+v", inner.Entries)
	}
	if _, err := m.Move([]string{AppsDir + "/Books/Fiction"}, AppsDir+"/Books", ConflictAsk); err != nil && !errors.Is(err, ErrExists) {
		t.Errorf("move inside a mounted library: %v", err)
	}
	if _, err := m.Trash([]string{AppsDir + "/Books"}); !errors.Is(err, ErrMounted) {
		t.Errorf("trash of a library an app mounts: %v", err)
	}
}

func TestUploadSessions(t *testing.T) {
	m := newManager(t)
	m.List("", ListOpts{})
	data := []byte(strings.Repeat("0123456789", 50))
	sum := sha256.Sum256(data)
	u, err := m.BeginUpload("in/box/big.txt", int64(len(data)), hex.EncodeToString(sum[:]), "2024-01-02T03:04:05Z", ConflictAsk)
	if err != nil {
		t.Fatal(err)
	}
	if u.Offset != 0 || u.ID == "" {
		t.Fatalf("begin = %+v", u)
	}
	// Chunks land at their offsets; a wrong offset says the real one.
	if u, err = m.AppendUpload(u.ID, 0, bytes.NewReader(data[:200])); err != nil || u.Offset != 200 {
		t.Fatalf("chunk 1: %+v %v", u, err)
	}
	var eo *ErrOffset
	if _, err := m.AppendUpload(u.ID, 100, bytes.NewReader(data[100:])); !errors.As(err, &eo) || eo.Offset != 200 {
		t.Fatalf("bad offset: %v", err)
	}
	if _, err := m.CompleteUpload(u.ID); !errors.Is(err, ErrUploadIncomplete) {
		t.Errorf("complete early: %v", err)
	}
	st, _ := m.UploadStatus(u.ID)
	if st.Offset != 200 {
		t.Errorf("status offset = %d", st.Offset)
	}
	if u, err = m.AppendUpload(u.ID, 200, bytes.NewReader(data[200:])); err != nil || u.Offset != 500 {
		t.Fatalf("chunk 2: %+v %v", u, err)
	}
	n, err := m.CompleteUpload(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n.Path != "in/box/big.txt" || n.Size != 500 || n.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("completed = %+v", n)
	}
	if n.Modified != "2024-01-02T03:04:05Z" {
		t.Errorf("mtime not applied: %s", n.Modified)
	}
	if d, _ := m.Stat("in"); d.Size != 500 {
		t.Errorf("ancestor size = %d", d.Size)
	}
	if ents, _ := os.ReadDir(filepath.Join(m.Root(), ReservedDir, "uploads")); len(ents) != 0 {
		t.Errorf("staging not cleaned: %v", ents)
	}

	// A digest mismatch is refused and nothing lands.
	u, _ = m.BeginUpload("bad.txt", 3, strings.Repeat("a", 64), "", ConflictAsk)
	m.AppendUpload(u.ID, 0, strings.NewReader("abc"))
	if _, err := m.CompleteUpload(u.ID); !errors.Is(err, ErrUploadCorrupt) {
		t.Errorf("corrupt: %v", err)
	}
	if exists(mustRoot(t, m), "bad.txt") {
		t.Error("corrupt upload landed")
	}
	// Conflicts: ask refuses, skip says so, rename keeps both.
	var c *ConflictError
	if _, err := m.BeginUpload("in/box/big.txt", 1, "", "", ConflictAsk); !errors.As(err, &c) {
		t.Errorf("ask: %v", err)
	}
	if u, err := m.BeginUpload("in/box/big.txt", 1, "", "", ConflictSkip); err != nil || !u.Skipped {
		t.Errorf("skip: %+v %v", u, err)
	}
	u, _ = m.BeginUpload("in/box/big.txt", 1, "", "", ConflictRename)
	m.AppendUpload(u.ID, 0, strings.NewReader("x"))
	if n, err := m.CompleteUpload(u.ID); err != nil || n.Path != "in/box/big 2.txt" {
		t.Errorf("rename: %+v %v", n, err)
	}
	// More bytes than declared is a lie, not a bigger file.
	u, _ = m.BeginUpload("lie.txt", 2, "", "", ConflictAsk)
	if _, err := m.AppendUpload(u.ID, 0, strings.NewReader("abcd")); err == nil {
		t.Error("oversize chunk accepted")
	}
}

func mustRoot(t *testing.T, m *Manager) *os.Root {
	t.Helper()
	r, err := m.openRoot()
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func inode(t *testing.T, p string) uint64 {
	t.Helper()
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return st.Sys().(*syscall.Stat_t).Ino
}

func TestSnapshotsLinkUnchangedFiles(t *testing.T) {
	m := newManager(t)
	backups := t.TempDir()
	m.BackupRoot = func() string { return backups }
	write(t, m, "docs/a.txt", "aaaa")
	write(t, m, "docs/b.txt", "bb")
	write(t, m, "skip/secret.txt", "s")
	write(t, m, "gone.txt", "g")
	if err := m.SetExcluded("skip", true); err != nil {
		t.Fatal(err)
	}

	p := jobs.NewProgress()
	first, err := m.RunSnapshot(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if first.Mode != "links" || first.Files != 3 || first.Bytes != 7 || first.NewBytes != 7 {
		t.Fatalf("first = %+v", first)
	}
	if _, err := os.Stat(filepath.Join(first.Path, "skip")); err == nil {
		t.Error("excluded folder was backed up")
	}
	if _, err := os.Readlink(filepath.Join(backups, "files", "latest")); err != nil {
		t.Error("no latest symlink")
	}

	// Night two: one file changed, one deleted, one new.
	time.Sleep(20 * time.Millisecond)
	write(t, m, "docs/a.txt", "aaaa-changed")
	os.Remove(filepath.Join(m.Root(), "gone.txt"))
	write(t, m, "new.txt", "n")
	second, err := m.RunSnapshot(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if second.Files != 3 || second.NewBytes != 13 {
		t.Fatalf("second = %+v", second)
	}
	if inode(t, filepath.Join(first.Path, "docs", "b.txt")) != inode(t, filepath.Join(second.Path, "docs", "b.txt")) {
		t.Error("unchanged file was copied, not linked")
	}
	if inode(t, filepath.Join(first.Path, "docs", "a.txt")) == inode(t, filepath.Join(second.Path, "docs", "a.txt")) {
		t.Error("changed file was linked, not copied")
	}
	if _, err := os.Stat(filepath.Join(second.Path, "gone.txt")); err == nil {
		t.Error("deleted file present in the new snapshot")
	}
	if _, err := os.Stat(filepath.Join(first.Path, "gone.txt")); err != nil {
		t.Error("deleted file missing from the old snapshot")
	}
	mf, err := readManifest(second.Path)
	if err != nil {
		t.Fatal(err)
	}
	linked := 0
	for _, f := range mf.Files {
		if f.Linked {
			linked++
		}
		if f.SHA256 == "" {
			t.Errorf("no digest for %s", f.Path)
		}
	}
	if linked != 1 {
		t.Errorf("linked = %d, want 1", linked)
	}

	// Time travel: the old snapshot lists what the root had then.
	l, err := m.SnapshotList(first.ID, "", ListOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(l); len(got) != 2 || got[0] != "docs" || got[1] != "gone.txt" {
		t.Errorf("snapshot listing = %v", got)
	}
	if len(l.Entries) > 0 && l.Entries[0].Entries == 0 {
		t.Error("snapshot folder listed without its item count")
	}
	f, n, err := m.SnapshotOpenFile(first.ID, "docs/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(f)
	f.Close()
	if string(b) != "aaaa" || n.Size != 4 {
		t.Errorf("old a.txt = %q %+v", b, n)
	}

	// Restore the deleted file to where it was, and a folder elsewhere.
	job, err := m.RestoreSnapshot(first.ID, []string{"gone.txt"}, "", false, ConflictAsk)
	if err != nil {
		t.Fatal(err)
	}
	if j := waitJob(t, m, job.ID); j.State != "done" {
		t.Fatalf("restore job: %+v", j)
	}
	if d, err := m.Stat("gone.txt"); err != nil || d.Size != 1 {
		t.Errorf("restored gone.txt: %+v %v", d, err)
	}
	m.Mkdir("recovered")
	job, _ = m.RestoreSnapshot(first.ID, []string{"docs"}, "recovered", true, ConflictAsk)
	if j := waitJob(t, m, job.ID); j.State != "done" {
		t.Fatalf("restore job 2: %+v", j)
	}
	if d, err := m.Stat("recovered/docs/a.txt"); err != nil || d.Size != 4 {
		t.Errorf("restored old a.txt: %+v %v", d, err)
	}
	var c *ConflictError
	if _, err := m.RestoreSnapshot(first.ID, []string{"docs/a.txt"}, "", false, ConflictAsk); !errors.As(err, &c) {
		t.Errorf("restore over existing: %v", err)
	}

	// Retention keeps the newest N and never the last.
	m.Store.SetSetting("backup_retention", "1")
	third, err := m.RunSnapshot(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := m.ListSnapshots()
	if len(rows) != 1 || rows[0].ID != third.ID {
		t.Errorf("after retention: %+v", rows)
	}
	if _, err := os.Stat(first.Path); err == nil {
		t.Error("pruned snapshot still on disk")
	}
	if err := m.DeleteSnapshot(third.ID); !errors.Is(err, ErrLastSnapshot) {
		t.Errorf("delete last: %v", err)
	}
	st := m.BackupStatus(time.Now().UTC())
	if !st.Enabled || st.Snapshots != 1 || st.LastMode != "links" || st.Excluded != 1 {
		t.Errorf("status = %+v", st)
	}
	// Verification passes on an intact snapshot and records it.
	m.VerifySnapshots(context.Background(), time.Now().UTC())
	if rows, _ := m.ListSnapshots(); rows[0].VerifiedAt == "" {
		t.Error("verify did not record")
	}
}

func TestExcludesFollowRenames(t *testing.T) {
	m := newManager(t)
	write(t, m, "a/b/c.txt", "c")
	m.SetExcluded("a/b", true)
	l, _ := m.List("a", ListOpts{})
	if !l.Entries[0].Excluded {
		t.Error("direct exclusion not stamped")
	}
	l, _ = m.List("a/b", ListOpts{})
	if !l.Entries[0].Excluded || l.Entries[0].ExcludedBy != "a/b" || !l.Dir.Excluded {
		t.Errorf("inherited exclusion not stamped: %+v dir=%+v", l.Entries[0], l.Dir)
	}
	if _, err := m.Rename("a/b", "z"); err != nil {
		t.Fatal(err)
	}
	ex, _ := m.Store.ListFileExcludes()
	if len(ex) != 1 || ex[0] != "a/z" {
		t.Errorf("after rename: %v", ex)
	}
	if _, err := m.Move([]string{"a/z"}, "", ConflictAsk); err != nil {
		t.Fatal(err)
	}
	ex, _ = m.Store.ListFileExcludes()
	if len(ex) != 1 || ex[0] != "z" {
		t.Errorf("after move: %v", ex)
	}
}

func TestThumbnails(t *testing.T) {
	m := newManager(t)
	m.List("", ListOpts{})
	// A real 40x20 PNG with a red left half.
	img := image.NewRGBA(image.Rect(0, 0, 40, 20))
	for y := 0; y < 20; y++ {
		for x := 0; x < 40; x++ {
			c := color.RGBA{0, 0, 255, 255}
			if x < 20 {
				c = color.RGBA{255, 0, 0, 255}
			}
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	write(t, m, "pics/wide.png", buf.String())
	f, sha, _, err := m.Thumbnail(context.Background(), "pics/wide.png", 10)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	th, _, err := image.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	if th.Bounds().Dx() != 40 || th.Bounds().Dy() != 20 {
		// Snapped to the smallest size (96): a 40px source is not upscaled.
		t.Errorf("thumb bounds = %v", th.Bounds())
	}
	if sha == "" {
		t.Error("no digest")
	}
	// The digest landed in the index as a side effect, and the cache file exists.
	d, _ := m.Stat("pics/wide.png")
	if d.SHA256 != sha || d.Width != 40 || d.Height != 20 {
		t.Errorf("detail after thumb = %+v", d)
	}
	if _, err := os.Stat(thumbPath(m.thumbsDir(), sha, 96)); err != nil {
		t.Error("thumbnail not cached")
	}
	if _, _, _, err := m.Thumbnail(context.Background(), "pics/nope.pdf", 96); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
	write(t, m, "doc.pdf", "%PDF")
	if _, _, _, err := m.Thumbnail(context.Background(), "doc.pdf", 96); !errors.Is(err, errNoThumb) {
		t.Errorf("pdf: %v", err)
	}
}

func TestSharesAndDAV(t *testing.T) {
	m := newManager(t)
	write(t, m, "pub/hello.txt", "hi there")
	m.List("", ListOpts{})
	token, share, err := m.CreateShare("pub", 7*24*time.Hour, "")
	if err != nil {
		t.Fatal(err)
	}
	if token == "" || share.Path != "pub" || share.ExpiresAt == "" || share.HasPassword {
		t.Fatalf("share = %+v token=%q", share, token)
	}
	got, err := m.ResolveShare(token)
	if err != nil || got.ID != share.ID {
		t.Fatalf("resolve: %+v %v", got, err)
	}
	if _, err := m.ResolveShare("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("bad token: %v", err)
	}
	// A rename follows the share.
	if _, err := m.Rename("pub", "public"); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.ResolveShare(token); got.Path != "public" {
		t.Errorf("share after rename = %+v", got)
	}
	// Expired links say so.
	tok2, _, _ := m.CreateShare("public/hello.txt", time.Nanosecond, "pw")
	time.Sleep(2 * time.Millisecond)
	if _, err := m.ResolveShare(tok2); !errors.Is(err, ErrShareExpired) {
		t.Errorf("expired: %v", err)
	}

	// WebDAV filesystem semantics over the same root.
	fsys := &davFS{m: m}
	ctx := context.Background()
	if err := fsys.Mkdir(ctx, "/dav-made", 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := fsys.OpenFile(ctx, "/dav-made/note.txt", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("from finder"))
	f.Close()
	if d, err := m.Stat("dav-made/note.txt"); err != nil || d.Size != 11 {
		t.Errorf("indexed after dav write: %+v %v", d, err)
	}
	if _, err := fsys.Stat(ctx, "/.fog"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("reserved dir visible over dav: %v", err)
	}
	if _, err := fsys.OpenFile(ctx, "/../etc/passwd", os.O_RDONLY, 0); err == nil {
		t.Error("escape over dav")
	}
	if err := fsys.Rename(ctx, "/dav-made/note.txt", "/public/moved.txt"); err != nil {
		t.Fatal(err)
	}
	if err := fsys.RemoveAll(ctx, "/dav-made"); err != nil {
		t.Fatal(err)
	}
	items, _ := m.ListTrash()
	if len(items) != 1 || items[0].Path != "dav-made" {
		t.Errorf("dav delete should trash: %+v", items)
	}
	rf, err := fsys.OpenFile(ctx, "/", os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	ents, _ := rf.Readdir(-1)
	rf.Close()
	for _, e := range ents {
		if e.Name() == ReservedDir {
			t.Error(".fog listed over dav")
		}
	}
}

// The default listing is what someone came to see, not what other systems
// left behind — and the count has to agree with the page, or the folder
// claims to hold rows nobody can scroll to.
func TestListingHidesDotfilesAndSystemNames(t *testing.T) {
	m := newManager(t)
	for _, name := range []string{"report.pdf", ".env", "Thumbs.db", "@eaDir", ".DS_Store", "holiday.jpg"} {
		write(t, m, name, "x")
	}
	names := func() []string {
		t.Helper()
		l, err := m.List("", ListOpts{})
		if err != nil {
			t.Fatal(err)
		}
		if l.Total != len(l.Entries) {
			t.Errorf("total %d but %d entries — the count must describe the page", l.Total, len(l.Entries))
		}
		var out []string
		for _, n := range l.Entries {
			out = append(out, n.Name)
		}
		sort.Strings(out)
		return out
	}
	if got := names(); !reflect.DeepEqual(got, []string{"holiday.jpg", "report.pdf"}) {
		t.Errorf("default listing = %v", got)
	}
	if got, err := m.Search("Thumbs", "", "", 0); err != nil || len(got) != 0 {
		t.Errorf("search should not reach past the same fence: %v %v", got, err)
	}

	// Dotfiles back on: the config file returns, the thumbnail cache does
	// not — it is system bookkeeping whichever way it is spelled.
	m.Store.SetSetting(SettingShowHidden, "on")
	if got := names(); !reflect.DeepEqual(got, []string{".env", "holiday.jpg", "report.pdf"}) {
		t.Errorf("with hidden shown = %v", got)
	}

	m.Store.SetSetting(SettingShowSystem, "on")
	if got := names(); !reflect.DeepEqual(got, []string{".DS_Store", ".env", "@eaDir", "Thumbs.db", "holiday.jpg", "report.pdf"}) {
		t.Errorf("with everything shown = %v", got)
	}
	if got, err := m.Search("Thumbs", "", "", 0); err != nil || len(got) != 1 {
		t.Errorf("search follows the same settings: %v %v", got, err)
	}

	// System off with hidden still on: .DS_Store goes, .env stays.
	m.Store.SetSetting(SettingShowSystem, "off")
	if got := names(); !reflect.DeepEqual(got, []string{".env", "holiday.jpg", "report.pdf"}) {
		t.Errorf("system off, hidden on = %v", got)
	}
}

// A download that doesn't hash to the required digest must leave whatever
// was already at the path untouched, and no staging file behind.
func TestMaterializeMismatchKeepsExistingFile(t *testing.T) {
	m := newManager(t)
	write(t, m, "sync/a.txt", "good bytes")
	good := sha256.Sum256([]byte("good bytes"))

	if _, err := m.Materialize("sync/a.txt", strings.NewReader("corrupt"), time.Time{}, hex.EncodeToString(good[:])); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("mismatched digest: err = %v, want ErrDigestMismatch", err)
	}
	if b, _ := os.ReadFile(filepath.Join(m.Root(), "sync/a.txt")); string(b) != "good bytes" {
		t.Fatalf("existing file replaced: %q", b)
	}
	if parts, _ := filepath.Glob(filepath.Join(m.Root(), uploadsDir, "*.part")); len(parts) != 0 {
		t.Fatalf("staging files left behind: %v", parts)
	}

	fresh := sha256.Sum256([]byte("new bytes"))
	got, err := m.Materialize("sync/a.txt", strings.NewReader("new bytes"), time.Time{}, strings.ToUpper(hex.EncodeToString(fresh[:])))
	if err != nil || got != hex.EncodeToString(fresh[:]) {
		t.Fatalf("matching digest: %q %v", got, err)
	}
	if b, _ := os.ReadFile(filepath.Join(m.Root(), "sync/a.txt")); string(b) != "new bytes" {
		t.Fatalf("matching download not placed: %q", b)
	}
}

func TestSnapshotSurvivesVanishingFilesAndLeftovers(t *testing.T) {
	m := newManager(t)
	backups := t.TempDir()
	m.BackupRoot = func() string { return backups }
	write(t, m, "cache/a.txt", "a")
	write(t, m, "cache/b.txt", "b")
	write(t, m, "keep.txt", "k")

	// A run fogd never finished, and a record whose directory is gone.
	orphan := filepath.Join(backups, "files", "20260101-000000.partial")
	os.MkdirAll(filepath.Join(orphan, "x"), 0o755)
	os.WriteFile(filepath.Join(orphan, "x", "y"), []byte("y"), 0o644)
	ghost, err := m.Store.CreateFileSnapshot(filepath.Join(backups, "files", "20260101-000001"), "links", 1, 1, 1, 0)
	if err != nil {
		t.Fatal(err)
	}

	// Whichever cache file the copying walk reaches first deletes the other:
	// listed, then gone before it is copied. (The preflight walks first;
	// the hook fires on the second sighting, once.)
	sightings, fired := map[string]int{}, false
	m.BackupSkip = func(rel string) bool {
		sightings[rel]++
		if sightings[rel] < 2 || fired {
			return false
		}
		switch rel {
		case "cache/a.txt":
			fired = true
			os.Remove(filepath.Join(m.Root(), "cache", "b.txt"))
		case "cache/b.txt":
			fired = true
			os.Remove(filepath.Join(m.Root(), "cache", "a.txt"))
		}
		return false
	}
	snap, err := m.RunSnapshot(context.Background(), jobs.NewProgress())
	if err != nil {
		t.Fatalf("a vanished file failed the snapshot: %v", err)
	}
	if snap.Files != 2 {
		t.Errorf("files = %d, want 2", snap.Files)
	}
	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Error("abandoned partial left behind")
	}
	rows, _ := m.ListSnapshots()
	for _, r := range rows {
		if r.ID == ghost {
			t.Error("record for a missing directory kept")
		}
	}
}

func TestCheckSpaceRefusesToFillTheDisk(t *testing.T) {
	dir := t.TempDir()
	// A 100 GiB disk with half free: the floor is max(minFreeBytes, 5 GiB).
	stubDiskSpace(t, 50<<30, 100<<30)
	if err := checkSpace(dir, 1); err != nil {
		t.Fatalf("one byte: %v", err)
	}
	if err := checkSpace(dir, 1<<60); !errors.Is(err, ErrBackupDiskFull) {
		t.Fatalf("an exabyte: %v", err)
	}
}

func TestSnapshotThatCannotFitWritesNothing(t *testing.T) {
	m := newManager(t)
	backups := t.TempDir()
	m.BackupRoot = func() string { return backups }
	write(t, m, "Movies/big.mkv", strings.Repeat("m", 4096))
	write(t, m, "docs/a.txt", "aaaa")
	// A 100 GiB disk exactly at its 5 GiB floor: nothing more fits.
	stubDiskSpace(t, 5<<30, 100<<30)
	_, err := m.RunSnapshot(context.Background(), jobs.NewProgress())
	if !errors.Is(err, ErrBackupDiskFull) {
		t.Fatalf("err = %v", err)
	}
	// Refused before copying: no partial tree was ever started.
	entries, _ := os.ReadDir(filepath.Join(backups, "files"))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".partial") {
			t.Fatalf("a partial snapshot was written: %s", e.Name())
		}
	}
	notices, _ := m.Store.ActiveNotices()
	found := false
	for _, n := range notices {
		if n.Kind == NoticeFilesBackupNoSpace && strings.Contains(n.Body, "Movies") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the notice doesn't name the biggest folder: %+v", notices)
	}
}

func TestSchedulerWaitsForTomorrowAfterADiskFullRun(t *testing.T) {
	m := newManager(t)
	backups := t.TempDir()
	m.BackupRoot = func() string { return backups }
	if err := m.Store.SetSetting(backup.SettingBackupHourUTC, "1"); err != nil {
		t.Fatal(err)
	}
	write(t, m, "docs/a.txt", strings.Repeat("a", 1024))
	stubDiskSpace(t, 5<<30, 100<<30)
	now := time.Date(2026, 10, 9, 2, 0, 0, 0, time.UTC)
	m.diskFull(1024)
	m.snapFullAt = now.Add(-30 * time.Minute)
	// Two hours on, still no room: no attempt.
	m.runDue(context.Background(), now.Add(2*time.Hour))
	if !m.lastSnapAttempt.IsZero() {
		t.Fatal("retried within the night with no more room")
	}
	// Room made: it tries again the same night.
	stubDiskSpace(t, 50<<30, 100<<30)
	m.runDue(context.Background(), now.Add(3*time.Hour))
	if m.lastSnapAttempt.IsZero() {
		t.Fatal("did not retry once there was room")
	}
}
