package store

import (
	"database/sql"
	"errors"
)

// Files backup state: the operator's exclusions and the record of every
// snapshot written. See migration 0026 for why these are here and not in
// the files index.

// FileSnapshot is one nightly (or manual) snapshot of the files root.
type FileSnapshot struct {
	ID         int64  `json:"id"`
	Path       string `json:"path"`
	CreatedAt  string `json:"createdAt"`
	Mode       string `json:"mode" ts:"\"links\" | \"mirror\""` // links | mirror
	Files      int64  `json:"files"`
	Bytes      int64  `json:"bytes"`
	NewBytes   int64  `json:"newBytes"`
	Warnings   int    `json:"warnings"`
	VerifiedAt string `json:"verifiedAt,omitempty"`
	// Offsite is filled by the files manager from the copy rows: "" (no
	// target), stored, uploading, pending.
	Offsite string `json:"offsite,omitempty" ts:"\"\" | \"verified\" | \"stored\" | \"uploading\" | \"pending\""`
}

const fileSnapshotCols = `id, path, created_at, mode, files, bytes, new_bytes, warnings, verified_at`

func scanFileSnapshot(sc scanner) (FileSnapshot, error) {
	var s FileSnapshot
	err := sc.Scan(&s.ID, &s.Path, &s.CreatedAt, &s.Mode, &s.Files, &s.Bytes, &s.NewBytes, &s.Warnings, &s.VerifiedAt)
	return s, err
}

func (s *Store) CreateFileSnapshot(path, mode string, files, bytes, newBytes int64, warnings int) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO file_snapshots (path, created_at, mode, files, bytes, new_bytes, warnings) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		path, now(), mode, files, bytes, newBytes, warnings)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListFileSnapshots returns every snapshot, newest first.
func (s *Store) ListFileSnapshots() ([]FileSnapshot, error) {
	return orEmpty(queryAll(s.db, scanFileSnapshot, `SELECT `+fileSnapshotCols+` FROM file_snapshots ORDER BY created_at DESC, id DESC`))
}

func (s *Store) FileSnapshotByID(id int64) (*FileSnapshot, error) {
	return ptr(queryOne(s.db, scanFileSnapshot, `SELECT `+fileSnapshotCols+` FROM file_snapshots WHERE id = ?`, id))
}

func (s *Store) DeleteFileSnapshot(id int64) error {
	_, err := s.db.Exec(`DELETE FROM file_snapshots WHERE id = ?`, id)
	return err
}

func (s *Store) SetFileSnapshotVerified(id int64) error {
	_, err := s.db.Exec(`UPDATE file_snapshots SET verified_at = ? WHERE id = ?`, now(), id)
	return err
}

// LatestFileSnapshotAt is the newest snapshot's timestamp, "" when none.
func (s *Store) LatestFileSnapshotAt() (string, error) {
	var at string
	err := s.db.QueryRow(`SELECT created_at FROM file_snapshots ORDER BY created_at DESC LIMIT 1`).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return at, err
}

// ---- exclusions ----

func (s *Store) ListFileExcludes() ([]string, error) {
	return orEmpty(queryAll(s.db, scanValue[string], `SELECT path FROM file_backup_excludes ORDER BY path`))
}

func (s *Store) AddFileExclude(path string) error {
	_, err := s.db.Exec(`INSERT INTO file_backup_excludes (path, created_at) VALUES (?, ?) ON CONFLICT(path) DO NOTHING`, path, now())
	return err
}

func (s *Store) RemoveFileExclude(path string) error {
	_, err := s.db.Exec(`DELETE FROM file_backup_excludes WHERE path = ?`, path)
	return err
}
