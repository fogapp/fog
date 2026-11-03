//go:build linux

package files

import (
	"errors"
	"strconv"

	"golang.org/x/sys/unix"
)

// openNoDevice opens name in dirfd only once it is known to be a regular
// file or a folder. A stat and then an open leave a window in which an app
// can swap a device node or FIFO in under the name, and merely opening
// some devices does something. So the name is first opened O_PATH (which
// opens nothing: no device driver runs), that descriptor is fstat'ed, and
// only a regular file or folder is reopened, through /proc/self/fd, with
// the real flags — the very inode that was checked. A name that does not
// exist and is to be created is created fresh (O_EXCL), so it is a regular
// file by construction; one that appears in between is looked at again.
func openNoDevice(dirfd int, name string, flags int, perm uint32) (int, error) {
	flags &^= unix.O_NOFOLLOW
	for try := 0; try < 3; try++ {
		pfd, err := openat(dirfd, name, unix.O_PATH|unix.O_NOFOLLOW, 0)
		if errors.Is(err, unix.ENOENT) && flags&unix.O_CREAT != 0 {
			fd, err := openat(dirfd, name, flags|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_NOCTTY, perm)
			if errors.Is(err, unix.EEXIST) && flags&unix.O_EXCL == 0 {
				continue
			}
			return fd, err
		}
		if err != nil {
			return -1, err
		}
		if flags&unix.O_CREAT != 0 && flags&unix.O_EXCL != 0 {
			unix.Close(pfd)
			return -1, unix.EEXIST
		}
		var st unix.Stat_t
		if err := unix.Fstat(pfd, &st); err != nil {
			unix.Close(pfd)
			return -1, err
		}
		switch statKind(&st) {
		case unix.S_IFREG, unix.S_IFDIR:
		case unix.S_IFLNK:
			unix.Close(pfd)
			return -1, unix.ELOOP
		default:
			unix.Close(pfd)
			return -1, errSpecialFile
		}
		fd, err := openat(unix.AT_FDCWD, "/proc/self/fd/"+strconv.Itoa(pfd), (flags&^(unix.O_CREAT|unix.O_EXCL))|unix.O_NONBLOCK|unix.O_NOCTTY, 0)
		unix.Close(pfd)
		return fd, err
	}
	return -1, unix.EEXIST
}
