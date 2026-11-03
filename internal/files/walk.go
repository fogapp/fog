package files

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// bigDir is the entry count above which a listing trusts the directory's
// mtime instead of re-reading it every time. Below it, a rescan is a few
// milliseconds and buys exact sizes and mtimes for files rewritten in
// place; above it, only an added, removed or renamed entry (which does bump
// the directory mtime) triggers the re-read.
const bigDir = 2000

var ErrWalking = errors.New("a walk is already running")

func inodeOf(info fs.FileInfo) int64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return int64(st.Ino)
	}
	return 0
}

func kindOf(mode fs.FileMode) string {
	switch {
	case mode.IsDir():
		return "dir"
	case mode.IsRegular():
		return "file"
	default:
		return "other"
	}
}

// readDir lists rel with lstat metadata for each entry, hiding the
// reserved directories (reservedAt). No link on the way to rel is followed
// (nofollow.go); a link inside it is listed as what it is.
func readDir(root *os.Root, rel string) ([]fs.FileInfo, error) {
	fd, err := openDirFD(root, rel)
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)
	return readDirAt(fd, rel)
}

// refreshDir brings the index rows for one directory in line with disk and
// returns the directory's own node. force re-reads regardless of size.
func (m *Manager) refreshDir(root *os.Root, ix *Index, rel string, force bool) (Node, error) {
	info, err := lstatNF(root, rel)
	if err != nil {
		return Node{}, err
	}
	if !info.IsDir() {
		return Node{}, ErrNotDirectory
	}
	existing, err := ix.Get(rel)
	found := err == nil
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Node{}, err
	}
	// A directory the index has never seen (created behind its back, or
	// by MkdirAll for an upload into a new folder): refresh its parent
	// first, which inserts this row, so the ancestor chain always exists
	// for the size bump below. Recursion is the path depth.
	if !found && rel != "" {
		parent, _ := Split(rel)
		if _, err := m.refreshDir(root, ix, parent, false); err != nil {
			return Node{}, err
		}
		if existing, err = ix.Get(rel); err == nil {
			found = true
		}
	}
	dm := info.ModTime().UnixNano()
	if !force && found && existing.dirMtime == dm && existing.Entries > bigDir {
		return existing, nil
	}
	ents, err := readDir(root, rel)
	if err != nil {
		return Node{}, err
	}
	children, err := ix.children(rel)
	if err != nil {
		return Node{}, err
	}
	plan := planDir(rel, ents, children, nil, 0)
	// Most listings find the folder as they left it. Then there is nothing
	// to write, and taking the writer lock and a transaction anyway would
	// queue every page view behind a walk or an upload for no reason.
	if found && len(plan.writes) == 0 && dirRowCurrent(existing, info, plan) {
		return existing, nil
	}
	err = ix.write(func(tx *sql.Tx) error {
		if err := applyDir(tx, rel, info, plan, 0); err != nil {
			return err
		}
		if found {
			return bumpAncestors(tx, rel, plan.total-existing.Size)
		}
		return nil
	})
	if err != nil {
		return Node{}, err
	}
	if plan.changed && m.OnChange != nil {
		m.OnChange(rel)
	}
	return ix.Get(rel)
}

// errRaced: the row changed between reading it and writing it.
var errRaced = errors.New("index row changed underneath")

// refreshFile brings one file's row in line with disk without re-reading
// its directory, which is all Stat needs to describe a single file.
// Anything one lstat cannot settle — no row yet, a kind change, a
// listed-only row, an entry gone from disk, a refresh racing this one —
// falls back to refreshing the parent directory.
func (m *Manager) refreshFile(root *os.Root, ix *Index, rel string) (Node, error) {
	parent, _ := Split(rel)
	whole := func() (Node, error) {
		if _, err := m.refreshDir(root, ix, parent, false); err != nil {
			return Node{}, err
		}
		return ix.Get(rel)
	}
	info, err := lstatNF(root, rel)
	if err != nil || !info.Mode().IsRegular() {
		return whole()
	}
	old, err := ix.Get(rel)
	if err != nil || old.Kind != "file" || old.Dehydrated {
		return whole()
	}
	size, mtime, ino := info.Size(), info.ModTime().UnixNano(), inodeOf(info)
	if old.Size == size && old.mtime == mtime && old.inode == ino {
		return old, nil
	}
	err = ix.write(func(tx *sql.Tx) error {
		// Only if the row is still the one read above: a refresh that got
		// in first has already moved the folder sizes, and bumping them by
		// this delta too would count the change twice.
		res, err := tx.Exec(`UPDATE nodes SET size = ?, mtime = ?, mode = ?, inode = ?, sha256 = '' WHERE id = ? AND size = ? AND mtime = ? AND inode = ?`,
			size, mtime, int64(info.Mode().Perm()), ino, old.id, old.Size, old.mtime, old.inode)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return errRaced
		}
		return bumpAncestors(tx, rel, size-old.Size)
	})
	if errors.Is(err, errRaced) {
		return whole()
	}
	if err != nil {
		return Node{}, err
	}
	if m.OnChange != nil {
		m.OnChange(parent)
	}
	return ix.Get(rel)
}

// dirPlan is what one directory needs written to match disk: the entry
// rows to insert, update or delete, in order, and the directory's own row
// as it should read afterwards. Working it out only reads; applyDir
// writes it.
type dirPlan struct {
	writes  []rowWrite
	total   int64 // the directory's bytes, as its own row records them
	entries int   // direct children, listed-only rows included
	changed bool  // an entry was inserted or deleted, or its bytes changed
}

// rowWrite is one statement of a plan, or (subtree set) the removal of an
// entry and everything under it.
type rowWrite struct {
	query   string
	args    []any
	subtree string
}

// planDir diffs one directory's entries on disk against what the index
// holds for it. sizes carries subtree sizes for child directories when the
// caller is a walk (nil for a listing refresh, which keeps whatever size
// was known).
func planDir(rel string, ents []fs.FileInfo, children map[string]Node, sizes map[string]int64, walkedAt int64) dirPlan {
	var p dirPlan
	write := func(query string, args ...any) {
		p.writes = append(p.writes, rowWrite{query: query, args: args})
	}
	seen := make(map[string]bool, len(ents))
	for _, fi := range ents {
		name := fi.Name()
		seen[name] = true
		kind := kindOf(fi.Mode())
		mtime := fi.ModTime().UnixNano()
		ino := inodeOf(fi)
		mode := int64(fi.Mode().Perm())
		var size int64
		old, had := children[name]
		switch kind {
		case "file":
			size = fi.Size()
		case "dir":
			if sizes != nil {
				size = sizes[name]
			} else if had && old.Kind == "dir" {
				size = old.Size
			}
		}
		p.total += size
		full := Join(rel, name)
		if had && old.Kind != kind {
			p.writes = append(p.writes, rowWrite{subtree: full})
			had = false
		}
		switch {
		case !had:
			p.changed = true
			// An upsert, not an insert: two refreshes of one directory can
			// run at once (two uploads landing in a new folder) and both
			// see the row missing. The second must not fail on the first.
			write(`INSERT INTO nodes (parent, name, kind, size, mtime, mode, inode, mime, sha256, dir_mtime, entries, walked_at, dehydrated)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, '', 0, 0, ?, 0)
				ON CONFLICT(parent, name) DO UPDATE SET
					kind = excluded.kind, size = CASE WHEN excluded.kind = 'dir' THEN MAX(size, excluded.size) ELSE excluded.size END,
					mtime = excluded.mtime, mode = excluded.mode, inode = excluded.inode, mime = excluded.mime, dehydrated = 0,
					sha256 = CASE WHEN kind = excluded.kind AND size = excluded.size AND mtime = excluded.mtime THEN sha256 ELSE '' END`,
				rel, name, kind, size, mtime, mode, ino, mimeOf(kind, name), dirWalked(kind, sizes, walkedAt))
		case kind == "dir":
			if sizes != nil {
				// A walk goes bottom-up, so the child's own pass has just
				// written this row; rewriting identical values is pure cost.
				if old.Size == size && old.mtime == mtime && old.inode == ino &&
					int64(old.modeBits) == mode && old.walkedAt == walkedAt {
					break
				}
				write(`UPDATE nodes SET size = ?, mtime = ?, mode = ?, inode = ?, walked_at = ? WHERE id = ?`,
					size, mtime, mode, ino, walkedAt, old.id)
			} else if old.mtime != mtime || old.inode != ino {
				write(`UPDATE nodes SET mtime = ?, mode = ?, inode = ? WHERE id = ?`, mtime, mode, ino, old.id)
			}
		case old.Dehydrated:
			// Bytes landed for a listed-only file: the row is a file on
			// disk now. The digest stays when the bytes match what the
			// origin described (a hydration); otherwise it is rehashed.
			p.changed = true
			write(`UPDATE nodes SET size = ?, mtime = ?, mode = ?, inode = ?, dehydrated = 0,
					sha256 = CASE WHEN size = ? AND mtime = ? THEN sha256 ELSE '' END WHERE id = ?`,
				size, mtime, mode, ino, size, mtime, old.id)
		case old.Size != size || old.mtime != mtime || old.inode != ino:
			// Changed bytes: the digest no longer describes them.
			p.changed = true
			write(`UPDATE nodes SET size = ?, mtime = ?, mode = ?, inode = ?, sha256 = '' WHERE id = ?`,
				size, mtime, mode, ino, old.id)
		}
	}
	dehydrated := 0
	for name, old := range children {
		if seen[name] {
			continue
		}
		if old.Dehydrated {
			// Listed, not held: the subscription that placed the row is
			// the only thing that removes it.
			dehydrated++
			continue
		}
		p.changed = true
		p.writes = append(p.writes, rowWrite{subtree: Join(rel, name)})
	}
	p.entries = len(seen) + dehydrated
	return p
}

// dirRowCurrent says whether the directory's own row already records what
// applyDir would write to it. The row keeps no "last listed" time — a
// listing refresh leaves walked_at alone — so its other columns are the
// whole comparison.
func dirRowCurrent(n Node, info fs.FileInfo, p dirPlan) bool {
	dm := info.ModTime().UnixNano()
	return n.Kind == "dir" && n.Size == p.total && n.mtime == dm && n.dirMtime == dm &&
		n.Entries == p.entries && n.modeBits == uint32(info.Mode().Perm()) && n.inode == inodeOf(info)
}

// applyDir writes a plan: the entry rows in order, then the directory's
// own row.
func applyDir(tx *sql.Tx, rel string, info fs.FileInfo, p dirPlan, walkedAt int64) error {
	stmts := txStmts{tx: tx}
	defer stmts.close()
	for _, w := range p.writes {
		var err error
		if w.subtree != "" {
			err = deleteSubtree(tx, w.subtree)
		} else {
			_, err = stmts.exec(w.query, w.args...)
		}
		if err != nil {
			return err
		}
	}
	parent, name := Split(rel)
	dm := info.ModTime().UnixNano()
	var walked any = nil
	if walkedAt > 0 {
		walked = walkedAt
	}
	_, err := tx.Exec(`INSERT INTO nodes (parent, name, kind, size, mtime, mode, inode, mime, sha256, dir_mtime, entries, walked_at)
		VALUES (?, ?, 'dir', ?, ?, ?, ?, '', '', ?, ?, COALESCE(?, 0))
		ON CONFLICT(parent, name) DO UPDATE SET
			kind = 'dir', size = excluded.size, mtime = excluded.mtime, mode = excluded.mode, inode = excluded.inode,
			dir_mtime = excluded.dir_mtime, entries = excluded.entries,
			walked_at = COALESCE(?, walked_at)`,
		parent, name, p.total, dm, int64(info.Mode().Perm()), inodeOf(info), dm, p.entries, walked, walked)
	return err
}

// txStmts prepares each per-row statement once per transaction. applyDir
// writes a row for every entry of a directory with the same handful of
// statements; without this SQLite compiles each one again per row.
type txStmts struct {
	tx *sql.Tx
	m  map[string]*sql.Stmt
}

func (t *txStmts) exec(query string, args ...any) (sql.Result, error) {
	st, ok := t.m[query]
	if !ok {
		var err error
		if st, err = t.tx.Prepare(query); err != nil {
			return nil, err
		}
		if t.m == nil {
			t.m = map[string]*sql.Stmt{}
		}
		t.m[query] = st
	}
	return st.Exec(args...)
}

func (t *txStmts) close() {
	for _, st := range t.m {
		st.Close()
	}
}

func mimeOf(kind, name string) string {
	if kind != "file" {
		return ""
	}
	return MimeFor(name)
}

func dirWalked(kind string, sizes map[string]int64, walkedAt int64) int64 {
	if kind == "dir" && sizes != nil {
		return walkedAt
	}
	return 0
}

// Walk re-indexes the whole tree bottom-up, one transaction per
// directory, computing every folder's size exactly. One at a time; a
// second caller gets ErrWalking.
func (m *Manager) Walk(ctx context.Context) error {
	if !m.walkMu.TryLock() {
		return ErrWalking
	}
	defer m.walkMu.Unlock()
	root, err := m.openRoot()
	if err != nil {
		return err
	}
	ix, err := m.Index()
	if err != nil {
		return err
	}
	m.walking.Store(true)
	defer m.walking.Store(false)
	start := time.Now()
	if _, err := m.walkDir(ctx, root, ix, "", start.UnixNano()); err != nil {
		return err
	}
	ix.setMeta("walked_at", start.UTC().Format(time.RFC3339))
	m.Log.Info("files walk complete", "dur", time.Since(start).Round(time.Millisecond).String())
	m.kickHasher()
	return nil
}

func (m *Manager) walkDir(ctx context.Context, root *os.Root, ix *Index, rel string, now int64) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if depthOf(rel) > MaxDepth {
		return 0, tooDeep(rel)
	}
	fd, err := openDirFD(root, rel)
	if err != nil {
		return 0, err
	}
	s, err := newDirStack(fd, MaxDepth-depthOf(rel))
	if err != nil {
		return 0, err
	}
	defer s.close()
	return m.walkDirAt(ctx, s, 0, ix, rel, now)
}

// walkDirAt indexes the folder at level of s (rel) and everything under
// it, each folder read through its parent's descriptor (deepwalk.go). A
// folder deeper than MaxDepth is left out of the index, sized as nothing.
func (m *Manager) walkDirAt(ctx context.Context, s *dirStack, level int, ix *Index, rel string, now int64) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	fd, err := s.fd(level)
	if err != nil {
		return 0, err
	}
	name := "."
	if rel != "" {
		_, name = Split(rel)
	}
	info := &statInfo{name: name}
	if err := unix.Fstat(fd, &info.st); err != nil {
		return 0, err
	}
	ents, err := readDirAt(fd, rel)
	if err != nil {
		return 0, err
	}
	sizes := map[string]int64{}
	for _, fi := range ents {
		if fi.IsDir() && rel == "" && isPlaceable(fi.Name()) && m.placementOffline(fi.Name()) != "" {
			// Apps or Content on a disk that is not there: the folder
			// underneath is an empty mount point, not the folder. Keep
			// what the index knows until the disk is back.
			if n, err := ix.Get(fi.Name()); err == nil {
				sizes[fi.Name()] = n.Size
			}
			continue
		}
		if fi.IsDir() {
			child := Join(rel, fi.Name())
			var sz int64
			_, err := s.push(fi.Name(), rawStat(fi))
			if err == nil {
				sz, err = m.walkDirAt(ctx, s, level+1, ix, child, now)
				s.pop()
			} else if errors.Is(err, ErrTooDeep) {
				m.Log.Warn("files walk: folder too deep to index", "path", DisplayName(child), "max_depth", MaxDepth)
			}
			if err != nil {
				if ctx.Err() != nil {
					return 0, err
				}
				// A subdirectory that vanished, cannot be read or is too
				// deep: size it as nothing and carry on; the next walk sees
				// it again.
				sz = 0
			}
			sizes[fi.Name()] = sz
		}
	}
	children, err := ix.children(rel)
	if err != nil {
		return 0, err
	}
	// A walk stamps walked_at on every directory, so its plan always has
	// the directory row to write; there is no quiet case to skip.
	plan := planDir(rel, ents, children, sizes, now)
	err = ix.write(func(tx *sql.Tx) error { return applyDir(tx, rel, info, plan, now) })
	return plan.total, err
}
