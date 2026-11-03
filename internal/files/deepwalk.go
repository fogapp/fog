package files

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// Deep trees.
//
// An app can nest folders as deep as it likes, and every walk fog makes of
// Drive — the index, a zip, the nightly snapshot, a copy between disks, a
// root move — has to survive whatever it finds. Two things go wrong with a
// naive walk on a tree thousands of levels deep: reopening each folder from
// the root costs as many opens as the folder is deep, so the walk grows with
// depth squared; and holding a descriptor per level runs the process out of
// them (EMFILE), which then fails everything else fog is doing.
//
// So every walk carries descriptors down with it (a dirStack: each folder is
// opened from its parent's descriptor, never from the root), holds at most
// dirBudget of them at once (the shallowest are closed and reopened by name
// from the nearest one still open, checked to be the same folder), and stops
// at MaxDepth levels below the Drive root with ErrTooDeep. Removal is the
// exception: deleting a tree never needs to go deep (removeAllAt hoists
// contents up instead), so a tree too deep to walk can still be thrown away.

// MaxDepth is how many folders deep below the Drive root a walk goes.
const MaxDepth = 256

// dirBudget is how many folder descriptors one walk holds at once.
const dirBudget = 16

type tooDeepError struct{}

func (tooDeepError) Error() string {
	return fmt.Sprintf("folders here are nested more than %d levels deep, and Drive goes no further", MaxDepth)
}

// Is makes a too-deep tree a bad request wherever ErrBadName is one.
func (tooDeepError) Is(target error) bool { return target == ErrBadName }

// ErrTooDeep is a walk that met a folder more than MaxDepth levels below
// the Drive root.
var ErrTooDeep error = tooDeepError{}

// errDirChanged is a folder that, reopened by name, is no longer the one
// the walk was in: renamed or replaced while the walk ran.
var errDirChanged = fmt.Errorf("%w: a folder changed while it was being read", ErrNotFound)

// depthOf is how many folders deep rel is below the root.
func depthOf(rel string) int {
	if rel == "" || rel == "." {
		return 0
	}
	return strings.Count(rel, "/") + 1
}

type stackLevel struct {
	name     string // in the level above; unused for the base
	dev, ino uint64
	fd       int // -1 while closed
}

// dirStack is the chain of folders from a walk's base down to where the
// walk is now, holding descriptors for at most dirBudget of them.
type dirStack struct {
	levels []stackLevel
	open   int // levels with fd >= 0, not counting the base
	limit  int // how many levels below the base push allows
}

// newDirStack starts a stack at the folder fd, which it owns and closes.
// limit is how many levels below fd the walk may go.
func newDirStack(fd, limit int) (*dirStack, error) {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd)
		return nil, err
	}
	return &dirStack{
		levels: []stackLevel{{dev: uint64(st.Dev), ino: uint64(st.Ino), fd: fd}},
		limit:  limit,
	}, nil
}

// borrowDirStack is newDirStack on a copy of fd; the caller keeps fd.
func borrowDirStack(fd, limit int) (*dirStack, error) {
	d, err := unix.Dup(fd)
	if err != nil {
		return nil, err
	}
	unix.CloseOnExec(d)
	return newDirStack(d, limit)
}

// depth is how many levels are below the base.
func (s *dirStack) depth() int { return len(s.levels) - 1 }

// fd is the descriptor for level i (0 is the base), reopened if it was
// closed to stay within the budget.
func (s *dirStack) fd(i int) (int, error) {
	if s.levels[i].fd >= 0 {
		return s.levels[i].fd, nil
	}
	j := i - 1
	for s.levels[j].fd < 0 {
		j--
	}
	for k := j + 1; k <= i; k++ {
		l := &s.levels[k]
		nfd, err := openat(s.levels[k-1].fd, l.name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		if err != nil {
			if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
				return -1, errDirChanged
			}
			return -1, err
		}
		var st unix.Stat_t
		if err := unix.Fstat(nfd, &st); err != nil || uint64(st.Dev) != l.dev || uint64(st.Ino) != l.ino {
			unix.Close(nfd)
			return -1, errDirChanged
		}
		l.fd = nfd
		s.open++
		s.evict(k)
	}
	return s.levels[i].fd, nil
}

// evict closes the shallowest open levels until the budget holds, never
// the base and never keep (the level about to be used).
func (s *dirStack) evict(keep int) {
	for i := 1; s.open > dirBudget && i < len(s.levels); i++ {
		if i != keep && s.levels[i].fd >= 0 {
			unix.Close(s.levels[i].fd)
			s.levels[i].fd = -1
			s.open--
		}
	}
}

// push opens name in the deepest level as the next level down, without
// following a link. want, when set, is what a listing said name was; a
// different folder by the time it is opened is errDirChanged. Past the
// limit it is ErrTooDeep and nothing is opened.
func (s *dirStack) push(name string, want *unix.Stat_t) (int, error) {
	if s.depth() >= s.limit {
		return -1, ErrTooDeep
	}
	pfd, err := s.fd(s.depth())
	if err != nil {
		return -1, err
	}
	nfd, err := openat(pfd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, componentErr(pfd, name, name, err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(nfd, &st); err != nil {
		unix.Close(nfd)
		return -1, err
	}
	if want != nil && (st.Dev != want.Dev || st.Ino != want.Ino) {
		unix.Close(nfd)
		return -1, errDirChanged
	}
	s.levels = append(s.levels, stackLevel{name: name, dev: uint64(st.Dev), ino: uint64(st.Ino), fd: nfd})
	s.open++
	s.evict(len(s.levels) - 1)
	return nfd, nil
}

// pop closes the deepest level.
func (s *dirStack) pop() {
	l := s.levels[len(s.levels)-1]
	if l.fd >= 0 {
		unix.Close(l.fd)
		s.open--
	}
	s.levels = s.levels[:len(s.levels)-1]
}

// close closes every level, the base included.
func (s *dirStack) close() {
	for len(s.levels) > 1 {
		s.pop()
	}
	if s.levels[0].fd >= 0 {
		unix.Close(s.levels[0].fd)
		s.levels[0].fd = -1
	}
}

// rawStat is the stat a listing's FileInfo carries, for push to check.
func rawStat(fi fs.FileInfo) *unix.Stat_t {
	switch st := fi.Sys().(type) {
	case *syscall.Stat_t:
		return &unix.Stat_t{Dev: st.Dev, Ino: st.Ino}
	}
	return nil
}

// readDirAt lists the folder fd (rel, for reservedAt) with lstat metadata
// for each entry, hiding the reserved directories. fd stays open and is
// read from the top whatever was read of it before.
func readDirAt(fd int, rel string) ([]fs.FileInfo, error) {
	ents, err := readEntriesAt(fd)
	if err != nil {
		return nil, err
	}
	out := ents[:0]
	for _, info := range ents {
		if reservedAt(rel, info.Name()) {
			continue
		}
		out = append(out, info)
	}
	return out, nil
}

// readEntriesAt lists every entry of the folder fd, each lstat'ed through
// fd itself (statNames). The names come from getdents on the descriptor;
// nothing here ever goes back to a path — os.File.Readdir on a file made
// with os.NewFile stats "name/entry" against the working directory on some
// Go versions (1.26) and through the descriptor on others (1.27), and a
// listing that silently came back empty would let the index walk drop every
// row and a snapshot record nothing.
func readEntriesAt(fd int) ([]fs.FileInfo, error) {
	names, err := readNames(fd)
	if err != nil {
		return nil, mapOSError(err)
	}
	return statNames(fd, names), nil
}

// statNames lstats each name in the folder dirfd (fstatat with
// AT_SYMLINK_NOFOLLOW). An entry that vanished in between is left out.
func statNames(dirfd int, names []string) []fs.FileInfo {
	out := make([]fs.FileInfo, 0, len(names))
	for _, n := range names {
		si := &statInfo{name: n}
		if fstatat(dirfd, n, &si.st) != nil {
			continue
		}
		out = append(out, si)
	}
	return out
}

// fileFD is the descriptor under an *os.File, for *at calls through it.
func fileFD(f *os.File) (int, error) {
	rc, err := f.SyscallConn()
	if err != nil {
		return -1, err
	}
	fd := -1
	if err := rc.Control(func(s uintptr) { fd = int(s) }); err != nil {
		return -1, err
	}
	return fd, nil
}

// deepError is ErrTooDeep naming where the walk stopped.
type deepError struct{ path string }

func (e *deepError) Error() string { return shortPath(e.path) + ": " + ErrTooDeep.Error() }
func (e *deepError) Unwrap() error { return ErrTooDeep }

// tooDeep names where a walk stopped.
func tooDeep(rel string) error { return &deepError{path: rel} }

// TooDeepPath is the folder a too-deep error names, or "".
func TooDeepPath(err error) string {
	var e *deepError
	if errors.As(err, &e) {
		return e.path
	}
	return ""
}

// shortPath is a path hundreds of levels deep made readable: its first
// three folders, an ellipsis, and its last, shown with DisplayName.
func shortPath(rel string) string {
	parts := strings.Split(rel, "/")
	if len(parts) <= 5 {
		return DisplayName(rel)
	}
	return DisplayName(strings.Join(parts[:3], "/")+"/…/"+parts[len(parts)-1]) +
		fmt.Sprintf(" (%d levels down)", len(parts))
}

// moveDepth is how deep a move between disks (root, placement, backup
// directory) walks below the folder it moves: MaxDepth with room for the
// backup directory's own levels above a snapshot of Drive.
const moveDepth = MaxDepth + 8

// checkDepthAt walks the folders under fd, holding a bounded number of
// descriptors, and refuses a tree with a folder more than limit levels
// down — before a copy starts, so a move is refused whole rather than
// failing partway through.
func checkDepthAt(ctx context.Context, fd, limit int, skip string) error {
	s, err := borrowDirStack(fd, limit)
	if err != nil {
		return err
	}
	defer s.close()
	var walk func(level int, rel string) error
	walk = func(level int, rel string) error {
		dfd, err := s.fd(level)
		if err != nil {
			return nil // changed under the walk; the copy says so
		}
		names, err := readNames(dfd)
		if err != nil {
			return nil
		}
		for _, n := range names {
			if err := ctx.Err(); err != nil {
				return err
			}
			if level == 0 && skipped(n, skip) {
				continue
			}
			if dfd, err = s.fd(level); err != nil {
				return nil
			}
			var st unix.Stat_t
			if fstatat(dfd, n, &st) != nil || statKind(&st) != unix.S_IFDIR {
				continue
			}
			child := Join(rel, n)
			if _, err := s.push(n, &st); err != nil {
				if errors.Is(err, ErrTooDeep) {
					return tooDeep(child)
				}
				continue
			}
			err := walk(level+1, child)
			s.pop()
			if err != nil {
				return err
			}
		}
		return nil
	}
	return walk(0, "")
}

// checkDepth is checkDepthAt on the directory at the absolute path dir.
func checkDepth(ctx context.Context, dir string, limit int, skip string) error {
	fd, err := openat(unix.AT_FDCWD, dir, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return &fs.PathError{Op: "open", Path: dir, Err: err}
	}
	defer unix.Close(fd)
	return checkDepthAt(ctx, fd, limit, skip)
}

// NoticeFilesMoveTooDeep: a move between disks was refused because the
// tree holds folders nested past what a copy walks.
const NoticeFilesMoveTooDeep = "files-move-toodeep"

// moveRefused turns a move that met a too-deep tree into an error that
// says what to do, and raises it in the Inbox: a move runs as a job the
// operator may not be watching, and until the folder is dealt with every
// retry fails the same way. Any other error passes through unchanged.
func (m *Manager) moveRefused(what string, err error) error {
	p := TooDeepPath(err)
	if p == "" {
		return err
	}
	msg := fmt.Sprintf("%s was refused before anything was copied: inside it, %s is nested more than %d levels deep, "+
		"and Drive copies no deeper. Move what matters in that folder higher up, or delete it, then try again.", what, shortPath(p), MaxDepth)
	if m.Store != nil {
		m.Store.CreateNotice(NoticeFilesMoveTooDeep, "", what+" was refused: folders nested too deeply", msg)
	}
	return fmt.Errorf("%w: %s", ErrTooDeep, msg)
}
