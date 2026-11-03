package store

import (
	"database/sql"
	"strings"
	"time"
	"unicode/utf8"
)

// Access to Files from outside the browser: WebDAV app passwords and
// public share links. See migration 0027.

// FilesAccessToken is one WebDAV app password (digest at rest).
type FilesAccessToken struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	CreatedAt  string `json:"createdAt"`
	LastUsedAt string `json:"lastUsedAt,omitempty"`
}

func (s *Store) CreateFilesAccessToken(name, digest string) (*FilesAccessToken, error) {
	createdAt := now()
	var id int64
	err := s.withTx(func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM files_access_tokens WHERE name = ?`, name).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return ErrNameTaken
		}
		res, err := tx.Exec(`INSERT INTO files_access_tokens (name, digest, created_at) VALUES (?, ?, ?)`, name, digest, createdAt)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		return nil, err
	}
	return &FilesAccessToken{ID: id, Name: name, CreatedAt: createdAt}, nil
}

func (s *Store) ListFilesAccessTokens() ([]FilesAccessToken, error) {
	return orEmpty(queryAll(s.db, scanFilesAccessToken, `SELECT `+filesAccessTokenCols+` FROM files_access_tokens ORDER BY created_at, id`))
}

const filesAccessTokenCols = `id, name, created_at, COALESCE(last_used_at, '')`

func scanFilesAccessToken(sc scanner) (FilesAccessToken, error) {
	var t FilesAccessToken
	err := sc.Scan(&t.ID, &t.Name, &t.CreatedAt, &t.LastUsedAt)
	return t, err
}

// FilesAccessTokenByDigest resolves a presented password and records the
// use, throttled like MCP tokens: WebDAV clients make dozens of requests
// a second and the store has one connection.
func (s *Store) FilesAccessTokenByDigest(digest string) (*FilesAccessToken, error) {
	t, err := ptr(queryOne(s.db, scanFilesAccessToken, `SELECT `+filesAccessTokenCols+` FROM files_access_tokens WHERE digest = ?`, digest))
	if err != nil {
		return nil, err
	}
	if at, perr := time.Parse(time.RFC3339, t.LastUsedAt); perr == nil && time.Since(at) < lastSeenThrottle {
		return t, nil
	}
	used := now()
	if _, err := s.db.Exec(`UPDATE files_access_tokens SET last_used_at = ? WHERE id = ?`, used, t.ID); err == nil {
		t.LastUsedAt = used
	}
	return t, nil
}

func (s *Store) FilesAccessTokenByID(id int64) (*FilesAccessToken, error) {
	return ptr(queryOne(s.db, scanFilesAccessToken, `SELECT `+filesAccessTokenCols+` FROM files_access_tokens WHERE id = ?`, id))
}

func (s *Store) DeleteFilesAccessToken(id int64) error {
	res, err := s.db.Exec(`DELETE FROM files_access_tokens WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- shares ----

// FileShare is one public link. The token itself is never stored.
type FileShare struct {
	ID          int64  `json:"id"`
	Path        string `json:"path"`
	CreatedAt   string `json:"createdAt"`
	ExpiresAt   string `json:"expiresAt,omitempty"`
	HasPassword bool   `json:"hasPassword"`
	Hits        int64  `json:"hits"`
	LastUsedAt  string `json:"lastUsedAt,omitempty"`
	// InTrash: the item went to the trash and the link is suspended until
	// it is restored. Broken: the path no longer holds the item the link
	// was made for (files.Manager.ListShares says; the store cannot).
	InTrash bool `json:"inTrash,omitempty"`
	Broken  bool `json:"broken,omitempty"`
	// Inode is the shared item's identity (migration 0083); 0 when not yet
	// bound.
	Inode int64 `json:"-"`
	// passwordHash is read only on the resolve path.
	passwordHash string
}

func (f *FileShare) PasswordHash() string { return f.passwordHash }

const fileShareCols = `id, path, created_at, expires_at, password_hash, hits, last_used_at, inode, trashed_with`

func scanFileShare(sc scanner) (FileShare, error) {
	var f FileShare
	var trashed string
	if err := sc.Scan(&f.ID, &f.Path, &f.CreatedAt, &f.ExpiresAt, &f.passwordHash, &f.Hits, &f.LastUsedAt, &f.Inode, &trashed); err != nil {
		return f, err
	}
	f.HasPassword = f.passwordHash != ""
	f.InTrash = trashed != ""
	return f, nil
}

// CreateFileShare records a link to the item at path whose inode is inode.
func (s *Store) CreateFileShare(digest, path, expiresAt, passwordHash string, inode int64) (*FileShare, error) {
	createdAt := now()
	res, err := s.db.Exec(`INSERT INTO file_shares (digest, path, created_at, expires_at, password_hash, inode) VALUES (?, ?, ?, ?, ?, ?)`,
		digest, path, createdAt, expiresAt, passwordHash, inode)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return &FileShare{ID: id, Path: path, CreatedAt: createdAt, ExpiresAt: expiresAt, HasPassword: passwordHash != "", Inode: inode}, nil
}

// ListFileShares returns every share, or the live ones for one path (and,
// for a folder, nothing beneath it — a share is of exactly one entry). A
// link suspended with its item in the trash is not the path's.
func (s *Store) ListFileShares(path string) ([]FileShare, error) {
	q := `SELECT ` + fileShareCols + ` FROM file_shares ORDER BY created_at DESC, id DESC`
	var args []any
	if path != "" || strings.TrimSpace(path) != "" {
		q = `SELECT ` + fileShareCols + ` FROM file_shares WHERE path = ? AND trashed_with = '' ORDER BY created_at DESC, id DESC`
		args = append(args, path)
	}
	return orEmpty(queryAll(s.db, scanFileShare, q, args...))
}

// fileSharesUnder is the WHERE clause for links at path or beneath it.
func fileSharesUnder(path string) (string, []any) {
	n := utf8.RuneCountInString(path)
	return `(path = ? OR (path LIKE ? ESCAPE '\' AND substr(path, 1, ?) = ?))`, []any{path, likeEscape(path) + "/%", n + 1, path + "/"}
}

// LiveFileSharesUnder returns the links at path or beneath it that are not
// suspended.
func (s *Store) LiveFileSharesUnder(path string) ([]FileShare, error) {
	w, args := fileSharesUnder(path)
	return orEmpty(queryAll(s.db, scanFileShare, `SELECT `+fileShareCols+` FROM file_shares WHERE trashed_with = '' AND `+w, args...))
}

// BindFileShare records the inode a link is of.
func (s *Store) BindFileShare(id, inode int64) error {
	_, err := s.db.Exec(`UPDATE file_shares SET inode = ? WHERE id = ?`, inode, id)
	return err
}

// SuspendFileShares suspends the live links at path or beneath it: the
// item went to the trash with receipt trashID.
func (s *Store) SuspendFileShares(path, trashID string) error {
	w, args := fileSharesUnder(path)
	_, err := s.db.Exec(`UPDATE file_shares SET trashed_with = ? WHERE trashed_with = '' AND `+w, append([]any{trashID}, args...)...)
	return err
}

// RestoreFileShares brings back the links suspended with trashID, the item
// that was at old now at new.
func (s *Store) RestoreFileShares(trashID, old, new string) error {
	n := utf8.RuneCountInString(old)
	_, err := s.db.Exec(`UPDATE file_shares SET trashed_with = '', path = ? || substr(path, ?) WHERE trashed_with = ?`, new, n+1, trashID)
	return err
}

// DeleteTrashedFileShares deletes the links suspended with trashID: the
// item is gone for good.
func (s *Store) DeleteTrashedFileShares(trashID string) error {
	_, err := s.db.Exec(`DELETE FROM file_shares WHERE trashed_with = ?`, trashID)
	return err
}

// DeleteFileSharesUnder deletes every link at path or beneath it.
func (s *Store) DeleteFileSharesUnder(path string) error {
	w, args := fileSharesUnder(path)
	_, err := s.db.Exec(`DELETE FROM file_shares WHERE `+w, args...)
	return err
}

func (s *Store) FileShareByDigest(digest string) (*FileShare, error) {
	return ptr(queryOne(s.db, scanFileShare, `SELECT `+fileShareCols+` FROM file_shares WHERE digest = ?`, digest))
}

func (s *Store) FileShareByID(id int64) (*FileShare, error) {
	return ptr(queryOne(s.db, scanFileShare, `SELECT `+fileShareCols+` FROM file_shares WHERE id = ?`, id))
}

// TouchFileShare counts a visit, throttled per share to one write a
// minute so a page full of thumbnails does not write sixty rows.
func (s *Store) TouchFileShare(id int64, lastUsed string) {
	if at, err := time.Parse(time.RFC3339, lastUsed); err == nil && time.Since(at) < time.Minute {
		s.db.Exec(`UPDATE file_shares SET hits = hits + 1 WHERE id = ?`, id)
		return
	}
	s.db.Exec(`UPDATE file_shares SET hits = hits + 1, last_used_at = ? WHERE id = ?`, now(), id)
}

func (s *Store) DeleteFileShare(id int64) error {
	res, err := s.db.Exec(`DELETE FROM file_shares WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
