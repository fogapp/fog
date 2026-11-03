package store

// SetAppReviewHold holds an app (or releases it): a held app was imported
// and has not been reviewed, and the engine refuses to deploy it.
func (s *Store) SetAppReviewHold(appID int64, hold bool) error {
	v := 0
	if hold {
		v = 1
	}
	_, err := s.db.Exec(`UPDATE apps SET review_hold = ?, updated_at = ? WHERE id = ?`, v, now(), appID)
	return err
}

// AppReviewHold reports whether an app is held for review. It reads the
// primary connection, not the read pool: the answer gates a deploy, and a
// confirmation a moment ago must be seen.
func (s *Store) AppReviewHold(appID int64) (bool, error) {
	var v int
	err := s.db.QueryRow(`SELECT review_hold FROM apps WHERE id = ?`, appID).Scan(&v)
	return v != 0, err
}
