package chunkstore

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// index maps chunk ids to where they are stored. It is a cache of what the
// pack headers say: a lost, stale or foreign index is rebuilt from the
// packs, so nothing lives only here.
type index struct {
	db *sql.DB
}

type chunkLoc struct {
	ID        ChunkID
	Pack      string
	Offset    int64
	StoredLen uint32
	RawLen    uint32
}

const indexSchema = `
CREATE TABLE IF NOT EXISTS meta (k TEXT PRIMARY KEY, v TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS packs (
	name TEXT PRIMARY KEY,
	size INTEGER NOT NULL,
	chunks INTEGER NOT NULL,
	added_at TEXT NOT NULL,
	checked_at TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS chunks (
	id BLOB PRIMARY KEY,
	pack TEXT NOT NULL,
	off INTEGER NOT NULL,
	slen INTEGER NOT NULL,
	rlen INTEGER NOT NULL
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS chunks_pack ON chunks(pack);
`

func openIndex(path, storeID string) (*index, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	open := func() (*sql.DB, error) {
		segs := strings.Split(path, "/")
		for i := range segs {
			segs[i] = url.PathEscape(segs[i])
		}
		db, err := sql.Open("sqlite", "file:"+strings.Join(segs, "/")+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=synchronous(NORMAL)")
		if err != nil {
			return nil, err
		}
		db.SetMaxOpenConns(1)
		if _, err := db.Exec(indexSchema); err != nil {
			db.Close()
			return nil, err
		}
		return db, nil
	}
	db, err := open()
	if err != nil {
		// A damaged cache is not worth failing over: start it again.
		slog.Warn("backup store index unreadable; rebuilding it", "path", path, "err", err)
		for _, p := range []string{path, path + "-wal", path + "-shm"} {
			os.Remove(p)
		}
		if db, err = open(); err != nil {
			return nil, err
		}
	}
	ix := &index{db: db}
	var have string
	err = db.QueryRow(`SELECT v FROM meta WHERE k = 'store'`).Scan(&have)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && have != storeID) {
		// Another store's index (the backups folder moved, or was
		// replaced): forget it all.
		if _, err := db.Exec(`DELETE FROM chunks; DELETE FROM packs; DELETE FROM meta;`); err != nil {
			db.Close()
			return nil, err
		}
		if _, err := db.Exec(`INSERT INTO meta (k, v) VALUES ('store', ?)`, storeID); err != nil {
			db.Close()
			return nil, err
		}
	} else if err != nil {
		db.Close()
		return nil, err
	}
	return ix, nil
}

func (ix *index) close() error { return ix.db.Close() }

func (ix *index) lookup(id ChunkID) (chunkLoc, bool, error) {
	l := chunkLoc{ID: id}
	err := ix.db.QueryRow(`SELECT pack, off, slen, rlen FROM chunks WHERE id = ?`, id[:]).
		Scan(&l.Pack, &l.Offset, &l.StoredLen, &l.RawLen)
	if errors.Is(err, sql.ErrNoRows) {
		return l, false, nil
	}
	return l, err == nil, err
}

func (ix *index) has(id ChunkID) (bool, error) {
	var one int
	err := ix.db.QueryRow(`SELECT 1 FROM chunks WHERE id = ?`, id[:]).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// addPack records a pack and its chunks. A chunk already stored elsewhere
// keeps its first home unless replace says the new copy wins (repack and
// heal move chunks deliberately).
func (ix *index) addPack(name string, size int64, entries []PackEntry, now time.Time, replace bool) error {
	tx, err := ix.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT OR REPLACE INTO packs (name, size, chunks, added_at) VALUES (?, ?, ?, ?)`,
		name, size, len(entries), now.UTC().Format(time.RFC3339)); err != nil {
		return err
	}
	verb := "INSERT OR IGNORE"
	if replace {
		verb = "INSERT OR REPLACE"
	}
	stmt, err := tx.Prepare(verb + ` INTO chunks (id, pack, off, slen, rlen) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, e := range entries {
		if _, err := stmt.Exec(e.ID[:], name, e.Offset, e.StoredLen, e.RawLen); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// dropPack forgets a pack: its rows go first, so a crash between this and
// removing the file leaves a pack the next open re-adds (and collection
// deletes again), never an index pointing at a file that is gone.
func (ix *index) dropPack(name string) error {
	tx, err := ix.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM chunks WHERE pack = ?`, name); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM packs WHERE name = ?`, name); err != nil {
		return err
	}
	return tx.Commit()
}

type packRow struct {
	Name      string
	Size      int64
	Chunks    int
	AddedAt   time.Time
	CheckedAt string
}

func (ix *index) packs() (map[string]packRow, error) {
	rows, err := ix.db.Query(`SELECT name, size, chunks, added_at, checked_at FROM packs`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]packRow{}
	for rows.Next() {
		var r packRow
		var added string
		if err := rows.Scan(&r.Name, &r.Size, &r.Chunks, &added, &r.CheckedAt); err != nil {
			return nil, err
		}
		r.AddedAt, _ = time.Parse(time.RFC3339, added)
		out[r.Name] = r
	}
	return out, rows.Err()
}

// chunksIn lists the index rows that point into one pack.
func (ix *index) chunksIn(pack string) ([]chunkLoc, error) {
	rows, err := ix.db.Query(`SELECT id, off, slen, rlen FROM chunks WHERE pack = ?`, pack)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []chunkLoc
	for rows.Next() {
		l := chunkLoc{Pack: pack}
		var id []byte
		if err := rows.Scan(&id, &l.Offset, &l.StoredLen, &l.RawLen); err != nil {
			return nil, err
		}
		copy(l.ID[:], id)
		out = append(out, l)
	}
	return out, rows.Err()
}

func (ix *index) markChecked(pack string, at time.Time) error {
	_, err := ix.db.Exec(`UPDATE packs SET checked_at = ? WHERE name = ?`, at.UTC().Format(time.RFC3339), pack)
	return err
}

func (ix *index) totals() (packs int, bytes int64, chunks int, err error) {
	err = ix.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(size), 0) FROM packs`).Scan(&packs, &bytes)
	if err == nil {
		err = ix.db.QueryRow(`SELECT COUNT(*) FROM chunks`).Scan(&chunks)
	}
	return
}

// reconcile brings the index level with the packs on disk: a pack written
// but not yet recorded (a crash between the two) is read in, and a pack
// the index names but the disk does not have — something removed it behind
// fog's back — means the index cannot be trusted, so it is rebuilt.
func (s *Store) reconcile() error {
	onDisk, err := s.listPacks()
	if err != nil {
		return err
	}
	known, err := s.ix.packs()
	if err != nil {
		return err
	}
	disk := make(map[string]bool, len(onDisk))
	for _, n := range onDisk {
		disk[n] = true
	}
	for n := range known {
		if !disk[n] {
			slog.Warn("backup store: a pack the index names is missing; rebuilding the index", "pack", n)
			return s.rebuildIndex(onDisk)
		}
	}
	for _, n := range onDisk {
		if _, ok := known[n]; ok {
			continue
		}
		if err := s.indexPackFile(n); err != nil {
			slog.Warn("backup store: unreadable pack left out of the index", "pack", n, "err", err)
		}
	}
	return nil
}

// RebuildIndex throws the index away and reads every pack header again.
func (s *Store) RebuildIndex() error {
	onDisk, err := s.listPacks()
	if err != nil {
		return err
	}
	return s.rebuildIndex(onDisk)
}

// A chunk in more than one pack — a damaged pack and the copy heal wrote,
// a sparse pack and its repacked copy — must come back pointing at the
// newer: the older is the one collection is waiting to remove. So packs are
// read oldest first and a later copy replaces an earlier one.
func (s *Store) rebuildIndex(onDisk []string) error {
	if _, err := s.ix.db.Exec(`DELETE FROM chunks; DELETE FROM packs;`); err != nil {
		return err
	}
	type aged struct {
		name string
		t    time.Time
	}
	order := make([]aged, 0, len(onDisk))
	for _, n := range onDisk {
		a := aged{name: n}
		if st, err := os.Stat(s.packPath(n)); err == nil {
			a.t = st.ModTime()
		}
		order = append(order, a)
	}
	sort.SliceStable(order, func(i, j int) bool { return order[i].t.Before(order[j].t) })
	for _, a := range order {
		if err := s.indexPack(a.name, true); err != nil {
			slog.Warn("backup store: unreadable pack left out of the index", "pack", a.name, "err", err)
		}
	}
	return nil
}

func (s *Store) indexPackFile(name string) error { return s.indexPack(name, false) }

func (s *Store) indexPack(name string, replace bool) error {
	f, err := os.Open(s.packPath(name))
	if err != nil {
		return err
	}
	defer f.Close()
	entries, size, err := s.readHeader(f)
	if err != nil {
		return fmt.Errorf("pack %s: %w", name, err)
	}
	st, _ := f.Stat()
	return s.ix.addPack(name, size, entries, st.ModTime(), replace)
}
