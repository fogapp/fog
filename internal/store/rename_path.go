package store

import (
	"database/sql"
	"strings"
	"unicode/utf8"
)

// RenamePath follows a rename or move of a Files entry: every path-keyed
// record on the entry or anything beneath it moves to the new path, in one
// transaction so a failure part-way can't leave some tables behind.
//
// Per table:
//   - backup exclusions and "keep on this box" exclusions: an exclusion that
//     would land on one already there merges into it (and is stamped now,
//     as re-adding it would);
//   - pins: a pin that would land on one the person already has is dropped
//     rather than duplicated;
//   - share links, federation shares (marked dirty, so peers refetch) and
//     contribution origins just move.
func (s *Store) RenamePath(old, new string) error {
	if old == new {
		return nil
	}
	return s.withTx(func(tx *sql.Tx) error {
		ts := now()
		steps := []struct {
			table, set string
			args       []any
			merge      bool
		}{
			{"file_backup_excludes", "created_at = ?", []any{ts}, true},
			{"file_share_excludes", "created_at = ?", []any{ts}, true},
			{"file_pins", "", nil, true},
			// fileSharesLive: a link suspended with an item in the trash
			// stays with that item, not with whatever now has its name.
			{fileSharesLive, "", nil, false},
			{"file_fed_shares", "dirty = 1", nil, false},
			{"file_fed_origins", "", nil, false},
		}
		for _, st := range steps {
			if err := renamePathIn(tx, st.table, st.set, st.args, st.merge, old, new); err != nil {
				return err
			}
		}
		return nil
	})
}

// renamePathIn rewrites table's path column from old (and its descendants)
// to new. set adds extra assignments (with args) to the update. With merge,
// a row whose new path collides with an existing one is ignored and then
// deleted — the existing row stands in for it.
func renamePathIn(tx *sql.Tx, table, set string, setArgs []any, merge bool, old, new string) error {
	// LIKE is case-insensitive for ASCII in SQLite, so the substr check
	// keeps "Foo" from matching "foo/…"; lengths are in characters, as
	// SQLite counts them for TEXT.
	n := utf8.RuneCountInString(old)
	where := `(path = ? OR (path LIKE ? ESCAPE '\' AND substr(path, 1, ?) = ?))`
	whereArgs := []any{old, likeEscape(old) + "/%", n + 1, old + "/"}
	if table == fileSharesLive {
		table, where = "file_shares", where+` AND trashed_with = ''`
	}

	verb := "UPDATE"
	if merge {
		verb = "UPDATE OR IGNORE"
	}
	assign := "path = ? || substr(path, ?)"
	if set != "" {
		assign += ", " + set
	}
	args := append([]any{new, n + 1}, setArgs...)
	args = append(args, whereArgs...)
	if _, err := tx.Exec(verb+" "+table+" SET "+assign+" WHERE "+where, args...); err != nil {
		return err
	}
	if merge {
		// Anything still under old collided with a row already at its new
		// path.
		if _, err := tx.Exec("DELETE FROM "+table+" WHERE "+where, whereArgs...); err != nil {
			return err
		}
	}
	return nil
}

// fileSharesLive names file_shares to renamePathIn with only the links
// that are not suspended.
const fileSharesLive = "file_shares(live)"

func likeEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	return strings.ReplaceAll(s, `_`, `\_`)
}

// MoveBackupDir follows the backup directory to a new place: every app
// backup and Files snapshot recorded under old is re-pointed under new,
// and the setting key is set to value, in one transaction — so fog never
// looks for its backups in one place while the rows name another.
//
// keep, when set, limits which records follow to those whose new path it
// accepts: an adopted directory holds only what was copied there, and a
// record of a snapshot that is not there keeps naming the old place.
func (s *Store) MoveBackupDir(key, value, old, new string, keep func(newPath string) bool) error {
	return s.withTx(func(tx *sql.Tx) error {
		for _, table := range []string{"backups", "file_snapshots"} {
			if keep == nil {
				if err := renamePathIn(tx, table, "", nil, false, old, new); err != nil {
					return err
				}
				continue
			}
			if err := keepPathsIn(tx, table, old, new, keep); err != nil {
				return err
			}
		}
		_, err := tx.Exec(`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
			key, value, now())
		return err
	})
}

// keepPathsIn is renamePathIn row by row, for the rows whose new path
// keep accepts.
func keepPathsIn(tx *sql.Tx, table, old, new string, keep func(string) bool) error {
	n := utf8.RuneCountInString(old)
	rows, err := tx.Query(`SELECT id, path FROM `+table+` WHERE path LIKE ? ESCAPE '\' AND substr(path, 1, ?) = ?`,
		likeEscape(old)+"/%", n+1, old+"/")
	if err != nil {
		return err
	}
	moved := map[int64]string{}
	for rows.Next() {
		var id int64
		var path string
		if err := rows.Scan(&id, &path); err != nil {
			rows.Close()
			return err
		}
		if p := new + strings.TrimPrefix(path, old); keep(p) {
			moved[id] = p
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for id, p := range moved {
		if _, err := tx.Exec(`UPDATE `+table+` SET path = ? WHERE id = ?`, p, id); err != nil {
			return err
		}
	}
	return nil
}
