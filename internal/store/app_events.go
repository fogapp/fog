// The household event stream's rows (docs/events-contract.md). The store
// only keeps and pages them; validation and the long-poll wake-up live in
// internal/appevents.
package store

import (
	"encoding/json"
)

// AppEvent is one published event. Source is the publishing app's name
// ("fog" for fogd itself — a reserved app name, so never an app's); Seq is
// the stream cursor.
type AppEvent struct {
	Seq     int64           `json:"seq"`
	Source  string          `json:"source"`
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	Subject string          `json:"subject,omitempty"`
	Time    string          `json:"time"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// InsertAppEvents appends events in one transaction, skipping any whose
// (source, id) is already in the stream — a publisher retrying a POST it
// never saw answered must not deliver twice. Returns how many were new.
func (s *Store) InsertAppEvents(evs []AppEvent) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	n, created := 0, now()
	for _, e := range evs {
		data := string(e.Data)
		if data == "" {
			data = "null"
		}
		res, err := tx.Exec(`INSERT INTO app_events (source, id, type, subject, time, data, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT (source, id) DO NOTHING`,
			e.Source, e.ID, e.Type, e.Subject, e.Time, data, created)
		if err != nil {
			return 0, err
		}
		if k, _ := res.RowsAffected(); k > 0 {
			n++
		}
	}
	return n, tx.Commit()
}

// AppEventsAfter pages the stream forward from a cursor, oldest first.
func (s *Store) AppEventsAfter(after int64, limit int) ([]AppEvent, error) {
	return queryAll(s.rdb, scanAppEvent, `SELECT seq, source, id, type, subject, time, data
		FROM app_events WHERE seq > ? ORDER BY seq LIMIT ?`, after, limit)
}

// AppEventBounds is the oldest and newest seq still kept; both 0 for an
// empty stream. A consumer whose cursor is below oldest-1 missed a prune.
func (s *Store) AppEventBounds() (oldest, newest int64, err error) {
	err = s.rdb.QueryRow(`SELECT COALESCE(MIN(seq), 0), COALESCE(MAX(seq), 0) FROM app_events`).Scan(&oldest, &newest)
	return oldest, newest, err
}

// PruneAppEvents forgets events published before cutoff (RFC 3339).
func (s *Store) PruneAppEvents(cutoff string) (int64, error) {
	return s.pruneBatched("app_events", "created_at < ?", cutoff)
}

func scanAppEvent(sc scanner) (AppEvent, error) {
	var e AppEvent
	var data string
	if err := sc.Scan(&e.Seq, &e.Source, &e.ID, &e.Type, &e.Subject, &e.Time, &data); err != nil {
		return e, err
	}
	if data != "null" {
		e.Data = json.RawMessage(data)
	}
	return e, nil
}
