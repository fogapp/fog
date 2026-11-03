// Moving an app's folder, on the app's behalf.
//
// Drive refuses to move or trash a folder an app mounts — that guard is
// what stops an operator emptying a running app's library by dragging it
// somewhere. It does not apply to the app itself moving: when fog is the
// one repointing the mount, the folder and the spec change together, which
// is the only way the two can stay true about each other.
package files

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"

	"golang.org/x/sys/unix"

	"gitea.lan/matthew/fog/internal/jobs"
)

// ErrDestNotEmpty is a move onto a folder that already holds something.
// The operator's answer to it is to adopt what is there instead, which is
// a different verb and a different sentence, so it is a distinct error
// rather than a message.
var ErrDestNotEmpty = errors.New("that folder already has files in it")

// MoveVolume relocates the tree at srcAbs — an absolute host path, whether
// or not it lies inside Drive — to rel inside Drive, and reports progress
// through p. srcAbs not existing is not a failure: a volume that was never
// deployed has nothing to move, and repointing it is the whole operation.
//
// The destination must be missing or empty. Adopting a folder that already
// holds a library is the caller's other verb: it moves no bytes at all and
// never reaches here.
func (m *Manager) MoveVolume(ctx context.Context, srcAbs, rel string, p *jobs.Progress) error {
	rel, err := CleanRel(rel)
	if err != nil {
		return err
	}
	if rel == "" {
		return ErrBadName
	}
	root, ix, err := m.rootAndIndex()
	if err != nil {
		return err
	}
	dstAbs := filepath.Join(m.Root(), filepath.FromSlash(rel))
	srcAbs = filepath.Clean(srcAbs)
	if srcAbs == dstAbs {
		return nil
	}
	if srcRel := pathUnderRoot(m.Root(), srcAbs); srcRel != "" && IsUnder(rel, srcRel) {
		return fmt.Errorf("%w: cannot move a folder into itself", ErrBadName)
	}
	// From here on nothing is named by a path (movefd.go): the destination
	// is its parent's descriptor and a name, reached from the root without
	// following a link; so is a source inside Drive. An app writing beside
	// either end cannot swap a link in between a check and the move — the
	// folder that lands is the one bound into the app's container.
	dstParent, dstName := Split(rel)
	if dstParent != "" {
		if err := mkdirAllNF(root, dstParent, 0o755); err != nil {
			return err
		}
		m.chownNewDirs(root, dstParent)
	}
	dpfd, err := openDirFD(root, dstParent)
	if err != nil {
		return err
	}
	defer unix.Close(dpfd)
	if err := requireEmptyAt(dpfd, dstName); err != nil {
		return err
	}

	srcName := filepath.Base(srcAbs)
	var spfd int
	if srcRel := pathUnderRoot(m.Root(), srcAbs); srcRel != "" {
		spfd, err = openDirFD(root, parentOfRel(srcRel))
	} else {
		// Outside Drive: fog's own data directory, which no app writes.
		spfd, err = openat(unix.AT_FDCWD, filepath.Dir(srcAbs), unix.O_RDONLY|unix.O_DIRECTORY, 0)
		if err != nil {
			err = mapOSError(&fs.PathError{Op: "open", Path: filepath.Dir(srcAbs), Err: err})
		}
	}
	if errors.Is(err, ErrNotFound) {
		return m.EnsureDir(rel)
	}
	if err != nil {
		return err
	}
	defer unix.Close(spfd)
	var sst unix.Stat_t
	switch err := fstatat(spfd, srcName, &sst); {
	case errors.Is(err, unix.ENOENT):
		// Nothing on disk yet: make the folder so the app finds it, and the
		// spec change the caller is about to store is the whole move.
		return m.EnsureDir(rel)
	case err != nil:
		return err
	case statKind(&sst) == unix.S_IFLNK:
		return fmt.Errorf("%w: %s", ErrSymlink, srcAbs)
	case statKind(&sst) != unix.S_IFDIR:
		return ErrNotDirectory
	}
	// One renameat when both ends share a filesystem, otherwise a copy,
	// verified, then the source removed — every step by descriptor.
	// Writes through fog to either end wait out the move (the app itself
	// is stopped by the caller).
	defer m.freeze(rel)()
	if old := pathUnderRoot(m.Root(), srcAbs); old != "" {
		defer m.freeze(old)()
	}
	if err := moveDirAt(ctx, spfd, srcName, dpfd, dstName, ReservedDir, p); err != nil {
		return m.moveRefused("Moving an app's folder to Files/"+DisplayName(rel), err)
	}

	p.Current("indexing")
	// The index describes the tree as it was. Inside Drive both ends are
	// indexed, so the subtree is carried across; from outside there is
	// nothing to carry and the destination's parent is simply re-read.
	old := pathUnderRoot(m.Root(), srcAbs)
	if old != "" {
		ix.moveSubtree(old, rel)
		m.followRename(old, rel)
		m.rebindShares(rel)
		if root, ix, err := m.rootAndIndex(); err == nil {
			m.refreshDir(root, ix, parentOfRel(old), false)
		}
	}
	if root, ix, err := m.rootAndIndex(); err == nil {
		m.refreshDir(root, ix, parentOfRel(rel), false)
		m.refreshDir(root, ix, rel, false)
	}
	if old != "" {
		m.renamed(parentOfRel(old), parentOfRel(rel))
	}
	return nil
}

// AdoptFolder is the other half of a move: no bytes travel, but the folder
// has to exist and be fog's before an app is pointed at it.
func (m *Manager) AdoptFolder(rel string) error {
	rel, err := CleanRel(rel)
	if err != nil {
		return err
	}
	if rel == "" {
		return ErrBadName
	}
	return m.EnsureDir(rel)
}

// pathUnderRoot turns an absolute path inside the Drive root into a
// root-relative one, or "" when it lies outside.
func pathUnderRoot(root, abs string) string {
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == "." || rel == ".." || filepath.IsAbs(rel) {
		return ""
	}
	if len(rel) >= 3 && rel[:3] == ".."+string(filepath.Separator) {
		return ""
	}
	return filepath.ToSlash(rel)
}
