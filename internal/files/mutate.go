package files

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/jobs"
)

// The verbs. Rename and move within the root are single renameat calls,
// which is why they are instant at any size and why trash can be a rename
// too. Copy streams and is a job. Every verb ends by bringing the index in
// line with the directories it touched, so the next listing is exact
// without waiting for a walk.

// Conflict says what to do when the destination name is taken.
type Conflict string

const (
	ConflictAsk     Conflict = ""        // refuse with ErrExists; the client asks the operator
	ConflictRename  Conflict = "rename"  // "name 2", "name 3", …
	ConflictReplace Conflict = "replace" // the existing entry goes to the trash first
	ConflictSkip    Conflict = "skip"
)

// ConflictError carries the entry in the way so the client can offer
// Replace / Keep both / Skip without a second round trip.
type ConflictError struct {
	Path     string `json:"path"`
	Existing Node   `json:"existing"`
}

func (e *ConflictError) Error() string { return "already exists: " + e.Path }
func (e *ConflictError) Unwrap() error { return ErrExists }

// ErrMounted refuses to move or trash a folder an app has as a volume.
var ErrMounted = errors.New("an app mounts this folder as a volume")

// Mounts is every app volume that points into the root, read once.
// Reading it means listing the apps and parsing every spec, so an
// operation takes it once and asks it about each entry, rather than going
// back to the store per item.
type Mounts struct {
	root string
	list []MountedPath
}

// Mounts reads the app volumes now.
func (m *Manager) Mounts() Mounts {
	if m.MountedPaths == nil {
		return Mounts{}
	}
	return Mounts{root: m.Root(), list: m.MountedPaths()}
}

// AppMounts is what fogd wires MountedPaths to: every host path an app's
// newest spec mounts as a volume, resolved against the live root. It reads
// every app and parses every spec, so it is called once per operation
// (Mounts), never per item. Nil when the store can't be read.
func (m *Manager) AppMounts() []MountedPath {
	list, err := apps.WithSpecs(m.Store)
	if err != nil {
		return nil
	}
	var out []MountedPath
	root := m.Root()
	for _, a := range list {
		if a.Spec == nil {
			continue
		}
		for _, v := range a.Spec.AllVolumes() {
			if p := v.HostPath(root); p != "" {
				out = append(out, MountedPath{Path: p, App: a.Name, Mount: v.Mount, ReadOnly: v.ReadOnly})
			}
		}
	}
	return out
}

// AppsMounting names the apps whose volumes are rel or inside it.
func (m *Manager) AppsMounting(rel string) []string { return m.Mounts().Apps(rel) }

// Under returns the mounts at rel or inside it, their paths cleaned.
func (ms Mounts) Under(rel string) []MountedPath {
	if len(ms.list) == 0 {
		return nil
	}
	abs := filepath.Join(ms.root, filepath.FromSlash(rel))
	var out []MountedPath
	for _, mp := range ms.list {
		mp.Path = filepath.Clean(mp.Path)
		if IsUnder(mp.Path, abs) {
			out = append(out, mp)
		}
	}
	return out
}

// Apps names, once each, the apps with a mount at rel or inside it.
func (ms Mounts) Apps(rel string) []string {
	var out []string
	for _, mp := range ms.Under(rel) {
		if !slices.Contains(out, mp.App) {
			out = append(out, mp.App)
		}
	}
	return out
}

// opMounts is one operation's view of the mounts, read from the store the
// first time a removal asks and reused for every item after. nil reads
// afresh on each call, for callers that guard a single entry.
type opMounts struct {
	ms   Mounts
	read bool
}

func (o *opMounts) get(m *Manager) Mounts {
	if o == nil {
		return m.Mounts()
	}
	if !o.read {
		o.ms, o.read = m.Mounts(), true
	}
	return o.ms
}

// guardRemove is guardWrite for an entry about to leave its place (a
// move, rename, trash, or a rename over it): it also refuses when rel is,
// or contains, a folder an app mounts. Creating needs no such check:
// adding a name takes nothing from under an app, even where a volume
// points at a folder that does not exist yet (creating it only gives the
// app what its spec expects).
func (m *Manager) guardRemove(om *opMounts, rel string) error {
	if mp := om.get(m).Under(rel); len(mp) > 0 {
		return fmt.Errorf("%w (%s mounts %s)", ErrMounted, mp[0].App, mp[0].Path)
	}
	return m.guardWrite(rel)
}

func exists(root *os.Root, rel string) bool { return existsNF(root, rel) }

// uniqueName finds a free "name 2", "name 3", … in dir, keeping the
// extension where the name has one ("photo 2.jpg", not "photo.jpg 2").
// The name an app chose can be up to maxNameBytes of anything, so the
// candidate is trimmed to fit, on a character boundary: the stem gives way
// first, and an extension too long to leave room for the number is treated
// as part of the stem.
func uniqueName(root *os.Root, dir, name string) string {
	ext := path.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	if ext != "" && (stem == "" || strings.HasSuffix(stem, ".")) {
		stem, ext = name, "" // dotfiles: ".env" has no extension to keep
	}
	for i := 2; ; i++ {
		cand := numberedName(stem, ext, i)
		if !exists(root, Join(dir, cand)) {
			return cand
		}
	}
}

// numberedName is stem + " i" + ext, no longer than maxNameBytes.
func numberedName(stem, ext string, i int) string {
	num := " " + strconv.Itoa(i)
	if len(num)+len(ext) >= maxNameBytes-1 {
		stem, ext = stem+ext, ""
	}
	if over := len(stem) + len(num) + len(ext) - maxNameBytes; over > 0 {
		stem = truncUTF8(stem, len(stem)-over)
	}
	return stem + num + ext
}

// truncUTF8 cuts s to at most n bytes without splitting a character.
func truncUTF8(s string, n int) string {
	if n >= len(s) {
		return s
	}
	if n < 0 {
		n = 0
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// caseRename picks how to rename from to to. Ordinarily it is a rename
// that refuses to replace, and taken says to is already something else.
// When the two names differ only in case they may be one entry — on a
// case-insensitive disk "a.txt" and "A.txt" are the same file, and the
// rename (which must be allowed to "replace", since to exists) changes its
// case. But on a case-sensitive disk they are two files, and replacing to
// would destroy one an app or the operator put there: so the replacing
// rename is used only when to is, right now, the very entry from is (same
// device and inode); a different entry under to is a conflict like any
// other, and a missing one gets the no-replace rename.
func caseRename(root *os.Root, from, to string) (rename func(string, string) error, taken bool) {
	n := nf(root)
	dst, err := lstatNF(root, to)
	if err != nil {
		return n.RenameNoReplace, false
	}
	if !strings.EqualFold(from, to) {
		return nil, true
	}
	src, err := lstatNF(root, from)
	if err != nil || !sameEntry(src, dst) {
		return nil, true
	}
	return func(from, to string) error { return renameViaTemp(n, from, to) }, false
}

// renameViaTemp changes the spelling of one entry whose old and new names
// are the same file to the disk (a case-insensitive or casefold disk).
// rename(2) onto the same file is allowed to do nothing (POSIX; casefold
// ext4 does exactly that), so the entry is first moved to a temporary name
// beside it and then to its new spelling — both steps by descriptor, both
// refusing to replace. If the new name is taken in between, the entry goes
// back where it was: nothing is ever replaced.
func renameViaTemp(n nfRoot, from, to string) error {
	parent, _ := Split(from)
	tmp := Join(parent, ".fog-rename-"+randHex(8))
	if err := n.RenameNoReplace(from, tmp); err != nil {
		return err
	}
	if err := n.RenameNoReplace(tmp, to); err != nil {
		if berr := n.RenameNoReplace(tmp, from); berr != nil {
			return fmt.Errorf("%w (and putting it back failed: %v; it is at %s)", err, berr, tmp)
		}
		return err
	}
	return nil
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// sameEntry says two lstats are of one file: same device, same inode.
func sameEntry(a, b fs.FileInfo) bool {
	sa, ok1 := a.Sys().(*syscall.Stat_t)
	sb, ok2 := b.Sys().(*syscall.Stat_t)
	return ok1 && ok2 && sa.Dev == sb.Dev && sa.Ino == sb.Ino
}

// resolveDest applies the conflict policy and returns the final destination
// (or "" to skip).
func (m *Manager) resolveDest(root *os.Root, ix *Index, dir, name string, c Conflict, om *opMounts) (string, error) {
	return m.resolveDestAs(root, ix, dir, name, c, false, om)
}

// resolveDestAs is resolveDest with the guards optionally off (an owned
// upload replacing a file the operator may not touch).
func (m *Manager) resolveDestAs(root *os.Root, ix *Index, dir, name string, c Conflict, owned bool, om *opMounts) (string, error) {
	dest := Join(dir, name)
	if !exists(root, dest) {
		return dest, nil
	}
	switch c {
	case ConflictSkip:
		return "", nil
	case ConflictRename:
		return Join(dir, uniqueName(root, dir, name)), nil
	case ConflictReplace:
		// Replacing is a trash, not a delete: it is undoable.
		if _, err := m.trashOneAs(root, ix, dest, owned, om); err != nil {
			return "", err
		}
		return dest, nil
	default:
		m.refreshDir(root, ix, dir, false)
		n, _ := ix.Get(dest)
		return "", &ConflictError{Path: dest, Existing: n}
	}
}

// Mkdir creates one folder.
func (m *Manager) Mkdir(rel string) (Node, error) {
	root, ix, err := m.rootAndIndex()
	if err != nil {
		return Node{}, err
	}
	if rel == "" {
		return Node{}, ErrBadName
	}
	parent, name := Split(rel)
	if err := ValidateName(parent, name); err != nil {
		return Node{}, err
	}
	if err := m.guardWrite(rel); err != nil {
		return Node{}, err
	}
	if err := nf(root).Mkdir(rel, 0o755); err != nil {
		return Node{}, mapOSError(err)
	}
	m.chown(root, rel)
	if _, err := m.refreshDir(root, ix, parent, false); err != nil {
		return Node{}, err
	}
	return ix.Get(rel)
}

// Rename changes one entry's name in place.
func (m *Manager) Rename(rel, newName string) (Node, error) {
	root, ix, err := m.rootAndIndex()
	if err != nil {
		return Node{}, err
	}
	if rel == "" {
		return Node{}, ErrBadName
	}
	parent, old := Split(rel)
	if err := ValidateName(parent, newName); err != nil {
		return Node{}, err
	}
	if newName == old {
		return ix.Get(rel)
	}
	if err := m.guardRemove(nil, rel); err != nil {
		return Node{}, err
	}
	dest := Join(parent, newName)
	// By descriptor, and refusing to replace in the same call: what is
	// renamed is the entry in the folder that was checked, and a name taken
	// in between is a conflict, not a casualty. A case-only rename is the
	// one exception (caseRename).
	rename, taken := caseRename(root, rel, dest)
	if taken {
		n, _ := ix.Get(dest)
		return Node{}, &ConflictError{Path: dest, Existing: n}
	}
	if err := rename(rel, dest); err != nil {
		if errors.Is(err, ErrExists) {
			n, _ := ix.Get(dest)
			return Node{}, &ConflictError{Path: dest, Existing: n}
		}
		return Node{}, mapOSError(err)
	}
	if err := ix.moveSubtree(rel, dest); err != nil {
		return Node{}, err
	}
	m.followRename(rel, dest)
	if _, err := m.refreshDir(root, ix, parent, false); err != nil {
		return Node{}, err
	}
	m.renamed(parent)
	return ix.Get(dest)
}

// MoveResult reports what a move did.
type MoveResult struct {
	Moved   []string `json:"moved"`
	Skipped []string `json:"skipped,omitempty"`
	// Job is set when some entries had to be copied (a destination on a
	// different filesystem inside the root); they move when it finishes.
	Job *jobs.Job `json:"job,omitempty"`
}

// Move relocates entries into the directory to.
func (m *Manager) Move(rels []string, to string, c Conflict) (MoveResult, error) {
	root, ix, err := m.rootAndIndex()
	if err != nil {
		return MoveResult{}, err
	}
	if err := requireDir(root, to); err != nil {
		return MoveResult{}, err
	}
	if err := m.guardWrite(Join(to, "_")); err != nil {
		return MoveResult{}, err
	}
	var res MoveResult
	var slow []string
	touched := map[string]bool{to: true}
	om := &opMounts{}
	for _, rel := range rels {
		if rel == "" {
			return res, ErrBadName
		}
		parent, name := Split(rel)
		if parent == to {
			res.Skipped = append(res.Skipped, rel)
			continue
		}
		if IsUnder(to, rel) {
			return res, fmt.Errorf("%w: cannot move %s into itself", ErrBadName, rel)
		}
		if err := m.guardRemove(om, rel); err != nil {
			return res, err
		}
		dest, err := m.resolveDest(root, ix, to, name, c, om)
		if err != nil {
			return res, err
		}
		if dest == "" {
			res.Skipped = append(res.Skipped, rel)
			continue
		}
		if err := nf(root).RenameNoReplace(rel, dest); err != nil {
			if errors.Is(err, syscall.EXDEV) {
				slow = append(slow, rel)
				continue
			}
			return res, mapOSError(err)
		}
		if err := ix.moveSubtree(rel, dest); err != nil {
			return res, err
		}
		m.followRename(rel, dest)
		touched[parent] = true
		res.Moved = append(res.Moved, dest)
	}
	for d := range touched {
		m.refreshDir(root, ix, d, false)
	}
	if len(res.Moved) > 0 {
		m.renamed(slices.Collect(maps.Keys(touched))...)
	}
	if len(slow) > 0 {
		job := m.copyJob(slow, to, c, true)
		res.Job = &job
	}
	return res, nil
}

// Copy duplicates entries into to as a job.
func (m *Manager) Copy(rels []string, to string, c Conflict) (jobs.Job, error) {
	root, ix, err := m.rootAndIndex()
	if err != nil {
		return jobs.Job{}, err
	}
	if err := requireDir(root, to); err != nil {
		return jobs.Job{}, err
	}
	if err := m.guardWrite(Join(to, "_")); err != nil {
		return jobs.Job{}, err
	}
	// Conflicts are decided before any bytes move, so the answer to "ask"
	// is a 409 now, not a half-finished job later.
	for _, rel := range rels {
		if rel == "" {
			return jobs.Job{}, ErrBadName
		}
		if !exists(root, rel) {
			return jobs.Job{}, ErrNotFound
		}
		if IsUnder(to, rel) && rel != "" {
			return jobs.Job{}, fmt.Errorf("%w: cannot copy %s into itself", ErrBadName, rel)
		}
		_, name := Split(rel)
		if c == ConflictAsk && exists(root, Join(to, name)) {
			m.refreshDir(root, ix, to, false)
			n, _ := ix.Get(Join(to, name))
			return jobs.Job{}, &ConflictError{Path: Join(to, name), Existing: n}
		}
	}
	return m.copyJob(rels, to, c, false), nil
}

func requireDir(root *os.Root, rel string) error {
	info, err := lstatNF(root, rel)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return ErrNotDirectory
	}
	return nil
}

// copyJob copies rels into to; with move set, each source is removed
// after its copy is verified.
func (m *Manager) copyJob(rels []string, to string, c Conflict, move bool) jobs.Job {
	label := "Copy"
	if move {
		label = "Move"
	}
	if len(rels) == 1 {
		_, name := Split(rels[0])
		label += " " + name
	} else {
		label += fmt.Sprintf(" %d items", len(rels))
	}
	return m.Jobs.Start("copy", label, func(ctx context.Context, p *jobs.Progress) error {
		root, ix, err := m.rootAndIndex()
		if err != nil {
			return err
		}
		var total int64
		for _, rel := range rels {
			total += treeSize(root, rel)
		}
		p.Set("", 0, total)
		var out []string
		om := &opMounts{}
		for _, rel := range rels {
			if err := ctx.Err(); err != nil {
				return err
			}
			_, name := Split(rel)
			dest, err := m.resolveDest(root, ix, to, name, c, om)
			if err != nil {
				return err
			}
			if dest == "" {
				continue
			}
			p.Current(name)
			if err := copyEntry(ctx, root, rel, dest, p); err != nil {
				nf(root).RemoveAll(dest)
				return err
			}
			m.chownTree(root, dest)
			if move {
				if err := nf(root).RemoveAll(rel); err != nil {
					return err
				}
				parent, _ := Split(rel)
				m.refreshDir(root, ix, parent, false)
				// What follows a rename follows this move too, and the
				// links are bound to the copies (new inodes on the new disk).
				m.followRename(rel, dest)
				m.rebindShares(dest)
			}
			// Index the copy's subtree, then let the parent pick up its size.
			if info, err := lstatNF(root, dest); err == nil && info.IsDir() {
				m.walkDir(ctx, root, ix, dest, time.Now().UnixNano())
			}
			m.refreshDir(root, ix, to, false)
			out = append(out, dest)
		}
		p.Result(map[string]any{"paths": out})
		m.kickHasher()
		return nil
	})
}

func treeSize(root *os.Root, rel string) int64 {
	var n int64
	info, err := lstatNF(root, rel)
	if err != nil {
		return 0
	}
	walkTree(root, rel, info, func(_ string, fi fs.FileInfo) error {
		if fi.Mode().IsRegular() {
			n += fi.Size()
		}
		return nil
	})
	return n
}

// copyEntry copies a file or a whole tree within the root, preserving
// modes and mtimes; symlinks are recreated as links (never resolved), other
// special files skipped.
func copyEntry(ctx context.Context, root *os.Root, src, dst string, p *jobs.Progress) error {
	info, err := lstatNF(root, src)
	if err != nil {
		return err
	}
	return walkTree(root, src, info, func(rel string, fi fs.FileInfo) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		target := dst + strings.TrimPrefix(rel, src)
		switch {
		case fi.IsDir():
			if err := nf(root).Mkdir(target, fi.Mode().Perm()); err != nil && !errors.Is(err, fs.ErrExist) {
				return mapOSError(err)
			}
		case fi.Mode()&fs.ModeSymlink != 0:
			link, err := nf(root).Readlink(rel)
			if err != nil {
				return err
			}
			nf(root).Remove(target)
			return nf(root).Symlink(link, target)
		case fi.Mode().IsRegular():
			if err := copyFileInRoot(root, rel, target, fi, p); err != nil {
				return err
			}
			p.Add(0, 1)
		}
		return nil
	})
}

func copyFileInRoot(root *os.Root, src, dst string, info fs.FileInfo, p *jobs.Progress) error {
	in, _, err := openFileNF(root, src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := checkNoLinks(root, dst); err != nil {
		return err
	}
	if _, err := atomicWrite(nf(root), dst, in, writeOpts{perm: info.Mode().Perm(), mtime: info.ModTime(), p: p}); err != nil {
		return mapOSError(err)
	}
	return nil
}

// chownTree applies files_owner to everything fog just created under rel.
func (m *Manager) chownTree(root *os.Root, rel string) {
	uid, gid, ok := m.Owner()
	if !ok {
		return
	}
	info, err := lstatNF(root, rel)
	if err != nil {
		return
	}
	walkTree(root, rel, info, func(p string, _ fs.FileInfo) error {
		nf(root).Lchown(p, uid, gid)
		return nil
	})
}

func (m *Manager) rootAndIndex() (*os.Root, *Index, error) {
	root, err := m.openRoot()
	if err != nil {
		return nil, nil, err
	}
	ix, err := m.Index()
	if err != nil {
		return nil, nil, err
	}
	return root, ix, nil
}

// renamed tells OnChange about the folders a rename or move touched. The
// index rows were carried across by moveSubtree before the refresh ran, so
// the refresh finds those folders as the index already describes them and
// stays silent — and federation would never hear that a shared folder
// gained or lost an entry.
func (m *Manager) renamed(dirs ...string) {
	if m.OnChange == nil {
		return
	}
	for i, d := range dirs {
		if !slices.Contains(dirs[:i], d) {
			m.OnChange(d)
		}
	}
}

// moveSubtree re-parents index rows after a rename: the entry's own row
// and, for a directory, every row beneath it — one UPDATE per level of
// prefix, no re-walk.
func (ix *Index) moveSubtree(old, new string) error {
	ix.wmu.Lock()
	defer ix.wmu.Unlock()
	tx, err := ix.db.Begin()
	if err != nil {
		return err
	}
	op, on := Split(old)
	np, nn := Split(new)
	// The destination row, if a stale one exists, gives way.
	if err := deleteSubtree(tx, new); err != nil {
		tx.Rollback()
		return err
	}
	if _, err := tx.Exec(`UPDATE nodes SET parent = ?, name = ? WHERE parent = ? AND name = ?`, np, nn, op, on); err != nil {
		tx.Rollback()
		return err
	}
	w, args := subtree("parent", old)
	if _, err := tx.Exec(`UPDATE nodes SET parent = ? || substr(parent, ?) WHERE `+w,
		append([]any{new, len(old) + 1}, args...)...); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}
