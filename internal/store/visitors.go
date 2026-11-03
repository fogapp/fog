// Visitors (docs/signin-contract.md, 0057): a paired household's
// person, signed in to an app here by their own fog. Thin queries; the rules
// are internal/server's visit_*_handlers.go.
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"gitea.lan/matthew/fog/internal/randx"
)

// CapPeopleVisit: a grant lets the peer sign this household's people in to
// its apps as visitors — this fog vouches for who they are. Config
// {"people": "members" | ["handle", …]}.
const CapPeopleVisit = "people.visit"

// ---- host side ----

// VisitorGrant is one peer's people allowed into one app, as role.
type VisitorGrant struct {
	PeerID int64  `json:"peerId"`
	Role   string `json:"role"`
}

func (s *Store) VisitorGrants(clientID string) ([]VisitorGrant, error) {
	return queryAll(s.db, func(sc scanner) (VisitorGrant, error) {
		var g VisitorGrant
		return g, sc.Scan(&g.PeerID, &g.Role)
	}, `SELECT peer_id, role FROM visitor_grants WHERE client_id = ? ORDER BY peer_id`, clientID)
}

// PutVisitorGrants replaces an app's visitor grants whole. A peer whose
// grant is withdrawn or changed loses what its visitors hold in the app:
// the app is told each session ended (the outbox), and their tokens go.
func (s *Store) PutVisitorGrants(clientID string, grants []VisitorGrant) error {
	return s.withTx(func(tx *sql.Tx) error {
		kept := map[int64]string{}
		for _, g := range grants {
			kept[g.PeerID] = g.Role
		}
		old, err := queryAll(tx, func(sc scanner) (VisitorGrant, error) {
			var g VisitorGrant
			return g, sc.Scan(&g.PeerID, &g.Role)
		}, `SELECT peer_id, role FROM visitor_grants WHERE client_id = ?`, clientID)
		if err != nil {
			return err
		}
		for _, g := range old {
			if role, ok := kept[g.PeerID]; ok && role == g.Role {
				continue
			}
			if _, err := tx.Exec(`INSERT INTO app_outbox (kind, target, idem_key, payload, next_at, created_at)
				SELECT 'logout', vc.client_id, lower(hex(randomblob(16))), json_object('sid', v.sid), ?, ?
				FROM visitor_session_clients vc
				JOIN visitor_sessions v ON v.id = vc.visitor_session_id
				JOIN oidc_clients c ON c.client_id = vc.client_id
				WHERE vc.client_id = ? AND v.peer_id = ? AND c.backchannel_uri != ''`, now(), now(), clientID, g.PeerID); err != nil {
				return err
			}
			for _, q := range []string{
				`DELETE FROM visitor_access_tokens WHERE client_id = ? AND visitor_session_id IN (SELECT id FROM visitor_sessions WHERE peer_id = ?)`,
				`DELETE FROM visitor_session_clients WHERE client_id = ? AND visitor_session_id IN (SELECT id FROM visitor_sessions WHERE peer_id = ?)`,
			} {
				if _, err := tx.Exec(q, clientID, g.PeerID); err != nil {
					return err
				}
			}
		}
		if _, err := tx.Exec(`DELETE FROM visitor_grants WHERE client_id = ?`, clientID); err != nil {
			return err
		}
		for _, g := range grants {
			if _, err := tx.Exec(`INSERT INTO visitor_grants (client_id, peer_id, role) VALUES (?, ?, ?)`, clientID, g.PeerID, g.Role); err != nil {
				return err
			}
		}
		return nil
	})
}

// VisitorGrant finds one: whether peer's people may visit client, as what.
func (s *Store) VisitorGrant(clientID string, peerID int64) (string, error) {
	return queryOne(s.db, scanValue[string], `SELECT role FROM visitor_grants WHERE client_id = ? AND peer_id = ?`, clientID, peerID)
}

// SetClientVisitorRole records the most an app lets a visitor be.
func (s *Store) SetClientVisitorRole(clientID, role string) error {
	_, err := s.db.Exec(`UPDATE oidc_clients SET visitor_role = ? WHERE client_id = ?`, role, clientID)
	return err
}

func (s *Store) ClientVisitorRole(clientID string) (string, error) {
	return queryOne(s.db, scanValue[string], `SELECT visitor_role FROM oidc_clients WHERE client_id = ?`, clientID)
}

// VisitorSession is a visitor signed in here.
type VisitorSession struct {
	ID          int64
	SID         string
	PeerID      int64
	Address     string
	Handle      string
	DisplayName string
	HomeSID     string
	AMR         []string
	AuthTime    time.Time
	ExpiresAt   time.Time
}

const visitorSessionCols = `id, sid, peer_id, address, handle, display_name, home_sid, amr, auth_time, expires_at`

func scanVisitorSession(sc scanner) (VisitorSession, error) {
	var v VisitorSession
	var amr, at, exp string
	err := sc.Scan(&v.ID, &v.SID, &v.PeerID, &v.Address, &v.Handle, &v.DisplayName, &v.HomeSID, &amr, &at, &exp)
	v.AMR, v.AuthTime, v.ExpiresAt = decodeAMR(amr), parseTime(at), parseTime(exp)
	return v, err
}

func (s *Store) CreateVisitorSession(tokenDigest, sid string, v VisitorSession) (int64, error) {
	amr, _ := json.Marshal(nonNil(v.AMR))
	res, err := s.db.Exec(`INSERT INTO visitor_sessions (token_digest, sid, peer_id, address, handle, display_name, home_sid, amr, auth_time, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, tokenDigest, sid, v.PeerID, v.Address, v.Handle, v.DisplayName, v.HomeSID, string(amr),
		v.AuthTime.UTC().Format(time.RFC3339Nano), now(), stamp(v.ExpiresAt))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// VisitorSessionByDigest resolves a visitor cookie; an expired session is
// removed on the way (its trigger tells the apps).
func (s *Store) VisitorSessionByDigest(digest string) (*VisitorSession, error) {
	v, err := queryOne(s.db, scanVisitorSession, `SELECT `+visitorSessionCols+` FROM visitor_sessions WHERE token_digest = ?`, digest)
	if err != nil {
		return nil, err
	}
	if time.Now().After(v.ExpiresAt) {
		s.db.Exec(`DELETE FROM visitor_sessions WHERE id = ?`, v.ID)
		return nil, ErrNotFound
	}
	return &v, nil
}

func (s *Store) VisitorSessionByID(id int64) (*VisitorSession, error) {
	return ptr(queryOne(s.db, scanVisitorSession, `SELECT `+visitorSessionCols+` FROM visitor_sessions WHERE id = ? AND expires_at > ?`, id, now()))
}

// EndVisitorSessions ends the visitor sessions a peer's session became
// here (the peer signed its person out at home).
func (s *Store) EndVisitorSessions(peerID int64, homeSID string) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM visitor_sessions WHERE peer_id = ? AND home_sid = ?`, peerID, homeSID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) DeleteVisitorSessionByDigest(digest string) error {
	_, err := s.db.Exec(`DELETE FROM visitor_sessions WHERE token_digest = ?`, digest)
	return err
}

func (s *Store) RecordVisitorSignIn(visitorSessionID int64, clientID string) error {
	_, err := s.db.Exec(`INSERT INTO visitor_session_clients (visitor_session_id, client_id) VALUES (?, ?) ON CONFLICT DO NOTHING`, visitorSessionID, clientID)
	return err
}

// CreateVisitorAccessToken keeps a visitor's access token; codeDigest is the
// code it was redeemed from, which a replay revokes.
func (s *Store) CreateVisitorAccessToken(digest, clientID string, visitorSessionID int64, codeDigest, grant string, ttl time.Duration) error {
	_, err := s.db.Exec(`INSERT INTO visitor_access_tokens (digest, client_id, visitor_session_id, code_digest, grant, expires_at) VALUES (?, ?, ?, ?, ?, ?)`,
		digest, clientID, visitorSessionID, codeDigest, grant, stamp(time.Now().Add(ttl)))
	return err
}

// RevokeVisitorAccessToken ends one visitor access token the client holds.
func (s *Store) RevokeVisitorAccessToken(digest, clientID string) error {
	_, err := s.db.Exec(`DELETE FROM visitor_access_tokens WHERE digest = ? AND client_id = ?`, digest, clientID)
	return err
}

// VisitorAccessToken is a live visitor access token.
type VisitorAccessToken struct {
	ClientID         string
	VisitorSessionID int64
	Grant            string
}

func (s *Store) VisitorAccessTokenByDigest(digest string) (*VisitorAccessToken, error) {
	return ptr(queryOne(s.db, func(sc scanner) (VisitorAccessToken, error) {
		var t VisitorAccessToken
		return t, sc.Scan(&t.ClientID, &t.VisitorSessionID, &t.Grant)
	}, `SELECT client_id, visitor_session_id, grant FROM visitor_access_tokens WHERE digest = ? AND expires_at > ?`, digest, now()))
}

// ---- both sides: the fog-to-fog flow ----

const (
	VisitPAR  = "par"
	VisitCode = "code"
	VisitTxn  = "txn"
)

// VisitRequest is a stored request, code or transaction of the flow.
type VisitRequest struct {
	PeerID    int64
	SessionID int64
	Params    string
	Grant     string
}

func (s *Store) CreateVisitRequest(digest, kind string, peerID, sessionID int64, params, grant string, ttl time.Duration) error {
	var sess any
	if sessionID != 0 {
		sess = sessionID
	}
	_, err := s.db.Exec(`INSERT INTO visit_requests (digest, kind, peer_id, session_id, params, grant, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		digest, kind, peerID, sess, params, grant, stamp(time.Now().Add(ttl)))
	return err
}

// PeekVisitRequest reads a live, unspent row without spending it.
func (s *Store) PeekVisitRequest(digest, kind string) (*VisitRequest, error) {
	return ptr(queryOne(s.db, func(sc scanner) (VisitRequest, error) {
		var r VisitRequest
		return r, sc.Scan(&r.PeerID, &r.SessionID, &r.Params, &r.Grant)
	}, `SELECT peer_id, COALESCE(session_id, 0), params, grant FROM visit_requests WHERE digest = ? AND kind = ? AND used_at IS NULL AND expires_at > ?`,
		digest, kind, now()))
}

// SpendVisitRequest marks a row used, exactly once.
func (s *Store) SpendVisitRequest(digest, kind string) (*VisitRequest, error) {
	var r VisitRequest
	err := s.withTx(func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE visit_requests SET used_at = ? WHERE digest = ? AND kind = ? AND used_at IS NULL AND expires_at > ?`, now(), digest, kind, now())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return tx.QueryRow(`SELECT peer_id, COALESCE(session_id, 0), params, grant FROM visit_requests WHERE digest = ?`, digest).
			Scan(&r.PeerID, &r.SessionID, &r.Params, &r.Grant)
	})
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// SpendVisitCode is SpendVisitRequest for a home's code, which says when
// it is replayed: a code presented again returns the row it was minted for
// and ErrReplayed, so the home can recall what the first redemption issued
// (RFC 6749 §4.1.2). A code that never existed, or ran out, is ErrNotFound.
func (s *Store) SpendVisitCode(digest string) (*VisitRequest, error) {
	r, err := s.SpendVisitRequest(digest, VisitCode)
	if err != ErrNotFound {
		return r, err
	}
	used, err := queryOne(s.db, func(sc scanner) (VisitRequest, error) {
		var r VisitRequest
		return r, sc.Scan(&r.PeerID, &r.SessionID, &r.Params, &r.Grant)
	}, `SELECT peer_id, COALESCE(session_id, 0), params, grant FROM visit_requests WHERE digest = ? AND kind = ? AND used_at IS NOT NULL`, digest, VisitCode)
	if err != nil {
		return nil, ErrNotFound
	}
	return &used, ErrReplayed
}

// EnqueueVisitLogout owes peer a visit logout for one of our sessions (by
// its sid): every visit it became there ends.
func (s *Store) EnqueueVisitLogout(peerID int64, sid string) error {
	_, err := s.db.Exec(`INSERT INTO app_outbox (kind, target, idem_key, payload, next_at, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		AppOutboxVisitLogout, fmt.Sprintf("peer:%d", peerID), randx.Hex(16), fmt.Sprintf(`{"sid":%q}`, sid), now(), now())
	return err
}

// RecordVisitOut notes, home side, that a peer signed one of our sessions
// in as a visitor: whom to tell when that session ends.
func (s *Store) RecordVisitOut(sessionID, peerID int64) error {
	_, err := s.db.Exec(`INSERT INTO visit_sessions_out (session_id, peer_id) VALUES (?, ?) ON CONFLICT DO NOTHING`, sessionID, peerID)
	return err
}

// RecallVisits is the home withdrawing its word: every session of ours a
// peer hosts, for someone stillAllowed no longer covers (nil: everyone),
// is owed a visit logout, and the peer is forgotten as a host of it.
// Called when people.visit or people.directory to that peer is switched
// off or narrowed, and on unpairing. stillAllowed runs outside any
// transaction, so it may read the store.
func (s *Store) RecallVisits(peerID int64, stillAllowed func(handle string) bool) error {
	type out struct {
		sessionID int64
		sid       string
		handle    string
	}
	rows, err := queryAll(s.db, func(sc scanner) (out, error) {
		var o out
		return o, sc.Scan(&o.sessionID, &o.sid, &o.handle)
	}, `SELECT vo.session_id, COALESCE(ps.sid, ''), p.handle FROM visit_sessions_out vo
		JOIN people_sessions ps ON ps.id = vo.session_id
		JOIN people p ON p.id = ps.person_id
		WHERE vo.peer_id = ?`, peerID)
	if err != nil {
		return err
	}
	recall := rows[:0]
	for _, o := range rows {
		if stillAllowed == nil || !stillAllowed(o.handle) {
			recall = append(recall, o)
		}
	}
	return s.withTx(func(tx *sql.Tx) error {
		for _, o := range recall {
			res, err := tx.Exec(`DELETE FROM visit_sessions_out WHERE session_id = ? AND peer_id = ?`, o.sessionID, peerID)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 || o.sid == "" {
				continue // recalled already, by a concurrent change
			}
			if _, err := tx.Exec(`INSERT INTO app_outbox (kind, target, idem_key, payload, next_at, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
				AppOutboxVisitLogout, fmt.Sprintf("peer:%d", peerID), randx.Hex(16), fmt.Sprintf(`{"sid":%q}`, o.sid), now(), now()); err != nil {
				return err
			}
		}
		return nil
	})
}

// PruneVisits drops expired requests and visitor sessions (the latter
// through their trigger, so apps hear).
func (s *Store) PruneVisits() (int64, error) {
	return s.pruneExpired(
		expiry{"visit_requests", "expires_at <= ?"},
		expiry{"visitor_sessions", "expires_at <= ?"},
		expiry{"visitor_access_tokens", "expires_at <= ?"},
	)
}
