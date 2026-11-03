// The OpenID provider's rows (docs/signin-contract.md): clients, the
// requests and codes in flight, access tokens, and who has signed in where.
// Thin queries; the protocol is internal/oidc's.
package store

import (
	"database/sql"
	"encoding/json"
	"strings"
	"time"
)

// OIDCClient is a client row. Secrets are digests; the plaintext is the
// vault's.
type OIDCClient struct {
	ClientID string
	AppID    int64
	// SecretDigest is the secret the running app holds. NextSecretDigest is
	// a rotation's new secret, waiting for a deploy to carry it; the app's
	// first use of it promotes it (PromoteOIDCClientSecret). PrevSecretDigest
	// is the one a promotion replaced, honoured until PrevSecretUntil so the
	// old half of a blue-green swap can finish (0073).
	SecretDigest     string
	NextSecretDigest string
	PrevSecretDigest string
	PrevSecretUntil  time.Time
	RedirectURIs     []string
	PostLogoutURIs   []string
	BackchannelURI   string
	SCIMBaseURL      string
	Scopes           []string
	RequirePAR       bool
	RequiredACR      string
	Roles            []string
	RoleMap          map[string]string
	// DeviceGrant lets the client sign people in on a device by code
	// (RFC 8628; 0056).
	DeviceGrant bool
}

const oidcClientCols = `client_id, app_id, secret_digest, next_secret_digest, prev_secret_digest, prev_secret_until, redirect_uris, post_logout_uris, backchannel_uri, scim_base_url, scopes, require_par, required_acr, roles, role_map, device_grant`

func scanOIDCClient(sc scanner) (OIDCClient, error) {
	var c OIDCClient
	var redirects, postLogout, scopes, roles, roleMap, prevUntil string
	err := sc.Scan(&c.ClientID, &c.AppID, &c.SecretDigest, &c.NextSecretDigest, &c.PrevSecretDigest, &prevUntil, &redirects, &postLogout, &c.BackchannelURI, &c.SCIMBaseURL, &scopes, &c.RequirePAR, &c.RequiredACR, &roles, &roleMap, &c.DeviceGrant)
	if err != nil {
		return c, err
	}
	c.PrevSecretUntil = parseTime(prevUntil)
	for _, f := range []struct {
		raw string
		dst any
	}{{redirects, &c.RedirectURIs}, {postLogout, &c.PostLogoutURIs}, {scopes, &c.Scopes}, {roles, &c.Roles}, {roleMap, &c.RoleMap}} {
		if err := json.Unmarshal([]byte(f.raw), f.dst); err != nil {
			return c, err
		}
	}
	return c, nil
}

func (s *Store) OIDCClient(clientID string) (*OIDCClient, error) {
	return ptr(queryOne(s.db, scanOIDCClient, `SELECT `+oidcClientCols+` FROM oidc_clients WHERE client_id = ?`, clientID))
}

func (s *Store) OIDCClientForApp(appID int64) (*OIDCClient, error) {
	return ptr(queryOne(s.db, scanOIDCClient, `SELECT `+oidcClientCols+` FROM oidc_clients WHERE app_id = ?`, appID))
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // only ever called with slices and maps of strings
	}
	return string(b)
}

// PutOIDCClient inserts or replaces a client's registration. The secret
// digests are set separately (RotateOIDCClientSecret), so re-registering on
// every deploy never disturbs them.
func (s *Store) PutOIDCClient(c OIDCClient) error {
	_, err := s.db.Exec(`INSERT INTO oidc_clients (client_id, app_id, secret_digest, redirect_uris, post_logout_uris, backchannel_uri, scim_base_url, scopes, require_par, required_acr, roles, role_map, device_grant, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(client_id) DO UPDATE SET app_id = excluded.app_id, redirect_uris = excluded.redirect_uris, post_logout_uris = excluded.post_logout_uris,
			backchannel_uri = excluded.backchannel_uri, scim_base_url = excluded.scim_base_url, scopes = excluded.scopes, require_par = excluded.require_par,
			required_acr = excluded.required_acr, roles = excluded.roles, role_map = excluded.role_map, device_grant = excluded.device_grant, updated_at = excluded.updated_at`,
		c.ClientID, c.AppID, c.SecretDigest, mustJSON(nonNil(c.RedirectURIs)), mustJSON(nonNil(c.PostLogoutURIs)), c.BackchannelURI, c.SCIMBaseURL,
		mustJSON(nonNil(c.Scopes)), c.RequirePAR, c.RequiredACR, mustJSON(nonNil(c.Roles)), mustJSON(nonNilMap(c.RoleMap)), c.DeviceGrant, now(), now())
	return err
}

func nonNilMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// StageOIDCClientSecret records a rotation: digest is the next secret,
// waiting for a deploy to carry it. The running app's secret is untouched,
// so a second rotation before a deploy only replaces the waiting one.
func (s *Store) StageOIDCClientSecret(clientID, digest string) error {
	_, err := s.db.Exec(`UPDATE oidc_clients SET next_secret_digest = ?, updated_at = ? WHERE client_id = ?`, digest, now(), clientID)
	return err
}

// PromoteOIDCClientSecret is the app showing it holds the next secret: it
// becomes current, and the one it replaces is honoured only until overlap
// from now. Idempotent, and a no-op unless next is still the waiting one.
func (s *Store) PromoteOIDCClientSecret(clientID, next string, overlap time.Duration) error {
	_, err := s.db.Exec(`UPDATE oidc_clients SET prev_secret_digest = secret_digest, prev_secret_until = ?, secret_digest = next_secret_digest,
		next_secret_digest = '', updated_at = ? WHERE client_id = ? AND next_secret_digest = ? AND next_secret_digest != ''`,
		stamp(time.Now().Add(overlap)), now(), clientID, next)
	return err
}

// ReplaceOIDCClientSecret is the emergency revoke: digest is the only secret
// that works from now, and everything minted for the client under the old
// one — requests in flight, codes, access tokens, device sign-ins — goes
// with it.
func (s *Store) ReplaceOIDCClientSecret(clientID, digest string) error {
	return s.withTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`UPDATE oidc_clients SET secret_digest = ?, next_secret_digest = '', prev_secret_digest = '', prev_secret_until = '', updated_at = ?
			WHERE client_id = ?`, digest, now(), clientID); err != nil {
			return err
		}
		for _, q := range []string{
			`DELETE FROM oidc_requests WHERE client_id = ?`,
			`DELETE FROM oidc_access_tokens WHERE client_id = ?`,
			`DELETE FROM visitor_access_tokens WHERE client_id = ?`,
			`DELETE FROM oidc_device_codes WHERE client_id = ?`,
		} {
			if _, err := tx.Exec(q, clientID); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) DeleteOIDCClient(clientID string) error {
	_, err := s.db.Exec(`DELETE FROM oidc_clients WHERE client_id = ?`, clientID)
	return err
}

// ---- requests and codes ----

const (
	OIDCRequestPAR  = "par"
	OIDCRequestCode = "code"
)

func (s *Store) CreateOIDCRequest(digest, kind, clientID string, sessionID int64, params, grant string, ttl time.Duration) error {
	var sess any
	if sessionID != 0 {
		sess = sessionID
	}
	_, err := s.db.Exec(`INSERT INTO oidc_requests (digest, kind, client_id, session_id, params, grant, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		digest, kind, clientID, sess, params, grant, stamp(time.Now().Add(ttl)))
	return err
}

// OIDCRequest is a stored request or code.
type OIDCRequest struct {
	ClientID  string
	SessionID int64
	Params    string
	Grant     string
}

// PeekOIDCRequest reads a live, unspent request without spending it: a
// pushed request survives the person signing in midway.
func (s *Store) PeekOIDCRequest(digest, kind string) (*OIDCRequest, error) {
	return ptr(queryOne(s.db, func(sc scanner) (OIDCRequest, error) {
		var r OIDCRequest
		return r, sc.Scan(&r.ClientID, &r.SessionID, &r.Params, &r.Grant)
	}, `SELECT client_id, COALESCE(session_id, 0), params, grant FROM oidc_requests
		WHERE digest = ? AND kind = ? AND used_at IS NULL AND expires_at > ?`, digest, kind, now()))
}

// ErrReplayed is a code presented a second time.
var ErrReplayed = errorString("authorization code replayed")

type errorString string

func (e errorString) Error() string { return string(e) }

// SpendOIDCRequest marks a request or code used, exactly once: the database
// decides, so two racing redemptions cannot both win. A code that was
// already spent returns ErrReplayed — and every access token minted from it,
// a person's or a visitor's, is revoked in the same transaction (RFC 6749
// §4.1.2).
func (s *Store) SpendOIDCRequest(digest, kind string) (*OIDCRequest, error) {
	var r OIDCRequest
	replayed := false
	err := s.withTx(func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE oidc_requests SET used_at = ? WHERE digest = ? AND kind = ? AND used_at IS NULL AND expires_at > ?`,
			now(), digest, kind, now())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			var used sql.NullString
			if tx.QueryRow(`SELECT used_at FROM oidc_requests WHERE digest = ? AND kind = ?`, digest, kind).Scan(&used) == nil && used.Valid {
				// Revoke what the first redemption minted — in this
				// transaction, which must therefore commit.
				for _, q := range []string{
					`DELETE FROM oidc_access_tokens WHERE code_digest = ?`,
					`DELETE FROM visitor_access_tokens WHERE code_digest = ?`,
				} {
					if _, err := tx.Exec(q, digest); err != nil {
						return err
					}
				}
				replayed = true
				return nil
			}
			return ErrNotFound
		}
		return tx.QueryRow(`SELECT client_id, COALESCE(session_id, 0), params, grant FROM oidc_requests WHERE digest = ?`, digest).
			Scan(&r.ClientID, &r.SessionID, &r.Params, &r.Grant)
	})
	if err != nil {
		return nil, err
	}
	if replayed {
		return nil, ErrReplayed
	}
	return &r, nil
}

// ---- access tokens ----

func (s *Store) CreateOIDCAccessToken(digest, clientID string, sessionID int64, codeDigest, grant string, ttl time.Duration) error {
	_, err := s.db.Exec(`INSERT INTO oidc_access_tokens (digest, client_id, session_id, code_digest, grant, expires_at) VALUES (?, ?, ?, ?, ?, ?)`,
		digest, clientID, sessionID, codeDigest, grant, stamp(time.Now().Add(ttl)))
	return err
}

// OIDCAccessToken is a live access token and what it was granted.
type OIDCAccessToken struct {
	ClientID  string
	SessionID int64
	Grant     string
}

func (s *Store) OIDCAccessTokenByDigest(digest string) (*OIDCAccessToken, error) {
	return ptr(queryOne(s.db, func(sc scanner) (OIDCAccessToken, error) {
		var t OIDCAccessToken
		return t, sc.Scan(&t.ClientID, &t.SessionID, &t.Grant)
	}, `SELECT client_id, session_id, grant FROM oidc_access_tokens WHERE digest = ? AND expires_at > ?`, digest, now()))
}

// RevokeOIDCAccessToken deletes a token, only on behalf of the client it was
// issued to (RFC 7009 §2.1).
func (s *Store) RevokeOIDCAccessToken(digest, clientID string) error {
	_, err := s.db.Exec(`DELETE FROM oidc_access_tokens WHERE digest = ? AND client_id = ?`, digest, clientID)
	return err
}

// RevokeOIDCAccessTokensByCode deletes every access token a code (or a
// device code) was redeemed for: what a replay of it revokes.
func (s *Store) RevokeOIDCAccessTokensByCode(codeDigest string) error {
	return s.withTx(func(tx *sql.Tx) error {
		for _, q := range []string{
			`DELETE FROM oidc_access_tokens WHERE code_digest = ?`,
			`DELETE FROM visitor_access_tokens WHERE code_digest = ?`,
		} {
			if _, err := tx.Exec(q, codeDigest); err != nil {
				return err
			}
		}
		return nil
	})
}

// ---- who signed in where ----

// RecordOIDCSignIn notes that a session signed in to a client, and that the
// client has seen the person: whom to tell when either ends.
func (s *Store) RecordOIDCSignIn(sessionID int64, clientID string, personID int64) error {
	return s.withTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO oidc_session_clients (session_id, client_id, first_at) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`,
			sessionID, clientID, now()); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO oidc_links (client_id, person_id, first_at, last_at) VALUES (?, ?, ?, ?)
			ON CONFLICT(client_id, person_id) DO UPDATE SET last_at = excluded.last_at`, clientID, personID, now(), now())
		return err
	})
}

// OIDCLink is one person an app has seen.
type OIDCLink struct {
	PersonID int64  `json:"-"`
	Handle   string `json:"handle"`
	FirstAt  string `json:"firstAt"`
	LastAt   string `json:"lastAt"`
}

func (s *Store) OIDCLinksForClient(clientID string) ([]OIDCLink, error) {
	return queryAll(s.db, func(sc scanner) (OIDCLink, error) {
		var l OIDCLink
		return l, sc.Scan(&l.PersonID, &l.Handle, &l.FirstAt, &l.LastAt)
	}, `SELECT l.person_id, p.handle, l.first_at, l.last_at FROM oidc_links l JOIN people p ON p.id = l.person_id
		WHERE l.client_id = ? ORDER BY l.last_at DESC`, clientID)
}

// OIDCClientsLinkedTo lists the clients that have seen a person: whom a
// change to them may concern.
func (s *Store) OIDCClientsLinkedTo(personID int64) ([]OIDCClient, error) {
	return queryAll(s.db, scanOIDCClient, `SELECT `+prefixCols("c.", oidcClientCols)+` FROM oidc_clients c
		JOIN oidc_links l ON l.client_id = c.client_id WHERE l.person_id = ? ORDER BY c.client_id`, personID)
}

// AllOIDCClients lists every registered client.
func (s *Store) AllOIDCClients() ([]OIDCClient, error) {
	return queryAll(s.db, scanOIDCClient, `SELECT `+oidcClientCols+` FROM oidc_clients ORDER BY client_id`)
}

// prefixCols qualifies a comma-separated column list with a table alias.
func prefixCols(prefix, cols string) string {
	parts := strings.Split(cols, ",")
	for i, p := range parts {
		parts[i] = prefix + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
}

// PruneOIDC drops expired and spent requests and expired access tokens.
func (s *Store) PruneOIDC() (int64, error) {
	return s.pruneExpired(
		// Spent codes are kept until they expire, so a replay inside the
		// window is recognised as one and revokes what it minted.
		expiry{"oidc_requests", "expires_at <= ?"},
		expiry{"oidc_access_tokens", "expires_at <= ?"},
	)
}

// UpdateSessionAssurance records a fresh proof on an existing session — a
// step-up (docs/signin-contract.md) — without minting a new session, so
// the sid apps already hold stays good.
func (s *Store) UpdateSessionAssurance(sessionID int64, amr []string) error {
	_, err := s.db.Exec(`UPDATE people_sessions SET amr = ?, auth_time = ? WHERE id = ?`, mustJSON(nonNil(amr)), time.Now().UTC().Format(time.RFC3339Nano), sessionID)
	return err
}

// SetPersonEmail sets (or clears) a person's optional, unverified email.
func (s *Store) SetPersonEmail(personID int64, email string) error {
	_, err := s.db.Exec(`UPDATE people SET email = ?, email_verified_at = NULL WHERE id = ?`, email, personID)
	return err
}

// ---- SCIM ----

// SCIMState is what fog last told an app about a person.
type SCIMState struct {
	ClientID string
	PersonID int64
	SCIMID   string
	Sent     string
	Active   bool
}

func (s *Store) SCIMStates(clientID string) (map[int64]SCIMState, error) {
	rows, err := queryAll(s.db, func(sc scanner) (SCIMState, error) {
		var st SCIMState
		return st, sc.Scan(&st.ClientID, &st.PersonID, &st.SCIMID, &st.Sent, &st.Active)
	}, `SELECT client_id, person_id, scim_id, sent, active FROM scim_state WHERE client_id = ?`, clientID)
	if err != nil {
		return nil, err
	}
	out := map[int64]SCIMState{}
	for _, r := range rows {
		out[r.PersonID] = r
	}
	return out, nil
}

func (s *Store) PutSCIMState(st SCIMState) error {
	_, err := s.db.Exec(`INSERT INTO scim_state (client_id, person_id, scim_id, sent, active, updated_at) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(client_id, person_id) DO UPDATE SET scim_id = excluded.scim_id, sent = excluded.sent, active = excluded.active, updated_at = excluded.updated_at`,
		st.ClientID, st.PersonID, st.SCIMID, st.Sent, st.Active, now())
	return err
}

// SCIMDeparture is someone deleted from fog whom an app still has to be
// told is no longer active (0073): their row is gone, so this is what is
// left of them.
type SCIMDeparture struct {
	ClientID    string
	PersonUID   string
	SCIMID      string
	UserName    string
	DisplayName string
}

func (s *Store) SCIMDepartures(clientID string) ([]SCIMDeparture, error) {
	return queryAll(s.db, func(sc scanner) (SCIMDeparture, error) {
		var d SCIMDeparture
		return d, sc.Scan(&d.ClientID, &d.PersonUID, &d.SCIMID, &d.UserName, &d.DisplayName)
	}, `SELECT client_id, person_uid, scim_id, user_name, display_name FROM scim_departures WHERE client_id = ? ORDER BY created_at, person_uid`, clientID)
}

// DeleteSCIMDeparture is the app having heard.
func (s *Store) DeleteSCIMDeparture(clientID, personUID string) error {
	_, err := s.db.Exec(`DELETE FROM scim_departures WHERE client_id = ? AND person_uid = ?`, clientID, personUID)
	return err
}

// SCIMFailing records a failed reconcile of a client and returns since
// when it has been failing and whether the operator was already told.
func (s *Store) SCIMFailing(clientID, lastError string) (since time.Time, noticed bool, err error) {
	if _, err := s.db.Exec(`INSERT INTO scim_health (client_id, failing_since, last_error) VALUES (?, ?, ?)
		ON CONFLICT(client_id) DO UPDATE SET last_error = excluded.last_error`, clientID, now(), lastError); err != nil {
		return time.Time{}, false, err
	}
	var at string
	err = s.db.QueryRow(`SELECT failing_since, noticed FROM scim_health WHERE client_id = ?`, clientID).Scan(&at, &noticed)
	return parseTime(at), noticed, err
}

func (s *Store) MarkSCIMNoticed(clientID string) error {
	_, err := s.db.Exec(`UPDATE scim_health SET noticed = 1 WHERE client_id = ?`, clientID)
	return err
}

// SCIMHealthy records a reconcile that worked; noticed says the operator
// had been told it was failing, so the notice can go.
func (s *Store) SCIMHealthy(clientID string) (noticed bool, err error) {
	err = s.db.QueryRow(`DELETE FROM scim_health WHERE client_id = ? RETURNING noticed`, clientID).Scan(&noticed)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return noticed, err
}

// SCIMClients lists the clients that take provisioning.
func (s *Store) SCIMClients() ([]OIDCClient, error) {
	return queryAll(s.db, scanOIDCClient, `SELECT `+oidcClientCols+` FROM oidc_clients WHERE scim_base_url != '' ORDER BY client_id`)
}
