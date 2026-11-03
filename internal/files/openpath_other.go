//go:build !linux

package files

import "golang.org/x/sys/unix"

// openNoDevice is openat with no link followed and no terminal taken. There
// is no O_PATH here; callers check what they opened (fstat) and close
// anything but a regular file or folder. Production runs on Linux, where
// nothing but a checked regular file or folder is ever opened.
func openNoDevice(dirfd int, name string, flags int, perm uint32) (int, error) {
	return openat(dirfd, name, flags|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_NOCTTY, perm)
}
