package store

import (
	"database/sql"
	"errors"
)

// NasShare is a NAS share fogd mounts on the host. Enabled shares are
// re-mounted at every fogd start; nothing is written to /etc/fstab.
type NasShare struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Proto      string `json:"proto" ts:"\"nfs\" | \"cifs\""` // "nfs" | "cifs"
	Server     string `json:"server"`
	Remote     string `json:"remote"`
	MountPoint string `json:"mountPoint"`
	Options    string `json:"options,omitempty"`
	Username   string `json:"username,omitempty"`
	Enabled    bool   `json:"enabled"`
	CreatedAt  string `json:"createdAt"`
}

func (s *Store) CreateNasShare(sh NasShare) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO nas_shares
		(name, proto, server, remote, mount_point, options, username, enabled, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sh.Name, sh.Proto, sh.Server, sh.Remote, sh.MountPoint, sh.Options, sh.Username, boolToInt(sh.Enabled), now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) ListNasShares() ([]NasShare, error) {
	return queryAll(s.db, func(sc scanner) (NasShare, error) {
		var sh NasShare
		var enabled int
		err := sc.Scan(&sh.ID, &sh.Name, &sh.Proto, &sh.Server, &sh.Remote, &sh.MountPoint,
			&sh.Options, &sh.Username, &enabled, &sh.CreatedAt)
		sh.Enabled = enabled != 0
		return sh, err
	}, `SELECT id, name, proto, server, remote, mount_point, options, username, enabled, created_at
		FROM nas_shares ORDER BY name`)
}

func (s *Store) GetNasShare(name string) (NasShare, error) {
	var sh NasShare
	var enabled int
	err := s.db.QueryRow(`SELECT id, name, proto, server, remote, mount_point, options, username, enabled, created_at
		FROM nas_shares WHERE name = ?`, name).Scan(
		&sh.ID, &sh.Name, &sh.Proto, &sh.Server, &sh.Remote, &sh.MountPoint,
		&sh.Options, &sh.Username, &enabled, &sh.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return NasShare{}, ErrNotFound
	}
	if err != nil {
		return NasShare{}, err
	}
	sh.Enabled = enabled != 0
	return sh, nil
}

// UpdateNasShare rewrites a share's connection details in place, keeping its
// name, enabled flag, and history. This is how a share follows its server to
// a new address without being deleted and recreated (which would also drop
// the vaulted password).
func (s *Store) UpdateNasShare(name string, sh NasShare) error {
	_, err := s.db.Exec(`UPDATE nas_shares SET proto = ?, server = ?, remote = ?, mount_point = ?, options = ?, username = ?
		WHERE name = ?`,
		sh.Proto, sh.Server, sh.Remote, sh.MountPoint, sh.Options, sh.Username, name)
	return err
}

func (s *Store) SetNasShareEnabled(name string, enabled bool) error {
	_, err := s.db.Exec(`UPDATE nas_shares SET enabled = ? WHERE name = ?`, boolToInt(enabled), name)
	return err
}

func (s *Store) DeleteNasShare(name string) error {
	_, err := s.db.Exec(`DELETE FROM nas_shares WHERE name = ?`, name)
	return err
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
