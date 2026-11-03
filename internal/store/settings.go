package store

import "database/sql"

// Setting keys used across fogd. Settings are operator-facing knobs edited
// through the UI; process-level knobs (paths, ports) stay in package config.
const (
	SettingBaseDomain = "base_domain" // e.g. home.mjw.io
	SettingServerName = "server_name" // display name for the dashboard
	SettingSetupDone  = "setup_done"  // "1" once the first-run wizard completes

	// SettingIgnoredUpdateVersion is the fogd release the operator
	// dismissed; the inbox stays quiet about it until a newer one ships.
	SettingIgnoredUpdateVersion = "ignored_update_version"

	// A USB install preseeds the admin account and mints a one-shot login
	// token so the browser that drove the installer lands on the dashboard
	// already signed in. Only the token's digest ever touches disk.
	SettingClaimDigest  = "claim_token_digest"
	SettingClaimExpires = "claim_token_expires" // RFC3339 deadline
	SettingClaimUser    = "claim_token_user"    // username the token signs in
)

func (s *Store) GetSetting(key string) (string, error) {
	var v string
	err := s.rdb.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, now())
	return err
}

func (s *Store) DeleteSetting(key string) error {
	_, err := s.db.Exec(`DELETE FROM settings WHERE key = ?`, key)
	return err
}

func (s *Store) AllSettings() (map[string]string, error) {
	m := map[string]string{}
	err := queryEach(s.db, func(sc scanner) error {
		var k, v string
		if err := sc.Scan(&k, &v); err != nil {
			return err
		}
		m[k] = v
		return nil
	}, `SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	return m, nil
}
