package files

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Drive never follows a symbolic link.
//
// Apps write into the folders they mount, so anything in Drive may have
// been put there by a program the operator does not trust, and a link is
// the cheapest way for one to make fog read or write somewhere it did not
// mean to: a share of Public that serves Private/secret.txt because Public
// gained a link to ../Private, a copy that lands in another app's folder, a
// volume that binds a different folder into a container than the one its
// spec names. *os.Root keeps every operation inside the root, but inside it
// follows links freely; that is not enough.
//
// So the rule is: a link in Drive is an entry of kind "other". It is
// listed, never opened, never descended into, never copied as what it
// points at, and a path that runs through one leads nowhere (ErrSymlink).
// Everything that reads bytes or lists a folder — downloads, share links,
// WebDAV, zips, thumbnails, the hasher, the index, federation's blob
// serving, the nightly snapshot — walks the path one component at a time
// with O_NOFOLLOW from the root's descriptor, so there is no window in
// which a component can be swapped for a link between a check and the
// open. Writes, renames, removals and mkdirs work the same way (nfRoot,
// mkdirAllNF, openNF): each opens the parent of every path it names with
// O_NOFOLLOW and acts with one *at call relative to that descriptor, so the
// folder it acts in is the folder it reached — an app flipping a folder and
// a link back and forth cannot make a move of Apps/Evil/sub/x take
// Private/x. guardWrite's checkNoLinks only turns such a path into a clear
// error up front; it is not what keeps the write in place. A copy recreates
// a link as a link, pointing where it pointed; it is never resolved.

// errSpecialFile is a name that turned out to be a device, FIFO or socket
// when it came to be opened: never opened.
var errSpecialFile = fmt.Errorf("%w: not a regular file", ErrBadName)

// ErrSymlink is a path that runs through (or ends at, where a file or
// folder was needed) a symbolic link.
var ErrSymlink = errors.New("a symbolic link is in the way, and Drive does not follow links")

// statInfo is an fs.FileInfo built from an fstatat, shaped like the one
// os.Lstat returns: Sys is a *syscall.Stat_t, which inodeOf and the rest of
// the package read.
type statInfo struct {
	name string
	st   unix.Stat_t
}

// unix.Stat_t and syscall.Stat_t are generated from the same C struct on
// every platform fog builds for; these fail to compile if that ever stops
// being true, which is what makes Sys's conversion sound.
var (
	_ [unsafe.Sizeof(unix.Stat_t{}) - unsafe.Sizeof(syscall.Stat_t{})]byte
	_ [unsafe.Sizeof(syscall.Stat_t{}) - unsafe.Sizeof(unix.Stat_t{})]byte
)

func (s *statInfo) Name() string       { return s.name }
func (s *statInfo) Size() int64        { return s.st.Size }
func (s *statInfo) IsDir() bool        { return s.Mode().IsDir() }
func (s *statInfo) ModTime() time.Time { return time.Unix(s.st.Mtim.Unix()) }
func (s *statInfo) Sys() any           { return (*syscall.Stat_t)(unsafe.Pointer(&s.st)) }

func (s *statInfo) Mode() fs.FileMode {
	m := uint32(s.st.Mode)
	mode := fs.FileMode(m & 0o777)
	switch m & unix.S_IFMT {
	case unix.S_IFBLK:
		mode |= fs.ModeDevice
	case unix.S_IFCHR:
		mode |= fs.ModeDevice | fs.ModeCharDevice
	case unix.S_IFDIR:
		mode |= fs.ModeDir
	case unix.S_IFIFO:
		mode |= fs.ModeNamedPipe
	case unix.S_IFLNK:
		mode |= fs.ModeSymlink
	case unix.S_IFSOCK:
		mode |= fs.ModeSocket
	}
	if m&unix.S_ISGID != 0 {
		mode |= fs.ModeSetgid
	}
	if m&unix.S_ISUID != 0 {
		mode |= fs.ModeSetuid
	}
	if m&unix.S_ISVTX != 0 {
		mode |= fs.ModeSticky
	}
	return mode
}

// components splits a root-relative path into names, refusing any that
// could step anywhere but down.
func components(rel string) ([]string, error) {
	if rel == "" || rel == "." {
		return nil, nil
	}
	if strings.HasPrefix(rel, "/") {
		return nil, ErrEscapes
	}
	parts := strings.Split(rel, "/")
	for _, p := range parts {
		switch p {
		case "..":
			return nil, ErrEscapes
		case "", ".":
			return nil, ErrBadName
		}
	}
	return parts, nil
}

func openat(dirfd int, name string, flags int, perm uint32) (int, error) {
	for {
		fd, err := unix.Openat(dirfd, name, flags|unix.O_CLOEXEC, perm)
		if err != unix.EINTR {
			return fd, err
		}
	}
}

func fstatat(dirfd int, name string, st *unix.Stat_t) error {
	for {
		err := unix.Fstatat(dirfd, name, st, unix.AT_SYMLINK_NOFOLLOW)
		if err != unix.EINTR {
			return err
		}
	}
}

// rootFD returns a new descriptor for the root directory itself.
func rootFD(root *os.Root) (int, error) {
	d, err := root.Open(".")
	if err != nil {
		return -1, mapOSError(err)
	}
	defer d.Close()
	rc, err := d.SyscallConn()
	if err != nil {
		return -1, err
	}
	fd, oerr := -1, error(nil)
	if err := rc.Control(func(s uintptr) { fd, oerr = openat(int(s), ".", unix.O_RDONLY|unix.O_DIRECTORY, 0) }); err != nil {
		return -1, err
	}
	if oerr != nil {
		return -1, oerr
	}
	return fd, nil
}

// componentErr turns a failed step into the package's errors, naming a link
// as ErrSymlink: O_NOFOLLOW reports one as ELOOP on Linux and macOS, EMLINK
// on the BSDs, and ENOTDIR when O_DIRECTORY was also asked for.
func componentErr(dirfd int, name, rel string, err error) error {
	if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.EMLINK) || errors.Is(err, unix.ENOTDIR) {
		var st unix.Stat_t
		if fstatat(dirfd, name, &st) == nil && uint32(st.Mode)&unix.S_IFMT == unix.S_IFLNK {
			return fmt.Errorf("%w: %s", ErrSymlink, rel)
		}
	}
	return mapOSError(&fs.PathError{Op: "openat", Path: rel, Err: err})
}

// openDirFD returns a descriptor for the directory rel, reached from the
// root one name at a time, none of them followed if it is a link. The
// caller closes it.
func openDirFD(root *os.Root, rel string) (int, error) {
	parts, err := components(rel)
	if err != nil {
		return -1, err
	}
	fd, err := rootFD(root)
	if err != nil {
		return -1, err
	}
	for i, p := range parts {
		nfd, err := openat(fd, p, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		if err != nil {
			err = componentErr(fd, p, strings.Join(parts[:i+1], "/"), err)
			unix.Close(fd)
			return -1, err
		}
		unix.Close(fd)
		fd = nfd
	}
	return fd, nil
}

// openDirNF opens the directory rel for listing without following a link
// anywhere on the way.
func openDirNF(root *os.Root, rel string) (*os.File, error) {
	fd, err := openDirFD(root, rel)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), osPath(rel)), nil
}

// lstatNF is root.Lstat that refuses a link among rel's folders: the last
// name may itself be a link (it is described, not followed), the ones
// before it may not.
func lstatNF(root *os.Root, rel string) (fs.FileInfo, error) {
	if rel == "" || rel == "." {
		fd, err := rootFD(root)
		if err != nil {
			return nil, err
		}
		defer unix.Close(fd)
		si := &statInfo{name: "."}
		if err := unix.Fstat(fd, &si.st); err != nil {
			return nil, err
		}
		return si, nil
	}
	parent, name := Split(rel)
	dfd, err := openDirFD(root, parent)
	if err != nil {
		return nil, err
	}
	defer unix.Close(dfd)
	if _, err := components(name); err != nil {
		return nil, err
	}
	si := &statInfo{name: name}
	if err := fstatat(dfd, name, &si.st); err != nil {
		return nil, mapOSError(&fs.PathError{Op: "lstat", Path: rel, Err: err})
	}
	return si, nil
}

// statKind is what a raw st_mode is, in the package's terms.
func statKind(st *unix.Stat_t) uint32 { return uint32(st.Mode) & unix.S_IFMT }

// openFileNF opens the regular file rel for reading. No link is followed at
// any component, and nothing but a regular file is ever opened: a FIFO
// would hang the reader and a device node is not Drive's to touch, so the
// entry is lstat'ed through the same parent descriptor first, and the open
// descriptor is checked to be that very file.
func openFileNF(root *os.Root, rel string) (*os.File, fs.FileInfo, error) {
	if rel == "" {
		return nil, nil, ErrIsDirectory
	}
	parent, name := Split(rel)
	dfd, err := openDirFD(root, parent)
	if err != nil {
		return nil, nil, err
	}
	defer unix.Close(dfd)
	if _, err := components(name); err != nil {
		return nil, nil, err
	}
	var before unix.Stat_t
	if err := fstatat(dfd, name, &before); err != nil {
		return nil, nil, mapOSError(&fs.PathError{Op: "lstat", Path: rel, Err: err})
	}
	switch statKind(&before) {
	case unix.S_IFREG:
	case unix.S_IFDIR:
		return nil, nil, ErrIsDirectory
	case unix.S_IFLNK:
		return nil, nil, fmt.Errorf("%w: %s", ErrSymlink, rel)
	default:
		return nil, nil, fmt.Errorf("%w: not a regular file", ErrBadName)
	}
	fd, err := openNoDevice(dfd, name, unix.O_RDONLY, 0)
	if errors.Is(err, errSpecialFile) {
		return nil, nil, err
	}
	if err != nil {
		return nil, nil, componentErr(dfd, name, rel, err)
	}
	si := &statInfo{name: name}
	if err := unix.Fstat(fd, &si.st); err != nil {
		unix.Close(fd)
		return nil, nil, err
	}
	if statKind(&si.st) != unix.S_IFREG || si.st.Ino != before.Ino || si.st.Dev != before.Dev {
		// Swapped between the lstat and the open.
		unix.Close(fd)
		return nil, nil, fmt.Errorf("%w: %s changed while it was being opened", ErrNotFound, rel)
	}
	if err := unix.SetNonblock(fd, false); err != nil {
		unix.Close(fd)
		return nil, nil, err
	}
	return os.NewFile(uintptr(fd), rel), si, nil
}

// openNF is root.OpenFile for WebDAV and the upload parts: any flags, no
// link followed anywhere, and only a regular file or a directory opened.
func openNF(root *os.Root, rel string, flag int, perm fs.FileMode) (*os.File, error) {
	if rel == "" || rel == "." {
		if flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC|os.O_APPEND) != 0 {
			return nil, ErrIsDirectory
		}
		return openDirNF(root, "")
	}
	parent, name := Split(rel)
	dfd, err := openDirFD(root, parent)
	if err != nil {
		return nil, err
	}
	defer unix.Close(dfd)
	if _, err := components(name); err != nil {
		return nil, err
	}
	var before unix.Stat_t
	switch err := fstatat(dfd, name, &before); {
	case err == nil:
		switch statKind(&before) {
		case unix.S_IFREG, unix.S_IFDIR:
		case unix.S_IFLNK:
			return nil, fmt.Errorf("%w: %s", ErrSymlink, rel)
		default:
			return nil, fmt.Errorf("%w: not a regular file", ErrBadName)
		}
	case errors.Is(err, unix.ENOENT) && flag&os.O_CREATE != 0:
	default:
		return nil, mapOSError(&fs.PathError{Op: "lstat", Path: rel, Err: err})
	}
	fd, err := openNoDevice(dfd, name, flag, uint32(perm.Perm()))
	if errors.Is(err, errSpecialFile) {
		return nil, err
	}
	if err != nil {
		return nil, componentErr(dfd, name, rel, err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd)
		return nil, err
	}
	if k := statKind(&st); k != unix.S_IFREG && k != unix.S_IFDIR {
		unix.Close(fd)
		return nil, fmt.Errorf("%w: not a regular file", ErrBadName)
	}
	if err := unix.SetNonblock(fd, false); err != nil {
		unix.Close(fd)
		return nil, err
	}
	return os.NewFile(uintptr(fd), rel), nil
}

// checkNoLinks reports ErrSymlink when a folder on the way to rel is a
// link. rel itself may be one (a link is renamed or trashed as itself), and
// a path whose folders do not all exist yet passes: there is no link among
// what is missing, and whatever creates it goes through mkdirAllNF.
func checkNoLinks(root *os.Root, rel string) error {
	parent, _ := Split(rel)
	fd, err := openDirFD(root, parent)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	unix.Close(fd)
	return nil
}

// mkdirAllNF is root.MkdirAll that creates (or walks through) each folder
// itself, refusing a link where a folder should be instead of following it.
func mkdirAllNF(root *os.Root, rel string, perm fs.FileMode) error {
	parts, err := components(rel)
	if err != nil {
		return err
	}
	fd, err := rootFD(root)
	if err != nil {
		return err
	}
	defer func() { unix.Close(fd) }()
	for i, p := range parts {
		nfd, err := openat(fd, p, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		if errors.Is(err, unix.ENOENT) {
			if merr := unix.Mkdirat(fd, p, uint32(perm.Perm())); merr != nil && !errors.Is(merr, unix.EEXIST) {
				return mapOSError(&fs.PathError{Op: "mkdirat", Path: strings.Join(parts[:i+1], "/"), Err: merr})
			}
			nfd, err = openat(fd, p, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		}
		if err != nil {
			return componentErr(fd, p, strings.Join(parts[:i+1], "/"), err)
		}
		unix.Close(fd)
		fd = nfd
	}
	return nil
}

// existsNF says whether rel names an entry, reached without following a
// link.
func existsNF(root *os.Root, rel string) bool {
	_, err := lstatNF(root, rel)
	return err == nil
}
