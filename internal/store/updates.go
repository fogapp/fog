package store

import "database/sql"

type AppUpdate struct {
	AppID        int64  `json:"appId"`
	LocalDigest  string `json:"localDigest"`
	RemoteDigest string `json:"remoteDigest"`
	Available    bool   `json:"available"`
	CheckedAt    string `json:"checkedAt"`
}

func (s *Store) SetAppUpdate(appID int64, local, remote string, available bool) error {
	av := 0
	if available {
		av = 1
	}
	_, err := s.db.Exec(`INSERT INTO app_updates (app_id, local_digest, remote_digest, available, checked_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(app_id) DO UPDATE SET local_digest=excluded.local_digest, remote_digest=excluded.remote_digest,
			available=excluded.available, checked_at=excluded.checked_at`,
		appID, local, remote, av, now())
	return err
}

func (s *Store) AppUpdateFor(appID int64) (*AppUpdate, error) {
	return ptr(queryOne(s.db, func(sc scanner) (AppUpdate, error) {
		var u AppUpdate
		var av int
		err := sc.Scan(&u.AppID, &u.LocalDigest, &u.RemoteDigest, &av, &u.CheckedAt)
		u.Available = av == 1
		return u, err
	}, `SELECT app_id, local_digest, remote_digest, available, checked_at FROM app_updates WHERE app_id = ?`, appID))
}

func (s *Store) AppUpdates() (map[int64]AppUpdate, error) {
	m := map[int64]AppUpdate{}
	err := queryEach(s.db, func(sc scanner) error {
		var u AppUpdate
		var av int
		if err := sc.Scan(&u.AppID, &u.LocalDigest, &u.RemoteDigest, &av, &u.CheckedAt); err != nil {
			return err
		}
		u.Available = av == 1
		m[u.AppID] = u
		return nil
	}, `SELECT app_id, local_digest, remote_digest, available, checked_at FROM app_updates`)
	if err != nil {
		return nil, err
	}
	return m, nil
}

// ---- backups ----

type Backup struct {
	ID        int64  `json:"id"`
	AppID     int64  `json:"appId"`
	Kind      string `json:"kind"`
	Path      string `json:"path"`
	SizeBytes int64  `json:"sizeBytes"`
	CreatedAt string `json:"createdAt"`
	// Delegated artifacts (fog-backup/1). All empty for legacy volume
	// snapshots.
	Format     string `json:"format,omitempty"`
	SHA256     string `json:"sha256,omitempty"`
	Manifest   string `json:"manifest,omitempty"`
	VerifiedAt string `json:"verifiedAt,omitempty"`
}

const backupCols = `id, app_id, kind, path, size_bytes, created_at, format, sha256, manifest, verified_at`

func scanBackup(sc scanner) (Backup, error) {
	var b Backup
	err := sc.Scan(&b.ID, &b.AppID, &b.Kind, &b.Path, &b.SizeBytes, &b.CreatedAt,
		&b.Format, &b.SHA256, &b.Manifest, &b.VerifiedAt)
	return b, err
}

func (s *Store) CreateBackup(appID int64, kind, path string, size int64) (int64, error) {
	return s.CreateArtifactBackup(appID, kind, path, size, "", "", "")
}

// CreateArtifactBackup records a delegated backup with its verified digest
// and denormalized manifest.
func (s *Store) CreateArtifactBackup(appID int64, kind, path string, size int64, format, sha256, manifest string) (int64, error) {
	return s.CreateArtifactBackupAt(appID, kind, path, size, format, sha256, manifest, "")
}

// CreateArtifactBackupAt records a backup with an explicit creation time —
// a snapshot fetched back from the locker keeps the time it was taken, so
// the scheduler and the staleness alarm are not fooled into seeing a
// fresh backup.
func (s *Store) CreateArtifactBackupAt(appID int64, kind, path string, size int64, format, sha256, manifest, createdAt string) (int64, error) {
	if createdAt == "" {
		createdAt = now()
	}
	res, err := s.db.Exec(`INSERT INTO backups (app_id, kind, path, size_bytes, created_at, format, sha256, manifest, verified_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, '')`,
		appID, kind, path, size, createdAt, format, sha256, manifest)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// SetBackupVerified stamps a backup as proven restorable (test-restore loop).
func (s *Store) SetBackupVerified(id int64) error {
	_, err := s.db.Exec(`UPDATE backups SET verified_at = ? WHERE id = ?`, now(), id)
	return err
}

// LatestBackupAt returns the created_at of the app's newest backup of any
// kind ("" when none) — the scheduler's due check and the staleness metric.
func (s *Store) LatestBackupAt(appID int64) (string, error) {
	var at string
	err := s.db.QueryRow(`SELECT created_at FROM backups WHERE app_id = ? ORDER BY id DESC LIMIT 1`, appID).Scan(&at)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return at, err
}

func (s *Store) ListBackups(appID int64, limit int) ([]Backup, error) {
	return queryAll(s.db, scanBackup, `SELECT `+backupCols+` FROM backups WHERE app_id = ? ORDER BY id DESC LIMIT ?`, appID, limit)
}

// LatestBackups is every app's newest backup keyed by app id — ListBackups
// with a limit of one, for the whole fleet in one query. Apps with no
// backups have no entry. idx_backups_app (app_id, id) answers the GROUP BY
// MAX(id) without touching the rows, so only the winners are read.
func (s *Store) LatestBackups() (map[int64]Backup, error) {
	return backupsByApp(s.rdb, `SELECT `+backupCols+` FROM backups
		WHERE id IN (SELECT MAX(id) FROM backups GROUP BY app_id)`)
}

// LatestBackupsOfFormat is, per app, the newest backup of format among that
// app's newest within backups — what scanning ListBackups(appID, within)
// for the first match answers, for every app in one query. Apps with no
// match in that window have no entry.
func (s *Store) LatestBackupsOfFormat(format string, within int) (map[int64]Backup, error) {
	return backupsByApp(s.rdb, `SELECT `+backupCols+` FROM backups WHERE id IN (
		SELECT MAX(id) FROM (
			SELECT id, app_id, format, ROW_NUMBER() OVER (PARTITION BY app_id ORDER BY id DESC) AS n
			FROM backups)
		WHERE n <= ? AND format = ? GROUP BY app_id)`, within, format)
}

func backupsByApp(q querier, query string, args ...any) (map[int64]Backup, error) {
	out := map[int64]Backup{}
	err := queryEach(q, func(sc scanner) error {
		b, err := scanBackup(sc)
		if err != nil {
			return err
		}
		out[b.AppID] = b
		return nil
	}, query, args...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// BackupRef is a backup row without its manifest — the columns a sweep over
// every backup needs to decide which ones to act on, leaving the full row
// (the manifest is the bulk of a delegated one) to BackupByID for the few
// it does.
type BackupRef struct {
	ID        int64
	AppID     int64
	Path      string
	CreatedAt string
}

// ListAllBackupRefs is every app's backups in one query, grouped by app
// with each app's newest first — ListBackups for the whole fleet, trimmed to
// BackupRef.
func (s *Store) ListAllBackupRefs() ([]BackupRef, error) {
	return queryAll(s.db, func(sc scanner) (BackupRef, error) {
		var b BackupRef
		err := sc.Scan(&b.ID, &b.AppID, &b.Path, &b.CreatedAt)
		return b, err
	}, `SELECT id, app_id, path, created_at FROM backups ORDER BY app_id, id DESC`)
}

func (s *Store) BackupByID(id int64) (*Backup, error) {
	return ptr(queryOne(s.db, scanBackup, `SELECT `+backupCols+` FROM backups WHERE id = ?`, id))
}

func (s *Store) DeleteBackup(id int64) error {
	_, err := s.db.Exec(`DELETE FROM backups WHERE id = ?`, id)
	return err
}
