package files

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// nfRoot is *os.Root's mutating half done by descriptor. os.Root confines
// every operation to the root but follows links inside it, so a check that
// a path's folders are real folders (checkNoLinks) and the operation after
// it would see different things if an app swapped a folder for a link in
// between — with RENAME_EXCHANGE it can flip one back and forth as fast as
// it likes — and the operator's "move Apps/Evil/sub/x" would move
// Private/x instead. Here each call opens the parent of every path it
// names one component at a time with O_NOFOLLOW (openDirFD) and then acts
// with one *at call relative to that descriptor, so there is nothing
// between a check and a use: the folder it acts in is the folder it
// reached. A path through a link is ErrSymlink.
//
// Every write, rename, removal and mkdir in Drive goes through here (or
// through openNF/mkdirAllNF, which work the same way).
type nfRoot struct{ r *os.Root }

func nf(r *os.Root) nfRoot { return nfRoot{r} }

// at runs fn with the descriptor of rel's parent and rel's last name.
func (n nfRoot) at(op, rel string, fn func(dirfd int, name string) error) error {
	if rel == "" || rel == "." {
		return ErrBadName
	}
	parent, name := Split(rel)
	if _, err := components(name); err != nil {
		return err
	}
	dfd, err := openDirFD(n.r, parent)
	if err != nil {
		return err
	}
	defer unix.Close(dfd)
	if err := fn(dfd, name); err != nil {
		return &fs.PathError{Op: op, Path: rel, Err: err}
	}
	return nil
}

// at2 is at for an operation that names two paths.
func (n nfRoot) at2(op, a, b string, fn func(afd int, an string, bfd int, bn string) error) error {
	return n.at(op, a, func(afd int, an string) error {
		return n.at(op, b, func(bfd int, bn string) error { return fn(afd, an, bfd, bn) })
	})
}

// Rename replaces whatever holds newname (a staging file landing).
func (n nfRoot) Rename(oldname, newname string) error {
	return n.at2("renameat", oldname, newname, unix.Renameat)
}

// RenameNoReplace refuses (ErrExists) when newname is taken, in the same
// call that renames.
func (n nfRoot) RenameNoReplace(oldname, newname string) error {
	if err := n.at2("renameat", oldname, newname, renameatNoReplace); err != nil {
		return mapOSError(err)
	}
	return nil
}

// renameatCheck is renameatNoReplace where the filesystem has no flag for
// it.
func renameatCheck(oldfd int, oldname string, newfd int, newname string) error {
	var st unix.Stat_t
	if err := fstatat(newfd, newname, &st); err == nil {
		return unix.EEXIST
	}
	return unix.Renameat(oldfd, oldname, newfd, newname)
}

// Remove removes one file, link or empty folder.
func (n nfRoot) Remove(rel string) error {
	return n.at("unlinkat", rel, func(dfd int, name string) error {
		err := unix.Unlinkat(dfd, name, 0)
		if errors.Is(err, unix.EISDIR) || errors.Is(err, unix.EPERM) {
			if err2 := unix.Unlinkat(dfd, name, unix.AT_REMOVEDIR); !errors.Is(err2, unix.ENOTDIR) {
				return err2
			}
		}
		return err
	})
}

// RemoveAll removes rel and everything under it; a missing rel is not an
// error.
func (n nfRoot) RemoveAll(rel string) error {
	err := n.at("removeall", rel, removeAllAt)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

func (n nfRoot) Mkdir(rel string, perm fs.FileMode) error {
	return n.at("mkdirat", rel, func(dfd int, name string) error {
		return unix.Mkdirat(dfd, name, uint32(perm.Perm()))
	})
}

func (n nfRoot) MkdirAll(rel string, perm fs.FileMode) error { return mkdirAllNF(n.r, rel, perm) }

func (n nfRoot) OpenFile(rel string, flag int, perm fs.FileMode) (*os.File, error) {
	return openNF(n.r, rel, flag, perm)
}

func (n nfRoot) WriteFile(rel string, data []byte, perm fs.FileMode) error {
	f, err := openNF(n.r, rel, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func (n nfRoot) ReadFile(rel string) ([]byte, error) {
	f, _, err := openFileNF(n.r, rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

func (n nfRoot) Chtimes(rel string, atime, mtime time.Time) error {
	return n.at("utimensat", rel, func(dfd int, name string) error {
		ts := []unix.Timespec{unix.NsecToTimespec(atime.UnixNano()), unix.NsecToTimespec(mtime.UnixNano())}
		return unix.UtimesNanoAt(dfd, name, ts, unix.AT_SYMLINK_NOFOLLOW)
	})
}

func (n nfRoot) Lchown(rel string, uid, gid int) error {
	if rel == "" {
		return nil
	}
	return n.at("fchownat", rel, func(dfd int, name string) error {
		return unix.Fchownat(dfd, name, uid, gid, unix.AT_SYMLINK_NOFOLLOW)
	})
}

func (n nfRoot) Symlink(target, rel string) error {
	return n.at("symlinkat", rel, func(dfd int, name string) error { return unix.Symlinkat(target, dfd, name) })
}

func (n nfRoot) Readlink(rel string) (string, error) {
	var out string
	err := n.at("readlinkat", rel, func(dfd int, name string) error {
		var err error
		out, err = readlinkat(dfd, name)
		return err
	})
	return out, err
}

// DirNames lists the names in the Drive folder rel, reached without
// following a link (the reserved folder included: callers that show names
// filter it themselves).
func (m *Manager) DirNames(rel string) ([]string, error) {
	root, err := m.openRoot()
	if err != nil {
		return nil, err
	}
	return nf(root).ReadDirNames(rel)
}

// ReadDirNames lists a folder by descriptor.
func (n nfRoot) ReadDirNames(rel string) ([]string, error) {
	f, err := openDirNF(n.r, rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.Readdirnames(-1)
}
