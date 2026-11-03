package files

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"

	"golang.org/x/sys/unix"

	"gitea.lan/matthew/fog/internal/fsx"
	"gitea.lan/matthew/fog/internal/jobs"
)

// Moving a folder by descriptor. A Drive volume move runs while apps may be
// writing next to both ends, so after the no-follow checks nothing is ever
// named by a path again: each end is a parent directory descriptor and a
// single name, reached from the root with O_NOFOLLOW (nofollow.go), and
// every step — the rename, each mkdir, open, link and unlink of a copy —
// is an *at call relative to a descriptor fog holds. A folder swapped for
// a link at either end is refused or replaced, never followed.

// renameat is the rename a move tries first; a var so tests can make two
// folders look like they are on different disks.
var renameat = func(oldfd int, oldname string, newfd int, newname string) error {
	return unix.Renameat(oldfd, oldname, newfd, newname)
}

// moveDirAt moves the folder srcName in srcDir to dstName in dstDir. The
// destination must be missing or an empty folder. On one filesystem it is
// one renameat; otherwise the tree is copied, verified and the source
// removed. skip names a top-level entry of the source that is not copied
// (the reserved directory).
func moveDirAt(ctx context.Context, srcDir int, srcName string, dstDir int, dstName, skip string, p *jobs.Progress) error {
	if err := requireEmptyAt(dstDir, dstName); err != nil {
		return err
	}
	p.Current("moving")
	// An empty destination folder gives way to the rename. Whatever an app
	// might put there in between is not followed: renaming a folder over a
	// link fails (ENOTDIR), over a non-empty folder fails (ENOTEMPTY).
	if err := unix.Unlinkat(dstDir, dstName, unix.AT_REMOVEDIR); err != nil && !errors.Is(err, unix.ENOENT) {
		return &fs.PathError{Op: "unlinkat", Path: dstName, Err: err}
	}
	err := renameat(srcDir, srcName, dstDir, dstName)
	if err == nil {
		return nil
	}
	if !errors.Is(err, unix.EXDEV) {
		return &fs.PathError{Op: "renameat", Path: dstName, Err: err}
	}

	// Different disks: copy into a fresh folder, verify, then remove.
	sfd, err := openat(srcDir, srcName, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return componentErr(srcDir, srcName, srcName, err)
	}
	defer unix.Close(sfd)
	var sst unix.Stat_t
	if err := unix.Fstat(sfd, &sst); err != nil {
		return err
	}
	// Refused whole, before anything is made, when the tree is nested past
	// what a copy walks.
	if err := checkDepthAt(ctx, sfd, MaxDepth, skip); err != nil {
		return err
	}
	var total, alloc int64
	sizeAllocAt(sfd, skip, &total, &alloc)
	if err := checkFreeAt(dstDir, alloc); err != nil {
		return err
	}
	if err := unix.Mkdirat(dstDir, dstName, uint32(sst.Mode)&0o7777); err != nil {
		return &fs.PathError{Op: "mkdirat", Path: dstName, Err: err}
	}
	dfd, err := openat(dstDir, dstName, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return componentErr(dstDir, dstName, dstName, err)
	}
	defer unix.Close(dfd)
	p.Set("copying", 0, total)
	// A copy that fails (a file changed under it, a tree past MaxDepth, a
	// full disk) leaves the source as it was and takes its half-made copy
	// away again: the destination was made fresh, above, for this move.
	if err := copyTreeAt(ctx, dfd, sfd, dfd, skip, "", map[[2]uint64]string{}, p, MaxDepth); err != nil {
		removeAllAt(dstDir, dstName)
		return err
	}
	p.Current("verifying")
	if err := verifyTreeAt(ctx, sfd, dfd, skip, "", MaxDepth); err != nil {
		removeAllAt(dstDir, dstName)
		return err
	}
	p.Current("removing the old copy")
	return removeAllAt(srcDir, srcName)
}

// requireEmptyAt accepts a missing name or an empty folder, and refuses a
// link, a file, or a folder with anything in it.
func requireEmptyAt(dir int, name string) error {
	var st unix.Stat_t
	switch err := fstatat(dir, name, &st); {
	case errors.Is(err, unix.ENOENT):
		return nil
	case err != nil:
		return err
	}
	switch statKind(&st) {
	case unix.S_IFDIR:
	case unix.S_IFLNK:
		return fmt.Errorf("%w: %s", ErrSymlink, name)
	default:
		return ErrDestNotEmpty
	}
	fd, err := openat(dir, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return componentErr(dir, name, name, err)
	}
	d := os.NewFile(uintptr(fd), name)
	defer d.Close()
	if names, _ := d.Readdirnames(1); len(names) > 0 {
		return ErrDestNotEmpty
	}
	return nil
}

func readNames(fd int) ([]string, error) {
	dup, err := unix.Dup(fd)
	if err != nil {
		return nil, err
	}
	unix.CloseOnExec(dup)
	// A dup shares the directory's read offset: start from the top, so a
	// folder listed once (sizeAt) lists again (copyTreeAt, verifyTreeAt).
	if _, err := unix.Seek(dup, 0, 0); err != nil {
		unix.Close(dup)
		return nil, err
	}
	d := os.NewFile(uintptr(dup), "")
	defer d.Close()
	names, err := d.Readdirnames(-1)
	sort.Strings(names)
	return names, err
}

// sizeAllocAt adds up the regular files under the folder fd — their sizes
// for progress, and the blocks they really occupy:
// what a sparse-aware copy will need at the other end. A 16 TiB file of
// holes an app planted counts for nothing there, and nothing here.
func sizeAllocAt(fd int, skip string, total, alloc *int64) {
	st, err := borrowDirStack(fd, MaxDepth)
	if err != nil {
		return
	}
	defer st.close()
	sizeLevel(st, 0, skip, total, alloc)
}

func sizeLevel(s *dirStack, level int, skip string, total, alloc *int64) {
	fd, err := s.fd(level)
	if err != nil {
		return
	}
	names, _ := readNames(fd)
	for _, n := range names {
		if skipped(n, skip) {
			continue
		}
		if fd, err = s.fd(level); err != nil {
			return
		}
		var st unix.Stat_t
		if fstatat(fd, n, &st) != nil {
			continue
		}
		switch statKind(&st) {
		case unix.S_IFREG:
			*total += st.Size
			*alloc += int64(st.Blocks) * 512
		case unix.S_IFDIR:
			if _, err := s.push(n, &st); err == nil {
				sizeLevel(s, level+1, "", total, alloc)
				s.pop()
			}
		}
	}
}

// copyTreeAt copies the contents of the folder src into the folder dst,
// both descriptors. Modes, mtimes, ownership, links (as links) and hard
// links (within the copy) are kept; sockets, devices and FIFOs are not
// files and are left behind. rel is where src is in the tree, for the hard
// link map (keyed by source device and inode, valued by the first copy's
// path in the destination).
//
// Both trees are walked through descriptor stacks (deepwalk.go): a bounded
// number of descriptors held however deep the tree, and nothing copied
// from deeper than MaxDepth below src — such a tree is refused with
// ErrTooDeep.
func copyTreeAt(ctx context.Context, top, src, dst int, skip, rel string, linked map[[2]uint64]string, p *jobs.Progress, limit int) error {
	ss, err := borrowDirStack(src, limit)
	if err != nil {
		return err
	}
	defer ss.close()
	ds, err := borrowDirStack(dst, limit)
	if err != nil {
		return err
	}
	defer ds.close()
	return copyTreeLevel(ctx, top, ss, ds, 0, skip, rel, linked, p)
}

func copyTreeLevel(ctx context.Context, top int, ss, ds *dirStack, level int, skip, rel string, linked map[[2]uint64]string, p *jobs.Progress) error {
	src, err := ss.fd(level)
	if err != nil {
		return fmt.Errorf("%s: %w", rel, err)
	}
	names, err := readNames(src)
	if err != nil {
		return err
	}
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		if skipped(name, skip) {
			continue
		}
		// Either descriptor may have been closed while a subfolder was
		// copied; reopened, it is checked to be the same folder.
		src, err := ss.fd(level)
		if err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		dst, err := ds.fd(level)
		if err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		info := &statInfo{name: name}
		if err := fstatat(src, name, &info.st); err != nil {
			if errors.Is(err, unix.ENOENT) {
				continue
			}
			return err
		}
		child := path.Join(rel, name)
		st := &info.st
		switch statKind(st) {
		case unix.S_IFDIR:
			if _, err := ss.push(name, st); err != nil {
				switch {
				case errors.Is(err, ErrTooDeep):
					return tooDeep(child)
				case errors.Is(err, errDirChanged):
					return fmt.Errorf("%s changed while it was being copied", child)
				}
				return fmt.Errorf("%s: %w", child, err)
			}
			if err := unix.Mkdirat(dst, name, uint32(st.Mode)&0o7777); err != nil {
				ss.pop()
				return &fs.PathError{Op: "mkdirat", Path: child, Err: err}
			}
			if _, err := ds.push(name, nil); err != nil {
				ss.pop()
				return fmt.Errorf("%s: %w", child, err)
			}
			err = copyTreeLevel(ctx, top, ss, ds, level+1, "", child, linked, p)
			if err == nil {
				var cd int
				if cd, err = ds.fd(level + 1); err == nil {
					err = finishFD(cd, info)
				}
			}
			ss.pop()
			ds.pop()
			if err != nil {
				return err
			}
		case unix.S_IFLNK:
			target, err := readlinkat(src, name)
			if err != nil {
				return err
			}
			if err := unix.Symlinkat(target, dst, name); err != nil {
				return &fs.PathError{Op: "symlinkat", Path: child, Err: err}
			}
			if os.Geteuid() == 0 {
				unix.Fchownat(dst, name, int(st.Uid), int(st.Gid), unix.AT_SYMLINK_NOFOLLOW)
			}
		case unix.S_IFREG:
			key := [2]uint64{uint64(st.Dev), uint64(st.Ino)}
			if st.Nlink > 1 {
				if first, ok := linked[key]; ok && linkWithin(top, first, dst, name) == nil {
					p.Add(info.Size(), 1)
					continue
				}
			}
			if err := copyFileBetween(ctx, src, dst, info, child, p); err != nil {
				return err
			}
			if st.Nlink > 1 {
				if _, ok := linked[key]; !ok {
					linked[key] = child
				}
			}
			p.Add(0, 1)
		}
	}
	return nil
}

// linkWithin hard-links the earlier copy first (a path relative to top,
// the copy's own top folder) as name in dst, reaching first's folder from
// top one name at a time without following a link.
func linkWithin(top int, first string, dst int, name string) error {
	parent, base := Split(first)
	pfd, err := unix.Dup(top)
	if err != nil {
		return err
	}
	for _, c := range strings.Split(parent, "/") {
		if c == "" {
			continue
		}
		nfd, err := openat(pfd, c, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		unix.Close(pfd)
		if err != nil {
			return err
		}
		pfd = nfd
	}
	defer unix.Close(pfd)
	return unix.Linkat(pfd, base, dst, name, 0)
}

// copyFileBetween copies the regular file info.name from src to dst: the
// source opened without following a link and checked to be the file that
// was listed, the copy created fresh (O_EXCL, O_NOFOLLOW) under a staging
// name and renamed into place within dst.
func copyFileBetween(ctx context.Context, src, dst int, info *statInfo, child string, p *jobs.Progress) error {
	in, err := openat(src, info.name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_NOCTTY, 0)
	if err != nil {
		return componentErr(src, info.name, child, err)
	}
	var now unix.Stat_t
	if err := unix.Fstat(in, &now); err != nil || statKind(&now) != unix.S_IFREG || now.Ino != info.st.Ino || now.Dev != info.st.Dev {
		unix.Close(in)
		return fmt.Errorf("%s changed while it was being copied", child)
	}
	unix.SetNonblock(in, false)
	inf := os.NewFile(uintptr(in), child)
	defer inf.Close()
	tmp := stagingName(info.name)
	unix.Unlinkat(dst, tmp, 0)
	out, err := openat(dst, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW, uint32(info.st.Mode)&0o7777)
	if err != nil {
		return &fs.PathError{Op: "openat", Path: child, Err: err}
	}
	outf := os.NewFile(uintptr(out), tmp)
	err = copySparse(ctx, inf, outf, info.st.Size, p)
	if err == nil {
		err = outf.Sync()
	}
	if err == nil {
		err = finishFD(out, info)
	}
	if cerr := outf.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = unix.Renameat(dst, tmp, dst, info.name)
	}
	if err != nil {
		unix.Unlinkat(dst, tmp, 0)
		return fmt.Errorf("copy %s: %w", child, err)
	}
	return nil
}

// finishFD stamps mode, owner and mtime from info onto fd.
func finishFD(fd int, info *statInfo) error {
	unix.Fchmod(fd, uint32(info.st.Mode)&0o7777)
	if os.Geteuid() == 0 {
		unix.Fchown(fd, int(info.st.Uid), int(info.st.Gid))
	}
	mt := unix.NsecToTimeval(info.ModTime().UnixNano())
	return unix.Futimes(fd, []unix.Timeval{mt, mt})
}

// verifyTreeAt checks that every regular file under src is in dst with the
// same size: what changed or vanished while the copy ran. Like the copy, it
// walks descriptor stacks and goes no deeper than MaxDepth.
func verifyTreeAt(ctx context.Context, src, dst int, skip, rel string, limit int) error {
	ss, err := borrowDirStack(src, limit)
	if err != nil {
		return err
	}
	defer ss.close()
	ds, err := borrowDirStack(dst, limit)
	if err != nil {
		return err
	}
	defer ds.close()
	return verifyTreeLevel(ctx, ss, ds, 0, skip, rel)
}

func verifyTreeLevel(ctx context.Context, ss, ds *dirStack, level int, skip, rel string) error {
	mismatch := func(p string) error { return fmt.Errorf("verify %s: the copy does not match the source", p) }
	src, err := ss.fd(level)
	if err != nil {
		return mismatch(rel)
	}
	names, err := readNames(src)
	if err != nil {
		return err
	}
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		if skipped(name, skip) {
			continue
		}
		src, err1 := ss.fd(level)
		dst, err2 := ds.fd(level)
		if err1 != nil || err2 != nil {
			return mismatch(rel)
		}
		var s, d unix.Stat_t
		if fstatat(src, name, &s) != nil {
			continue
		}
		child := path.Join(rel, name)
		switch statKind(&s) {
		case unix.S_IFREG:
			// Size and modification time (the copy carries it; a coarse
			// disk keeps it to two seconds): a file rewritten in place while
			// the copy ran, even to the same size, is a mismatch.
			if err := fstatat(dst, name, &d); err != nil || statKind(&d) != unix.S_IFREG || d.Size != s.Size ||
				absDiff(d.Mtim.Sec, s.Mtim.Sec) > 2 {
				return mismatch(child)
			}
		case unix.S_IFDIR:
			if _, err := ss.push(name, &s); err != nil {
				if errors.Is(err, ErrTooDeep) {
					return tooDeep(child)
				}
				return mismatch(child)
			}
			if _, err := ds.push(name, nil); err != nil {
				ss.pop()
				return mismatch(child)
			}
			err := verifyTreeLevel(ctx, ss, ds, level+1, "", child)
			ss.pop()
			ds.pop()
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// removeAllAt removes name in dir and everything under it, by descriptor:
// a link is removed as itself, and a folder swapped for one mid-way is
// unlinked, not descended into. It never goes down more than one level
// (emptyDirAt), so a tree of any depth is removed holding two descriptors.
func removeAllAt(dir int, name string) error {
	if err := unix.Unlinkat(dir, name, 0); err == nil || errors.Is(err, unix.ENOENT) {
		return nil
	}
	fd, err := openat(dir, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
			// Swapped for a link or a file since the unlink: unlink that.
			if err := unix.Unlinkat(dir, name, 0); err == nil || errors.Is(err, unix.ENOENT) {
				return nil
			}
		}
		return &fs.PathError{Op: "openat", Path: name, Err: err}
	}
	err = emptyDirAt(fd, name)
	unix.Close(fd)
	if err != nil {
		return err
	}
	if err := unix.Unlinkat(dir, name, unix.AT_REMOVEDIR); err != nil && !errors.Is(err, unix.ENOENT) {
		return &fs.PathError{Op: "unlinkat", Path: name, Err: err}
	}
	return nil
}

// emptyRounds bounds emptyDirAt's passes over a folder that keeps refilling.
const emptyRounds = 1 << 20

// emptyDirAt removes everything in the folder fd without descending into
// it: each subfolder's entries are renamed up into fd (where the next pass
// meets them), and the emptied subfolder removed. A chain n folders deep
// takes n passes, each touching one entry, and only fd and one subfolder
// are ever open. Everything renamed was already doomed, so a rename that
// lands on a name taken by another doomed entry costs nothing; one that
// cannot (a folder onto a non-empty folder, a file onto a folder) tries
// another name.
func emptyDirAt(fd int, rel string) error {
	for round := 0; round < emptyRounds; round++ {
		names, err := readNames(fd)
		if err != nil {
			return err
		}
		if len(names) == 0 {
			return nil
		}
		progress := false
		for _, n := range names {
			if err := unix.Unlinkat(fd, n, 0); err == nil {
				progress = true
				continue
			} else if errors.Is(err, unix.ENOENT) {
				continue
			}
			c, err := openat(fd, n, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
			if err != nil {
				if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
					continue // gone, or now a link or file: the next pass unlinks it
				}
				return &fs.PathError{Op: "openat", Path: path.Join(rel, n), Err: err}
			}
			inner, err := readNames(c)
			for _, in := range inner {
				if err != nil {
					break
				}
				if uerr := unix.Unlinkat(c, in, 0); uerr == nil || errors.Is(uerr, unix.ENOENT) {
					progress = true
					continue // a file or link: no need to move it first
				}
				var moved bool
				moved, err = hoistAt(c, in, fd)
				if moved {
					progress = true
				} else if err == nil {
					// A different disk mounted inside: empty that one
					// folder in place, one level further down.
					err = removeAllAt(c, in)
				}
			}
			unix.Close(c)
			if err != nil {
				return fmt.Errorf("remove %s: %w", path.Join(rel, n), err)
			}
			switch err := unix.Unlinkat(fd, n, unix.AT_REMOVEDIR); {
			case err == nil:
				progress = true
			case errors.Is(err, unix.ENOENT), errors.Is(err, unix.ENOTEMPTY), errors.Is(err, unix.EEXIST):
				// Refilled meanwhile: the next pass takes it.
			default:
				return &fs.PathError{Op: "unlinkat", Path: path.Join(rel, n), Err: err}
			}
		}
		if !progress {
			return &fs.PathError{Op: "remove", Path: rel, Err: unix.ENOTEMPTY}
		}
	}
	return &fs.PathError{Op: "remove", Path: rel, Err: unix.ENOTEMPTY}
}

// hoistAt renames name in the folder from up into the folder to under a
// fresh name. moved is false, with no error, when the two are on different
// disks.
func hoistAt(from int, name string, to int) (moved bool, err error) {
	for try := 0; try < 8; try++ {
		var b [8]byte
		rand.Read(b[:])
		tmp := ".fog-rm-" + hex.EncodeToString(b[:])
		err = unix.Renameat(from, name, to, tmp)
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, unix.ENOENT):
			return false, nil // gone already
		case errors.Is(err, unix.EXDEV):
			return false, nil
		case errors.Is(err, unix.EEXIST), errors.Is(err, unix.ENOTEMPTY), errors.Is(err, unix.EISDIR), errors.Is(err, unix.ENOTDIR):
			continue
		default:
			return false, &fs.PathError{Op: "renameat", Path: name, Err: err}
		}
	}
	return false, &fs.PathError{Op: "renameat", Path: name, Err: err}
}

func absDiff[T int32 | int64](a, b T) T {
	if a > b {
		return a - b
	}
	return b - a
}

// copyChunk is how much copySparse moves between looks at its context.
const copyChunk = 1 << 20

// copySparse copies size bytes of in to out, only the parts that hold
// data: holes are found with SEEK_DATA/SEEK_HOLE and left as holes (the
// final truncate sets the length), so a sparse file an app planted — 16
// TiB of nothing — costs its real blocks, not a full disk or a copy that
// never ends. ctx is checked every chunk: a cancelled move stops mid-file.
// A filesystem that cannot report holes is read whole, but chunks of zeros
// are still not written.
func copySparse(ctx context.Context, in, out *os.File, size int64, p *jobs.Progress) error {
	ifd, err := fileFD(in)
	if err != nil {
		return err
	}
	buf := make([]byte, copyChunk)
	for off := int64(0); off < size; {
		if err := ctx.Err(); err != nil {
			return err
		}
		data, err := unix.Seek(ifd, off, unix.SEEK_DATA)
		if errors.Is(err, unix.ENXIO) {
			break // the rest is a hole
		}
		hole := size
		if err != nil {
			data = off // no hole support: everything is data
		} else if h, err := unix.Seek(ifd, data, unix.SEEK_HOLE); err == nil {
			hole = min(h, size)
		}
		for pos := data; pos < hole; {
			if err := ctx.Err(); err != nil {
				return err
			}
			n, rerr := in.ReadAt(buf[:min(int64(len(buf)), hole-pos)], pos)
			if n > 0 {
				// A chunk of zeros is left a hole even where the source
				// cannot say it is one (NFS, FUSE): the truncate below
				// gives the file its length, and a hole reads as zeros.
				if len(bytes.Trim(buf[:n], "\x00")) > 0 {
					if _, err := out.WriteAt(buf[:n], pos); err != nil {
						return err
					}
				}
				p.Add(int64(n), 0)
				pos += int64(n)
			}
			if rerr != nil {
				if errors.Is(rerr, io.EOF) {
					hole, size = pos, pos // shrank under us; verify will say
					break
				}
				return rerr
			}
		}
		off = hole
	}
	return out.Truncate(size)
}

// checkFreeAt refuses a copy into the folder dirfd that would not fit:
// need bytes of real blocks, plus a margin.
func checkFreeAt(dirfd int, need int64) error {
	var st unix.Statfs_t
	if err := unix.Fstatfs(dirfd, &st); err != nil {
		return nil // cannot tell; the copy will say if it runs out
	}
	free := int64(st.Bavail) * int64(st.Bsize)
	if margin := int64(256 << 20); free < need+margin {
		return fmt.Errorf("%w: the copy needs %s and the destination has %s free", ErrNoSpace, fsx.HumanBytes(need), fsx.HumanBytes(free))
	}
	return nil
}
