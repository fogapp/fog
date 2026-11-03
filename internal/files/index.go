package files

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// The index is its own SQLite file, not a set of tables in fog.db. The
// store deliberately runs on one connection; a walk that upserts a million
// rows through it would stall every other request in fog for the duration.
// Here readers and the single writer are separate connections, and because
// the index is a cache of the tree — rebuildable from a walk at any time —
// a schema change simply drops and recreates it instead of migrating.
const indexSchemaVersion = 2

const indexSchema = `
CREATE TABLE nodes (
    id        INTEGER PRIMARY KEY,
    parent    TEXT NOT NULL,             -- root-relative dir path, '' for the root
    name      TEXT NOT NULL,             -- '' only for the root row itself
    kind      TEXT NOT NULL,             -- file | dir | other
    size      INTEGER NOT NULL,          -- files: bytes; dirs: subtree bytes as of walked_at
    mtime     INTEGER NOT NULL,          -- unix nanoseconds
    mode      INTEGER NOT NULL,
    inode     INTEGER NOT NULL,
    mime      TEXT NOT NULL DEFAULT '',
    sha256    TEXT NOT NULL DEFAULT '',  -- '' until hashed
    dir_mtime INTEGER NOT NULL DEFAULT 0, -- dirs: the mtime the listing was taken at
    entries   INTEGER NOT NULL DEFAULT 0, -- dirs: direct child count
    walked_at INTEGER NOT NULL DEFAULT 0, -- dirs: when the subtree was last fully sized (unix nanos)
    dehydrated INTEGER NOT NULL DEFAULT 0, -- files: listed here, bytes at a peer (a previews subscription)
    UNIQUE (parent, name)
);
CREATE INDEX nodes_parent_name  ON nodes(parent, name COLLATE NOCASE);
CREATE INDEX nodes_parent_mtime ON nodes(parent, mtime);
CREATE INDEX nodes_parent_size  ON nodes(parent, size);
CREATE INDEX nodes_sha          ON nodes(sha256) WHERE sha256 <> '';
CREATE INDEX nodes_unhashed     ON nodes(kind) WHERE sha256 = '' AND kind = 'file' AND dehydrated = 0;
CREATE VIRTUAL TABLE nodes_fts USING fts5(name, content='nodes', content_rowid='id', tokenize='trigram');
CREATE TRIGGER nodes_ai AFTER INSERT ON nodes BEGIN
    INSERT INTO nodes_fts(rowid, name) VALUES (new.id, new.name);
END;
CREATE TRIGGER nodes_ad AFTER DELETE ON nodes BEGIN
    INSERT INTO nodes_fts(nodes_fts, rowid, name) VALUES ('delete', old.id, old.name);
END;
CREATE TRIGGER nodes_au AFTER UPDATE OF name ON nodes BEGIN
    INSERT INTO nodes_fts(nodes_fts, rowid, name) VALUES ('delete', old.id, old.name);
    INSERT INTO nodes_fts(rowid, name) VALUES (new.id, new.name);
END;
CREATE TABLE meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
`

// Index is the connection pool plus the one writer lock. SQLite serializes
// writers itself (busy_timeout), but holding the lock in Go keeps a walk's
// per-directory transactions from interleaving with a listing's refresh in
// ways that produce confusing "database is locked" errors under -race.
type Index struct {
	db  *sql.DB
	wmu sync.Mutex
	// writes counts the transactions write has committed — the seam tests
	// use to check that a listing which found nothing new wrote nothing.
	writes atomic.Int64
	// corrupt is set the first time SQLite reports the file damaged; the
	// Manager then replaces the whole file (Manager.Index).
	corrupt atomic.Bool
}

// note flags the index corrupt when err says so. The index is a cache of
// the tree, so a damaged file is thrown away and rebuilt by a walk rather
// than repaired — jen's, its full-text table torn by an fsck, failed every
// listing with "database disk image is malformed" until it was moved aside
// by hand.
func (ix *Index) note(err error) {
	if isCorrupt(err) {
		ix.corrupt.Store(true)
	}
}

func isCorrupt(err error) bool {
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return false
	}
	code := se.Code() & 0xff // the primary code, without the extended bits
	return code == sqlite3.SQLITE_CORRUPT || code == sqlite3.SQLITE_NOTADB
}

// Corrupt reports whether SQLite has said the file is damaged.
func (ix *Index) Corrupt() bool { return ix.corrupt.Load() }

// write runs fn in one transaction under the writer lock, committing when
// it returns nil and rolling back otherwise.
func (ix *Index) write(fn func(tx *sql.Tx) error) (err error) {
	defer func() { ix.note(err) }()
	ix.wmu.Lock()
	defer ix.wmu.Unlock()
	tx, err := ix.db.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	ix.writes.Add(1)
	return nil
}

func sqliteURIPath(path string) string {
	segs := strings.Split(path, "/")
	for i, seg := range segs {
		segs[i] = strings.ReplaceAll(strings.ReplaceAll(seg, "?", "%3F"), "#", "%23")
	}
	return strings.Join(segs, "/")
}

// OpenIndex opens (or creates, or recreates on a schema change) the index
// database at path.
func OpenIndex(path string) (*Index, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)", sqliteURIPath(path))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	ix := &Index{db: db}
	if err := ix.ensureSchema(); err != nil {
		db.Close()
		return nil, err
	}
	return ix, nil
}

func (ix *Index) Close() error { return ix.db.Close() }

func (ix *Index) ensureSchema() error {
	var v int
	if err := ix.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	if v == indexSchemaVersion {
		return nil
	}
	return ix.Reset()
}

// Reset drops everything and recreates the schema. The next walk refills
// it. Used on a schema bump and when the root changes.
func (ix *Index) Reset() error {
	ix.wmu.Lock()
	defer ix.wmu.Unlock()
	stmts := []string{
		`DROP TRIGGER IF EXISTS nodes_ai`, `DROP TRIGGER IF EXISTS nodes_ad`, `DROP TRIGGER IF EXISTS nodes_au`,
		`DROP TABLE IF EXISTS nodes_fts`, `DROP TABLE IF EXISTS nodes`, `DROP TABLE IF EXISTS meta`,
	}
	for _, s := range stmts {
		if _, err := ix.db.Exec(s); err != nil {
			return err
		}
	}
	// One Exec runs the whole script (the driver iterates statements), which
	// matters because trigger bodies contain semicolons of their own.
	if _, err := ix.db.Exec(indexSchema); err != nil {
		return fmt.Errorf("index schema: %w", err)
	}
	_, err := ix.db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, indexSchemaVersion))
	return err
}

// Node is one entry as the API sees it. Times are RFC3339 UTC strings like
// the rest of fog's API; the index keeps nanoseconds for sorting.
type Node struct {
	Path     string `json:"path"`
	Name     string `json:"name"`
	Kind     string `json:"kind" ts:"\"file\" | \"dir\" | \"other\""` // file | dir | other
	Size     int64  `json:"size"`
	Modified string `json:"modified"`
	Mode     string `json:"mode,omitempty"`
	Mime     string `json:"mime,omitempty"`
	SHA256   string `json:"sha256,omitempty"`
	// Directories only.
	Entries int `json:"entries,omitempty"`
	// WalkedAt is when a directory's subtree size was last fully computed;
	// absent means the size is a lower bound from the entries seen so far.
	WalkedAt string `json:"walkedAt,omitempty"`
	// Excluded from the nightly snapshot — directly, or (ExcludedBy) by an
	// ancestor folder.
	Excluded   bool   `json:"excluded,omitempty"`
	ExcludedBy string `json:"excludedBy,omitempty"`
	// Folders: apps whose volumes point at this folder or inside it, and
	// — for the folder itself — where each app sees it.
	MountedBy []string `json:"mountedBy,omitempty"`
	Mounts    []Mount  `json:"mounts,omitempty"`
	// Role names a folder fog gives a meaning to: the Apps and Content
	// folders at the root, an app's own folder directly inside Apps, a
	// library directly inside Content.
	Role string `json:"role,omitempty" ts:"\"apps\" | \"app-folder\" | \"content\" | \"library\""`
	// Federation facts, stamped by the files-across-peers subsystem: marked
	// "keep on this box" (directly or by an ancestor), the peers this entry
	// is shared with (a folder that is a share, or lies inside one), and —
	// for a copy of a peer's file — whose it is and whether the bytes are
	// here.
	Kept       bool      `json:"kept,omitempty"`
	KeptBy     string    `json:"keptBy,omitempty"`
	SharedWith []string  `json:"sharedWith,omitempty"`
	Peer       *PeerMark `json:"peer,omitempty"`
	// Dehydrated: the index lists it, the disk does not hold it — an entry
	// of a previews subscription, fetched from its peer when opened.
	Dehydrated bool `json:"dehydrated,omitempty"`

	id       int64
	mtime    int64
	dirMtime int64
	inode    int64
	modeBits uint32
	walkedAt int64
}

func (n Node) IsDir() bool { return n.Kind == "dir" }

// Mount is one app's view of a folder: the container path it sees the
// folder at, and whether the spec mounts it read-only.
type Mount struct {
	App      string `json:"app"`
	Mount    string `json:"mount"`
	ReadOnly bool   `json:"readOnly,omitempty"`
}

// Folder roles (Node.Role).
const (
	RoleApps      = "apps"       // the Apps folder at the root
	RoleAppFolder = "app-folder" // one app's own folder directly inside Apps
	RoleContent   = "content"    // the Content folder at the root
	RoleLibrary   = "library"    // one content library directly inside Content
)

// ModifiedNanos is the modification time as the index keeps it.
func (n Node) ModifiedNanos() int64 { return n.mtime }

// PeerMark says an entry is (or lies inside) a copy of a peer's share, or
// was contributed by a peer at this origin.
type PeerMark struct {
	Peer         string `json:"peer"`                                     // display label
	Instance     string `json:"instance"`                                 // instance ID
	Kind         string `json:"kind" ts:"\"replica\" | \"contribution\""` // replica | contribution
	ReadOnly     bool   `json:"readOnly"`
	Hydrated     *bool  `json:"hydrated,omitempty"` // replicas: bytes present locally
	Subscription int64  `json:"subscription,omitempty"`
	Revoked      bool   `json:"revoked,omitempty"`
	Backup       bool   `json:"backup,omitempty"`                                            // replicas: included in the nightly files backup
	Detail       string `json:"detail,omitempty" ts:"\"full\" | \"previews\" | \"listing\""` // replicas: full | previews — what this box keeps
	Pinned       bool   `json:"pinned,omitempty"`                                            // replicas: kept offline by the operator
	// Contribute mode: the subscription's mode; an entry that is ours in a
	// peer's folder (editable here, theirs to hold), possibly turned away;
	// a third party's entry (Author), a stub when their bytes cannot reach
	// us; at the origin, a contribution whose author has since left.
	Mode     string `json:"mode,omitempty" ts:"\"read\" | \"contribute\""`
	Mine     bool   `json:"mine,omitempty"`
	Rejected bool   `json:"rejected,omitempty"`
	Author   string `json:"author,omitempty"`
	Stub     bool   `json:"stub,omitempty"`
}

const nodeCols = `id, parent, name, kind, size, mtime, mode, inode, mime, sha256, dir_mtime, entries, walked_at, dehydrated`

type rowScanner interface{ Scan(...any) error }

func scanNode(row rowScanner) (Node, error) {
	var n Node
	var parent string
	var mode int64
	var dehydrated int
	err := row.Scan(&n.id, &parent, &n.Name, &n.Kind, &n.Size, &n.mtime, &mode, &n.inode, &n.Mime, &n.SHA256, &n.dirMtime, &n.Entries, &n.walkedAt, &dehydrated)
	if err != nil {
		return n, err
	}
	n.Dehydrated = dehydrated != 0
	n.Path = Join(parent, n.Name)
	if n.SHA256 == unhashable {
		n.SHA256 = ""
	}
	n.modeBits = uint32(mode)
	n.Modified = nanosToRFC3339(n.mtime)
	n.Mode = modeString(n.Kind, n.modeBits)
	if n.Kind == "dir" && n.walkedAt > 0 {
		n.WalkedAt = nanosToRFC3339(n.walkedAt)
	}
	return n, nil
}

func nanosToRFC3339(ns int64) string {
	if ns == 0 {
		return ""
	}
	return time.Unix(0, ns).UTC().Format(time.RFC3339)
}

// Get returns the row for rel, or ErrNotFound.
func (ix *Index) Get(rel string) (_ Node, err error) {
	defer func() { ix.note(err) }()
	parent, name := Split(rel)
	n, err := scanNode(ix.db.QueryRow(`SELECT `+nodeCols+` FROM nodes WHERE parent = ? AND name = ?`, parent, name))
	if errors.Is(err, sql.ErrNoRows) {
		return n, ErrNotFound
	}
	return n, err
}

// children returns every direct child of dir keyed by name.
func (ix *Index) children(dir string) (_ map[string]Node, err error) {
	defer func() { ix.note(err) }()
	rows, err := ix.db.Query(`SELECT `+nodeCols+` FROM nodes WHERE parent = ? AND name <> ''`, dir)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Node{}
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out[n.Name] = n
	}
	return out, rows.Err()
}

// Sort orders a listing accepts.
type Sort string

const (
	SortName  Sort = "name"
	SortMtime Sort = "mtime"
	SortSize  Sort = "size"
	SortKind  Sort = "kind"
)

// ListOpts drives one page of a directory listing.
type ListOpts struct {
	Sort   Sort
	Desc   bool
	Cursor string
	Limit  int
	// ShowHidden and ShowSystem lift the two default exclusions: dotfiles,
	// and the bookkeeping other systems leave in every folder. Manager.List
	// fills them from the settings; the zero value hides both.
	ShowHidden bool
	ShowSystem bool
}

// Page is one page of children. Next is the cursor for the following page,
// empty on the last one.
type Page struct {
	Entries []Node
	Next    string
	Total   int
}

// Directories always come first, in every sort — that is what both the
// path picker and every desktop file manager do, and it keeps the folder
// you are looking for out of a sea of files.
type cursor struct {
	Group int    `json:"g"`
	Key   string `json:"k"`
	ID    int64  `json:"i"`
}

func encodeCursor(c cursor) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(s string) (cursor, bool) {
	var c cursor
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || json.Unmarshal(b, &c) != nil {
		return c, false
	}
	return c, true
}

func sortColumn(s Sort) (col string, numeric bool) {
	switch s {
	case SortMtime:
		return "mtime", true
	case SortSize:
		return "size", true
	case SortKind:
		return "mime", false
	default:
		return "name", false
	}
}

func keyOf(n Node, s Sort) string {
	switch s {
	case SortMtime:
		return strconv.FormatInt(n.mtime, 10)
	case SortSize:
		return strconv.FormatInt(n.Size, 10)
	case SortKind:
		return n.Mime
	default:
		return n.Name
	}
}

// List returns one page of dir's children using keyset pagination over
// (group, sort key, id): a page never repeats or skips an entry because
// rows were added or removed while the user scrolled — the failure mode of
// OFFSET paging on a live directory.
func (ix *Index) List(dir string, o ListOpts) (_ Page, err error) {
	defer func() { ix.note(err) }()
	if o.Limit <= 0 || o.Limit > 2000 {
		o.Limit = 500
	}
	col, numeric := sortColumn(o.Sort)
	dirn := ">"
	order := "ASC"
	if o.Desc {
		dirn, order = "<", "DESC"
	}
	collate := ""
	if !numeric {
		collate = " COLLATE NOCASE"
	}
	grp := `CASE WHEN kind = 'dir' THEN 0 ELSE 1 END`
	vis, visArgs := visibilityFilter(o, "name")
	q := `SELECT ` + nodeCols + ` FROM nodes WHERE parent = ? AND name <> ''` + vis
	args := append([]any{dir}, visArgs...)
	if c, ok := decodeCursor(o.Cursor); ok && o.Cursor != "" {
		var key any = c.Key
		if numeric {
			key, _ = strconv.ParseInt(c.Key, 10, 64)
		}
		q += fmt.Sprintf(` AND (%s > ? OR (%s = ? AND (%s %s ?%s OR (%s = ?%s AND id > ?))))`,
			grp, grp, col, dirn, collate, col, collate)
		args = append(args, c.Group, c.Group, key, key, c.ID)
	}
	q += fmt.Sprintf(` ORDER BY %s ASC, %s%s %s, id ASC LIMIT ?`, grp, col, collate, order)
	args = append(args, o.Limit+1)

	rows, err := ix.db.Query(q, args...)
	if err != nil {
		return Page{}, err
	}
	defer rows.Close()
	var p Page
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return Page{}, err
		}
		p.Entries = append(p.Entries, n)
	}
	if err := rows.Err(); err != nil {
		return Page{}, err
	}
	if len(p.Entries) > o.Limit {
		p.Entries = p.Entries[:o.Limit]
		last := p.Entries[len(p.Entries)-1]
		g := 1
		if last.IsDir() {
			g = 0
		}
		p.Next = encodeCursor(cursor{Group: g, Key: keyOf(last, o.Sort), ID: last.id})
	}
	if p.Entries == nil {
		p.Entries = []Node{}
	}
	countArgs := append([]any{dir}, visArgs...)
	if err := ix.db.QueryRow(`SELECT COUNT(*) FROM nodes WHERE parent = ? AND name <> ''`+vis, countArgs...).Scan(&p.Total); err != nil {
		return Page{}, err
	}
	return p, nil
}

// Search finds entries whose name contains q (case-insensitive), optionally
// under a directory and of one kind. Trigram FTS answers substring queries
// of three or more characters in one index probe; shorter queries fall back
// to a prefix LIKE, which the (parent, name) index makes tolerable.
func (ix *Index) Search(q, under, kind string, limit int, o ListOpts) (_ []Node, err error) {
	return ix.search(q, under, kind, limit, o, nil)
}

// searchMaxQuery bounds a search string: a name is at most 255 bytes, so
// nothing longer can match, and an FTS query costs by its length.
const searchMaxQuery = 255

// SearchScope narrows a search to what one reader may see: rows under any
// of Hide are left out, unless they are also under one of Show. HideDot
// also leaves out everything inside a folder whose name starts with a dot
// (the listing filter only looks at an entry's own name).
type SearchScope struct {
	Hide, Show []string
	HideDot    bool
}

// pathUnder is a WHERE fragment matching rows whose full path is dir or
// lies under it.
func pathUnder(dir string) (string, []any) {
	parent, name := Split(dir)
	w, args := subtree("n.parent", dir)
	return "((n.parent = ? AND n.name = ?) OR " + w + ")", append([]any{parent, name}, args...)
}

func anyUnder(dirs []string) (string, []any) {
	var parts []string
	var args []any
	for _, d := range dirs {
		w, a := pathUnder(d)
		parts = append(parts, w)
		args = append(args, a...)
	}
	return "(" + strings.Join(parts, " OR ") + ")", args
}

func (ix *Index) search(q, under, kind string, limit int, o ListOpts, sc *SearchScope) (_ []Node, err error) {
	defer func() { ix.note(err) }()
	q = strings.TrimSpace(q)
	if q == "" || len(q) > searchMaxQuery {
		return []Node{}, nil
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var (
		sqlq string
		args []any
	)
	where := ` AND n.name <> ''`
	if under != "" {
		w, a := subtree("n.parent", under)
		where += ` AND ` + w
		args = append(args, a...)
	}
	if kind != "" {
		where += ` AND n.kind = ?`
		args = append(args, kind)
	}
	vis, visArgs := visibilityFilter(o, "n.name")
	where += vis
	args = append(args, visArgs...)
	if sc != nil {
		if len(sc.Hide) > 0 {
			h, ha := anyUnder(sc.Hide)
			if len(sc.Show) > 0 {
				sh, sa := anyUnder(sc.Show)
				h, ha = "("+h+" AND NOT "+sh+")", append(ha, sa...)
			}
			where += " AND NOT " + h
			args = append(args, ha...)
		}
		if sc.HideDot {
			where += ` AND n.parent NOT LIKE '.%' AND n.parent NOT LIKE '%/.%'`
		}
	}
	if len([]rune(q)) >= 3 {
		sqlq = `SELECT ` + qualify(nodeCols, "n") + ` FROM nodes_fts f JOIN nodes n ON n.id = f.rowid WHERE nodes_fts MATCH ?` + where +
			` ORDER BY length(n.name) ASC, n.name COLLATE NOCASE ASC LIMIT ?`
		args = append([]any{ftsLiteral(q)}, args...)
	} else {
		sqlq = `SELECT ` + qualify(nodeCols, "n") + ` FROM nodes n WHERE n.name LIKE ? ESCAPE '\'` + where +
			` ORDER BY length(n.name) ASC, n.name COLLATE NOCASE ASC LIMIT ?`
		args = append([]any{escapeLike(q) + "%"}, args...)
	}
	args = append(args, limit)
	rows, err := ix.db.Query(sqlq, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Node{}
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ftsLiteral quotes a user string as one FTS5 string token so its
// punctuation is matched, not parsed as query syntax.
func ftsLiteral(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func qualify(cols, alias string) string {
	parts := strings.Split(cols, ", ")
	for i, p := range parts {
		parts[i] = alias + "." + p
	}
	return strings.Join(parts, ", ")
}

// Stats is what the Storage page shows.
type Stats struct {
	Files    int64 `json:"files"`
	Dirs     int64 `json:"dirs"`
	Bytes    int64 `json:"bytes"`
	Unhashed int64 `json:"unhashed"`
}

func (ix *Index) Stats() (_ Stats, err error) {
	defer func() { ix.note(err) }()
	var s Stats
	err = ix.db.QueryRow(`SELECT
		COALESCE(SUM(CASE WHEN kind = 'file' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN kind = 'dir' AND name <> '' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN kind = 'file' THEN size ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN kind = 'file' AND sha256 = '' THEN 1 ELSE 0 END), 0)
		FROM nodes`).Scan(&s.Files, &s.Dirs, &s.Bytes, &s.Unhashed)
	return s, err
}

// meta helpers: small key/value facts about the index itself (last walk).
func (ix *Index) setMeta(key, value string) error {
	_, err := ix.db.Exec(`INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

func (ix *Index) getMeta(key string) string {
	var v string
	ix.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	return v
}

// deleteSubtree removes rel and everything under it.
func deleteSubtree(tx *sql.Tx, rel string) error {
	parent, name := Split(rel)
	if _, err := tx.Exec(`DELETE FROM nodes WHERE parent = ? AND name = ?`, parent, name); err != nil {
		return err
	}
	w, args := subtree("parent", rel)
	_, err := tx.Exec(`DELETE FROM nodes WHERE `+w, args...)
	return err
}

// bumpAncestors adds delta bytes to every directory above rel, the root
// row included. Sizes of directories are maintained incrementally by fog's
// own writes and corrected by walks.
func bumpAncestors(tx *sql.Tx, rel string, delta int64) error {
	if delta == 0 {
		return nil
	}
	// From rel's parent up to and including the root row; rel's own row is
	// the caller's business (syncDir just wrote it), and the root has no
	// ancestors at all.
	for rel != "" {
		parent, _ := Split(rel)
		pp, pn := Split(parent)
		if _, err := tx.Exec(`UPDATE nodes SET size = MAX(0, size + ?) WHERE parent = ? AND name = ?`, delta, pp, pn); err != nil {
			return err
		}
		rel = parent
	}
	return nil
}

// UnhashedBatch returns up to n files still lacking a digest.
func (ix *Index) UnhashedBatch(n int) ([]Node, error) {
	rows, err := ix.db.Query(`SELECT `+nodeCols+` FROM nodes WHERE kind = 'file' AND sha256 = '' AND dehydrated = 0 LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// SetHash records a digest for rel, but only if the file is still the one
// that was hashed — a rewrite during hashing would otherwise pin a stale
// digest to new bytes.
func (ix *Index) SetHash(rel string, size, mtime int64, sha string) error {
	if sha != unhashable && !validDigest(sha) {
		return fmt.Errorf("%w: not a sha256 digest", ErrBadName)
	}
	parent, name := Split(rel)
	ix.wmu.Lock()
	defer ix.wmu.Unlock()
	_, err := ix.db.Exec(`UPDATE nodes SET sha256 = ? WHERE parent = ? AND name = ? AND size = ? AND mtime = ?`, sha, parent, name, size, mtime)
	return err
}

// Duplicates lists other files sharing rel's digest.
func (ix *Index) Duplicates(sha, exceptRel string, limit int) ([]string, error) {
	if sha == "" {
		return nil, nil
	}
	rows, err := ix.db.Query(`SELECT parent, name FROM nodes WHERE sha256 = ? AND kind = 'file' LIMIT ?`, sha, limit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p, n string
		if err := rows.Scan(&p, &n); err != nil {
			return nil, err
		}
		if rel := Join(p, n); rel != exceptRel {
			out = append(out, rel)
		}
	}
	return out, rows.Err()
}

// ---- dehydrated rows ----
//
// A previews subscription lists files this box does not hold. Their rows
// carry the origin's size, time and digest and the dehydrated mark; the
// walker keeps them when the disk has nothing at that name, and clears the
// mark the moment bytes land there. Callers hold wmu.

func (ix *Index) putDehydratedLocked(rel string, size, mtime int64, sha, mime string) error {
	parent, name := Split(rel)
	// The digest and type are the origin's word. A digest names cache files
	// (thumbPath) and so must be one; a type is shown, so it is bounded.
	if !validDigest(sha) {
		sha = ""
	}
	if len(mime) > 255 || strings.ContainsAny(mime, "\r\n") {
		mime = ""
	}
	_, err := ix.db.Exec(`INSERT INTO nodes (parent, name, kind, size, mtime, mode, inode, mime, sha256, dir_mtime, entries, walked_at, dehydrated)
		VALUES (?, ?, 'file', ?, ?, 420, 0, ?, ?, 0, 0, 0, 1)
		ON CONFLICT(parent, name) DO UPDATE SET
			kind = 'file', size = excluded.size, mtime = excluded.mtime, mode = 420, inode = 0,
			mime = excluded.mime, sha256 = excluded.sha256, dehydrated = 1`,
		parent, name, size, mtime, mime, sha)
	return err
}

func (ix *Index) markDehydratedLocked(rel string) error {
	parent, name := Split(rel)
	res, err := ix.db.Exec(`UPDATE nodes SET dehydrated = 1 WHERE parent = ? AND name = ? AND kind = 'file'`, parent, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (ix *Index) dropDehydratedLocked(rel string) error {
	parent, name := Split(rel)
	_, err := ix.db.Exec(`DELETE FROM nodes WHERE parent = ? AND name = ? AND dehydrated = 1`, parent, name)
	return err
}
