// Household sign-in (docs/signin-contract.md): person ULIDs, signing keys,
// who may sign in to which app, and the gate's per-host sessions. Thin
// queries only; the rules live in internal/identity and internal/signing.
package store

import (
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"gitea.lan/matthew/fog/internal/randx"
)

func nonNil(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

func decodeAMR(s string) []string {
	var v []string
	if json.Unmarshal([]byte(s), &v) != nil {
		return nil
	}
	return v
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// backfillPersonUIDs gives every person from before 0050 their ULID. Runs
// on every open; after the first it finds nothing to do.
func (s *Store) backfillPersonUIDs() error {
	ids, err := queryAll(s.db, scanValue[int64], `SELECT id FROM people WHERE uid IS NULL`)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := s.db.Exec(`UPDATE people SET uid = ? WHERE id = ? AND uid IS NULL`, randx.ULID(), id); err != nil {
			return err
		}
	}
	return nil
}

// PersonByUID finds a person by the stable half of their address.
func (s *Store) PersonByUID(uid string) (*Person, error) {
	return ptr(queryOne(s.db, scanPerson, `SELECT `+personCols+` FROM people WHERE uid = ?`, uid))
}

// ---- signing keys ----

// SigningKey is one key's public half and schedule. The private half is a
// vault secret the caller owns (internal/signing).
type SigningKey struct {
	KID         string
	Alg         string
	PublicJWK   string
	PublishAt   time.Time
	SignFrom    time.Time
	SignUntil   time.Time
	UnpublishAt time.Time
}

func scanSigningKey(sc scanner) (SigningKey, error) {
	var k SigningKey
	var pub, from, until, unpub string
	err := sc.Scan(&k.KID, &k.Alg, &k.PublicJWK, &pub, &from, &until, &unpub)
	k.PublishAt, k.SignFrom, k.SignUntil, k.UnpublishAt = parseTime(pub), parseTime(from), parseTime(until), parseTime(unpub)
	return k, err
}

func (s *Store) ListSigningKeys() ([]SigningKey, error) {
	return queryAll(s.db, scanSigningKey, `SELECT kid, alg, public_jwk, publish_at, sign_from, sign_until, unpublish_at FROM signing_keys ORDER BY sign_from`)
}

func (s *Store) InsertSigningKey(k SigningKey) error {
	_, err := s.db.Exec(`INSERT INTO signing_keys (kid, alg, public_jwk, publish_at, sign_from, sign_until, unpublish_at, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		k.KID, k.Alg, k.PublicJWK, stamp(k.PublishAt), stamp(k.SignFrom), stamp(k.SignUntil), stamp(k.UnpublishAt), now())
	return err
}

// UpdateSigningKeySchedule rewrites a key's schedule: how an emergency
// rotation retires a key at once.
func (s *Store) UpdateSigningKeySchedule(k SigningKey) error {
	_, err := s.db.Exec(`UPDATE signing_keys SET publish_at = ?, sign_from = ?, sign_until = ?, unpublish_at = ? WHERE kid = ?`,
		stamp(k.PublishAt), stamp(k.SignFrom), stamp(k.SignUntil), stamp(k.UnpublishAt), k.KID)
	return err
}

func (s *Store) DeleteSigningKey(kid string) error {
	_, err := s.db.Exec(`DELETE FROM signing_keys WHERE kid = ?`, kid)
	return err
}

// ---- sign-in policy ----

// SigninPolicy is who may sign in to one app (internal/identity reads it).
type SigninPolicy struct {
	AppID    int64
	Audience string
	// People holds the chosen people (audience "people") and per-person
	// role overrides under any audience, keyed by person id; "" = no
	// override.
	People map[int64]string
}

const (
	AudienceOperator  = "operator"
	AudienceHousehold = "household"
	AudiencePeople    = "people"
)

// SigninPolicyFor returns an app's policy, or the default — operator only —
// when none was ever set.
func (s *Store) SigninPolicyFor(appID int64) (*SigninPolicy, error) {
	p := &SigninPolicy{AppID: appID, Audience: AudienceOperator, People: map[int64]string{}}
	aud, err := queryOne(s.db, scanValue[string], `SELECT audience FROM signin_policies WHERE app_id = ?`, appID)
	switch {
	case err == nil:
		p.Audience = aud
	case err != ErrNotFound:
		return nil, err
	}
	type row struct {
		id   int64
		role string
	}
	rows, err := queryAll(s.db, func(sc scanner) (row, error) {
		var r row
		return r, sc.Scan(&r.id, &r.role)
	}, `SELECT person_id, role FROM signin_policy_people WHERE app_id = ?`, appID)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		p.People[r.id] = r.role
	}
	return p, nil
}

// PutSigninPolicy replaces an app's policy whole.
func (s *Store) PutSigninPolicy(p SigninPolicy) error {
	return s.withTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO signin_policies (app_id, audience, updated_at) VALUES (?, ?, ?)
			ON CONFLICT(app_id) DO UPDATE SET audience = excluded.audience, updated_at = excluded.updated_at`,
			p.AppID, p.Audience, now()); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM signin_policy_people WHERE app_id = ?`, p.AppID); err != nil {
			return err
		}
		for id, role := range p.People {
			if _, err := tx.Exec(`INSERT INTO signin_policy_people (app_id, person_id, role) VALUES (?, ?, ?)`, p.AppID, id, role); err != nil {
				return err
			}
		}
		return nil
	})
}

// ---- the gate ----

// GateCode is a pending handshake: the host it was minted for, the fog
// session behind it, and where to send the browser afterwards.
type GateCode struct {
	Host      string
	SessionID int64
	Next      string
	// Binding is the digest of the value the gated host put in the browser
	// that began the handshake (0073): the callback must present it.
	Binding string
}

func (s *Store) CreateGateCode(digest, host string, sessionID int64, next, binding string, ttl time.Duration) error {
	_, err := s.db.Exec(`INSERT INTO gate_codes (digest, host, session_id, next, binding, expires_at) VALUES (?, ?, ?, ?, ?, ?)`,
		digest, host, sessionID, next, binding, stamp(time.Now().Add(ttl)))
	return err
}

// RedeemGateCode spends a code for host exactly once. Unknown, expired,
// already used, or minted for another host all read as ErrNotFound — the
// database decides single use, so two racing redemptions cannot both win.
func (s *Store) RedeemGateCode(digest, host string) (*GateCode, error) {
	var gc GateCode
	err := s.withTx(func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE gate_codes SET used_at = ? WHERE digest = ? AND host = ? AND used_at IS NULL AND expires_at > ?`,
			now(), digest, host, now())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return tx.QueryRow(`SELECT host, session_id, next, binding FROM gate_codes WHERE digest = ?`, digest).Scan(&gc.Host, &gc.SessionID, &gc.Next, &gc.Binding)
	})
	if err != nil {
		return nil, err
	}
	return &gc, nil
}

// CreateGateSession binds a gate cookie for host to a fog session. It ends
// when the fog session does, by cascade, and never outlives it.
func (s *Store) CreateGateSession(digest, host string, sessionID int64) error {
	_, err := s.db.Exec(`INSERT INTO gate_sessions (digest, host, session_id, created_at, expires_at)
		SELECT ?, ?, id, ?, expires_at FROM people_sessions WHERE id = ?`,
		digest, host, now(), sessionID)
	return err
}

// GateSessionPerson resolves a gate cookie on host to the fog session and
// person behind it. A cookie presented on a host it was not minted for is
// not found.
func (s *Store) GateSessionPerson(digest, host string) (*PersonSession, *Person, error) {
	tokenDigest, err := queryOne(s.db, scanValue[string], `SELECT ps.token_digest FROM gate_sessions g
		JOIN people_sessions ps ON ps.id = g.session_id
		WHERE g.digest = ? AND g.host = ? AND g.expires_at > ?`, digest, host, now())
	if err != nil {
		return nil, nil, err
	}
	return s.SessionPerson(tokenDigest)
}

// SessionByID loads a live session and its person by id.
func (s *Store) SessionByID(id int64) (*PersonSession, *Person, error) {
	tokenDigest, err := queryOne(s.db, scanValue[string], `SELECT token_digest FROM people_sessions WHERE id = ?`, id)
	if err != nil {
		return nil, nil, err
	}
	return s.SessionPerson(tokenDigest)
}

// PruneGate drops spent and expired handshakes and expired gate sessions.
func (s *Store) PruneGate() (int64, error) {
	return s.pruneExpired(
		expiry{"gate_codes", "expires_at <= ? OR used_at IS NOT NULL"},
		expiry{"gate_sessions", "expires_at <= ?"},
	)
}

// ---- person pages ----

// PersonPage is where a person lives in one app.
type PersonPage struct {
	App   string `json:"app"`
	URL   string `json:"url"`
	Label string `json:"label"`
}

func (s *Store) PutPersonPage(appID int64, address, url, label string) error {
	_, err := s.db.Exec(`INSERT INTO app_person_pages (app_id, address, url, label, updated_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(app_id, address) DO UPDATE SET url = excluded.url, label = excluded.label, updated_at = excluded.updated_at`,
		appID, address, url, label, now())
	return err
}

func (s *Store) DeletePersonPage(appID int64, address string) error {
	_, err := s.db.Exec(`DELETE FROM app_person_pages WHERE app_id = ? AND address = ?`, appID, address)
	return err
}

// PersonPages lists every app's page for an address.
func (s *Store) PersonPages(address string) ([]PersonPage, error) {
	return queryAll(s.db, func(sc scanner) (PersonPage, error) {
		var p PersonPage
		return p, sc.Scan(&p.App, &p.URL, &p.Label)
	}, `SELECT a.name, pp.url, pp.label FROM app_person_pages pp JOIN apps a ON a.id = pp.app_id WHERE pp.address = ? ORDER BY a.name`, address)
}

// AllPersonPages is PersonPages for every address at once, keyed by
// address — one query for a directory listing instead of one per person.
func (s *Store) AllPersonPages() (map[string][]PersonPage, error) {
	rows, err := s.db.Query(`SELECT pp.address, a.name, pp.url, pp.label FROM app_person_pages pp JOIN apps a ON a.id = pp.app_id ORDER BY pp.address, a.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]PersonPage{}
	for rows.Next() {
		var addr string
		var p PersonPage
		if err := rows.Scan(&addr, &p.App, &p.URL, &p.Label); err != nil {
			return nil, err
		}
		out[addr] = append(out[addr], p)
	}
	return out, rows.Err()
}

// SharedChatRooms lists the rooms both addresses are current participants
// of — "your conversations with Dave" (docs/signin-contract.md). Room
// ids and kinds only: membership is what fog already holds; what was said
// is sealed and stays on people's devices.
func (s *Store) SharedChatRooms(me, them string) ([]string, error) {
	return queryAll(s.db, scanValue[string], `SELECT a.room_id FROM chat_participants a
		JOIN chat_participants b ON b.room_id = a.room_id
		JOIN chat_rooms r ON r.id = a.room_id
		WHERE a.person_addr = ? AND a.left_seq IS NULL AND b.person_addr = ? AND b.left_seq IS NULL AND r.archived_at IS NULL
		ORDER BY a.room_id`, me, them)
}

// PersonByRef finds a person by the second half of an address: a person
// ULID (the form addresses are moving to, docs/signin-contract.md) or a
// handle (the form chat has used). The two cannot collide — handles are
// lowercase, ULIDs uppercase Crockford base-32 — so either resolves.
func (s *Store) PersonByRef(ref string) (*Person, error) {
	if isULID(ref) {
		return s.PersonByUID(ref)
	}
	return s.PersonByHandle(ref)
}

func isULID(s string) bool {
	if len(s) != 26 {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789ABCDEFGHJKMNPQRSTVWXYZ", c) {
			return false
		}
	}
	return true
}
