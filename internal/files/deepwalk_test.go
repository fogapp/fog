package files

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"

	"gitea.lan/matthew/fog/internal/jobs"
)

// mkChain builds dir/d/d/…/d, depth folders deep, by descriptor (the path
// is longer than PATH_MAX), with a file f.txt in the folder at fileAt.
func mkChain(t *testing.T, dir string, depth, fileAt int) {
	t.Helper()
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= depth; i++ {
		if err := unix.Mkdirat(fd, "d", 0o755); err != nil {
			t.Fatal(err)
		}
		nfd, err := unix.Openat(fd, "d", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		unix.Close(fd)
		if err != nil {
			t.Fatal(err)
		}
		fd = nfd
		if i == fileAt {
			f, err := unix.Openat(fd, "f.txt", unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, 0o644)
			if err != nil {
				t.Fatal(err)
			}
			unix.Write(f, []byte("deep"))
			unix.Close(f)
		}
	}
	unix.Close(fd)
}

// chainPath is the relative path of the folder depth levels down a chain.
func chainPath(depth int) string {
	return strings.TrimSuffix(strings.Repeat("d/", depth), "/")
}

// lowFDLimit caps this process's descriptors for the rest of the test, so
// a walk that holds one per level fails with EMFILE where it would have
// before.
func lowFDLimit(t *testing.T, n uint64) {
	t.Helper()
	var old unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &old); err != nil {
		t.Fatal(err)
	}
	lim := old
	lim.Cur = n
	if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &lim); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unix.Setrlimit(unix.RLIMIT_NOFILE, &old) })
}

// An app nests folders 1500 deep in its own folder. Every walk fog makes of
// Drive survives it: the index and the nightly snapshot carry on without
// the part past MaxDepth, a zip and a copy refuse it with ErrTooDeep, and a
// delete removes all of it — none of them running the process out of
// descriptors (the limit here is far below one per level) and none taking
// time that grows with depth squared.
func TestDeepTreeIsWalkedWithBoundedDescriptors(t *testing.T) {
	m := newManager(t)
	backups := t.TempDir()
	m.BackupRoot = func() string { return backups }
	write(t, m, "Apps/evil/top.txt", "top")
	write(t, m, "Docs/keep.txt", "keep")
	mkChain(t, filepath.Join(m.Root(), "Apps", "evil"), 1500, 20)
	lowFDLimit(t, 128)
	ctx := context.Background()
	start := time.Now()

	// The index walk finishes and indexes what is within reach.
	if err := m.Walk(ctx); err != nil {
		t.Fatalf("walk: %v", err)
	}
	deepFile := "Apps/evil/" + chainPath(20) + "/f.txt"
	if _, err := m.Stat(deepFile); err != nil {
		t.Fatalf("a file 22 levels down is not indexed: %v", err)
	}

	// A zip of the app's folder refuses, clearly.
	err := m.WriteZip(ctx, io.Discard, []string{"Apps/evil"})
	if !errors.Is(err, ErrTooDeep) || !errors.Is(err, ErrBadName) {
		t.Fatalf("zip of a too-deep tree: %v, want ErrTooDeep (a bad request)", err)
	}
	// One within reach still zips.
	var buf bytes.Buffer
	if err := m.WriteZip(ctx, &buf, []string{"Docs"}); err != nil {
		t.Fatalf("zip of a shallow tree: %v", err)
	}

	// The snapshot backs up everyone's files, leaving out only the part
	// past MaxDepth.
	row, err := m.RunSnapshot(ctx, jobs.NewProgress())
	if err != nil {
		t.Fatalf("snapshot failed because one app nested folders: %v", err)
	}
	for _, rel := range []string{"Docs/keep.txt", "Apps/evil/top.txt", deepFile} {
		if _, err := os.Stat(filepath.Join(row.Path, filepath.FromSlash(rel))); err != nil {
			t.Errorf("snapshot is missing %s: %v", rel[:min(len(rel), 40)], err)
		}
	}

	// And says so in the Inbox, naming the folder: a backup that reports
	// success while leaving part of Drive out must not be quiet about it.
	if n := activeNotice(t, m, NoticeFilesTooDeep); !strings.Contains(n, "Files/Apps/evil/d/…/d") {
		t.Fatalf("too-deep notice does not name the folder: %q", n)
	}

	// A root move's walk refuses rather than running out of descriptors.
	err = copyTree(ctx, filepath.Join(m.Root(), "Apps"), t.TempDir(), "", jobs.NewProgress())
	if !errors.Is(err, ErrTooDeep) {
		t.Fatalf("root-move copy: %v, want ErrTooDeep", err)
	}

	// Throwing it away works, however deep.
	root, err := m.openRoot()
	if err != nil {
		t.Fatal(err)
	}
	if err := nf(root).RemoveAll("Apps/evil"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(m.Root(), "Apps", "evil")); !os.IsNotExist(err) {
		t.Fatalf("the deep tree is still there: %v", err)
	}
	if d := time.Since(start); d > 60*time.Second {
		t.Fatalf("deep-tree walks took %s", d)
	}
}

// A copy between disks (a volume move) of a tree deep enough that the old
// copy held more descriptors than the limit allows completes within the
// limit; one past MaxDepth is refused, its source untouched and its
// half-made copy taken away.
func TestDeepCopyBetweenDisksIsBounded(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	mkChain(t, filepath.Join(src), 200, 200)
	if err := os.Rename(filepath.Join(src, "d"), filepath.Join(src, "ok")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(src, "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	mkChain(t, filepath.Join(src, "deep"), MaxDepth+5, 3)

	old := renameat
	renameat = func(int, string, int, string) error { return unix.EXDEV }
	t.Cleanup(func() { renameat = old })
	lowFDLimit(t, 128)

	sfd, err := unix.Open(src, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(sfd)
	dfd, err := unix.Open(dst, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(dfd)
	ctx := context.Background()

	if err := moveDirAt(ctx, sfd, "ok", dfd, "ok", "", jobs.NewProgress()); err != nil {
		t.Fatalf("copying a 200-deep tree under a 128-descriptor limit: %v", err)
	}
	got, err := readAt(dfd, "ok/"+chainPath(199)+"/f.txt")
	if err != nil || got != "deep" {
		t.Fatalf("the deepest file did not arrive: %q, %v", got, err)
	}
	if _, err := os.Lstat(filepath.Join(src, "ok")); !os.IsNotExist(err) {
		t.Fatalf("source left behind after the move: %v", err)
	}

	err = moveDirAt(ctx, sfd, "deep", dfd, "deep", "", jobs.NewProgress())
	if !errors.Is(err, ErrTooDeep) {
		t.Fatalf("copy past MaxDepth: %v, want ErrTooDeep", err)
	}
	if got, err := readAt(sfd, "deep/"+chainPath(3)+"/f.txt"); err != nil || got != "deep" {
		t.Fatalf("the refused copy touched its source: %q, %v", got, err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "deep")); !os.IsNotExist(err) {
		t.Fatalf("the refused copy left a half-made copy: %v", err)
	}
}

// readAt reads rel below the folder fd one name at a time (rel can be
// longer than PATH_MAX).
func readAt(fd int, rel string) (string, error) {
	parts := strings.Split(rel, "/")
	cur, err := unix.Dup(fd)
	if err != nil {
		return "", err
	}
	for _, p := range parts[:len(parts)-1] {
		n, err := unix.Openat(cur, p, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		unix.Close(cur)
		if err != nil {
			return "", err
		}
		cur = n
	}
	f, err := unix.Openat(cur, parts[len(parts)-1], unix.O_RDONLY|unix.O_NOFOLLOW, 0)
	unix.Close(cur)
	if err != nil {
		return "", err
	}
	b := make([]byte, 64)
	n, _ := unix.Read(f, b)
	unix.Close(f)
	return string(b[:n]), nil
}

// The descriptor stack closes folders to stay within its budget and, going
// back up, reopens them by name — refusing one that was swapped for another
// folder in the meantime rather than carrying on in the wrong place.
func TestDirStackReopensOnlyTheSameFolder(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	mkChain(t, dir, dirBudget*3, 0)
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		t.Fatal(err)
	}
	s, err := newDirStack(fd, MaxDepth)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	for i := 0; i < dirBudget*3; i++ {
		if _, err := s.push("d", nil); err != nil {
			t.Fatal(err)
		}
		if s.open > dirBudget {
			t.Fatalf("%d descriptors open, budget %d", s.open, dirBudget)
		}
	}
	// Level 2 was closed long ago. Swap it for a different folder.
	l2 := filepath.Join(dir, "d", "d")
	if err := os.Rename(l2, l2+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(l2, 0o755); err != nil {
		t.Fatal(err)
	}
	for s.depth() > 2 {
		s.pop()
	}
	if _, err := s.fd(2); !errors.Is(err, errDirChanged) {
		t.Fatalf("reopened a swapped folder: %v", err)
	}
	if _, err := s.fd(1); err != nil {
		t.Fatalf("level 1 is unchanged: %v", err)
	}
}

// A name an app chose can be 255 bytes, almost all extension. Asking for a
// free "name 2" of it used to slice with a negative bound and crash the
// server; it now gives a name that fits, cut on a character boundary.
func TestUniqueNameFitsAnyName(t *testing.T) {
	t.Parallel()

	m := newManager(t)
	names := []string{
		"a." + strings.Repeat("x", 252),
		"." + strings.Repeat("x", 254),
		strings.Repeat("é", 126) + ".t",
		strings.Repeat("写", 80) + "." + strings.Repeat("x", 10),
		"z." + strings.Repeat("写", 84),
	}
	for _, name := range names {
		if len(name) > maxNameBytes {
			t.Fatalf("test name too long: %d", len(name))
		}
		write(t, m, "u/"+name, "x")
	}
	root, err := m.openRoot()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		got := uniqueName(root, "u", name)
		if len(got) > maxNameBytes || !utf8.ValidString(got) || got == name {
			t.Errorf("uniqueName(%d bytes) = %q (%d bytes)", len(name), got, len(got))
		}
		if err := ValidateName("u", got); err != nil {
			t.Errorf("uniqueName gave an invalid name %q: %v", got, err)
		}
	}
	for i := 2; i < 100000; i *= 7 {
		for _, name := range names {
			if got := numberedName(name, "", i); len(got) > maxNameBytes || !utf8.ValidString(got) {
				t.Errorf("numberedName(…, %d) = %d bytes", i, len(got))
			}
		}
	}
}

// Renaming notes.txt to Notes.txt on a case-sensitive disk, where
// Notes.txt is another file, used to replace — destroy — it. It is a
// conflict, through Drive and through WebDAV alike. On a case-insensitive
// disk the two names are one file, and the rename still just changes case.
func TestCaseOnlyRenameNeverDestroysAnotherFile(t *testing.T) {
	t.Parallel()

	m := newManager(t)
	write(t, m, "d/notes.txt", "lower")
	write(t, m, "d/Notes.txt", "UPPER")
	lower, _ := os.ReadFile(filepath.Join(m.Root(), "d", "notes.txt"))
	if string(lower) == "UPPER" {
		// Case-insensitive: one file. The case-only rename must still work.
		if _, err := m.Rename("d/notes.txt", "NOTES.txt"); err != nil {
			t.Fatalf("case-only rename on a case-insensitive disk: %v", err)
		}
		ents, _ := os.ReadDir(filepath.Join(m.Root(), "d"))
		if len(ents) != 1 || ents[0].Name() != "NOTES.txt" {
			t.Fatalf("after the rename: %v", ents)
		}
		t.Skip("case-insensitive disk: the destroy case cannot arise here (run on Linux)")
	}
	m.Walk(context.Background())

	var ce *ConflictError
	if _, err := m.Rename("d/notes.txt", "Notes.txt"); !errors.As(err, &ce) {
		t.Fatalf("Rename onto another file differing in case: %v, want a conflict", err)
	}
	if err := m.renameAny("d/notes.txt", "d/Notes.txt"); !errors.As(err, &ce) {
		t.Fatalf("WebDAV rename onto another file differing in case: %v, want a conflict", err)
	}
	if err := m.renameAny("d/notes.txt", "D/notes.txt"); err == nil {
		t.Fatal("WebDAV rename into a folder that differs in case and is not there succeeded")
	}
	for name, want := range map[string]string{"notes.txt": "lower", "Notes.txt": "UPPER"} {
		got, err := os.ReadFile(filepath.Join(m.Root(), "d", name))
		if err != nil || string(got) != want {
			t.Fatalf("%s = %q, %v; want %q", name, got, err, want)
		}
	}
	// A case-only rename with nothing in the way still works.
	if _, err := m.Rename("d/notes.txt", "NOTES.txt"); err != nil {
		t.Fatalf("plain case-only rename: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(m.Root(), "d", "NOTES.txt")); string(got) != "lower" {
		t.Fatalf("NOTES.txt = %q", got)
	}
}

// A failed backup is pushed to the operator's phone, and the error names
// the path it failed on — a name an app chose. It arrives as DisplayName,
// so a line separator or an escape cannot forge a second line or recolour
// the notification.
func TestBackupFailurePushShowsSafeNames(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("root reads a folder whatever its mode")
	}
	m := newManager(t)
	backups := t.TempDir()
	m.BackupRoot = func() string { return backups }
	evil := "x\u2028Backup OK\x1b[32m\u202Egpj.exe"
	write(t, m, "Apps/evil/"+evil+"/f.txt", "f")
	locked := filepath.Join(m.Root(), "Apps", "evil", evil)
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })

	got := make(chan string, 1)
	m.Notify = func(title, msg string, _ bool) {
		if title == "Drive backup failed" {
			got <- msg
		}
	}
	if _, err := m.StartSnapshot("test"); err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-got:
		for _, bad := range []string{"\u2028", "\x1b", "\u202E"} {
			if strings.Contains(msg, bad) {
				t.Fatalf("push text carries %q raw: %q", bad, msg)
			}
		}
		if !strings.Contains(msg, "⟨U+2028⟩") {
			t.Fatalf("push text does not show the name: %q", msg)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("no failure was pushed")
	}
}

func activeNotice(t *testing.T, m *Manager, kind string) string {
	t.Helper()
	ns, err := m.Store.ActiveNotices()
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range ns {
		if n.Kind == kind {
			return n.Title + "\n" + n.Body
		}
	}
	return ""
}

// Moving an app's folder across disks when an app nested it 300 deep is
// refused before anything is copied, with an error that says what to do,
// and an Inbox issue — not a job failing halfway through a copy.
func TestVolumeMoveOfATooDeepTreeIsRefusedUpFront(t *testing.T) {
	m := newManager(t)
	src := filepath.Join(t.TempDir(), "vol")
	os.Mkdir(src, 0o755)
	mkChain(t, src, MaxDepth+44, 2)
	old := renameat
	renameat = func(int, string, int, string) error { return unix.EXDEV }
	t.Cleanup(func() { renameat = old })
	write(t, m, "Apps/.keep", "")

	err := m.MoveVolume(context.Background(), src, "Apps/vol", jobs.NewProgress())
	if !errors.Is(err, ErrTooDeep) || !strings.Contains(err.Error(), "before anything was copied") {
		t.Fatalf("MoveVolume: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(m.Root(), "Apps", "vol", "d")); !os.IsNotExist(err) {
		t.Fatalf("a refused move copied something: %v", err)
	}
	if got, err := readAt(unixOpen(t, src), chainPath(2)+"/f.txt"); err != nil || got != "deep" {
		t.Fatalf("source changed: %q %v", got, err)
	}
	if n := activeNotice(t, m, NoticeFilesMoveTooDeep); !strings.Contains(n, "d/d/d/…/d") {
		t.Fatalf("no Inbox issue naming the folder: %q", n)
	}
}

func unixOpen(t *testing.T, dir string) int {
	t.Helper()
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unix.Close(fd) })
	return fd
}

// A move between disks copies, verifies, then removes the source. A write
// landing in between used to be lost with the source: fog now refuses
// writes into a folder being moved (and stops the apps using it), and the
// verify compares modification times as well as sizes, so a file rewritten
// in place to the same size during the copy is a mismatch, not a loss.
func TestMoveFreezesWritesAndVerifiesMtimes(t *testing.T) {
	t.Parallel()

	m := newManager(t)
	write(t, m, "Docs/a.txt", "aaaa")
	unfreeze := m.freeze("Docs")
	if _, err := m.Mkdir("Docs/new"); !errors.Is(err, ErrMoving) {
		t.Fatalf("a write into a folder being moved: %v", err)
	}
	if _, err := m.Materialize("Docs/b.txt", strings.NewReader("b"), time.Now(), ""); !errors.Is(err, ErrMoving) {
		t.Fatalf("a peer write into a folder being moved: %v", err)
	}
	if _, err := m.Mkdir("Other"); err != nil {
		t.Fatalf("a write elsewhere: %v", err)
	}
	unfreeze()
	if _, err := m.Mkdir("Docs/new"); err != nil {
		t.Fatalf("after the move: %v", err)
	}

	src, dst := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(src, "f"), []byte("1234"), 0o644)
	if err := copyTree(context.Background(), src, dst, "", jobs.NewProgress()); err != nil {
		t.Fatal(err)
	}
	if err := verifyTree(context.Background(), src, dst, ""); err != nil {
		t.Fatalf("an intact copy: %v", err)
	}
	later := time.Now().Add(time.Hour)
	os.WriteFile(filepath.Join(src, "f"), []byte("5678"), 0o644) // same size, rewritten
	os.Chtimes(filepath.Join(src, "f"), later, later)
	if err := verifyTree(context.Background(), src, dst, ""); err == nil {
		t.Fatal("a same-size rewrite during the copy passed verification")
	}
}

// An app plants a 4 GiB file that is all hole. A copy between disks used
// to write every zero — 16 TiB would fill any destination, or never end on
// a compressing one. The copy now keeps holes as holes, and the free-space
// check counts the blocks files really occupy.
func TestCopyKeepsHolesAndCountsRealBlocks(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	f, err := os.Create(filepath.Join(src, "sparse"))
	if err != nil {
		t.Fatal(err)
	}
	f.WriteAt([]byte("head"), 0)
	if err := f.Truncate(4 << 30); err != nil {
		t.Fatal(err)
	}
	f.WriteAt([]byte("tail"), 4<<30-4)
	f.Close()
	var st unix.Stat_t
	unix.Stat(filepath.Join(src, "sparse"), &st)
	if st.Blocks*512 > 64<<20 {
		t.Skip("this filesystem does not keep holes")
	}
	start := time.Now()
	if err := copyTree(context.Background(), src, dst, "", jobs.NewProgress()); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 30*time.Second {
		t.Fatalf("copying a hole took %s", d)
	}
	var out unix.Stat_t
	unix.Stat(filepath.Join(dst, "sparse"), &out)
	if out.Size != 4<<30 || out.Blocks*512 > 64<<20 {
		t.Fatalf("the copy is %d bytes in %d blocks", out.Size, out.Blocks)
	}
	b := make([]byte, 4)
	g, _ := os.Open(filepath.Join(dst, "sparse"))
	g.ReadAt(b, 4<<30-4)
	g.Close()
	if string(b) != "tail" {
		t.Fatalf("data after the hole: %q", b)
	}
	sfd := unixOpen(t, src)
	var total, alloc int64
	sizeAllocAt(sfd, "", &total, &alloc)
	if total != 4<<30 || alloc > 64<<20 {
		t.Fatalf("logical %d, allocated %d", total, alloc)
	}
	if err := checkFreeAt(unixOpen(t, dst), 1<<62); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("a copy that cannot fit must be refused up front: %v", err)
	}
}

// expiring is a context that reports itself cancelled after n checks.
type expiring struct {
	context.Context
	n int
}

func (e *expiring) Err() error {
	if e.n--; e.n < 0 {
		return context.Canceled
	}
	return nil
}

// A cancelled move stops in the middle of a large file, not at the next
// file: the context is checked every chunk.
func TestCopyStopsMidFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	in, _ := os.Create(filepath.Join(dir, "in"))
	in.Write(make([]byte, 16<<20))
	in.Close()
	r, _ := os.Open(filepath.Join(dir, "in"))
	defer r.Close()
	w, _ := os.Create(filepath.Join(dir, "out"))
	defer w.Close()
	err := copySparse(&expiring{Context: context.Background(), n: 4}, r, w, 16<<20, jobs.NewProgress())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("copy: %v", err)
	}
	if fi, _ := w.Stat(); fi.Size() >= 16<<20 {
		t.Fatalf("the copy ran to the end (%d bytes) after it was cancelled", fi.Size())
	}
}

// A file name at the 255-byte limit — an app's, or about 82 CJK characters
// — used to fail every Drive backup and every copy: they staged it under
// the name plus a suffix, which is too long to create. Staging names are
// now fixed-length and random.
func TestNamesAtTheLimitBackUpAndCopy(t *testing.T) {
	t.Parallel()

	m := newManager(t)
	backups := t.TempDir()
	m.BackupRoot = func() string { return backups }
	long := strings.Repeat("n", 251) + ".txt"
	cjk := strings.Repeat("文", 82) + ".txt"
	if len(long) != 255 || len(cjk) > 255 {
		t.Fatalf("test names: %d %d", len(long), len(cjk))
	}
	write(t, m, "Docs/"+long, "long")
	write(t, m, "Docs/"+cjk, "cjk")

	row, err := m.RunSnapshot(context.Background(), jobs.NewProgress())
	if err != nil {
		t.Fatalf("snapshot with a 255-byte name: %v", err)
	}
	for name, want := range map[string]string{long: "long", cjk: "cjk"} {
		if b, _ := os.ReadFile(filepath.Join(row.Path, "Docs", name)); string(b) != want {
			t.Fatalf("snapshot copy of %d-byte name: %q", len(name), b)
		}
	}
	if _, err := m.Mkdir("Other"); err != nil {
		t.Fatal(err)
	}
	job, err := m.Copy([]string{"Docs/" + long, "Docs/" + cjk}, "Other", ConflictAsk)
	if err != nil {
		t.Fatal(err)
	}
	if j := waitJob(t, m, job.ID); j.State != "done" {
		t.Fatalf("copy job with a 255-byte name: %+v", j)
	}
	if b, _ := os.ReadFile(filepath.Join(m.Root(), "Other", long)); string(b) != "long" {
		t.Fatalf("copied: %q", b)
	}
	dst := t.TempDir()
	if err := copyTree(context.Background(), filepath.Join(m.Root(), "Docs"), dst, "", jobs.NewProgress()); err != nil {
		t.Fatalf("cross-disk copy with a 255-byte name: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, cjk)); string(b) != "cjk" {
		t.Fatalf("cross-disk copy: %q", b)
	}
}

// Where the source cannot report its holes (NFS, FUSE), a file of zeros
// read as data used to be written as data. Chunks of zeros are now left as
// holes in the copy.
func TestCopySkipsZeroChunks(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	p := filepath.Join(dir, "zeros")
	os.WriteFile(p, append(make([]byte, 16<<20), 'x'), 0o644) // written, not sparse
	r, _ := os.Open(p)
	defer r.Close()
	w, _ := os.Create(filepath.Join(dir, "out"))
	defer w.Close()
	if err := copySparse(context.Background(), r, w, 16<<20+1, jobs.NewProgress()); err != nil {
		t.Fatal(err)
	}
	var st unix.Stat_t
	unix.Fstat(int(w.Fd()), &st)
	if st.Size != 16<<20+1 || st.Blocks*512 > 4<<20 {
		t.Fatalf("copy: %d bytes in %d blocks", st.Size, st.Blocks)
	}
	r2, _ := os.ReadFile(filepath.Join(dir, "out"))
	if r2[16<<20] != 'x' || r2[0] != 0 {
		t.Fatal("content changed")
	}
}
