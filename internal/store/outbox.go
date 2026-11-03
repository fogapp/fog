// The outbox (0052, app_outbox): durable notices fog owes an app — today, that a session
// or a person is gone. Rows are written by triggers as sessions end, and by
// EnqueueLogout for what only fog's policy knows (an app narrowed to fewer
// people, "sign everyone out"); internal/signin delivers them.
package store

import (
	"time"

	"gitea.lan/matthew/fog/internal/randx"
)

const AppOutboxLogout = "logout"

// AppOutboxVisitLogout tells a paired fog (target "peer:<id>") that a
// session of ours it hosts as a visitor ended (docs/signin-contract.md).
const AppOutboxVisitLogout = "visit_logout"

// AppOutboxRow is one owed delivery.
type AppOutboxRow struct {
	ID        int64
	Kind      string
	Target    string
	IdemKey   string
	Payload   string
	Attempts  int
	LastError string
	Noticed   bool
	CreatedAt time.Time
}

// DueAppOutbox lists rows whose time has come, oldest first.
func (s *Store) DueAppOutbox(now time.Time, limit int) ([]AppOutboxRow, error) {
	return queryAll(s.db, func(sc scanner) (AppOutboxRow, error) {
		var r AppOutboxRow
		var created string
		err := sc.Scan(&r.ID, &r.Kind, &r.Target, &r.IdemKey, &r.Payload, &r.Attempts, &r.LastError, &r.Noticed, &created)
		r.CreatedAt = parseTime(created)
		return r, err
	}, `SELECT id, kind, target, idem_key, payload, attempts, last_error, noticed, created_at FROM app_outbox
		WHERE next_at <= ? ORDER BY id LIMIT ?`, stamp(now), limit)
}

func (s *Store) DeleteAppOutbox(id int64) error {
	_, err := s.db.Exec(`DELETE FROM app_outbox WHERE id = ?`, id)
	return err
}

// RetryAppOutbox records a failed attempt and when to try again.
func (s *Store) RetryAppOutbox(id int64, next time.Time, lastError string) error {
	_, err := s.db.Exec(`UPDATE app_outbox SET attempts = attempts + 1, next_at = ?, last_error = ? WHERE id = ?`, stamp(next), lastError, id)
	return err
}

func (s *Store) MarkAppOutboxNoticed(id int64) error {
	_, err := s.db.Exec(`UPDATE app_outbox SET noticed = 1 WHERE id = ?`, id)
	return err
}

// EnqueueLogout owes client a logout for a person (by their ULID), for the
// cases no trigger sees: fog's policy no longer lets them in.
func (s *Store) EnqueueLogout(clientID, uid string) error {
	_, err := s.db.Exec(`INSERT INTO app_outbox (kind, target, idem_key, payload, next_at, created_at)
		SELECT ?, client_id, ?, json_object('uid', ?), ?, ? FROM oidc_clients WHERE client_id = ? AND backchannel_uri != ''`,
		AppOutboxLogout, randx.Hex(16), uid, now(), now(), clientID)
	return err
}

// EnqueueLogoutAll owes client a logout that ends every session it started
// from a sign-in issued before before: an emergency key rotation's, which
// must reach sessions no sub or sid names.
func (s *Store) EnqueueLogoutAll(clientID string, before time.Time) error {
	_, err := s.db.Exec(`INSERT INTO app_outbox (kind, target, idem_key, payload, next_at, created_at)
		SELECT ?, client_id, ?, json_object('all', ?), ?, ? FROM oidc_clients WHERE client_id = ? AND backchannel_uri != ''`,
		AppOutboxLogout, randx.Hex(16), before.Unix(), now(), now(), clientID)
	return err
}

// EnqueueVisitorLogouts owes client a logout for every visitor session that
// signed in to it and is still live.
func (s *Store) EnqueueVisitorLogouts(clientID string) (int, error) {
	res, err := s.db.Exec(`INSERT INTO app_outbox (kind, target, idem_key, payload, next_at, created_at)
		SELECT 'logout', vc.client_id, lower(hex(randomblob(16))), json_object('sid', v.sid), ?, ?
		FROM visitor_session_clients vc
		JOIN visitor_sessions v ON v.id = vc.visitor_session_id
		JOIN oidc_clients c ON c.client_id = vc.client_id
		WHERE vc.client_id = ? AND c.backchannel_uri != ''`, now(), now(), clientID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// CountAppOutbox is how many deliveries are owed, by target.
func (s *Store) CountAppOutbox() (map[string]int, error) {
	type row struct {
		target string
		n      int
	}
	rows, err := queryAll(s.db, func(sc scanner) (row, error) {
		var r row
		return r, sc.Scan(&r.target, &r.n)
	}, `SELECT target, COUNT(*) FROM app_outbox GROUP BY target`)
	if err != nil {
		return nil, err
	}
	out := map[string]int{}
	for _, r := range rows {
		out[r.target] = r.n
	}
	return out, nil
}

// LinkedPersonUIDs is everyone an app has seen, with their ULIDs.
func (s *Store) LinkedPersonUIDs(clientID string) (map[int64]string, error) {
	type row struct {
		id  int64
		uid string
	}
	rows, err := queryAll(s.db, func(sc scanner) (row, error) {
		var r row
		return r, sc.Scan(&r.id, &r.uid)
	}, `SELECT p.id, COALESCE(p.uid, '') FROM oidc_links l JOIN people p ON p.id = l.person_id WHERE l.client_id = ?`, clientID)
	if err != nil {
		return nil, err
	}
	out := map[int64]string{}
	for _, r := range rows {
		out[r.id] = r.uid
	}
	return out, nil
}
