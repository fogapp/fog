package files

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/jobs"
	"gitea.lan/matthew/fog/internal/store/storetest"
)

// pngHeaderOnly is a PNG that declares w×h pixels and holds none: a few
// dozen bytes that image.Decode would answer by allocating w*h*4.
func pngHeaderOnly(w, h uint32) []byte {
	var b bytes.Buffer
	b.WriteString("\x89PNG\r\n\x1a\n")
	chunk := func(typ string, data []byte) {
		binary.Write(&b, binary.BigEndian, uint32(len(data)))
		c := crc32.NewIEEE()
		c.Write([]byte(typ))
		c.Write(data)
		b.WriteString(typ)
		b.Write(data)
		binary.Write(&b, binary.BigEndian, c.Sum32())
	}
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], w)
	binary.BigEndian.PutUint32(ihdr[4:], h)
	ihdr[8], ihdr[9] = 8, 6 // 8-bit RGBA
	chunk("IHDR", ihdr)
	chunk("IDAT", []byte{0x78, 0x9c, 0x03, 0x00, 0x00, 0x00, 0x00, 0x01})
	chunk("IEND", nil)
	return b.Bytes()
}

// An app plants a small PNG that declares 20000×20000 pixels where the
// operator will browse. Rendering its thumbnail must refuse it from the
// header, not allocate 1.6 GB finding out.
func TestThumbnailRefusesDecompressionBomb(t *testing.T) {
	t.Parallel()

	m := newManager(t)
	bomb := pngHeaderOnly(20000, 20000)
	write(t, m, "Content/Photos/bomb.png", string(bomb))

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, _, _, err := m.Thumbnail(context.Background(), "Content/Photos/bomb.png", 320)
	runtime.ReadMemStats(&after)
	if !errors.Is(err, errNoThumb) {
		t.Fatalf("bomb thumbnail: %v, want errNoThumb", err)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 32<<20 {
		t.Fatalf("refusing the bomb allocated %d MB", grew>>20)
	}

	// Within the limits, the same path still works.
	var ok bytes.Buffer
	jpeg.Encode(&ok, image.NewRGBA(image.Rect(0, 0, 64, 48)), nil)
	write(t, m, "Content/Photos/ok.jpg", ok.String())
	f, _, _, err := m.Thumbnail(context.Background(), "Content/Photos/ok.jpg", 96)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
}

func TestDecodeCostLimits(t *testing.T) {
	t.Parallel()

	cases := []struct {
		w, h   int
		format string
		ok     bool
	}{
		{4000, 3000, "jpeg", true},   // 12 MP phone photo
		{8000, 6000, "jpeg", true},   // 48 MP
		{20000, 20000, "png", false}, // 400 MP
		{9000, 8000, "png", false},   // 72 MP: over the pixel limit
		{1, 70_000_000, "png", false},
	}
	for _, c := range cases {
		_, err := decodeCost(image.Config{Width: c.w, Height: c.h, ColorModel: color.RGBAModel}, c.format)
		if (err == nil) != c.ok {
			t.Errorf("%dx%d %s: err=%v, want ok=%v", c.w, c.h, c.format, err, c.ok)
		}
	}
}

// The share-link attack: an app that can write into a shared folder fills
// a subfolder with decoys, waits for them to be indexed, then swaps the
// subfolder for a link to elsewhere in Drive. Nothing that serves the
// share — Stat, OpenFile, List, the zip, the hasher, thumbnails — may then
// reach Private.
func TestNothingFollowsALinkSwappedIntoDrive(t *testing.T) {
	t.Parallel()

	m := newManager(t)
	write(t, m, "Public/sub/secret.txt", "decoy")
	write(t, m, "Public/hello.txt", "hi")
	write(t, m, "Private/secret.txt", "the operator's secret")
	if _, err := m.List("Public/sub", ListOpts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Stat("Public/sub/secret.txt"); err != nil {
		t.Fatal(err)
	}

	root := m.Root()
	if err := os.RemoveAll(filepath.Join(root, "Public/sub")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../Private", filepath.Join(root, "Public/sub")); err != nil {
		t.Fatal(err)
	}

	if _, err := m.Stat("Public/sub/secret.txt"); err == nil {
		t.Error("Stat reached through the link")
	}
	if f, _, err := m.OpenFile("Public/sub/secret.txt"); err == nil {
		b, _ := io.ReadAll(f)
		f.Close()
		t.Fatalf("OpenFile followed the link and read %q", b)
	} else if !errors.Is(err, ErrSymlink) {
		t.Errorf("OpenFile: %v, want ErrSymlink", err)
	}
	if _, err := m.List("Public/sub", ListOpts{}); err == nil {
		t.Error("List descended into the link")
	}
	if _, err := m.HashNow("Public/sub/secret.txt"); err == nil {
		t.Error("HashNow read through the link")
	}
	if _, err := m.Lstat("Public/sub/secret.txt"); !errors.Is(err, ErrSymlink) {
		t.Errorf("Lstat: %v, want ErrSymlink", err)
	}
	// The link itself is an entry like any other, of kind "other".
	if k, err := m.KindOf("Public/sub"); err != nil || k != "other" {
		t.Errorf("KindOf the link = %q, %v", k, err)
	}

	var buf bytes.Buffer
	if err := m.WriteZip(context.Background(), &buf, []string{"Public"}); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range zr.File {
		if strings.Contains(f.Name, "secret") {
			t.Errorf("the zip of Public holds %s", f.Name)
		}
	}

	// A file that is itself a link is not opened either.
	os.Symlink("../Private/secret.txt", filepath.Join(root, "Public/leak.txt"))
	if f, _, err := m.OpenFile("Public/leak.txt"); err == nil {
		f.Close()
		t.Error("OpenFile followed a link to a file")
	}
	if _, _, _, err := m.Thumbnail(context.Background(), "Public/sub/x.png", 96); err == nil {
		t.Error("Thumbnail reached through the link")
	}
}

// Writes refuse a path that runs through a link: an upload or a new
// folder must land where it was asked to, not in another app's folder.
func TestWritesRefuseALinkOnTheWay(t *testing.T) {
	t.Parallel()

	m := newManager(t)
	write(t, m, "Content/Books/a.txt", "a")
	write(t, m, "Apps/Vault/db.sqlite3", "vault")
	root := m.Root()
	os.Symlink("../Apps/Vault", filepath.Join(root, "Content/Library"))

	if _, err := m.Mkdir("Content/Library/new"); !errors.Is(err, ErrSymlink) {
		t.Errorf("Mkdir through a link: %v", err)
	}
	if _, err := m.BeginUpload("Content/Library/up.txt", 1, "", "", ConflictAsk); !errors.Is(err, ErrSymlink) {
		t.Errorf("BeginUpload through a link: %v", err)
	}
	if _, err := m.Trash([]string{"Content/Library/db.sqlite3"}); !errors.Is(err, ErrSymlink) {
		t.Errorf("Trash through a link: %v", err)
	}
	if _, err := m.Rename("Content/Library/db.sqlite3", "x"); !errors.Is(err, ErrSymlink) {
		t.Errorf("Rename through a link: %v", err)
	}
	// An app volume naming the folder is never bound through the link.
	if err := m.EnsureDir("Content/Library/data"); !errors.Is(err, ErrSymlink) {
		t.Errorf("EnsureDir through a link: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "Apps/Vault/data")); err == nil {
		t.Error("EnsureDir created a folder inside the link's target")
	}
	if b, _ := os.ReadFile(filepath.Join(root, "Apps/Vault/db.sqlite3")); string(b) != "vault" {
		t.Errorf("the link's target changed: %q", b)
	}
}

// A copy stages its bytes beside the destination, in a folder an app may
// write. A link planted at the staging name must not have the copy's bytes
// written through it into another app's file.
func TestCopyStagingIgnoresAPlantedLink(t *testing.T) {
	t.Parallel()

	m := newManager(t)
	write(t, m, "Docs/report.pdf", "the report")
	write(t, m, "Apps/Vault/db.sqlite3", "vault")
	write(t, m, "Content/Shared/.keep", "")
	root := m.Root()
	os.Symlink("../../Apps/Vault/db.sqlite3", filepath.Join(root, "Content/Shared/report.pdf.fog-copying"))

	job, err := m.Copy([]string{"Docs/report.pdf"}, "Content/Shared", ConflictAsk)
	if err != nil {
		t.Fatal(err)
	}
	if j := waitJob(t, m, job.ID); j.State != "done" {
		t.Fatalf("copy job: %+v", j)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "Apps/Vault/db.sqlite3")); string(b) != "vault" {
		t.Fatalf("the copy wrote through the planted link: the vault now holds %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "Content/Shared/report.pdf")); string(b) != "the report" {
		t.Fatalf("the copy: %q", b)
	}
}

// A FIFO planted where a file was must not hang a reader: the hasher, a
// download, a thumbnail all refuse it at once.
func TestAPlantedFIFOIsNeverOpened(t *testing.T) {
	t.Parallel()

	m := newManager(t)
	write(t, m, "Content/x.jpg", "x")
	p := filepath.Join(m.Root(), "Content/pipe.jpg")
	if err := syscall.Mkfifo(p, 0o644); err != nil {
		t.Skip("no mkfifo:", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if f, _, err := m.OpenFile("Content/pipe.jpg"); err == nil {
			f.Close()
			t.Error("opened a FIFO")
		}
		root, _ := m.openRoot()
		if _, err := hashFile(root, Node{Path: "Content/pipe.jpg"}); err == nil {
			t.Error("hashed a FIFO")
		}
		if _, _, _, err := m.Thumbnail(context.Background(), "Content/pipe.jpg", 96); err == nil {
			t.Error("thumbnailed a FIFO")
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a reader hung on a FIFO")
	}
}

// A share link is of an item. Trashed, its link is suspended and a new item
// at the same path is not served; restored, the link comes back with it;
// purged, the link is gone. Replaced behind fog's back, the link leads
// nowhere.
func TestShareFollowsTheItemNotThePath(t *testing.T) {
	t.Parallel()

	m := newManager(t)
	write(t, m, "Public/report.pdf", "v1")
	token, sh, err := m.CreateShare("Public/report.pdf", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.ResolveShare(token); err != nil {
		t.Fatal(err)
	}

	items, err := m.Trash([]string{"Public/report.pdf"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.ResolveShare(token); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a trashed item's link still resolves: %v", err)
	}
	// Something new takes the name: the old link must not serve it.
	write(t, m, "Public/report.pdf", "something else entirely")
	if _, err := m.ResolveShare(token); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the link came back to life for a new item at its path: %v", err)
	}
	if l, _ := m.ListShares("Public/report.pdf"); len(l) != 0 {
		t.Fatalf("the new item is listed with the old item's link: %+v", l)
	}
	if all, _ := m.ListShares(""); len(all) != 1 || !all[0].InTrash {
		t.Fatalf("all links = %+v, want one in the trash", all)
	}

	// Restored: the name is taken, so it comes back as "report 2.pdf", and
	// its link with it.
	out, err := m.RestoreTrash([]string{items[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	got, err := m.ResolveShare(token)
	if err != nil {
		t.Fatalf("the restored item's link: %v", err)
	}
	if got.Path != out[0] || got.ID != sh.ID {
		t.Fatalf("link follows to %q, restored to %q", got.Path, out[0])
	}

	// Renamed by fog: the link follows.
	if _, err := m.Rename(out[0], "final.pdf"); err != nil {
		t.Fatal(err)
	}
	if got, err := m.ResolveShare(token); err != nil || got.Path != "Public/final.pdf" {
		t.Fatalf("after a rename: %+v %v", got, err)
	}

	// Trashed and purged: gone for good.
	items, _ = m.Trash([]string{"Public/final.pdf"})
	if err := m.PurgeTrash(items[0].ID); err != nil {
		t.Fatal(err)
	}
	if all, _ := m.ListShares(""); len(all) != 0 {
		t.Fatalf("a purged item's link survives: %+v", all)
	}

	// Deleted and replaced behind fog's back.
	write(t, m, "Public/other.txt", "one")
	token2, _, err := m.CreateShare("Public/other.txt", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(m.Root(), "Public/other.txt")
	tmp := p + ".new"
	os.WriteFile(tmp, []byte("two"), 0o644)
	os.Rename(tmp, p)
	if _, err := m.ResolveShare(token2); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a link to a replaced file still resolves: %v", err)
	}
	if all, _ := m.ListShares(""); len(all) != 1 || !all[0].Broken {
		t.Fatalf("the replaced file's link should show broken: %+v", all)
	}
}

// A folder's link survives what happens inside it.
func TestShareOfAFolderSurvivesItsContentsChanging(t *testing.T) {
	t.Parallel()

	m := newManager(t)
	write(t, m, "Public/a.txt", "a")
	token, _, err := m.CreateShare("Public", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	write(t, m, "Public/b.txt", "b")
	m.Trash([]string{"Public/a.txt"})
	if _, err := m.ResolveShare(token); err != nil {
		t.Fatalf("folder link after its contents changed: %v", err)
	}
}

// An upload bigger than the room left is refused at the start.
func TestUploadRefusedWhenItCannotFit(t *testing.T) {
	m := newManager(t)
	stubDiskSpace(t, 1<<20, 1<<30)
	if _, err := m.BeginUpload("big.bin", 2<<20, "", "", ConflictAsk); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("BeginUpload past the free space: %v", err)
	}
	if _, err := m.BeginUpload("small.bin", 1<<10, "", "", ConflictAsk); err != nil {
		t.Fatal(err)
	}
}

// Two chunks sent at one offset at once: one lands, the other is told the
// real offset; the part never holds both.
func TestConcurrentChunksAtOneOffset(t *testing.T) {
	t.Parallel()

	m := newManager(t)
	u, err := m.BeginUpload("x.bin", 8, "", "", ConflictAsk)
	if err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 2)
	for _, body := range []string{"AAAAAAAA", "BBBBBBBB"} {
		go func() {
			_, err := m.AppendUpload(u.ID, 0, strings.NewReader(body))
			errs <- err
		}()
	}
	var oks int
	for range 2 {
		if err := <-errs; err == nil {
			oks++
		}
	}
	if oks != 1 {
		t.Fatalf("%d appends at offset 0 succeeded, want 1", oks)
	}
	n, err := m.CompleteUpload(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(m.Root(), n.Path))
	if s := string(b); s != "AAAAAAAA" && s != "BBBBBBBB" {
		t.Fatalf("interleaved part: %q", s)
	}
}

// A move between disks copies the tree. A link in it is copied as a link,
// never as what it points at: a link an app planted to /etc must not put
// the host's files into a folder the app can read.
func TestCopyTreeCopiesLinksAsLinks(t *testing.T) {
	t.Parallel()

	from, to, outside := t.TempDir(), t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(outside, "shadow"), []byte("host secret"), 0o600)
	os.MkdirAll(filepath.Join(from, "Apps", "A", "data"), 0o755)
	os.WriteFile(filepath.Join(from, "Apps", "A", "data", "ok.txt"), []byte("ok"), 0o644)
	os.Symlink(outside, filepath.Join(from, "Apps", "A", "data", "etc"))
	os.Symlink(filepath.Join(outside, "shadow"), filepath.Join(from, "Apps", "A", "data", "shadow"))

	m := newManager(t)
	job := m.Jobs.Start("copy", "copy", func(ctx context.Context, p *jobs.Progress) error {
		return copyTree(ctx, from, to, "", p)
	})
	if j := waitJob(t, m, job.ID); j.State != "done" {
		t.Fatalf("job = %+v", j)
	}
	if b, _ := os.ReadFile(filepath.Join(to, "Apps", "A", "data", "ok.txt")); string(b) != "ok" {
		t.Fatalf("the copy: %q", b)
	}
	for _, name := range []string{"etc", "shadow"} {
		fi, err := os.Lstat(filepath.Join(to, "Apps", "A", "data", name))
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Errorf("%s was not copied as a link: %v %v", name, fi, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(to, "Apps", "A", "data", "etc", "shadow")); err == nil {
		if fi, _ := os.Lstat(filepath.Join(to, "Apps", "A", "data", "etc")); fi.Mode()&os.ModeSymlink == 0 {
			t.Error("the copy descended into a link")
		}
	}
}

// Digests from a peer name cache files; one that is not a digest must
// never get as far as a path.
func TestThumbCacheRefusesANonDigest(t *testing.T) {
	t.Parallel()

	m := newManager(t)
	for _, bad := range []string{"../../../../tmp/x", "ab", strings.Repeat("Z", 64)} {
		if err := m.StoreThumb(bad, 96, []byte("x")); err == nil {
			t.Errorf("StoreThumb(%q) accepted", bad)
		}
		if f, err := m.CachedThumb(bad, 96); err == nil {
			f.Close()
			t.Errorf("CachedThumb(%q) opened something", bad)
		}
	}
}

// On a case-insensitive root (macOS, a CIFS share) ".FOG" is fog's own
// directory. No spelling of it may be addressed, listed or written.
func TestReservedNamesIgnoreCase(t *testing.T) {
	t.Parallel()

	for _, p := range []string{".FOG/trash", ".Fog", "apps/.FOG/uploads", "CONTENT/.fog", "Content/.fOg/trash"} {
		if _, err := CleanRel(p); !errors.Is(err, ErrReserved) {
			t.Errorf("CleanRel(%q) = %v, want ErrReserved", p, err)
		}
	}
	if _, err := CleanRel("Docs/.FOG"); err != nil {
		t.Errorf("a .FOG deep inside Drive is an ordinary folder: %v", err)
	}
	for _, c := range []struct{ parent, name string }{{"", ".FOG"}, {"apps", ".Fog"}, {"Content", ".FOG"}} {
		if err := ValidateName(c.parent, c.name); !errors.Is(err, ErrReserved) {
			t.Errorf("ValidateName(%q, %q) = %v", c.parent, c.name, err)
		}
	}
	m := newManager(t)
	write(t, m, "Docs/a.txt", "a")
	if err := os.Mkdir(filepath.Join(m.Root(), ".FOG"), 0o755); err != nil && !os.IsExist(err) {
		t.Fatal(err)
	}
	l, err := m.List("", ListOpts{ShowHidden: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range l.Entries {
		if strings.EqualFold(e.Name, ReservedDir) {
			t.Errorf("the root listing shows %q", e.Name)
		}
	}
}

// A Drive move from a case-sensitive disk to a case-insensitive one. An app
// put a link "A" (to a folder outside) beside a folder "a" holding a file.
// Copying by path, "A" was made as a link and then "a/pwn.txt" was written
// through it — outside the destination, as root. By descriptor, the folder
// "a" meets the name taken and the copy stops; nothing lands outside.
func TestMoveToCaseInsensitiveDiskCannotWriteThroughALink(t *testing.T) {
	t.Parallel()

	if runtime.GOOS != "darwin" {
		t.Skip("needs a case-sensitive and a case-insensitive APFS volume side by side")
	}
	from := filepath.Join(caseSensitiveDir(t), "drive")
	outside := t.TempDir()
	os.MkdirAll(filepath.Join(from, "a"), 0o755)
	os.WriteFile(filepath.Join(from, "a", "pwn.txt"), []byte("written outside"), 0o644)
	if err := os.Symlink(outside, filepath.Join(from, "A")); err != nil {
		t.Fatal(err)
	}
	to := t.TempDir() // the default APFS volume: case-insensitive
	probe := filepath.Join(to, "Probe")
	os.WriteFile(probe, nil, 0o644)
	if _, err := os.Stat(filepath.Join(to, "probe")); err != nil {
		t.Skip("the destination volume is case-sensitive")
	}
	os.Remove(probe)

	m := newManager(t)
	job := m.Jobs.Start("copy", "copy", func(ctx context.Context, p *jobs.Progress) error {
		return copyTree(ctx, from, to, "", p)
	})
	j := waitJob(t, m, job.ID)
	if _, err := os.Lstat(filepath.Join(outside, "pwn.txt")); err == nil {
		t.Fatal("the move wrote through a link outside its destination")
	}
	if j.State == "done" {
		t.Fatalf("a copy that cannot place a folder must fail, not report done: %+v", j)
	}
}

// caseSensitiveDir mounts a small case-sensitive APFS volume for the test
// (macOS's own volumes fold case) and returns its mount point.
func caseSensitiveDir(t *testing.T) string {
	t.Helper()
	img := filepath.Join(t.TempDir(), "cs.sparseimage")
	mnt := t.TempDir()
	if out, err := exec.Command("hdiutil", "create", "-size", "40m", "-fs", "Case-sensitive APFS", "-volname", "fogcs", "-type", "SPARSE", img).CombinedOutput(); err != nil {
		t.Skipf("hdiutil create: %s %v", out, err)
	}
	if out, err := exec.Command("hdiutil", "attach", "-nobrowse", "-mountpoint", mnt, img).CombinedOutput(); err != nil {
		t.Skipf("hdiutil attach: %s %v", out, err)
	}
	t.Cleanup(func() { exec.Command("hdiutil", "detach", "-force", mnt).Run() })
	return mnt
}

// An app writes a file "X" beside a folder "x/" (and "Y" beside "y"). On a
// case-insensitive backup disk the second of each cannot be held, and
// that used to fail the whole nightly snapshot — every app's and the
// operator's files unbacked because of one app's names. Now the clashing
// entry is left out and named in the Inbox, and the rest is backed up.
func TestSnapshotSurvivesCaseCollisions(t *testing.T) {
	t.Parallel()

	if runtime.GOOS != "darwin" {
		t.Skip("needs a case-sensitive Drive and a case-insensitive backup disk")
	}
	dir := caseSensitiveDir(t)
	st, err := storetest.Open(filepath.Join(dir, "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m := New(dir, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(m.Close)
	backups := t.TempDir()
	if !caseInsensitive(backups) {
		t.Skip("the backup volume is case-sensitive")
	}
	m.BackupRoot = func() string { return backups }
	write(t, m, "Docs/keep.txt", "keep")
	write(t, m, "Apps/evil/X", "file")
	write(t, m, "Apps/evil/x/inner.txt", "folder")
	write(t, m, "Apps/evil/Y", "upper")
	write(t, m, "Apps/evil/y", "lower")

	row, err := m.RunSnapshot(context.Background(), jobs.NewProgress())
	if err != nil {
		t.Fatalf("one app's names failed the whole snapshot: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(row.Path, "Docs", "keep.txt")); string(b) != "keep" {
		t.Fatal("the rest of Drive was not backed up")
	}
	if b, _ := os.ReadFile(filepath.Join(row.Path, "Apps", "evil", "Y")); string(b) != "upper" {
		t.Fatalf("a clashing name was written over the first: %q", b)
	}
	ns, _ := m.Store.ActiveNotices()
	found := false
	for _, n := range ns {
		found = found || (n.Kind == NoticeFilesCollided && strings.Contains(n.Body, "Apps/evil/x") && strings.Contains(n.Body, "Apps/evil/y"))
	}
	if !found {
		t.Fatalf("the left-out entries must be named in the Inbox: %+v", ns)
	}
}

// A user's "café.txt" (NFC) and an app's "café.txt" written NFD are two
// files on ext4, but one name on a backup disk that normalises (APFS, ZFS
// formD, CIFS from APFS). Folding case alone missed it, and the second
// copy silently replaced the first. The disk itself now decides: a name
// that lands on an entry this snapshot already placed is left out and
// reported, never written over it.
//
// Runs in the Linux VM, whose /Users is the Mac's (normalising) disk.
func TestSnapshotCatchesNormalisationCollisions(t *testing.T) {
	t.Parallel()

	m := newManager(t)
	nfc, nfd := "caf\u00e9.txt", "cafe\u0301.txt"
	write(t, m, "Docs/"+nfc, "the user's")
	write(t, m, "Docs/"+nfd, "the app's!")
	if b, _ := os.ReadFile(filepath.Join(m.Root(), "Docs", nfc)); string(b) != "the user's" {
		t.Skip("the Drive disk itself folds normalisation")
	}
	wd, _ := os.Getwd()
	backups, err := os.MkdirTemp(wd, ".snaptest-")
	if err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() { os.RemoveAll(backups) })
	os.WriteFile(filepath.Join(backups, nfc), nil, 0o600)
	_, err = os.Lstat(filepath.Join(backups, nfd))
	os.Remove(filepath.Join(backups, nfc))
	if err != nil {
		t.Skip("no normalising disk to back up to here (run in the VM)")
	}
	m.BackupRoot = func() string { return backups }

	row, err := m.RunSnapshot(context.Background(), jobs.NewProgress())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	mf, err := readManifest(row.Path)
	if err != nil {
		t.Fatal(err)
	}
	if len(mf.Collided) != 1 {
		t.Fatalf("one of the two must be reported left out: %q", mf.Collided)
	}
	var kept snapFile
	for _, f := range mf.Files {
		if f.Path == "Docs/"+nfc || f.Path == "Docs/"+nfd {
			kept = f
		}
	}
	got, _ := os.ReadFile(filepath.Join(row.Path, "Docs", nfc))
	if sum := sha256.Sum256(got); hex.EncodeToString(sum[:]) != kept.SHA256 {
		t.Fatalf("the backed-up bytes are not the kept entry's (%s): %q", kept.Path, got)
	}
	ns, _ := m.Store.ActiveNotices()
	found := false
	for _, n := range ns {
		found = found || n.Kind == NoticeFilesCollided
	}
	if !found {
		t.Fatal("the left-out entry must be named in the Inbox")
	}
}

// On a case-insensitive disk (APFS) or a casefold one (ext4 with +F), a
// rename that changes only case is a rename onto the same file — which
// POSIX lets rename(2) do nothing about, and casefold ext4 does nothing.
// Drive then said "not found" and the name stayed as it was. It now goes
// by a temporary name, so the new spelling sticks, through Drive and
// WebDAV alike, and nothing is ever replaced.
//
// Set FOG_CASEFOLD_DIR to a +F directory on casefold ext4 to run it there;
// otherwise it runs where the temp directory folds case (macOS).
func TestCaseOnlyRenameOnAFoldingDisk(t *testing.T) {
	t.Parallel()

	base := os.Getenv("FOG_CASEFOLD_DIR")
	if base == "" {
		base = t.TempDir()
	}
	dir, err := os.MkdirTemp(base, "fold-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if !caseInsensitive(dir) {
		t.Skip("this disk tells case apart (set FOG_CASEFOLD_DIR)")
	}
	st, err := storetest.Open(filepath.Join(t.TempDir(), "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m := New(dir, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(m.Close)
	write(t, m, "Docs/report.txt", "the report")
	write(t, m, "Docs/notes/a.txt", "a")
	m.Walk(context.Background())

	names := func() []string {
		ents, _ := os.ReadDir(filepath.Join(m.Root(), "Docs"))
		var out []string
		for _, e := range ents {
			out = append(out, e.Name())
		}
		return out
	}
	n, err := m.Rename("Docs/report.txt", "Report.txt")
	if err != nil {
		t.Fatalf("case-only rename: %v", err)
	}
	if n.Name != "Report.txt" || !slices.Contains(names(), "Report.txt") || slices.Contains(names(), "report.txt") {
		t.Fatalf("the new spelling did not stick: node %q, on disk %v", n.Name, names())
	}
	if b, _ := os.ReadFile(filepath.Join(m.Root(), "Docs", "Report.txt")); string(b) != "the report" {
		t.Fatalf("content: %q", b)
	}
	// A folder, through WebDAV's MOVE.
	if err := m.renameAny("Docs/notes", "Docs/Notes"); err != nil {
		t.Fatalf("WebDAV case-only move: %v", err)
	}
	if !slices.Contains(names(), "Notes") {
		t.Fatalf("folder spelling did not stick: %v", names())
	}
	if b, _ := os.ReadFile(filepath.Join(m.Root(), "Docs", "Notes", "a.txt")); string(b) != "a" {
		t.Fatalf("folder content: %q", b)
	}
	for _, nm := range names() {
		if strings.HasPrefix(nm, ".fog-rename-") {
			t.Fatalf("a temporary name was left behind: %s", nm)
		}
	}
}
