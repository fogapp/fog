// Named MCP credentials: one row per AI client the operator has connected.
// Tokens are stored as digests only (session discipline), and lookup is by
// digest — matching the plaintext against every row would mean holding it
// in memory once per token for no gain.
package store

import (
	"database/sql"
	"errors"
	"time"
)

// ErrNameTaken is returned instead of a raw UNIQUE violation, so callers can
// answer 409 rather than 500 when the operator reuses a name.
var ErrNameTaken = errors.New("name already in use")

type MCPToken struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	// CanPostChat lets the assistant holding this token post into chat rooms
	// the fog bot is in. Off unless the operator checked it at creation.
	CanPostChat bool `json:"canPostChat"`
	// CanReadAllFiles lets the token's Drive tools reach Apps/ (every app's
	// own data, credential stores among it) and dotfiles. Without it those
	// are out of reach. Off unless the operator checked it at creation.
	CanReadAllFiles bool `json:"canReadAllFiles"`
	// RFC3339. LastUsedAt is empty until the token authenticates a request,
	// and lags by up to lastSeenThrottle after that.
	CreatedAt  string `json:"createdAt"`
	LastUsedAt string `json:"lastUsedAt,omitempty"`
}

// CreateMCPToken records a new named token, whether it may post to chat,
// and whether its Drive tools reach app data and dotfiles.
// The name check and the insert share a transaction so a duplicate is a
// clean ErrNameTaken rather than a constraint error from the index backing
// it up.
func (s *Store) CreateMCPToken(name, digest string, canPostChat, canReadAllFiles bool) (*MCPToken, error) {
	createdAt := now()
	post, allFiles := 0, 0
	if canPostChat {
		post = 1
	}
	if canReadAllFiles {
		allFiles = 1
	}
	var id int64
	err := s.withTx(func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM mcp_tokens WHERE name = ?`, name).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return ErrNameTaken
		}
		res, err := tx.Exec(`INSERT INTO mcp_tokens (name, digest, created_at, can_post_chat, can_read_all_files) VALUES (?, ?, ?, ?, ?)`,
			name, digest, createdAt, post, allFiles)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		return nil, err
	}
	return &MCPToken{ID: id, Name: name, CreatedAt: createdAt, CanPostChat: canPostChat, CanReadAllFiles: canReadAllFiles}, nil
}

func (s *Store) ListMCPTokens() ([]MCPToken, error) {
	return orEmpty(queryAll(s.db, scanMCPToken, `SELECT `+mcpTokenCols+` FROM mcp_tokens ORDER BY created_at, id`))
}

const mcpTokenCols = `id, name, created_at, COALESCE(last_used_at, ''), can_post_chat, can_read_all_files`

func scanMCPToken(sc scanner) (MCPToken, error) {
	var t MCPToken
	var post, allFiles int
	err := sc.Scan(&t.ID, &t.Name, &t.CreatedAt, &t.LastUsedAt, &post, &allFiles)
	t.CanPostChat = post == 1
	t.CanReadAllFiles = allFiles == 1
	return t, err
}

// MCPTokenByDigest resolves a presented token and records that it was used,
// at most once per lastSeenThrottle — this runs on every MCP call, and the
// store holds a single connection, so an unconditional UPDATE would take a
// write lock per tool call just to move a timestamp.
func (s *Store) MCPTokenByDigest(digest string) (*MCPToken, error) {
	t, err := ptr(queryOne(s.db, scanMCPToken, `SELECT `+mcpTokenCols+` FROM mcp_tokens WHERE digest = ?`, digest))
	if err != nil {
		return nil, err
	}
	at, perr := time.Parse(time.RFC3339, t.LastUsedAt)
	if perr == nil && time.Since(at) < lastSeenThrottle {
		return t, nil
	}
	used := now()
	if _, err := s.db.Exec(`UPDATE mcp_tokens SET last_used_at = ? WHERE id = ?`, used, t.ID); err == nil {
		t.LastUsedAt = used
	}
	return t, nil
}

// MCPTokenByID fetches one token row without touching last_used_at — this is
// the management path (naming a token in an audit entry), not the auth path.
func (s *Store) MCPTokenByID(id int64) (*MCPToken, error) {
	return ptr(queryOne(s.db, scanMCPToken, `SELECT `+mcpTokenCols+` FROM mcp_tokens WHERE id = ?`, id))
}

func (s *Store) DeleteMCPToken(id int64) error {
	res, err := s.db.Exec(`DELETE FROM mcp_tokens WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
