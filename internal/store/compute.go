// Compute nodes (docs/compute-contract.md): machines paired with fog
// that lend it their hardware. The node's credential is kept as a digest,
// as MCP tokens are. Inventory and budget stay JSON here; internal/compute
// gives them their shape.
package store

import (
	"database/sql"
	"errors"
	"time"
)

// ComputeNode is one paired machine.
type ComputeNode struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Inventory is the node's last report of its machine (compute.Inventory).
	Inventory string `json:"-"`
	// Budget is the operator's override of the machine's slots
	// (compute.Slots), or empty for what its node probed.
	Budget     string `json:"-"`
	Paused     bool   `json:"paused"`
	CreatedBy  string `json:"createdBy"`
	CreatedAt  string `json:"createdAt"`
	LastSeenAt string `json:"lastSeenAt,omitempty"`
	// ConnectedAt is when the machine's service first polled; "" while it
	// never has.
	ConnectedAt string `json:"connectedAt,omitempty"`
	// TokenDigest is the digest of the node's credential: also the key
	// fog presents to its inference listener (computenode.InferenceSecret).
	TokenDigest string `json:"-"`
	// PrevTokenDigest is the digest the node held before it last rotated,
	// taken until it's heard with the new one; "" once it has.
	PrevTokenDigest string `json:"-"`
}

const computeNodeCols = `id, name, inventory, budget, paused, created_by, created_at, last_seen_at, token_digest, connected_at, prev_token_digest`

func scanComputeNode(sc scanner) (ComputeNode, error) {
	var n ComputeNode
	err := sc.Scan(&n.ID, &n.Name, &n.Inventory, &n.Budget, &n.Paused, &n.CreatedBy, &n.CreatedAt, &n.LastSeenAt, &n.TokenDigest, &n.ConnectedAt, &n.PrevTokenDigest)
	return n, err
}

// ErrCodeSpent is a join code that was never issued, already used, or
// expired: the three are told apart to nobody.
var ErrCodeSpent = errors.New("that code is not valid any more")

// CreateComputeJoinCode records a one-time pairing code by its verifier
// (compute keeps the code's salt and stretched key there, never the code).
func (s *Store) CreateComputeJoinCode(digest, createdBy string, ttl time.Duration) error {
	at := time.Now().UTC()
	_, err := s.db.Exec(`INSERT INTO compute_join_codes (digest, created_by, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		digest, createdBy, at.Format(time.RFC3339), at.Add(ttl).Format(time.RFC3339))
	return err
}

// LiveComputeJoinCodes answers the verifiers of every code not yet spent or
// expired: a machine proves it holds one without naming it.
func (s *Store) LiveComputeJoinCodes() ([]string, error) {
	return orEmpty(queryAll(s.db, scanValue[string], `SELECT digest FROM compute_join_codes WHERE expires_at >= ?`, now()))
}

// PairComputeNode spends a join code and records the node it paired, in
// one transaction, so a code pairs one machine however many try it at once.
// Expired codes are swept on the way.
func (s *Store) PairComputeNode(codeDigest string, n *ComputeNode, tokenDigest string) error {
	return s.withTx(func(tx *sql.Tx) error {
		nowAt := now()
		if _, err := tx.Exec(`DELETE FROM compute_join_codes WHERE expires_at < ?`, nowAt); err != nil {
			return err
		}
		var by string
		err := tx.QueryRow(`SELECT created_by FROM compute_join_codes WHERE digest = ?`, codeDigest).Scan(&by)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrCodeSpent
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM compute_join_codes WHERE digest = ?`, codeDigest); err != nil {
			return err
		}
		n.CreatedBy, n.CreatedAt, n.LastSeenAt = by, nowAt, nowAt
		_, err = tx.Exec(`INSERT INTO compute_nodes (id, name, token_digest, inventory, created_by, created_at, last_seen_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, n.ID, n.Name, tokenDigest, n.Inventory, n.CreatedBy, n.CreatedAt, n.LastSeenAt)
		return err
	})
}

// RotateComputeCredential replaces a node's credential digest with fresh,
// keeping the one it presented as the previous until it's heard with the
// new.
func (s *Store) RotateComputeCredential(id, presented, fresh string) error {
	return s.affectOne(`UPDATE compute_nodes SET token_digest = ?, prev_token_digest = ? WHERE id = ?`, fresh, presented, id)
}

// ForgetPreviousComputeCredential drops a node's previous digest: it was
// heard with its current one.
func (s *Store) ForgetPreviousComputeCredential(id string) error {
	_, err := s.db.Exec(`UPDATE compute_nodes SET prev_token_digest = '' WHERE id = ? AND prev_token_digest != ''`, id)
	return err
}

// ComputeNodeByToken resolves a node's credential.
func (s *Store) ComputeNodeByToken(digest string) (*ComputeNode, error) {
	return ptr(queryOne(s.db, scanComputeNode, `SELECT `+computeNodeCols+` FROM compute_nodes WHERE token_digest = ?`, digest))
}

// ComputeNode fetches one node.
func (s *Store) ComputeNode(id string) (*ComputeNode, error) {
	return ptr(queryOne(s.db, scanComputeNode, `SELECT `+computeNodeCols+` FROM compute_nodes WHERE id = ?`, id))
}

// ListComputeNodes answers every paired node, oldest first.
func (s *Store) ListComputeNodes() ([]ComputeNode, error) {
	return orEmpty(queryAll(s.db, scanComputeNode, `SELECT `+computeNodeCols+` FROM compute_nodes ORDER BY created_at, id`))
}

// SetComputeInventory records a node's report of its machine and when it
// was last heard from.
func (s *Store) SetComputeInventory(id, inventory, seen string) error {
	return s.affectOne(`UPDATE compute_nodes SET inventory = ?, last_seen_at = ? WHERE id = ?`, inventory, seen, id)
}

// TouchComputeNode records when a node was last heard from.
func (s *Store) TouchComputeNode(id, seen string) error {
	return s.affectOne(`UPDATE compute_nodes SET last_seen_at = ? WHERE id = ?`, seen, id)
}

// SetComputeNodeSettings changes what the operator controls about a node:
// its name, its budget (JSON, or empty for the probe's) and whether it's
// paused.
func (s *Store) SetComputeNodeSettings(id, name, budget string, paused bool) error {
	return s.affectOne(`UPDATE compute_nodes SET name = ?, budget = ?, paused = ? WHERE id = ?`, name, budget, paused, id)
}

// DeleteComputeNode forgets a node; its credential stops working at once.
func (s *Store) DeleteComputeNode(id string) error {
	return s.affectOne(`DELETE FROM compute_nodes WHERE id = ?`, id)
}

// affectOne runs a write that must touch exactly one row.
func (s *Store) affectOne(query string, args ...any) error {
	res, err := s.db.Exec(query, args...)
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

// SetAppCompute records what an app's fog.yaml declares about compute, as
// JSON; "" is none.
func (s *Store) SetAppCompute(appID int64, declared string) error {
	_, err := s.db.Exec(`UPDATE apps SET compute = ?, updated_at = ? WHERE id = ?`, declared, now(), appID)
	return err
}

// AllAppCompute answers every app's recorded compute declaration by id,
// leaving out the apps that declare none.
func (s *Store) AllAppCompute() (map[int64]string, error) {
	out := map[int64]string{}
	err := queryEach(s.db, func(sc scanner) error {
		var id int64
		var raw string
		if err := sc.Scan(&id, &raw); err != nil {
			return err
		}
		out[id] = raw
		return nil
	}, `SELECT id, compute FROM apps WHERE compute != ''`)
	return out, err
}

// ComputeTranscoder answers an app's name for a pool's transcoder.
func (s *Store) ComputeTranscoder(appID int64, pool string) (string, error) {
	return queryOne(s.db, scanValue[string], `SELECT remote_id FROM compute_transcoders WHERE app_id = ? AND pool = ?`, appID, pool)
}

// SetComputeTranscoder records an app's name for a pool's transcoder.
func (s *Store) SetComputeTranscoder(appID int64, pool, remoteID string) error {
	_, err := s.db.Exec(`INSERT INTO compute_transcoders (app_id, pool, remote_id, created_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (app_id, pool) DO UPDATE SET remote_id = excluded.remote_id`, appID, pool, remoteID, now())
	return err
}

// DeleteComputeTranscoder forgets an app's name for a pool's transcoder.
func (s *Store) DeleteComputeTranscoder(appID int64, pool string) error {
	_, err := s.db.Exec(`DELETE FROM compute_transcoders WHERE app_id = ? AND pool = ?`, appID, pool)
	return err
}

// ComputeGrantOff reports whether the operator switched pool off for app.
func (s *Store) ComputeGrantOff(appID int64, pool string) (bool, error) {
	off, err := queryOne(s.db, scanValue[bool], `SELECT off FROM compute_grants WHERE app_id = ? AND pool = ?`, appID, pool)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return off, err
}

// SetComputeGrant switches pool on or off for app.
func (s *Store) SetComputeGrant(appID int64, pool string, on bool) error {
	if on {
		_, err := s.db.Exec(`DELETE FROM compute_grants WHERE app_id = ? AND pool = ?`, appID, pool)
		return err
	}
	_, err := s.db.Exec(`INSERT INTO compute_grants (app_id, pool, off) VALUES (?, ?, 1)
		ON CONFLICT (app_id, pool) DO UPDATE SET off = 1`, appID, pool)
	return err
}

// AppCompute answers one app's recorded compute declaration; "" is none.
func (s *Store) AppCompute(appID int64) (string, error) {
	return queryOne(s.db, scanValue[string], `SELECT compute FROM apps WHERE id = ?`, appID)
}

// MarkComputeNodeConnected records a machine's first poll, once.
func (s *Store) MarkComputeNodeConnected(id, at string) error {
	_, err := s.db.Exec(`UPDATE compute_nodes SET connected_at = ? WHERE id = ? AND connected_at = ''`, at, id)
	return err
}
