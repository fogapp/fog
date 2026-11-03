package files

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"gitea.lan/matthew/fog/internal/jobs"
)

// Changing the root is a setting write only when the current root holds
// nothing. With content, it is a job: move the tree (a rename when both
// sit on one filesystem, a copy-verify-delete otherwise) or adopt an
// existing tree at the new location and leave the old one alone. The
// setting flips only once the job succeeds, so a failed copy leaves fog
// pointing at the tree it can still see.

var ErrRootHasContent = errors.New("the current Drive root has content; move or adopt through Drive")

// PrepareRootChange is the check the settings handler runs before storing
// files_root: a valid, usable destination and an empty current root.
func (m *Manager) PrepareRootChange(newRoot string) error {
	if newRoot == "" {
		newRoot = m.DefaultRoot()
	}
	newRoot = filepath.Clean(newRoot)
	cur := m.Root()
	if newRoot == cur {
		return nil
	}
	if err := m.Validate(newRoot); err != nil {
		return err
	}
	has, err := hasContent(cur)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if has {
		return ErrRootHasContent
	}
	return nil
}

// hasContent reports whether dir holds anything besides fog's reserved
// directory.
func hasContent(dir string) (bool, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	for _, e := range ents {
		if !strings.EqualFold(e.Name(), ReservedDir) {
			return true, nil
		}
	}
	return false, nil
}

// MoveRoot starts the job. mode is "move" or "adopt".
func (m *Manager) MoveRoot(to, mode string) (jobs.Job, error) {
	if to == "" {
		to = m.DefaultRoot()
	}
	to = filepath.Clean(to)
	cur := m.Root()
	if to == cur {
		return jobs.Job{}, errors.New("that is already the files root")
	}
	if mode != "move" && mode != "adopt" {
		return jobs.Job{}, errors.New("mode must be move or adopt")
	}
	if err := m.Validate(to); err != nil {
		return jobs.Job{}, err
	}
	if mode == "move" {
		if has, _ := hasContent(to); has {
			return jobs.Job{}, fmt.Errorf("%s is not empty; choose Adopt to use what is there, or pick an empty folder", to)
		}
	}
	label := "Adopt files at " + to
	if mode == "move" {
		label = "Move files to " + to
	}
	job, ok := m.Jobs.StartExclusive("root-move", label, func(ctx context.Context, p *jobs.Progress) error {
		// Apps and Content placed on other disks are binds into the old
		// root. They come down first — a copy would otherwise carry their
		// bytes into the new root and the delete after it remove them —
		// and go back up over the new root once it is in place.
		if err := m.unbindAll(ctx, cur); err != nil {
			m.EnsurePlacements(context.WithoutCancel(ctx))
			return err
		}
		if mode == "move" {
			if under := mountsUnder(cur); len(under) > 0 {
				m.EnsurePlacements(context.WithoutCancel(ctx))
				return fmt.Errorf("%s has another disk mounted inside it (%s); unmount it first", cur, under[0])
			}
			// Nothing writes into Drive while it is copied: fog refuses
			// writes, and the apps with folders in it are stopped. Their
			// writes during the copy would otherwise be lost with the
			// source.
			defer m.freeze("")()
			if m.StopAppsUnder != nil {
				p.Current("stopping the apps that use Drive")
				stopped, serr := m.StopAppsUnder(ctx, cur)
				if len(stopped) > 0 && m.StartApps != nil {
					defer func() {
						p.Current("starting the apps again")
						m.StartApps(context.WithoutCancel(ctx), stopped)
					}()
				}
				if serr != nil {
					m.EnsurePlacements(context.WithoutCancel(ctx))
					return serr
				}
			}
			m.releaseRoot()
			if err := moveTree(ctx, cur, to, ReservedDir, p); err != nil {
				m.EnsurePlacements(context.WithoutCancel(ctx))
				return m.moveRefused("Moving Drive", err)
			}
		}
		if err := m.Store.SetSetting(SettingRoot, to); err != nil {
			return err
		}
		m.Store.Audit("system", "files.root", to, mode)
		m.RootChanged()
		if mode == "move" {
			// A copy across disks gives everything a new inode.
			m.rebindShares("")
		}
		// Apps whose volumes name a Files folder have the old root baked
		// into their compose files; they follow the tree to its new home.
		if m.OnRootMoved != nil {
			m.OnRootMoved()
		}
		p.Current("indexing")
		return m.Walk(ctx)
	}, "placement", backupMoveJob)
	if !ok {
		return jobs.Job{}, errors.New("a Drive move is already running")
	}
	return job, nil
}

// releaseRoot closes the cached root before the Drive root itself moves:
// renaming a directory out from under an open descriptor works on Linux,
// but the cached *os.Root would then name the wrong place. Only a root
// move needs this; a volume move relocates a folder inside (or into) the
// tree, and closing the shared root then would fail every List, Batch and
// hash running alongside it.
func (m *Manager) releaseRoot() {
	m.rmu.Lock()
	if m.root != nil {
		m.root.Close()
		m.root = nil
		m.rootPath = ""
	}
	m.rmu.Unlock()
}

// deviceOf is the filesystem a path is on; a var so tests can put two
// directories on different disks.
var deviceOf = func(path string) (uint64, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Dev), nil
}

// moveTree relocates everything under from to to, which exists and is
// empty (Validate created it). Same filesystem: one rename. Otherwise a
// copy that verifies every file's size before the source is removed.
// skip names a top-level directory the copy leaves out (the root's
// reserved directory), or is empty.
func moveTree(ctx context.Context, from, to, skip string, p *jobs.Progress) error {
	renamed, err := relocateTree(ctx, from, to, skip, p)
	if err != nil || renamed {
		return err
	}
	p.Current("removing the old copy")
	return os.RemoveAll(from)
}

// relocateTree is moveTree up to the delete: renamed says the tree was
// renamed into place; otherwise it was copied and verified, and the old
// copy is still at from for the caller to remove once nothing names it.
func relocateTree(ctx context.Context, from, to, skip string, p *jobs.Progress) (renamed bool, err error) {
	fromDev, err1 := deviceOf(from)
	toDev, err2 := deviceOf(to)
	if err1 == nil && err2 == nil && fromDev == toDev {
		p.Current("moving")
		if err := os.RemoveAll(to); err != nil {
			return false, err
		}
		if err := os.Rename(from, to); err == nil {
			return true, nil
		}
		// Fall through to the copy on a rename failure (a bind mount can
		// report one device and still refuse).
		if err := os.MkdirAll(to, 0o755); err != nil {
			return false, err
		}
	}

	// A tree an app nested past what a walk goes is refused before a byte
	// is copied, naming the folder, not halfway through; so is one the
	// destination has no room for, counted in the blocks its files really
	// occupy (a sparse file is copied sparse).
	if err := checkDepth(ctx, from, moveDepth, skip); err != nil {
		return false, err
	}
	sfd, dfd, err := openPair(from, to)
	if err != nil {
		return false, err
	}
	var total, alloc int64
	sizeAllocAt(sfd, skip, &total, &alloc)
	ferr := checkFreeAt(dfd, alloc)
	unix.Close(sfd)
	unix.Close(dfd)
	if ferr != nil {
		return false, ferr
	}
	p.Set("copying", 0, total)
	// A copy that fails or is cancelled (the job's Cancel; checked every
	// megabyte) takes its half-made copy away again: the destination was
	// empty when the move began, and the source is untouched.
	if err := copyTree(ctx, from, to, skip, p); err != nil {
		emptyDir(to)
		return false, err
	}
	p.Current("verifying")
	if err := verifyTree(ctx, from, to, skip); err != nil {
		emptyDir(to)
		return false, err
	}
	return false, nil
}

// emptyDir removes everything inside the directory at path, by descriptor.
func emptyDir(path string) {
	fd, err := openat(unix.AT_FDCWD, path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return
	}
	defer unix.Close(fd)
	emptyDirAt(fd, path)
}

// skipped says name is the entry a move leaves behind (the reserved
// directory), in any case: on a case-insensitive disk ".FOG" is it.
func skipped(name, skip string) bool { return skip != "" && strings.EqualFold(name, skip) }

// copyTree copies from into to, preserving modes, mtimes, ownership,
// symlinks and hard links. Files are written to a temporary name and
// renamed so a crash leaves no half file under the final name. Hard links
// matter for the backup directory, whose Drive snapshots are each a full
// tree of links to the one before: copied file by file, every night would
// cost the whole Drive again.
//
// Both sides go by descriptor, end to end (copyTreeAt): apps may be
// writing into the source while it is copied, and its names are theirs. A
// link is copied as a link, never followed; and on the destination side
// nothing is ever named by a path, so a link "A" the copy just made cannot
// redirect a folder "a" that follows it on a case-insensitive disk — the
// mkdirat meets the name taken and the copy stops, rather than writing
// through the link outside the destination as root.
func copyTree(ctx context.Context, from, to, skip string, p *jobs.Progress) error {
	sfd, dfd, err := openPair(from, to)
	if err != nil {
		return err
	}
	defer unix.Close(sfd)
	defer unix.Close(dfd)
	return copyTreeAt(ctx, dfd, sfd, dfd, skip, "", map[[2]uint64]string{}, p, moveDepth)
}

// openPair opens the two ends of a move as folders; the destination's last
// component is not followed.
func openPair(from, to string) (sfd, dfd int, err error) {
	sfd, err = openat(unix.AT_FDCWD, from, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return -1, -1, &fs.PathError{Op: "open", Path: from, Err: err}
	}
	dfd, err = openat(unix.AT_FDCWD, to, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		unix.Close(sfd)
		return -1, -1, &fs.PathError{Op: "open", Path: to, Err: err}
	}
	return sfd, dfd, nil
}

func readlinkat(dirfd int, name string) (string, error) {
	for n := 256; ; n *= 2 {
		buf := make([]byte, n)
		got, err := unix.Readlinkat(dirfd, name, buf)
		if err != nil {
			return "", err
		}
		if got < n {
			return string(buf[:got]), nil
		}
	}
}

type progressWriter struct {
	w io.Writer
	p *jobs.Progress
}

func (pw *progressWriter) Write(b []byte) (int, error) {
	n, err := pw.w.Write(b)
	pw.p.Add(int64(n), 0)
	return n, err
}

// verifyTree checks that every regular file under from exists under to
// with the same size, by descriptor on both sides (verifyTreeAt). Sizes,
// not hashes: the copy just streamed every byte through fog and an error
// would have surfaced; what this catches is a file that changed or
// vanished while the copy ran.
func verifyTree(ctx context.Context, from, to, skip string) error {
	sfd, dfd, err := openPair(from, to)
	if err != nil {
		return err
	}
	defer unix.Close(sfd)
	defer unix.Close(dfd)
	return verifyTreeAt(ctx, sfd, dfd, skip, "", moveDepth)
}
