package store

// Notice is a persistent operator to-do: something Fog cannot do itself
// and must not let slip by (revoke a deploy key on the git host, act on a
// degraded state, …). Active until explicitly dismissed.
//
// Two rules keep the Notices tab from becoming a second copy of the rest of
// the Inbox — which is what it had drifted into, five kinds deep:
//
//   - If a scanner sweep can re-derive the condition, it is an issue, not a
//     notice. An issue auto-resolves when the world changes; a notice waits
//     for a person. Filing both leaves the operator dismissing by hand a row
//     that already cleared itself one tab over.
//   - A count of other Inbox rows is a badge, never a row. The bell and the
//     tab strip already carry those numbers, straight from the same stores.
//
// What is left is what the tab is for: a moment fog observed and cannot
// re-derive, needing a human step outside fog.
type Notice struct {
	ID        int64  `json:"id"`
	CreatedAt string `json:"createdAt"`
	Kind      string `json:"kind"`
	App       string `json:"app"`
	Title     string `json:"title"`
	Body      string `json:"body"`
}

// CreateNotice records a notice. One active notice per (kind, app): a
// repeat replaces the existing one instead of stacking duplicates. The
// partial unique index (0024) makes the upsert atomic, so concurrent
// producers land on the same row — the old check-then-insert let them race
// past each other and stack duplicates.
func (s *Store) CreateNotice(kind, app, title, body string) error {
	_, err := s.db.Exec(`INSERT INTO notices (created_at, kind, app, title, body) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (kind, app) WHERE dismissed_at IS NULL
		DO UPDATE SET title = excluded.title, body = excluded.body, created_at = excluded.created_at`,
		now(), kind, app, title, body)
	return err
}

// CountActiveNotices is len(ActiveNotices()) without reading them.
func (s *Store) CountActiveNotices() (int64, error) {
	var n int64
	err := s.db.QueryRow(`SELECT COUNT(*) FROM notices WHERE dismissed_at IS NULL`).Scan(&n)
	return n, err
}

func (s *Store) ActiveNotices() ([]Notice, error) {
	return queryAll(s.db, func(sc scanner) (Notice, error) {
		var n Notice
		err := sc.Scan(&n.ID, &n.CreatedAt, &n.Kind, &n.App, &n.Title, &n.Body)
		return n, err
	}, `SELECT id, created_at, kind, app, title, body FROM notices WHERE dismissed_at IS NULL ORDER BY id DESC`)
}

func (s *Store) DismissNotice(id int64) error {
	_, err := s.db.Exec(`UPDATE notices SET dismissed_at = ? WHERE id = ?`, now(), id)
	return err
}

// DismissNoticeByKind retracts the active notice for (kind, app), letting a
// producer whose condition cleared take its own notice back down instead of
// leaving a stale to-do for the operator to dismiss by hand.
func (s *Store) DismissNoticeByKind(kind, app string) error {
	_, err := s.db.Exec(`UPDATE notices SET dismissed_at = ? WHERE kind = ? AND app = ? AND dismissed_at IS NULL`, now(), kind, app)
	return err
}
