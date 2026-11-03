package store

import "database/sql"

// Approval is one thing an operator approved an app to do.
type Approval struct {
	Kind  string // "host", "port", "raw"
	Value string
}

// AddAppApprovals records approvals for an app; one already there stays.
func (s *Store) AddAppApprovals(appID int64, approvals []Approval, actor string) error {
	t := now()
	for _, a := range approvals {
		if _, err := s.db.Exec(`INSERT OR IGNORE INTO app_approvals (app_id, kind, value, actor, created_at) VALUES (?, ?, ?, ?, ?)`,
			appID, a.Kind, a.Value, actor, t); err != nil {
			return err
		}
	}
	return nil
}

// SetAppApprovals replaces an app's approvals with exactly these: what an
// operator's save of its settings says, withdrawals included.
func (s *Store) SetAppApprovals(appID int64, approvals []Approval, actor string) error {
	return s.withTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM app_approvals WHERE app_id = ?`, appID); err != nil {
			return err
		}
		t := now()
		for _, a := range approvals {
			if _, err := tx.Exec(`INSERT OR IGNORE INTO app_approvals (app_id, kind, value, actor, created_at) VALUES (?, ?, ?, ?, ?)`,
				appID, a.Kind, a.Value, actor, t); err != nil {
				return err
			}
		}
		return nil
	})
}

// AppApprovals answers an app's approvals. Read on the primary: it gates a
// deploy, and an approval a moment ago must be seen.
func (s *Store) AppApprovals(appID int64) (map[Approval]bool, error) {
	rows, err := s.db.Query(`SELECT kind, value FROM app_approvals WHERE app_id = ?`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[Approval]bool{}
	for rows.Next() {
		var a Approval
		if err := rows.Scan(&a.Kind, &a.Value); err != nil {
			return nil, err
		}
		out[a] = true
	}
	return out, rows.Err()
}

// SetAppReviewedRev records the revision an operator confirmed an imported
// app's review at.
func (s *Store) SetAppReviewedRev(appID, rev int64) error {
	_, err := s.db.Exec(`UPDATE apps SET reviewed_rev = ?, updated_at = ? WHERE id = ?`, rev, now(), appID)
	return err
}

// AppReviewedRev answers the revision an app's review was confirmed at, 0
// for an app never held.
func (s *Store) AppReviewedRev(appID int64) (int64, error) {
	var v int64
	err := s.db.QueryRow(`SELECT reviewed_rev FROM apps WHERE id = ?`, appID).Scan(&v)
	return v, err
}

// HeldApps names every app held for review.
func (s *Store) HeldApps() (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT name FROM apps WHERE review_hold != 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out[n] = true
	}
	return out, rows.Err()
}
