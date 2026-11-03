package store

import (
	"database/sql"
	"errors"
	"strings"
)

// scanner is a row a query produced: *sql.Row or *sql.Rows.
type scanner interface{ Scan(...any) error }

// querier is what both *sql.DB and *sql.Tx offer for reads.
type querier interface {
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// Row scanners in this package share one shape, scanX(sc) (X, error), so
// any of them can be handed to queryAll or queryOne.

// queryAll runs query and scans every row with scan. No rows is a nil
// slice; wrap in orEmpty where callers need [] rather than null.
func queryAll[T any](q querier, scan func(scanner) (T, error), query string, args ...any) ([]T, error) {
	rows, err := q.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []T
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// queryOne runs a single-row query and scans it with scan; no row is
// ErrNotFound.
func queryOne[T any](q querier, scan func(scanner) (T, error), query string, args ...any) (T, error) {
	v, err := scan(q.QueryRow(query, args...))
	if errors.Is(err, sql.ErrNoRows) {
		var zero T
		return zero, ErrNotFound
	}
	return v, err
}

// queryEach runs query and hands each row to fn, for loops that fold rows
// into something other than a slice.
func queryEach(q querier, fn func(scanner) error, query string, args ...any) error {
	rows, err := q.Query(query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := fn(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// scanValue scans a single-column row into a T.
func scanValue[T any](sc scanner) (T, error) {
	var v T
	err := sc.Scan(&v)
	return v, err
}

// inChunk is the most values one IN (...) list carries, well under
// SQLite's bound-parameter limit.
const inChunk = 500

// inList renders vals as the placeholders and arguments of an IN (...)
// list. Callers split long lists with slices.Chunk(vals, inChunk).
func inList[T any](vals []T) (string, []any) {
	args := make([]any, len(vals))
	for i, v := range vals {
		args[i] = v
	}
	return strings.TrimSuffix(strings.Repeat("?, ", len(vals)), ", "), args
}

// orEmpty turns a nil result into an empty slice, for lists that encode to
// JSON as [] even when there is nothing in them.
func orEmpty[T any](xs []T, err error) ([]T, error) {
	if err != nil {
		return nil, err
	}
	if xs == nil {
		xs = []T{}
	}
	return xs, nil
}

// ptr adapts a (T, error) result to the (*T, error) most getters return,
// nil on any error.
func ptr[T any](v T, err error) (*T, error) {
	if err != nil {
		return nil, err
	}
	return &v, nil
}
