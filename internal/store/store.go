// Package store is fogd's state layer: a single SQLite database (WAL mode)
// holding operator config, auth state, and the audit trail. The database is
// authoritative for *desired* state; what's actually running is materialized
// to plain files on disk so apps survive fogd itself.
package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"net/url"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

type Store struct {
	db *sql.DB
	// rdb is a small pool of query-only connections for the hot, polled
	// reads (dashboards, settings, chat replay). db stays a single
	// connection — SQLite has one writer anyway, and migrate's
	// foreign_keys toggle relies on there being exactly one — but with only
	// that connection every read queued behind every write, and a
	// VACUUM INTO stalled the whole API. WAL lets readers run alongside the
	// writer and see everything it has committed, so a read routed here
	// after a write returns sees that write.
	rdb  *sql.DB
	path string
}

// readConns bounds the read pool: enough for a few open tabs polling at
// once, few enough that SQLite's per-connection page cache stays small.
const readConns = 4

// sqliteURIPath percent-escapes a filesystem path for a file: DSN. The driver
// splits the DSN at the first '?' and SQLite's URI parser treats '#' as a
// fragment marker, so a raw path containing either would silently truncate
// the pragma list — WAL, busy_timeout, and FK enforcement all lost. SQLite
// decodes %XX escapes in URI paths, so escaping each segment keeps such
// paths opening correctly.
func sqliteURIPath(path string) string {
	segs := strings.Split(path, "/")
	for i, seg := range segs {
		segs[i] = url.PathEscape(seg)
	}
	return strings.Join(segs, "/")
}

func Open(path string) (*Store, error) {
	// modernc.org/sqlite: single writer; busy_timeout smooths concurrent access.
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)&_pragma=synchronous(NORMAL)", sqliteURIPath(path))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, path: path}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if err := s.backfillPersonUIDs(); err != nil {
		db.Close()
		return nil, fmt.Errorf("person ids: %w", err)
	}
	// Opened after migrate, so the file is already in WAL mode and every
	// table exists. query_only makes a write routed here by mistake fail
	// loudly instead of racing the writer.
	rdsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=query_only(1)", sqliteURIPath(path))
	rdb, err := sql.Open("sqlite", rdsn)
	if err != nil {
		db.Close()
		return nil, err
	}
	rdb.SetMaxOpenConns(readConns)
	rdb.SetMaxIdleConns(readConns)
	s.rdb = rdb
	return s, nil
}

func (s *Store) Close() error {
	rerr := s.rdb.Close()
	if err := s.db.Close(); err != nil {
		return err
	}
	return rerr
}

// DB exposes the underlying handle to tests that need to reach past the
// store's methods (backdating rows, simulating loss). Production code goes
// through named methods; nothing outside a _test.go file should call it.
func (s *Store) DB() *sql.DB { return s.db }

// SnapshotTo writes a consistent copy of the whole database to path, which
// must not exist yet. It runs on a connection of its own: on the writer it
// held up every other request for as long as the copy took, and the read
// pool's query_only refuses VACUUM INTO even though it only reads this
// database.
func (s *Store) SnapshotTo(ctx context.Context, path string) error {
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)", sqliteURIPath(s.path)))
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.ExecContext(ctx, `VACUUM INTO ?`, path)
	return err
}

func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		name TEXT PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return err
	}
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return err
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	// What has run, read once: a query per migration is most of what an
	// already-migrated open costs.
	applied := map[string]bool{}
	rows, err := s.db.Query(`SELECT name FROM schema_migrations`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		applied[name] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, name := range names {
		if applied[name] {
			continue
		}
		body, err := fs.ReadFile(migrationsFS, "migrations/"+name)
		if err != nil {
			return err
		}
		// SQLite can't ALTER a column, so schema changes to a table with
		// children need the rebuild dance (create new, copy, drop, rename) —
		// and with foreign_keys ON, DROP TABLE cascades the children away.
		// The pragma is a no-op inside a transaction, so migrations marked
		// fog:fk-off get it toggled around theirs, with an integrity check
		// inside the transaction, before the commit: a rebuild that left
		// dangling references rolls back whole, never half-lands as applied.
		// (pragma_foreign_key_check scans regardless of the pragma's state.)
		fkOff := strings.Contains(string(body), "fog:fk-off")
		if fkOff {
			if _, err := s.db.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
				return err
			}
		}
		err = func() error {
			tx, err := s.db.Begin()
			if err != nil {
				return err
			}
			if _, err := tx.Exec(string(body)); err != nil {
				tx.Rollback()
				return fmt.Errorf("%s: %w", name, err)
			}
			if fkOff {
				var table string
				checkErr := tx.QueryRow(`SELECT "table" FROM pragma_foreign_key_check LIMIT 1`).Scan(&table)
				if checkErr == nil {
					tx.Rollback()
					return fmt.Errorf("%s: left dangling foreign keys in %s", name, table)
				}
				if checkErr != sql.ErrNoRows {
					tx.Rollback()
					return checkErr
				}
			}
			if _, err := tx.Exec(`INSERT INTO schema_migrations (name, applied_at) VALUES (?, ?)`, name, now()); err != nil {
				tx.Rollback()
				return err
			}
			return tx.Commit()
		}()
		if fkOff {
			s.db.Exec(`PRAGMA foreign_keys=ON`)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

// QuickCheck runs SQLite's quick_check pragma and returns its verdict ("ok"
// when the file is sound). Used after an unclean shutdown so the dashboard's
// "recovered" notice states a verified fact, not a hope.
func (s *Store) QuickCheck() (string, error) {
	var v string
	err := s.db.QueryRow(`PRAGMA quick_check`).Scan(&v)
	return v, err
}
