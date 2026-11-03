//go:build linux

package apps

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// pinSupported: this system can pin bind sources.
const pinSupported = true

// openNoFollow opens rel under root as an O_PATH descriptor — a folder, or
// at its last component a file (a config file, /etc/localtime's zone) —
// following no symbolic link anywhere in rel: openat2 with
// RESOLVE_NO_SYMLINKS (and RESOLVE_BENEATH, so ".." can't climb out), or,
// on a kernel without openat2, one component at a time with O_NOFOLLOW.
// root itself is opened as named: it is the operator's (Drive's root, or
// "/" for a host path), and nothing an app writes is above it.
func openNoFollow(root, rel string) (int, error) {
	rootFD, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("opening %s: %w", root, err)
	}
	rel = strings.Trim(filepath.Clean("/"+rel), "/")
	if rel == "" {
		return rootFD, nil
	}
	defer unix.Close(rootFD)
	fd, err := unix.Openat2(rootFD, rel, &unix.OpenHow{
		Flags:   unix.O_PATH | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_BENEATH,
	})
	if err == nil {
		return fd, nil
	}
	if !errors.Is(err, unix.ENOSYS) && !errors.Is(err, unix.EPERM) {
		return -1, linkErr(root, rel, err)
	}
	// No openat2 (Linux < 5.6, or a seccomp filter): walk it by hand.
	cur, err := unix.Dup(rootFD)
	if err != nil {
		return -1, err
	}
	comps := strings.Split(rel, "/")
	for i, c := range comps {
		if c == ".." || c == "." || c == "" {
			unix.Close(cur)
			return -1, fmt.Errorf("%s: %q is not a plain path", rel, c)
		}
		flags := unix.O_PATH | unix.O_NOFOLLOW | unix.O_CLOEXEC
		if i < len(comps)-1 {
			flags |= unix.O_DIRECTORY
		}
		next, err := unix.Openat(cur, c, flags, 0)
		unix.Close(cur)
		if err != nil {
			return -1, linkErr(root, rel, err)
		}
		cur = next
	}
	// O_NOFOLLOW on an O_PATH open of a link opens the link itself.
	var st unix.Stat_t
	if err := unix.Fstat(cur, &st); err == nil && st.Mode&unix.S_IFMT == unix.S_IFLNK {
		unix.Close(cur)
		return -1, linkErr(root, rel, unix.ELOOP)
	}
	return cur, nil
}

func linkErr(root, rel string, err error) error {
	if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.EXDEV) {
		return fmt.Errorf("%s under %s passes through a symbolic link (or isn't a folder); fog never mounts through one — give the real path: %w", rel, root, err)
	}
	return fmt.Errorf("%s under %s: %w", rel, root, err)
}

// ensureBindsTmpfs makes dir a tmpfs fog owns, if it isn't one already.
// Pins live on it so that after a reboot, until fogd has pinned them
// again, the paths compose names don't exist, and Docker refuses to start
// the container rather than bind whatever is there.
func ensureBindsTmpfs(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	var fs unix.Statfs_t
	if err := unix.Statfs(dir, &fs); err != nil {
		return err
	}
	var self, parent unix.Stat_t
	if err := unix.Stat(dir, &self); err != nil {
		return err
	}
	if err := unix.Stat(filepath.Dir(dir), &parent); err != nil {
		return err
	}
	if fs.Type == unix.TMPFS_MAGIC && self.Dev != parent.Dev {
		return nil
	}
	return unix.Mount("fog-binds", dir, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "mode=0700,size=4m")
}

// pinBind binds the folder (or file) at rel under root onto target, resolved with no
// symbolic link followed, from the descriptor — so the pin is that folder,
// and a link swapped in for it (or for any folder above it) afterwards
// changes nothing. target must lie in binds, fog's tmpfs. An existing pin
// of the same folder is kept; one of another (the folder was removed and
// made again) is replaced. owner, "uid:gid" or "", re-owns the folder's
// root through the descriptor.
func pinBind(binds, root, rel, target, owner string) error {
	if err := checkPinTarget(binds, target); err != nil {
		return err
	}
	if err := ensureBindsTmpfs(binds); err != nil {
		return fmt.Errorf("preparing %s: %w", binds, err)
	}
	fd, err := openNoFollow(root, rel)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	var src unix.Stat_t
	if err := unix.Fstat(fd, &src); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	isDir := src.Mode&unix.S_IFMT == unix.S_IFDIR
	// The mount point is the pin's kind: a folder for a folder, an empty
	// file for anything else.
	makePoint := func() error {
		if isDir {
			if err := os.Mkdir(target, 0o700); err != nil && !os.IsExist(err) {
				return err
			}
			return nil
		}
		f, err := os.OpenFile(target, os.O_CREATE|os.O_RDONLY, 0o600)
		if err != nil {
			return err
		}
		return f.Close()
	}
	if err := makePoint(); err != nil && !errors.Is(err, unix.ENOTDIR) && !errors.Is(err, unix.EISDIR) {
		return err
	}
	var cur unix.Stat_t
	if err := unix.Lstat(target, &cur); err != nil {
		return err
	}
	switch {
	case cur.Dev == src.Dev && cur.Ino == src.Ino:
		// Already this folder or file.
	default:
		if mounted, _ := isMountPoint(target); mounted {
			if err := unix.Unmount(target, unix.MNT_DETACH); err != nil {
				return fmt.Errorf("replacing the pin at %s: %w", target, err)
			}
		}
		// A pin of the other kind before (a folder that is a file now):
		// its empty mount point goes, and one of the right kind is made.
		if err := unix.Lstat(target, &cur); err == nil && (cur.Mode&unix.S_IFMT == unix.S_IFDIR) != isDir {
			if err := removePoint(target); err != nil {
				return err
			}
			if err := makePoint(); err != nil {
				return err
			}
		}
		proc := "/proc/self/fd/" + strconv.Itoa(fd)
		if err := unix.Mount(proc, target, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
			return fmt.Errorf("pinning %s under %s: %w", rel, root, err)
		}
	}
	if owner == "" {
		return nil
	}
	uid, gid, err := parseOwner(owner)
	if err != nil {
		return err
	}
	if !isDir {
		if err := unix.Fchownat(fd, "", uid, gid, unix.AT_EMPTY_PATH); err != nil {
			return &PinWarning{Msg: fmt.Sprintf("chown %s %s: %v", owner, rel, err)}
		}
		return nil
	}
	dir, err := unix.Open("/proc/self/fd/"+strconv.Itoa(fd), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(dir)
	if err := unix.Fchown(dir, uid, gid); err != nil {
		// A share that refuses chown (root-squashed NFS) keeps the pin:
		// as before pinning, the owner is a warning, not a failed deploy.
		return &PinWarning{Msg: fmt.Sprintf("chown %s %s: %v", owner, rel, err)}
	}
	return nil
}

// isMountPoint reports whether p is the root of a mount: its device differs
// from its parent's, or (a bind of a folder on the same device) it is listed
// as a mount point.
func isMountPoint(p string) (bool, error) {
	var self, parent unix.Stat_t
	if err := unix.Lstat(p, &self); err != nil {
		return false, err
	}
	if err := unix.Lstat(filepath.Dir(p), &parent); err != nil {
		return false, err
	}
	if self.Dev != parent.Dev {
		return true, nil
	}
	raw, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if f := strings.Fields(line); len(f) > 4 && unescapeMountinfo(f[4]) == p {
			return true, nil
		}
	}
	return false, nil
}

func unescapeMountinfo(s string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(s)
}

// unpinBind removes the pin at target — or, when target is an app's folder
// of pins rather than a pin, every pin in it and then the folder. It only
// ever unmounts and removes empty directories: a pin's contents are the
// app's data, and are never walked.
func unpinBind(binds, target string, keep ...string) error {
	if err := checkPinTarget(binds, target); err != nil {
		return err
	}
	if mounted, _ := isMountPoint(target); !mounted {
		entries, _ := os.ReadDir(target)
		kept := false
		for _, e := range entries {
			if slices.Contains(keep, e.Name()) {
				kept = true
				continue
			}
			if err := unpinOne(filepath.Join(target, e.Name())); err != nil {
				return err
			}
		}
		if kept {
			return nil
		}
	}
	return unpinOne(target)
}

func unpinOne(p string) error {
	if mounted, _ := isMountPoint(p); mounted {
		if err := unix.Unmount(p, unix.MNT_DETACH); err != nil && !errors.Is(err, unix.EINVAL) {
			return fmt.Errorf("unpinning %s: %w", p, err)
		}
	}
	return removePoint(p)
}

// removePoint removes an unmounted pin's mount point: an empty tmpfs
// folder (rmdir, never RemoveAll) or an empty file. A point still mounted
// is busy and stays.
func removePoint(p string) error {
	var st unix.Stat_t
	if err := unix.Lstat(p, &st); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return err
	}
	rm := unix.Rmdir
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		rm = unix.Unlink
	}
	if err := rm(p); err != nil && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("removing %s: %w", p, err)
	}
	return nil
}
