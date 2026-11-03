package backup

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"gitea.lan/matthew/fog/internal/apps"
)

// App volumes are walked, never trusted as paths.
//
// fogd runs as root, and everything below an app volume's root — and, for an
// app with nested volumes, the volume root itself and directories above it,
// rewritable through an outer mount — may have been put there by the app. A
// path through such a tree can be a symlink to anywhere: another app's
// volume, /etc. os.MkdirAll, os.OpenRoot, os.Stat and os.Remove all follow
// links in the path, so fog never hands them a volume path. Instead a volume
// is addressed as a trusted anchor (fog's data directory for a managed
// volume, Drive's root for one in the app's Drive folder — operator choices,
// opened as they are) plus components below it, and opened one component at
// a time with O_NOFOLLOW from the anchor's descriptor: a link anywhere below
// the anchor is refused, there is no window to swap one in between a check
// and the open, and the final entry must be a real directory. The same
// pattern as Drive's own no-follow walk (internal/files/nofollow.go).

// volumeLoc is a directory addressed from a trusted anchor.
type volumeLoc struct {
	anchor string   // trusted: fog's data dir or Drive's root
	rel    []string // components below the anchor; none may be a link
}

func (l volumeLoc) path() string {
	return filepath.Join(append([]string{l.anchor}, l.rel...)...)
}

func (l volumeLoc) join(more ...string) volumeLoc {
	return volumeLoc{anchor: l.anchor, rel: append(append([]string(nil), l.rel...), more...)}
}

// errLinkInVolume is a volume path that runs through, or ends at, something
// other than a real directory — a planted symlink, most likely.
var errLinkInVolume = errors.New("is a symbolic link or not a directory")

// volumeLoc locates one of an app's own volumes from its trusted anchor —
// the same volumes volumePath answers for, ok=false for one fog does not own.
func (m *Manager) volumeLoc(appName string, v apps.Volume) (volumeLoc, bool) {
	switch {
	case v.Name == "" || m.Mat == nil:
		return volumeLoc{}, false
	case v.Managed():
		rel, ok := relComponents(filepath.ToSlash(filepath.Join("data", appName, v.Name)))
		return volumeLoc{anchor: m.Mat.DataDir, rel: rel}, ok
	case v.IsAppData(appName):
		if m.Mat.FilesRoot == nil {
			return volumeLoc{}, false
		}
		root := m.Mat.FilesRoot()
		if root == "" {
			return volumeLoc{}, false
		}
		rel, ok := relComponents(v.Files)
		return volumeLoc{anchor: root, rel: rel}, ok
	}
	return volumeLoc{}, false
}

// relComponents splits a slash path into components, refusing a traversal.
func relComponents(p string) ([]string, bool) {
	var out []string
	for _, c := range strings.Split(p, "/") {
		switch c {
		case "", ".":
			continue
		case "..":
			return nil, false
		}
		out = append(out, c)
	}
	return out, len(out) > 0
}

const dirFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC

// openDirNoFollow opens loc's directory without following a link at any
// component below the anchor. With create, missing components are made
// (0755) — each then opened no-follow like the rest. The caller closes the
// returned descriptor.
func openDirNoFollow(loc volumeLoc, create bool) (int, error) {
	fd, err := unix.Open(loc.anchor, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, &os.PathError{Op: "open", Path: loc.anchor, Err: err}
	}
	for i, c := range loc.rel {
		nfd, err := unix.Openat(fd, c, dirFlags, 0)
		if errors.Is(err, unix.ENOENT) && create {
			if merr := unix.Mkdirat(fd, c, 0o755); merr != nil && !errors.Is(merr, unix.EEXIST) {
				unix.Close(fd)
				return -1, &os.PathError{Op: "mkdir", Path: loc.pathTo(i), Err: merr}
			}
			nfd, err = unix.Openat(fd, c, dirFlags, 0)
		}
		unix.Close(fd)
		if err != nil {
			p := loc.pathTo(i)
			if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.EMLINK) {
				return -1, fmt.Errorf("%s %w — refusing to follow it", p, errLinkInVolume)
			}
			return -1, &os.PathError{Op: "open", Path: p, Err: err}
		}
		fd = nfd
	}
	return fd, nil
}

// pathTo is the path of the first i+1 components, for messages.
func (l volumeLoc) pathTo(i int) string {
	return filepath.Join(append([]string{l.anchor}, l.rel[:i+1]...)...)
}

// checkVolumeDir proves loc is a real directory reached without a link.
// exists is false (and err nil) when it is simply absent.
func checkVolumeDir(loc volumeLoc) (exists bool, err error) {
	fd, err := openDirNoFollow(loc, false)
	if err != nil {
		var pe *os.PathError
		if errors.As(err, &pe) && errors.Is(pe.Err, unix.ENOENT) {
			return false, nil
		}
		return false, err
	}
	unix.Close(fd)
	return true, nil
}

// regularAt reports whether name in the directory dirfd is a regular file,
// looked at without following a link.
func regularAt(dirfd int, name string) bool {
	var st unix.Stat_t
	if err := unix.Fstatat(dirfd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return false
	}
	return st.Mode&unix.S_IFMT == unix.S_IFREG
}

// createAt replaces name in dirfd with a fresh regular file — any existing
// entry, a planted symlink included, is unlinked first and the file is made
// O_EXCL|O_NOFOLLOW — and returns it open for writing.
func createAt(dirfd int, name string, perm uint32) (*os.File, error) {
	if err := unix.Unlinkat(dirfd, name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return nil, &os.PathError{Op: "unlink", Path: name, Err: err}
	}
	fd, err := unix.Openat(dirfd, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, perm)
	if err != nil {
		return nil, &os.PathError{Op: "create", Path: name, Err: err}
	}
	return os.NewFile(uintptr(fd), name), nil
}

// writeAt writes r to a fresh name in dirfd and fsyncs it.
func writeAt(dirfd int, name string, r io.Reader) error {
	out, err := createAt(dirfd, name, 0o644)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, r)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}
