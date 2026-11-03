package store

import (
	"errors"
)

// FilePin is one entry a person pinned to their sidebar's Drive list.
type FilePin struct {
	Path      string `json:"path"`
	Kind      string `json:"kind"` // file | dir, as it was when pinned
	CreatedAt string `json:"createdAt"`
}

// ErrFilePinLimit refuses a pin past MaxFilePins.
var ErrFilePinLimit = errors.New("too many pins")

// MaxFilePins bounds one person's pins: the list lives in a sidebar, and
// past a couple of dozen it is a second file browser, not a shortcut list.
const MaxFilePins = 30

// ListFilePins returns a person's pins in the order they were pinned.
func (s *Store) ListFilePins(personID int64) ([]FilePin, error) {
	return orEmpty(queryAll(s.db, func(sc scanner) (FilePin, error) {
		var p FilePin
		err := sc.Scan(&p.Path, &p.Kind, &p.CreatedAt)
		return p, err
	}, `SELECT path, kind, created_at FROM file_pins WHERE person_id = ? ORDER BY created_at, rowid`, personID))
}

// AddFilePin pins path for the person; pinning it again is a no-op.
// ErrFilePinLimit when the person already has MaxFilePins.
func (s *Store) AddFilePin(personID int64, path, kind string) error {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM file_pins WHERE person_id = ? AND path != ?`, personID, path).Scan(&n); err != nil {
		return err
	}
	if n >= MaxFilePins {
		return ErrFilePinLimit
	}
	_, err := s.db.Exec(`INSERT INTO file_pins (person_id, path, kind, created_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(person_id, path) DO UPDATE SET kind = excluded.kind`, personID, path, kind, now())
	return err
}

// RemoveFilePin unpins path for the person.
func (s *Store) RemoveFilePin(personID int64, path string) error {
	_, err := s.db.Exec(`DELETE FROM file_pins WHERE person_id = ? AND path = ?`, personID, path)
	return err
}
