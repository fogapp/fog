// The federation outbox: per-peer, per-kind store-and-forward queues for
// everything this instance sends its peers (chat fan-out, flare events). Bounded —
// the local copy is the durable record and these streams are a courtesy.
package store

import (
	"database/sql"
	"fmt"
	"strings"
)

type OutboxRow struct {
	ID       int64
	PeerID   int64
	AppName  string
	Payload  string
	Attempts int
}

// outboxCap bounds each (peer, kind) queue. When a peer is down for long,
// the oldest queued payloads go first.
const outboxCap = 1000

func (s *Store) EnqueueFedOutbox(peerID int64, kind, appName, payload string) error {
	return s.EnqueueFedOutboxAll([]int64{peerID}, kind, appName, payload)
}

// EnqueueFedOutboxAll queues the same payload for several peers in one
// transaction — a hub fanning one event out to every follower.
func (s *Store) EnqueueFedOutboxAll(peerIDs []int64, kind, appName, payload string) error {
	if len(peerIDs) == 0 {
		return nil
	}
	n := now()
	return s.withTx(func(tx *sql.Tx) error {
		for _, peerID := range peerIDs {
			if _, err := tx.Exec(`INSERT INTO fed_outbox (peer_id, kind, app_name, payload, next_attempt_at, created_at)
				VALUES (?, ?, ?, ?, ?, ?)`, peerID, kind, appName, payload, n, n); err != nil {
				return err
			}
			if _, err := tx.Exec(`DELETE FROM fed_outbox WHERE peer_id = ? AND kind = ? AND id NOT IN
				(SELECT id FROM fed_outbox WHERE peer_id = ? AND kind = ? ORDER BY id DESC LIMIT ?)`,
				peerID, kind, peerID, kind, outboxCap); err != nil {
				return err
			}
		}
		return nil
	})
}

// DueFedOutbox returns one peer's due rows for a kind, oldest first.
func (s *Store) DueFedOutbox(peerID int64, kind string, limit int) ([]OutboxRow, error) {
	return queryAll(s.db, func(sc scanner) (OutboxRow, error) {
		var r OutboxRow
		err := sc.Scan(&r.ID, &r.PeerID, &r.AppName, &r.Payload, &r.Attempts)
		return r, err
	}, `SELECT id, peer_id, app_name, payload, attempts FROM fed_outbox
		WHERE peer_id = ? AND kind = ? AND next_attempt_at <= ? ORDER BY id LIMIT ?`,
		peerID, kind, now(), limit)
}

// PeersWithDueFedOutbox lists the peers with rows of a kind due now.
func (s *Store) PeersWithDueFedOutbox(kind string) ([]int64, error) {
	return queryAll(s.db, scanValue[int64], `SELECT DISTINCT peer_id FROM fed_outbox WHERE kind = ? AND next_attempt_at <= ?`, kind, now())
}

func (s *Store) DeleteFedOutbox(ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	q, args := inClause(`DELETE FROM fed_outbox WHERE id IN`, ids)
	_, err := s.db.Exec(q, args...)
	return err
}

// DropFedOutboxKind discards everything of one kind queued for a peer.
func (s *Store) DropFedOutboxKind(peerID int64, kind string) error {
	_, err := s.db.Exec(`DELETE FROM fed_outbox WHERE peer_id = ? AND kind = ?`, peerID, kind)
	return err
}

// DeferFedOutbox reschedules failed rows to nextAttempt (RFC3339), bumping
// the attempt counter.
func (s *Store) DeferFedOutbox(ids []int64, nextAttempt string) error {
	if len(ids) == 0 {
		return nil
	}
	q, args := inClause(`UPDATE fed_outbox SET attempts = attempts + 1, next_attempt_at = ? WHERE id IN`, ids)
	_, err := s.db.Exec(q, append([]any{nextAttempt}, args...)...)
	return err
}

func inClause(prefix string, ids []int64) (string, []any) {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return fmt.Sprintf("%s (%s)", prefix, strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")), args
}
