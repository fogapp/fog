package files

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"gitea.lan/matthew/fog/internal/backup"
	"gitea.lan/matthew/fog/internal/chunkstore"
	"gitea.lan/matthew/fog/internal/fsx"
	"gitea.lan/matthew/fog/internal/jobs"
	"gitea.lan/matthew/fog/internal/store"
)

// Drive's snapshots live in the incremental store (docs/store-contract.md)
// beside the apps' backups. A night reads only what changed — a file whose
// size and modification time match the night before is carried over by its
// chunks, unread — and stores only the chunks no earlier snapshot has, so a
// large file that changes a little costs the little. The store needs no
// hardlinks, so a share without them (SMB) keeps full history too. Every
// snapshot is complete: browsing, restoring and deleting one never touches
// another.

// DriveSeries is the store series Drive's snapshots belong to.
const DriveSeries = "drive"

// ModeStore is a snapshot the store holds.
const ModeStore = "store"

// driveMeta is what a store snapshot of Drive records about itself.
type driveMeta struct {
	Kind     string   `json:"kind"` // "drive"
	Root     string   `json:"root"`
	Excluded []string `json:"excluded,omitempty"`
	Torn     []string `json:"torn,omitempty"`
}

// snapEntries is one store snapshot's entries, indexed for browsing.
type snapEntries struct {
	list   []chunkstore.Entry
	byPath map[string]int
	kids   map[string][]int
}

// entryCache keeps the last snapshot browsed: opening folder after folder
// of one snapshot reads its entry list once.
type entryCache struct {
	mu   sync.Mutex
	id   string
	ents *snapEntries
}

func (m *Manager) storeEntries(cs *chunkstore.Store, row *store.FileSnapshot) (*chunkstore.Snapshot, *snapEntries, error) {
	snap, err := cs.Snapshot(row.StoreSnapshot)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrNotFound, err)
	}
	m.entries.mu.Lock()
	defer m.entries.mu.Unlock()
	if m.entries.id == snap.ID {
		return snap, m.entries.ents, nil
	}
	se := &snapEntries{byPath: map[string]int{}, kids: map[string][]int{}}
	err = cs.Walk(snap, func(e chunkstore.Entry) error {
		i := len(se.list)
		se.list = append(se.list, e)
		se.byPath[e.Path] = i
		parent, _ := Split(e.Path)
		se.kids[parent] = append(se.kids[parent], i)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	m.entries.id, m.entries.ents = snap.ID, se
	return snap, se, nil
}

// under lists the entries at or below rel, in pre-order.
func (se *snapEntries) under(rel string) []chunkstore.Entry {
	var out []chunkstore.Entry
	for _, e := range se.list {
		if rel == "" || e.Path == rel || strings.HasPrefix(e.Path, rel+"/") {
			out = append(out, e)
		}
	}
	return out
}

func (m *Manager) chunkStore() (*chunkstore.Store, error) {
	if m.ChunkStore == nil {
		return nil, errors.New("no backup store")
	}
	return m.ChunkStore()
}

// runStoreSnapshot writes tonight's Drive snapshot into the store.
func (m *Manager) runStoreSnapshot(ctx context.Context, p *jobs.Progress) (store.FileSnapshot, error) {
	base, err := m.snapshotsDir()
	if err != nil {
		return store.FileSnapshot{}, err
	}
	root, err := m.openRoot()
	if err != nil {
		return store.FileSnapshot{}, err
	}
	for _, folder := range placeable {
		if reason := m.placementOffline(folder); reason != "" {
			return store.FileSnapshot{}, fmt.Errorf("%s lives at %s, which is not available (%s); skipping the snapshot", folder, m.Location(folder), reason)
		}
	}
	cs, err := m.chunkStore()
	if err != nil {
		return store.FileSnapshot{}, err
	}
	rows, err := m.Store.ListFileSnapshots()
	if err != nil {
		return store.FileSnapshot{}, err
	}
	// What the newest store snapshot holds, by path: a file unchanged since
	// is carried over without being read.
	prev := map[string]chunkstore.Entry{}
	for i := range rows {
		if rows[i].StoreSnapshot == "" {
			continue
		}
		if _, se, err := m.storeEntries(cs, &rows[i]); err == nil {
			for _, e := range se.list {
				if e.Type == chunkstore.TypeFile {
					prev[e.Path] = e
				}
			}
		}
		break
	}
	taken := map[string]bool{}
	for _, r := range rows {
		taken[filepath.Base(r.Path)] = true
	}
	stamp := fsx.FreeStamp(time.Now(), func(s string) bool { return taken[s] })
	es := m.excludes()
	if ix, err := m.Index(); err == nil {
		if st, err := ix.Stats(); err == nil {
			p.Set("starting", 0, st.Bytes)
		}
	}
	w, err := cs.Begin(DriveSeries)
	if err != nil {
		return store.FileSnapshot{}, err
	}
	defer w.Abort()
	gauge := &spaceGauge{dir: cs.Dir()}
	meta := driveMeta{Kind: "drive", Root: m.Root(), Excluded: es.sorted}
	var tooDeep []string
	var bytes int64
	walkErr := m.snapWalk(ctx, root, "", func(p string) { tooDeep = append(tooDeep, p) }, func(rel string, info fs.FileInfo) error {
		if es.prunes(rel) || (m.BackupSkip != nil && rel != "" && m.BackupSkip(rel)) {
			if info.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if rel == "" {
			return nil
		}
		if info.IsDir() {
			return w.Record(chunkstore.Entry{Path: rel, Type: chunkstore.TypeDir, Mode: uint32(info.Mode().Perm()), MTime: info.ModTime().UnixNano()})
		}
		if !info.Mode().IsRegular() {
			return nil // symlinks and specials are not backed up
		}
		p.Current(rel)
		if old, ok := prev[rel]; ok && old.Size == info.Size() && old.MTime == info.ModTime().UnixNano() {
			old.Mode = uint32(info.Mode().Perm())
			if err := w.Record(old); err == nil {
				bytes += old.Size
				p.Add(old.Size, 1)
				return nil
			}
			// Its chunks are gone from the store (collected after a
			// repair): read it afresh.
		}
		if err := gauge.reserve(info.Size()); err != nil {
			m.diskFull(info.Size())
			m.Store.CreateNotice(NoticeFilesBackupNoSpace, "", "Drive backup stopped: the backup disk is nearly full",
				err.Error()+". Exclude large folders from Drive backups, free space there, or move the backup directory to a bigger disk.")
			return err
		}
		e, torn, err := storeFile(ctx, w, root, rel, info)
		if errors.Is(err, ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
			return nil // deleted between the listing and the read
		}
		if err != nil {
			return fmt.Errorf("reading %s: %w", rel, err)
		}
		if torn {
			meta.Torn = append(meta.Torn, rel)
		}
		bytes += e.Size
		p.Add(e.Size, 1)
		return w.Record(e)
	})
	if walkErr != nil {
		return store.FileSnapshot{}, walkErr
	}
	mb, _ := json.Marshal(meta)
	snap, err := w.Commit(stamp, time.Now(), mb)
	if err != nil {
		return store.FileSnapshot{}, err
	}
	id, err := m.Store.CreateStoreFileSnapshot(filepath.Join(base, stamp), snap.ID, "", int64(snap.Files), bytes, snap.New, len(meta.Torn))
	if err != nil {
		cs.Delete(snap.ID)
		return store.FileSnapshot{}, err
	}
	m.Store.DismissNoticeByKind(NoticeFilesBackupStale, "")
	m.Store.DismissNoticeByKind(NoticeFilesBackupNoSpace, "")
	m.Store.DismissNoticeByKind(NoticeFilesBackupNoLinks, "")
	m.noteTooDeep(tooDeep)
	m.pruneStoreSnapshots(cs)
	m.importClassicSnapshots(ctx, cs, base, 3*time.Hour)
	row, err := m.Store.FileSnapshotByID(id)
	if err != nil {
		return store.FileSnapshot{}, err
	}
	m.Log.Info("files snapshot written", "stamp", stamp, "files", snap.Files, "bytes", bytes, "new", snap.New, "torn", len(meta.Torn), "mode", ModeStore)
	return *row, nil
}

// storeFile reads one file into the store, re-reading it if it changed
// while it was read (three tries) and reporting it torn if it never held
// still — the same rule the copies the store replaced followed.
func storeFile(ctx context.Context, w *chunkstore.Writer, root *os.Root, rel string, info fs.FileInfo) (chunkstore.Entry, bool, error) {
	e := chunkstore.Entry{Path: rel, Type: chunkstore.TypeFile, Mode: uint32(info.Mode().Perm())}
	for attempt := 0; attempt < 4; attempt++ {
		if err := ctx.Err(); err != nil {
			return e, false, err
		}
		in, _, err := openFileNF(root, rel)
		if err != nil {
			return e, false, err
		}
		chunks, size, sum, err := w.Content(in)
		in.Close()
		if err != nil {
			return e, false, err
		}
		e.Chunks, e.Size, e.SHA256, e.MTime = chunks, size, sum, info.ModTime().UnixNano()
		after, err := lstatNF(root, rel)
		if err != nil {
			return e, false, err
		}
		if after.Size() == info.Size() && after.ModTime().Equal(info.ModTime()) && size == info.Size() {
			return e, false, nil
		}
		info = after
	}
	return e, true, nil
}

// pruneStoreSnapshots applies the history policy apps follow to Drive's
// store snapshots; the newest always stays.
func (m *Manager) pruneStoreSnapshots(cs *chunkstore.Store) {
	rows, err := m.Store.ListFileSnapshots()
	if err != nil {
		return
	}
	var series []store.FileSnapshot
	var at []string
	for _, r := range rows {
		if r.Mode == ModeStore {
			series = append(series, r)
			at = append(at, r.CreatedAt)
		}
	}
	hist, _ := m.Store.GetSetting(backup.SettingBackupHistory)
	keep := backup.KeepByHistory(at, hist)
	for i, r := range series {
		if i == 0 || keep[i] {
			continue
		}
		if err := cs.Delete(r.StoreSnapshot); err != nil {
			m.Log.Warn("files snapshot prune", "snapshot", r.StoreSnapshot, "err", err)
			continue
		}
		m.Store.DeleteFileSnapshot(r.ID)
	}
}

// ThinOldest removes the oldest Drive store snapshot when there are more
// than three — the disk-space guard's lever on Drive. It reports whether
// it removed one.
func (m *Manager) ThinOldest() bool {
	cs, err := m.chunkStore()
	if err != nil {
		return false
	}
	rows, err := m.Store.ListFileSnapshots()
	if err != nil {
		return false
	}
	var series []store.FileSnapshot
	for _, r := range rows {
		if r.Mode == ModeStore {
			series = append(series, r)
		}
	}
	if len(series) <= 3 {
		return false
	}
	oldest := series[len(series)-1]
	if cs.Delete(oldest.StoreSnapshot) != nil {
		return false
	}
	m.Store.DeleteFileSnapshot(oldest.ID)
	return true
}

// importClassicSnapshots brings the hardlink snapshots and the mirror that
// predate the store into it, oldest first, then removes their trees — so
// Drive's history carries over and the space they held comes back. A file
// hardlinked from one night to the next is read once: the inode says it is
// the same file. Bounded by budget; the rest carries on the next night.
func (m *Manager) importClassicSnapshots(ctx context.Context, cs *chunkstore.Store, base string, budget time.Duration) {
	rows, err := m.Store.ListFileSnapshots()
	if err != nil {
		return
	}
	deadline := time.Now().Add(budget)
	byInode := map[[2]uint64]chunkstore.Entry{}
	mirrorDone := false
	for i := len(rows) - 1; i >= 0; i-- {
		r := rows[i]
		if ctx.Err() != nil || time.Now().After(deadline) {
			return
		}
		switch r.Mode {
		case "links":
		case "mirror":
			if mirrorDone || r.ID != newestMirror(rows) {
				if r.ID != newestMirror(rows) {
					m.Store.DeleteFileSnapshot(r.ID) // a mirror's older rows were history of runs, not copies
				}
				continue
			}
		default:
			continue
		}
		if _, err := os.Stat(r.Path); err != nil {
			continue
		}
		id, err := m.importTree(ctx, cs, r, byInode)
		if err != nil {
			m.Log.Warn("files snapshot: importing into the backup store", "path", r.Path, "err", err)
			return
		}
		if err := m.Store.SetFileSnapshotStore(r.ID, id); err != nil {
			return
		}
		if r.Mode == "mirror" {
			mirrorDone = true
			// What the mirror set aside as deleted is the only way back to
			// a file removed in the last 30 days: it stays, beside the
			// store, until it ages out as it always did.
			os.Rename(filepath.Join(r.Path, mirrorDeleted), filepath.Join(base, keptDeletedDir))
		}
		if err := os.RemoveAll(r.Path); err != nil {
			m.Log.Warn("files snapshot: removing an imported tree", "path", r.Path, "err", err)
		}
	}
	m.pruneKeptDeleted(base)
	os.Remove(filepath.Join(base, "latest"))
	os.Remove(base) // only once empty
}

// keptDeletedDir holds an imported mirror's set-aside deletions,
// <stamp>/<path> as the mirror kept them, until they are 30 days old.
const keptDeletedDir = "mirror-deleted"

func (m *Manager) pruneKeptDeleted(base string) {
	dir := filepath.Join(base, keptDeletedDir)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-mirrorDeletedKeep)
	for _, e := range ents {
		if t, ok := fsx.ParseStamp(e.Name()); ok && t.Before(cutoff) {
			os.RemoveAll(filepath.Join(dir, e.Name()))
		}
	}
	os.Remove(dir) // only once empty
}

func newestMirror(rows []store.FileSnapshot) int64 {
	for _, r := range rows {
		if r.Mode == "mirror" {
			return r.ID
		}
	}
	return 0
}

// importTree writes one snapshot directory into the store.
func (m *Manager) importTree(ctx context.Context, cs *chunkstore.Store, r store.FileSnapshot, byInode map[[2]uint64]chunkstore.Entry) (string, error) {
	w, err := cs.Begin(DriveSeries)
	if err != nil {
		return "", err
	}
	defer w.Abort()
	var torn []string
	err = filepath.WalkDir(r.Path, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, _ := filepath.Rel(r.Path, path)
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if rel == snapshotManifest || rel == mirrorDeleted {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			return w.Record(chunkstore.Entry{Path: rel, Type: chunkstore.TypeDir, Mode: uint32(info.Mode().Perm()), MTime: info.ModTime().UnixNano()})
		}
		if !d.Type().IsRegular() {
			return nil
		}
		var key [2]uint64
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			key = [2]uint64{uint64(st.Dev), uint64(st.Ino)}
			if e, ok := byInode[key]; ok && e.Size == info.Size() {
				e.Path = rel
				return w.Record(e)
			}
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		chunks, size, sum, err := w.Content(f)
		f.Close()
		if err != nil {
			return err
		}
		e := chunkstore.Entry{Path: rel, Type: chunkstore.TypeFile, Mode: uint32(info.Mode().Perm()),
			MTime: info.ModTime().UnixNano(), Size: size, SHA256: sum, Chunks: chunks}
		if key != [2]uint64{} {
			byInode[key] = e
		}
		return w.Record(e)
	})
	if err != nil {
		return "", err
	}
	created, _ := time.Parse(time.RFC3339, r.CreatedAt)
	mb, _ := json.Marshal(driveMeta{Kind: "drive", Root: m.Root(), Torn: torn})
	snap, err := w.Commit(filepath.Base(r.Path), created, mb)
	if err != nil {
		return "", err
	}
	// The tree is deleted once this returns: prove the copy first.
	if err := chunkstore.ReadBack(ctx, cs, snap); err != nil {
		cs.Delete(snap.ID)
		return "", fmt.Errorf("the imported copy does not read back: %w", err)
	}
	return snap.ID, nil
}

// ---- browsing, opening, restoring a store snapshot ----

func entryNode(e chunkstore.Entry) Node {
	_, name := Split(e.Path)
	mode := fs.FileMode(e.Mode).Perm()
	if e.Type == chunkstore.TypeDir {
		mode |= fs.ModeDir
	}
	t := time.Unix(0, e.MTime)
	n := Node{Path: e.Path, Name: name, Kind: kindOf(mode), mtime: e.MTime}
	n.Modified = t.UTC().Format(time.RFC3339)
	n.Mode = mode.String()
	if n.Kind == "file" {
		n.Size = e.Size
		n.Mime = MimeFor(n.Name)
	}
	return n
}

func (m *Manager) storeList(row *store.FileSnapshot, rel string, o ListOpts) (Listing, error) {
	cs, err := m.chunkStore()
	if err != nil {
		return Listing{}, err
	}
	_, se, err := m.storeEntries(cs, row)
	if err != nil {
		return Listing{}, err
	}
	dir := Node{Path: "", Kind: "dir", Mode: fs.ModeDir.String()}
	if rel != "" {
		i, ok := se.byPath[rel]
		if !ok {
			return Listing{}, ErrNotFound
		}
		if se.list[i].Type != chunkstore.TypeDir {
			return Listing{}, ErrNotDirectory
		}
		dir = entryNode(se.list[i])
	}
	var nodes []Node
	for _, i := range se.kids[rel] {
		n := entryNode(se.list[i])
		if n.Kind == "dir" {
			n.Entries = len(se.kids[n.Path])
		}
		nodes = append(nodes, n)
	}
	if o.Sort == "" {
		o.Sort = SortName
	}
	sortNodes(nodes, o.Sort, o.Desc)
	if o.Limit <= 0 || o.Limit > 2000 {
		o.Limit = 500
	}
	start, _ := strconv.Atoi(o.Cursor)
	if start < 0 || start > len(nodes) {
		start = 0
	}
	end := start + o.Limit
	next := ""
	if end < len(nodes) {
		next = strconv.Itoa(end)
	} else {
		end = len(nodes)
	}
	page := nodes[start:end]
	if page == nil {
		page = []Node{}
	}
	dir.Entries = len(nodes)
	l := Listing{Path: rel, Root: rel == "", Dir: dir, Entries: page, Next: next, Total: len(nodes), Sort: o.Sort, Desc: o.Desc}
	if rel != "" {
		l.Parent, _ = Split(rel)
	}
	return l, nil
}

// storeOpenFile rebuilds one file into the store's scratch space and opens
// it; the name is removed at once, so the open file is all that is left.
func (m *Manager) storeOpenFile(row *store.FileSnapshot, rel string) (*os.File, Node, error) {
	if rel == "" {
		return nil, Node{}, ErrIsDirectory
	}
	cs, err := m.chunkStore()
	if err != nil {
		return nil, Node{}, err
	}
	_, se, err := m.storeEntries(cs, row)
	if err != nil {
		return nil, Node{}, err
	}
	i, ok := se.byPath[rel]
	if !ok {
		return nil, Node{}, ErrNotFound
	}
	e := se.list[i]
	if e.Type != chunkstore.TypeFile {
		return nil, Node{}, ErrIsDirectory
	}
	dir, err := cs.TempDir("open-*")
	if err != nil {
		return nil, Node{}, err
	}
	defer os.RemoveAll(dir)
	f, err := os.Create(filepath.Join(dir, "f"))
	if err != nil {
		return nil, Node{}, err
	}
	r, _ := cs.Open(e)
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return nil, Node{}, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return nil, Node{}, err
	}
	return f, entryNode(e), nil
}

func (m *Manager) storeRestore(row *store.FileSnapshot, rels []string, to string, toSet bool, c Conflict, release func()) (jobs.Job, error) {
	cs, err := m.chunkStore()
	if err != nil {
		release()
		return jobs.Job{}, err
	}
	_, se, err := m.storeEntries(cs, row)
	if err != nil {
		release()
		return jobs.Job{}, err
	}
	root, ix, err := m.rootAndIndex()
	if err != nil {
		release()
		return jobs.Job{}, err
	}
	if toSet {
		if err := requireDir(root, to); err != nil {
			release()
			return jobs.Job{}, err
		}
	}
	for _, rel := range rels {
		if _, ok := se.byPath[rel]; !ok && rel != "" {
			release()
			return jobs.Job{}, ErrNotFound
		}
		dest := restoreDest(rel, to, toSet)
		if c == ConflictAsk && dest != "" && exists(root, dest) {
			m.refreshDir(root, ix, parentOfRel(dest), false)
			n, _ := ix.Get(dest)
			release()
			return jobs.Job{}, &ConflictError{Path: dest, Existing: n}
		}
	}
	label := fmt.Sprintf("Restore from %s", row.CreatedAt[:10])
	return m.Jobs.Start("restore", label, func(ctx context.Context, p *jobs.Progress) error {
		defer release()
		var total int64
		for _, rel := range rels {
			for _, e := range se.under(rel) {
				total += e.Size
			}
		}
		p.Set("", 0, total)
		var out []string
		om := &opMounts{}
		restore := func(src, parent, name string) error {
			d, err := m.restoreOneStore(ctx, cs, se, root, ix, src, parent, name, c, om, p)
			if err == nil && d != "" {
				out = append(out, d)
			}
			return err
		}
		for _, rel := range rels {
			if err := ctx.Err(); err != nil {
				return err
			}
			dest := restoreDest(rel, to, toSet)
			if dest == "" {
				for _, i := range se.kids[""] {
					_, name := Split(se.list[i].Path)
					if err := restore(se.list[i].Path, "", name); err != nil {
						return err
					}
				}
				continue
			}
			parent, name := Split(dest)
			if err := restore(rel, parent, name); err != nil {
				return err
			}
		}
		p.Result(map[string]any{"paths": out})
		m.kickHasher()
		return nil
	}), nil
}

func (m *Manager) restoreOneStore(ctx context.Context, cs *chunkstore.Store, se *snapEntries, root *os.Root, ix *Index, src, parent, name string, c Conflict, om *opMounts, p *jobs.Progress) (string, error) {
	if err := m.guardWrite(Join(parent, name)); err != nil {
		return "", err
	}
	if parent != "" {
		if err := mkdirAllNF(root, parent, 0o755); err != nil {
			return "", err
		}
	}
	dest, err := m.resolveDest(root, ix, parent, name, c, om)
	if err != nil || dest == "" {
		return "", err
	}
	p.Current(src)
	isDir := false
	for _, e := range se.under(src) {
		if err := ctx.Err(); err != nil {
			nf(root).RemoveAll(dest)
			return "", err
		}
		target := dest + strings.TrimPrefix(e.Path, src)
		if e.Type == chunkstore.TypeDir {
			if e.Path == src {
				isDir = true
			}
			if err := nf(root).Mkdir(target, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
				nf(root).RemoveAll(dest)
				return "", mapOSError(err)
			}
			continue
		}
		if e.Type != chunkstore.TypeFile {
			continue
		}
		if err := checkNoLinks(root, target); err != nil {
			nf(root).RemoveAll(dest)
			return "", err
		}
		r, _ := cs.Open(e)
		_, err := atomicWrite(nf(root), target, r, writeOpts{perm: 0o644, mtime: time.Unix(0, e.MTime), p: p})
		if err != nil {
			nf(root).RemoveAll(dest)
			return "", mapOSError(err)
		}
		p.Add(0, 1)
	}
	m.chownTree(root, dest)
	if isDir {
		m.walkDir(ctx, root, ix, dest, time.Now().UnixNano())
	}
	m.refreshDir(root, ix, parent, false)
	return dest, nil
}

// verifyStoreSnapshot reads a random sample of the snapshot's files back
// from the store — every chunk checked against its id, every file against
// its digest — and checks the store still holds every chunk it names.
func (m *Manager) verifyStoreSnapshot(ctx context.Context, row *store.FileSnapshot, now time.Time) {
	cs, err := m.chunkStore()
	if err != nil {
		return
	}
	snap, se, err := m.storeEntries(cs, row)
	fail := func(detail string) {
		m.Store.CreateNotice(NoticeFilesBackupCorrupt, "", "Drive snapshot failed verification", detail+" The backup disk may be failing; fog will repair what it can from the offsite copy, but consider checking the disk or share.")
		if m.Notify != nil {
			m.Notify("Drive snapshot failed verification", detail, false)
		}
	}
	if err != nil {
		fail(fmt.Sprintf("The newest Drive snapshot could not be read: %v.", err))
		return
	}
	if missing, err := cs.Missing(snap); err != nil || len(missing) > 0 {
		fail(fmt.Sprintf("The newest Drive snapshot refers to %d pieces the backup store no longer has.", len(missing)))
		return
	}
	var files []chunkstore.Entry
	for _, e := range se.list {
		if e.Type == chunkstore.TypeFile {
			files = append(files, e)
		}
	}
	rng := rand.New(rand.NewSource(now.UnixNano()))
	rng.Shuffle(len(files), func(i, j int) { files[i], files[j] = files[j], files[i] })
	n := min(max(len(files)/100, 5), verifySampleMax, len(files))
	var bad []string
	for _, e := range files[:n] {
		if ctx.Err() != nil {
			return
		}
		r, _ := cs.Open(e)
		if _, err := io.Copy(io.Discard, r); err != nil {
			bad = append(bad, e.Path)
		}
	}
	if len(bad) > 0 {
		fail(fmt.Sprintf("%d of %d sampled files in the newest Drive snapshot no longer read back as they were written (for example %s).", len(bad), n, DisplayName(bad[0])))
		return
	}
	m.Store.DismissNoticeByKind(NoticeFilesBackupCorrupt, "")
	m.Store.SetFileSnapshotVerified(row.ID)
}

// RepointStoreSnapshots follows the store being rewritten (encryption
// turned on or off): rows are pointed at the new snapshot ids.
func (m *Manager) RepointStoreSnapshots(moved map[string]string) {
	for from, to := range moved {
		m.Store.RepointFileSnapshotStore(from, to)
	}
	m.entries.mu.Lock()
	m.entries.id, m.entries.ents = "", nil
	m.entries.mu.Unlock()
}

// sortedKids is the entries of a store snapshot directly under rel, names
// only (tests).
func (se *snapEntries) sortedKids(rel string) []string {
	var out []string
	for _, i := range se.kids[rel] {
		_, n := Split(se.list[i].Path)
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
