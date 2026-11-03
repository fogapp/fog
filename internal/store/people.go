// People, their credentials, invites, devices, sessions, key packages, and
// backups — the household directory chat is built on
// (docs/chat-contract.md). Every method is a thin query; the rules (handle
// syntax, attestation
// verification, write-once identity) live in internal/people.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"gitea.lan/matthew/fog/internal/randx"
)

const (
	PersonRoleOperator = "operator"
	PersonRoleMember   = "member"
	PersonRoleGuest    = "guest"

	PersonStatusActive    = "active"
	PersonStatusSuspended = "suspended"

	CredentialPasskey  = "passkey"
	CredentialPassword = "password"
)

// ErrNotFound is the store's "no such row" for every lookup that returns one.
var ErrNotFound = errors.New("not found")

// ErrIdentitySet is returned when a person's identity key is written a second
// time. Only the recovery flow may clear it first.
var ErrIdentitySet = errors.New("identity key already set")

// lastSeenThrottle bounds how often an active session rewrites last_seen_at.
//
// This runs on every authenticated request, and the store holds a single
// connection, so an unconditional UPDATE made every API call — including the
// six background polls an open dashboard runs — take a write lock on SQLite
// just to record a timestamp nothing reads at that resolution.
const lastSeenThrottle = 5 * time.Minute

// deviceSeenThrottle is the same bound for a device's last_seen_at, which
// the device list shows, so it stays closer to the truth.
const deviceSeenThrottle = time.Minute

type Person struct {
	ID          int64  `json:"id"`
	Handle      string `json:"handle"`
	DisplayName string `json:"displayName"`
	Role        string `json:"role" ts:"\"operator\" | \"member\" | \"guest\""`
	Status      string `json:"status" ts:"\"active\" | \"suspended\""`
	IdentityKey []byte `json:"identityKey,omitempty"`
	// UID is the person ULID, the stable half of their address
	// (person:<instance>/<uid>) — unchanged by a rename, unlike Handle.
	UID string `json:"uid"`
	// Email is optional and unverified until fog can send mail
	// (docs/signin-contract.md). Never used to link an account.
	Email      string `json:"email,omitempty"`
	CreatedAt  string `json:"createdAt"`
	LastSeenAt string `json:"lastSeenAt,omitempty"`
}

const personCols = `id, handle, display_name, role, status, identity_key, COALESCE(uid, ''), email, created_at, last_seen_at`

func scanPerson(sc scanner) (Person, error) {
	var p Person
	err := sc.Scan(&p.ID, &p.Handle, &p.DisplayName, &p.Role, &p.Status, &p.IdentityKey, &p.UID, &p.Email, &p.CreatedAt, &p.LastSeenAt)
	return p, err
}

func (s *Store) CreatePerson(handle, displayName, role string) (int64, error) {
	return insertPerson(s.db, handle, displayName, role)
}

// CreateBotPerson adds an active bot whose identity key is its own device
// key.
func (s *Store) CreateBotPerson(handle, displayName string, identityKey []byte) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO people (handle, display_name, role, status, identity_key, uid, created_at) VALUES (?, ?, 'bot', ?, ?, ?, ?)`,
		handle, displayName, PersonStatusActive, identityKey, randx.ULID(), now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ErrSetupDone refuses a first operator when the box already has one.
var ErrSetupDone = errors.New("setup already completed")

// CreateFirstOperator creates the box's first operator and their password
// in one transaction, refusing with ErrSetupDone if an operator exists:
// two first-run requests racing cannot both win, and a failed credential
// write cannot leave an operator nobody can sign in as (which would also
// close setup for good).
func (s *Store) CreateFirstOperator(handle, displayName, passwordHash string) (int64, error) {
	var id int64
	err := s.withTx(func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM people WHERE role = ?`, PersonRoleOperator).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return ErrSetupDone
		}
		var err error
		if id, err = insertPerson(tx, handle, displayName, PersonRoleOperator); err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO people_credentials (person_id, kind, data, label, created_at) VALUES (?, ?, ?, ?, ?)`,
			id, CredentialPassword, passwordHash, "password", now())
		return err
	})
	return id, err
}

// JoinPerson spends a live invite on a new person with the invite's role
// and, when passwordHash is set, their password — all or nothing. A spent,
// expired or unknown invite is ErrNotFound; a taken handle is
// ErrAlreadyExists.
func (s *Store) JoinPerson(digest, handle, displayName, passwordHash string) (int64, *PersonInvite, error) {
	var id int64
	var inv PersonInvite
	err := s.withTx(func(tx *sql.Tx) error {
		var err error
		if inv, err = queryOne(tx, scanOpenPersonInvite, openPersonInviteQuery, digest, now()); err != nil {
			return err
		}
		if id, err = insertPerson(tx, handle, displayName, inv.Role); err != nil {
			return err
		}
		if passwordHash != "" {
			if _, err := tx.Exec(`INSERT INTO people_credentials (person_id, kind, data, label, created_at) VALUES (?, ?, ?, ?, ?)`,
				id, CredentialPassword, passwordHash, "", now()); err != nil {
				return err
			}
		}
		_, err = tx.Exec(`UPDATE people_invites SET used_at = ?, used_by = ? WHERE id = ?`, now(), id, inv.ID)
		return err
	})
	if err != nil {
		return 0, nil, err
	}
	return id, &inv, nil
}

func insertPerson(db execer, handle, displayName, role string) (int64, error) {
	res, err := db.Exec(`INSERT INTO people (handle, display_name, role, status, uid, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		handle, displayName, role, PersonStatusActive, randx.ULID(), now())
	if err != nil {
		if isUniqueViolation(err) {
			return 0, fmt.Errorf("the handle %q %w", handle, ErrAlreadyExists)
		}
		return 0, err
	}
	return res.LastInsertId()
}

// CountOperators is what first-run setup asks: does this box have an
// administrator yet. It replaced CountUsers when the operator stopped being
// a row in a table of its own.
func (s *Store) CountOperators() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM people WHERE role = ?`, PersonRoleOperator).Scan(&n)
	return n, err
}

// PruneExpiredSessions clears out person sessions past their expiry. The
// resolver already refuses (and deletes) an expired one it happens to load;
// this is the sweep for the ones nobody comes back for.
func (s *Store) PruneExpiredSessions() (int64, error) {
	return s.pruneBatched("people_sessions", "expires_at < ?", time.Now().UTC().Format(time.RFC3339))
}

func (s *Store) PersonByID(id int64) (*Person, error) {
	return ptr(queryOne(s.db, scanPerson, `SELECT `+personCols+` FROM people WHERE id = ?`, id))
}

func (s *Store) PersonByHandle(handle string) (*Person, error) {
	return ptr(queryOne(s.db, scanPerson, `SELECT `+personCols+` FROM people WHERE handle = ?`, handle))
}

func (s *Store) ListPeople() ([]Person, error) {
	return queryAll(s.db, scanPerson, `SELECT `+personCols+` FROM people ORDER BY role = 'operator' DESC, handle`)
}

func (s *Store) UpdatePerson(id int64, displayName, role, status string) error {
	_, err := s.db.Exec(`UPDATE people SET display_name = ?, role = ?, status = ? WHERE id = ?`, displayName, role, status, id)
	return err
}

func (s *Store) DeletePerson(id int64) error {
	_, err := s.db.Exec(`DELETE FROM people WHERE id = ?`, id)
	return err
}

func (s *Store) TouchPersonSeen(id int64) {
	s.db.Exec(`UPDATE people SET last_seen_at = ? WHERE id = ?`, now(), id)
}

// SetPersonIdentityKey writes the identity key exactly once. A second write
// is ErrIdentitySet — the recovery flow clears it (ResetPersonIdentity) first.
func (s *Store) SetPersonIdentityKey(id int64, key []byte) error {
	res, err := s.db.Exec(`UPDATE people SET identity_key = ? WHERE id = ? AND identity_key IS NULL`, key, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := s.PersonByID(id); err != nil {
			return err
		}
		return ErrIdentitySet
	}
	return nil
}

// ResetPersonIdentity clears the identity key and revokes every device, so
// the next device to log in starts a new identity. Every room the person is
// in will see the reset as unverified devices.
func (s *Store) ResetPersonIdentity(id int64) error {
	return s.withTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`UPDATE people SET identity_key = NULL WHERE id = ?`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE people_devices SET revoked_at = ? WHERE person_id = ? AND revoked_at IS NULL`, now(), id); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM people_key_packages WHERE device_id IN (SELECT id FROM people_devices WHERE person_id = ?)`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM people_sessions WHERE person_id = ? AND device_id IS NOT NULL`, id); err != nil {
			return err
		}
		return nil
	})
}

// ---- credentials ----

type PersonCredential struct {
	ID         int64  `json:"id"`
	PersonID   int64  `json:"-"`
	Kind       string `json:"kind"`
	Data       string `json:"-"`
	Label      string `json:"label"`
	CreatedAt  string `json:"createdAt"`
	LastUsedAt string `json:"lastUsedAt,omitempty"`
}

func (s *Store) AddPersonCredential(personID int64, kind, data, label string) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO people_credentials (person_id, kind, data, label, created_at) VALUES (?, ?, ?, ?, ?)`,
		personID, kind, data, label, now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) PersonCredentials(personID int64) ([]PersonCredential, error) {
	return queryAll(s.db, scanPersonCredential, `SELECT `+personCredentialCols+` FROM people_credentials WHERE person_id = ? ORDER BY id`, personID)
}

const personCredentialCols = `id, person_id, kind, data, label, created_at, last_used_at`

func scanPersonCredential(sc scanner) (PersonCredential, error) {
	var c PersonCredential
	err := sc.Scan(&c.ID, &c.PersonID, &c.Kind, &c.Data, &c.Label, &c.CreatedAt, &c.LastUsedAt)
	return c, err
}

func (s *Store) UpdatePersonCredential(id int64, data string) error {
	_, err := s.db.Exec(`UPDATE people_credentials SET data = ?, last_used_at = ? WHERE id = ?`, data, now(), id)
	return err
}

// SetPersonPassword replaces the person's password credential (or adds one).
func (s *Store) SetPersonPassword(personID int64, hash string) error {
	res, err := s.db.Exec(`UPDATE people_credentials SET data = ? WHERE person_id = ? AND kind = ?`, hash, personID, CredentialPassword)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	_, err = s.AddPersonCredential(personID, CredentialPassword, hash, "")
	return err
}

func (s *Store) DeletePersonCredential(personID, id int64) error {
	_, err := s.db.Exec(`DELETE FROM people_credentials WHERE id = ? AND person_id = ?`, id, personID)
	return err
}

// ---- invites ----

type PersonInvite struct {
	ID          int64  `json:"id"`
	HandleHint  string `json:"handleHint"`
	DisplayHint string `json:"displayHint"`
	Role        string `json:"role"`
	CreatedBy   string `json:"createdBy"`
	CreatedAt   string `json:"createdAt"`
	ExpiresAt   string `json:"expiresAt"`
	UsedAt      string `json:"usedAt,omitempty"`
	UsedBy      int64  `json:"usedBy,omitempty"`
}

func (s *Store) CreatePersonInvite(secretDigest, handleHint, displayHint, role, createdBy string, ttl time.Duration) (int64, error) {
	n := time.Now().UTC()
	res, err := s.db.Exec(`INSERT INTO people_invites (secret_digest, handle_hint, display_hint, role, created_by, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		secretDigest, handleHint, displayHint, role, createdBy, n.Format(time.RFC3339), n.Add(ttl).Format(time.RFC3339))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) ListPersonInvites() ([]PersonInvite, error) {
	return queryAll(s.db, func(sc scanner) (PersonInvite, error) {
		var i PersonInvite
		err := sc.Scan(&i.ID, &i.HandleHint, &i.DisplayHint, &i.Role, &i.CreatedBy, &i.CreatedAt, &i.ExpiresAt, &i.UsedAt, &i.UsedBy)
		return i, err
	}, `SELECT id, handle_hint, display_hint, role, created_by, created_at, expires_at, COALESCE(used_at, ''), COALESCE(used_by, 0) FROM people_invites ORDER BY id DESC`)
}

// PersonInviteByDigest returns a live (unused, unexpired) invite.
func (s *Store) PersonInviteByDigest(digest string) (*PersonInvite, error) {
	return ptr(queryOne(s.db, scanOpenPersonInvite, openPersonInviteQuery, digest, now()))
}

// openPersonInviteQuery finds an unused, unexpired invite by its secret's
// digest.
const openPersonInviteQuery = `SELECT id, handle_hint, display_hint, role, created_by, created_at, expires_at FROM people_invites WHERE secret_digest = ? AND used_at IS NULL AND expires_at > ?`

func scanOpenPersonInvite(sc scanner) (PersonInvite, error) {
	var i PersonInvite
	err := sc.Scan(&i.ID, &i.HandleHint, &i.DisplayHint, &i.Role, &i.CreatedBy, &i.CreatedAt, &i.ExpiresAt)
	return i, err
}

func (s *Store) DeletePersonInvite(id int64) error {
	_, err := s.db.Exec(`DELETE FROM people_invites WHERE id = ?`, id)
	return err
}

// ---- sessions ----

type PersonSession struct {
	ID         int64
	PersonID   int64
	DeviceID   int64
	ClientKind string
	ExpiresAt  time.Time
	// SID names the session to apps; AMR and AuthTime say how and when it
	// was proven (docs/signin-contract.md).
	SID      string
	AMR      []string
	AuthTime time.Time
	// RemoteAddr is the address it was signed in from: what a session on a
	// plain-http address is pinned to.
	RemoteAddr string
	// IssuedAs is how fog handed it out (0075): SessionHost, SessionPlain or
	// SessionDevice.
	IssuedAs string
	// BrowserDigest is the digest of the fog_browser cookie set beside a
	// cookie session (0076); "" for one never set as a cookie.
	BrowserDigest string
}

// How a session was handed out (0075).
const (
	SessionHost   = "host"
	SessionPlain  = "plain"
	SessionDevice = "device"
	// SessionChat and SessionChatHost are a browser's chat device's
	// credential: its own cookie, reaching chat alone; the browser's own
	// session is untouched by it. Which cookie fog set is recorded, as for
	// the session: SessionChatHost was set as __Host-fog_chat and only
	// that name ever carries it unpinned; SessionChat, a plain fog_chat,
	// is pinned however it is presented.
	SessionChat     = "chat"
	SessionChatHost = "chat-host"
)

// IsChatCredential says whether a session is a browser's chat credential.
func IsChatCredential(issuedAs string) bool {
	return issuedAs == SessionChat || issuedAs == SessionChatHost
}

// SetSessionIssuedAs records how a session just minted was handed out.
func (s *Store) SetSessionIssuedAs(id int64, kind string) error {
	_, err := s.db.Exec(`UPDATE people_sessions SET issued_as = ? WHERE id = ?`, kind, id)
	return err
}

// LiveSessionAddrs is every address a live session was signed in from:
// the household's addresses, which a flood of strangers must never crowd
// out of a limiter (internal/auth, StrangerLimiter).
func (s *Store) LiveSessionAddrs() ([]string, error) {
	return queryAll(s.db, func(sc scanner) (string, error) {
		var a string
		err := sc.Scan(&a)
		return a, err
	}, `SELECT DISTINCT remote_addr FROM people_sessions WHERE remote_addr != '' AND expires_at > ?`, time.Now().UTC().Format(time.RFC3339))
}

// SetSessionBrowser binds a session to the browser cookie set beside it.
func (s *Store) SetSessionBrowser(tokenDigest, browserDigest string) error {
	_, err := s.db.Exec(`UPDATE people_sessions SET browser_digest = ? WHERE token_digest = ?`, browserDigest, tokenDigest)
	return err
}

// CreatePersonSession stores a new session. amr lists how the person proved
// themselves (RFC 8176: "pwd", "pop", "user"); the session's sid is minted
// here.
func (s *Store) CreatePersonSession(personID, deviceID int64, tokenDigest, clientKind string, ttl time.Duration, userAgent, remoteAddr string, amr []string) (int64, error) {
	n := time.Now().UTC()
	var dev any
	if deviceID != 0 {
		dev = deviceID
	}
	amrJSON, err := json.Marshal(nonNil(amr))
	if err != nil {
		return 0, err
	}
	res, err := s.db.Exec(`INSERT INTO people_sessions (person_id, device_id, token_digest, client_kind, created_at, expires_at, last_seen_at, user_agent, remote_addr, sid, amr, auth_time)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		personID, dev, tokenDigest, clientKind, n.Format(time.RFC3339), n.Add(ttl).Format(time.RFC3339), n.Format(time.RFC3339), userAgent, remoteAddr,
		randx.Hex(16), string(amrJSON), n.Format(time.RFC3339Nano))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// SessionPerson resolves a session token to its session and person in one
// query: the lookup behind every signed-in request, forward-auth checks for
// each asset of a gated app included. The session's and its device's
// last_seen_at are rewritten only once stale, so a steady stream of requests
// reads without writing.
func (s *Store) SessionPerson(tokenDigest string) (*PersonSession, *Person, error) {
	sess, p := &PersonSession{}, &Person{}
	var expires, lastSeen, deviceSeen, amr, authTime string
	err := s.db.QueryRow(`SELECT ps.id, ps.person_id, COALESCE(ps.device_id, 0), ps.client_kind, ps.expires_at, ps.last_seen_at,
			COALESCE(ps.sid, ''), ps.amr, ps.auth_time, ps.remote_addr, ps.issued_as, ps.browser_digest,
			COALESCE(d.last_seen_at, ''),
			p.id, p.handle, p.display_name, p.role, p.status, p.identity_key, COALESCE(p.uid, ''), p.email, p.created_at, p.last_seen_at
		FROM people_sessions ps
		JOIN people p ON p.id = ps.person_id
		LEFT JOIN people_devices d ON d.id = ps.device_id
		WHERE ps.token_digest = ?`, tokenDigest).
		Scan(&sess.ID, &sess.PersonID, &sess.DeviceID, &sess.ClientKind, &expires, &lastSeen,
			&sess.SID, &amr, &authTime, &sess.RemoteAddr, &sess.IssuedAs, &sess.BrowserDigest,
			&deviceSeen,
			&p.ID, &p.Handle, &p.DisplayName, &p.Role, &p.Status, &p.IdentityKey, &p.UID, &p.Email, &p.CreatedAt, &p.LastSeenAt)
	if err == sql.ErrNoRows {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	sess.AMR, sess.AuthTime = decodeAMR(amr), parseTime(authTime)
	if err := s.touchSession(sess, expires, lastSeen); err != nil {
		return nil, nil, err
	}
	if sess.DeviceID != 0 && p.Status == PersonStatusActive && stale(deviceSeen, deviceSeenThrottle) {
		s.TouchDeviceSeen(sess.DeviceID)
	}
	return sess, p, nil
}

// touchSession finishes a session lookup: an expired session is deleted and
// reads as not found, and a live one has last_seen_at moved once stale.
func (s *Store) touchSession(sess *PersonSession, expires, lastSeen string) error {
	var err error
	sess.ExpiresAt, err = time.Parse(time.RFC3339, expires)
	if err != nil {
		return err
	}
	if time.Now().After(sess.ExpiresAt) {
		s.db.Exec(`DELETE FROM people_sessions WHERE id = ?`, sess.ID)
		return ErrNotFound
	}
	if stale(lastSeen, lastSeenThrottle) {
		s.db.Exec(`UPDATE people_sessions SET last_seen_at = ? WHERE id = ?`, now(), sess.ID)
	}
	return nil
}

// stale reports whether an RFC 3339 last-seen stamp is older than d, or
// missing or unreadable.
func stale(stamp string, d time.Duration) bool {
	at, err := time.Parse(time.RFC3339, stamp)
	return err != nil || time.Since(at) >= d
}

func (s *Store) DeletePersonSessionByDigest(tokenDigest string) error {
	_, err := s.db.Exec(`DELETE FROM people_sessions WHERE token_digest = ?`, tokenDigest)
	return err
}

// PersonSessionView is a session as the person (or an admin) sees it.
type PersonSessionView struct {
	ID         int64  `json:"id"`
	DeviceID   int64  `json:"deviceId,omitempty"`
	DeviceName string `json:"deviceName,omitempty"`
	ClientKind string `json:"clientKind"`
	CreatedAt  string `json:"createdAt"`
	ExpiresAt  string `json:"expiresAt"`
	LastSeenAt string `json:"lastSeenAt"`
	UserAgent  string `json:"userAgent,omitempty"`
	RemoteAddr string `json:"remoteAddr,omitempty"`
}

// ListPersonSessions lists a person's live sessions, newest activity first.
func (s *Store) ListPersonSessions(personID int64) ([]PersonSessionView, error) {
	return queryAll(s.db, func(sc scanner) (PersonSessionView, error) {
		var v PersonSessionView
		err := sc.Scan(&v.ID, &v.DeviceID, &v.DeviceName, &v.ClientKind, &v.CreatedAt, &v.ExpiresAt, &v.LastSeenAt, &v.UserAgent, &v.RemoteAddr)
		return v, err
	}, `SELECT s.id, COALESCE(s.device_id, 0), COALESCE(d.name, ''), s.client_kind, s.created_at, s.expires_at, s.last_seen_at, s.user_agent, s.remote_addr
		FROM people_sessions s LEFT JOIN people_devices d ON d.id = s.device_id
		WHERE s.person_id = ? AND s.expires_at > ? ORDER BY s.last_seen_at DESC`, personID, now())
}

// DeletePersonSession revokes one session, scoped to its owner.
func (s *Store) DeletePersonSession(id, personID int64) error {
	res, err := s.db.Exec(`DELETE FROM people_sessions WHERE id = ? AND person_id = ?`, id, personID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeletePersonSessions(personID int64) error {
	_, err := s.db.Exec(`DELETE FROM people_sessions WHERE person_id = ?`, personID)
	return err
}

// PrunePersonSessions ends a person's least recently used browser sessions
// beyond keep, so a loop of sign-ins can't grow their session table without
// end. Never a native device's token (each device holds one, and devices
// are capped), never except — the session just made — and never one seen
// within idle: a session in use is never the one ended. The logout
// triggers (0073) tell the apps those sessions signed in to.
func (s *Store) PrunePersonSessions(personID int64, keep int, except int64, idle time.Duration) (int64, error) {
	cutoff := time.Now().UTC().Add(-idle).Format(time.RFC3339)
	res, err := s.db.Exec(`DELETE FROM people_sessions WHERE person_id = ? AND issued_as NOT IN ('device', 'chat', 'chat-host') AND id != ? AND last_seen_at < ? AND id NOT IN
		(SELECT id FROM people_sessions WHERE person_id = ? AND issued_as NOT IN ('device', 'chat', 'chat-host') ORDER BY last_seen_at DESC, id DESC LIMIT ?)`,
		personID, except, cutoff, personID, keep)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// BindSessionDevice ties a browser's own session to the chat device it
// just registered or proved: nothing new is minted.
func (s *Store) BindSessionDevice(sessionID, deviceID int64) error {
	_, err := s.db.Exec(`UPDATE people_sessions SET device_id = ? WHERE id = ?`, deviceID, sessionID)
	return err
}

// DeletePersonSessionsOfDevice ends a device's sessions but except: a
// device holds one token at a time.
func (s *Store) DeletePersonSessionsOfDevice(deviceID, except int64) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM people_sessions WHERE device_id = ? AND id != ?`, deviceID, except)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeletePersonSessionsExcept ends every session a person holds but keep:
// a new password ends the sessions the old one may have opened, short of
// the one that changed it. It reports how many ended.
func (s *Store) DeletePersonSessionsExcept(personID, keep int64) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM people_sessions WHERE person_id = ? AND id != ?`, personID, keep)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// AccountActivity is what happened to a person's own sign-in — the audit
// entries about them whose action is one of actions — newest first: what
// Settings › Your sign-in shows them.
func (s *Store) AccountActivity(handle string, actions []string, limit int) ([]AuditEntry, error) {
	if len(actions) == 0 {
		return []AuditEntry{}, nil
	}
	args := []any{handle}
	marks := make([]string, len(actions))
	for i, a := range actions {
		marks[i] = "?"
		args = append(args, a)
	}
	args = append(args, limit)
	return s.scanAudit(`SELECT id, at, actor, action, subject, detail FROM audit_log
		WHERE subject = ? AND action IN (`+strings.Join(marks, ", ")+`) ORDER BY id DESC LIMIT ?`, args...)
}

// ---- devices ----

type PersonDevice struct {
	ID          int64  `json:"id"`
	PersonID    int64  `json:"personId"`
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	DeviceKey   []byte `json:"deviceKey"`
	Attestation []byte `json:"attestation,omitempty"`
	CreatedAt   string `json:"createdAt"`
	LastSeenAt  string `json:"lastSeenAt,omitempty"`
	RevokedAt   string `json:"revokedAt,omitempty"`
}

// Verified is display shorthand: an attested device is one the person
// vouched for from another device they hold.
func (d PersonDevice) Verified() bool { return len(d.Attestation) > 0 }

const deviceCols = `id, person_id, name, kind, device_key, attestation, created_at, last_seen_at, COALESCE(revoked_at, '')`

func scanDevice(sc scanner) (PersonDevice, error) {
	var d PersonDevice
	err := sc.Scan(&d.ID, &d.PersonID, &d.Name, &d.Kind, &d.DeviceKey, &d.Attestation, &d.CreatedAt, &d.LastSeenAt, &d.RevokedAt)
	return d, err
}

func (s *Store) CreatePersonDevice(personID int64, name, kind string, deviceKey, attestation []byte) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO people_devices (person_id, name, kind, device_key, attestation, created_at, last_seen_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		personID, name, kind, deviceKey, attestation, now(), now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) PersonDeviceByID(id int64) (*PersonDevice, error) {
	return ptr(queryOne(s.db, scanDevice, `SELECT `+deviceCols+` FROM people_devices WHERE id = ?`, id))
}

func (s *Store) PersonDeviceByKey(key []byte) (*PersonDevice, error) {
	return ptr(queryOne(s.db, scanDevice, `SELECT `+deviceCols+` FROM people_devices WHERE device_key = ?`, key))
}

// PersonDevices lists a person's live (unrevoked) devices.
func (s *Store) PersonDevices(personID int64) ([]PersonDevice, error) {
	return queryAll(s.db, scanDevice, `SELECT `+deviceCols+` FROM people_devices WHERE person_id = ? AND revoked_at IS NULL ORDER BY id`, personID)
}

// AllPersonDevices is PersonDevices for everyone at once, keyed by person
// id, each person's devices in id order. People with no live devices have
// no entry.
func (s *Store) AllPersonDevices() (map[int64][]PersonDevice, error) {
	out := map[int64][]PersonDevice{}
	err := queryEach(s.db, func(sc scanner) error {
		d, err := scanDevice(sc)
		if err != nil {
			return err
		}
		out[d.PersonID] = append(out[d.PersonID], d)
		return nil
	}, `SELECT `+deviceCols+` FROM people_devices WHERE revoked_at IS NULL ORDER BY person_id, id`)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) SetDeviceAttestation(id int64, attestation []byte) error {
	_, err := s.db.Exec(`UPDATE people_devices SET attestation = ? WHERE id = ?`, attestation, id)
	return err
}

func (s *Store) RenameDevice(id int64, name string) error {
	_, err := s.db.Exec(`UPDATE people_devices SET name = ? WHERE id = ?`, name, id)
	return err
}

func (s *Store) TouchDeviceSeen(id int64) {
	s.db.Exec(`UPDATE people_devices SET last_seen_at = ? WHERE id = ?`, now(), id)
}

// RevokeDevice retires a device: its sessions end and its unconsumed key
// packages vanish, so nobody can add it to a room again.
func (s *Store) RevokeDevice(id int64) error {
	return s.withTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`UPDATE people_devices SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`, now(), id); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM people_sessions WHERE device_id = ?`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM people_key_packages WHERE device_id = ?`, id); err != nil {
			return err
		}
		return nil
	})
}

// ---- key packages ----

func (s *Store) AddKeyPackages(deviceID int64, ciphersuite int, packages [][]byte) error {
	return s.withTx(func(tx *sql.Tx) error {
		for _, kp := range packages {
			if _, err := tx.Exec(`INSERT INTO people_key_packages (device_id, ciphersuite, key_package, published_at) VALUES (?, ?, ?, ?)`,
				deviceID, ciphersuite, kp, now()); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) CountKeyPackages(deviceID int64) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM people_key_packages WHERE device_id = ? AND consumed_at IS NULL`, deviceID).Scan(&n)
	return n, err
}

// UnusedKeyPackageCounts counts every device's unconsumed KeyPackages,
// keyed by device id; a device with none is absent.
func (s *Store) UnusedKeyPackageCounts() (map[int64]int, error) {
	out := map[int64]int{}
	err := queryEach(s.db, func(sc scanner) error {
		var id int64
		var n int
		if err := sc.Scan(&id, &n); err != nil {
			return err
		}
		out[id] = n
		return nil
	}, `SELECT device_id, COUNT(*) FROM people_key_packages WHERE consumed_at IS NULL GROUP BY device_id`)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ConsumeKeyPackage hands out one unconsumed KeyPackage for a device and
// stamps it consumed. ErrNotFound when the device has none left.
func (s *Store) ConsumeKeyPackage(deviceID int64, ciphersuite int) ([]byte, error) {
	type pkg struct {
		id int64
		kp []byte
	}
	var p pkg
	err := s.withTx(func(tx *sql.Tx) error {
		var err error
		p, err = queryOne(tx, func(sc scanner) (pkg, error) {
			var p pkg
			err := sc.Scan(&p.id, &p.kp)
			return p, err
		}, `SELECT id, key_package FROM people_key_packages WHERE device_id = ? AND ciphersuite = ? AND consumed_at IS NULL ORDER BY id LIMIT 1`, deviceID, ciphersuite)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE people_key_packages SET consumed_at = ? WHERE id = ?`, now(), p.id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return p.kp, nil
}

// PruneKeyPackages removes consumed packages older than the cutoff.
func (s *Store) PruneKeyPackages(cutoff string) (int64, error) {
	return s.pruneBatched("people_key_packages", "consumed_at IS NOT NULL AND consumed_at < ?", cutoff)
}

// ---- backups ----

type PersonBackup struct {
	Blob      []byte
	Version   int64
	UpdatedAt string
}

func (s *Store) PutPersonBackup(personID int64, blob []byte, version int64) error {
	_, err := s.db.Exec(`INSERT INTO people_backups (person_id, blob, version, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(person_id) DO UPDATE SET blob = excluded.blob, version = excluded.version, updated_at = excluded.updated_at`,
		personID, blob, version, now())
	return err
}

func (s *Store) PersonBackup(personID int64) (*PersonBackup, error) {
	return ptr(queryOne(s.db, func(sc scanner) (PersonBackup, error) {
		var b PersonBackup
		err := sc.Scan(&b.Blob, &b.Version, &b.UpdatedAt)
		return b, err
	}, `SELECT blob, version, updated_at FROM people_backups WHERE person_id = ?`, personID))
}

func (s *Store) DeletePersonBackup(personID int64) error {
	_, err := s.db.Exec(`DELETE FROM people_backups WHERE person_id = ?`, personID)
	return err
}
