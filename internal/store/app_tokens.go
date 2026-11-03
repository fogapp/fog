// Digest lookup for FOG_API_TOKEN: the app-facing /app/v1 surface resolves
// its bearer tokens here (session-token discipline — only digests stored).
package store

func (s *Store) SetAppAPITokenDigest(appID int64, digest string) error {
	_, err := s.db.Exec(`INSERT INTO app_api_tokens (app_id, digest, created_at) VALUES (?, ?, ?)
		ON CONFLICT(app_id) DO UPDATE SET digest = excluded.digest`,
		appID, digest, now())
	return err
}

func (s *Store) AppByAPITokenDigest(digest string) (*App, error) {
	appID, err := queryOne(s.db, scanValue[int64], `SELECT app_id FROM app_api_tokens WHERE digest = ?`, digest)
	if err != nil {
		return nil, err
	}
	return s.AppByID(appID)
}
