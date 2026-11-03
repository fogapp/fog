package store

import (
	"database/sql"
	"fmt"
)

// withTx runs fn in a transaction: committed when fn returns nil, rolled
// back when it returns an error (which withTx returns) or panics.
func (s *Store) withTx(fn func(*sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() // no-op after Commit
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// pruneBatched deletes the rows of table matching where, pruneBatch at a
// time, and returns how many went. The store runs one connection, so an
// unbounded DELETE over years of rows would hold every request behind it;
// small batches let them interleave.
//
// table and where are this package's own constants, never input. table
// must be an ordinary rowid table.
func (s *Store) pruneBatched(table, where string, args ...any) (int64, error) {
	q := fmt.Sprintf(`DELETE FROM %s WHERE rowid IN (SELECT rowid FROM %s WHERE (%s) LIMIT ?)`, table, table, where)
	args = append(args, pruneBatch)
	var total int64
	for {
		r, err := s.db.Exec(q, args...)
		if err != nil {
			return total, err
		}
		n, _ := r.RowsAffected()
		total += n
		if n < pruneBatch {
			return total, nil
		}
	}
}

// expiry names a table and what makes one of its rows dead. where takes
// one argument, the current time.
type expiry struct{ table, where string }

// pruneExpired deletes the dead rows of each table in turn, batched like
// pruneBatched, and returns how many went in all — the shape of every
// sign-in table's cleanup: short-lived rows that carry their own expiry.
func (s *Store) pruneExpired(rules ...expiry) (int64, error) {
	at := now()
	var total int64
	for _, r := range rules {
		n, err := s.pruneBatched(r.table, r.where, at)
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}
