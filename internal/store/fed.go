// Federation state: this instance's identity, its peers, what each peer is
// allowed to do here and what this instance does toward it, and the
// single-use invites that pair new peers (docs/federation-contract.md).
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// ErrAlreadyExists reports a uniqueness collision — a peer name or instance
// ID that is already taken. Callers branch on it with errors.Is; the wrapped
// message stays human-readable.
var ErrAlreadyExists = errors.New("already exists")

// isUniqueViolation matches SQLite's UNIQUE-constraint failure by driver
// error code rather than message text.
func isUniqueViolation(err error) bool {
	var se *sqlite.Error
	return errors.As(err, &se) && se.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE
}

// Capability names and directions. A 'grant' row says what the peer may do
// against this instance; a 'use' row says what this instance actively does
// toward the peer. Enforcement always reads grant rows on the receiving side.
const (
	CapFlareIngest   = "flare.ingest"
	CapAppsDirectory = "apps.directory"
	CapAppsIntroduce = "apps.introduce"
	// CapFeedbackIngest: a grant lets the peer send its household's
	// feedback reports here; a use row sends ours there, config
	// {"mode":"note"|"full"} — the words alone, or with their dossier.
	CapFeedbackIngest = "feedback.ingest"
	// CapPeopleDirectory: a grant lets the peer see this household's people
	// — address, handle, display name, and (with chat) identity keys — never
	// email, sessions, roles in apps or presence (docs/signin-contract.md).
	// Config {"people": "members" | ["handle", …]}. Granted by
	// default at pairing.
	CapPeopleDirectory = "people.directory"

	DirGrant = "grant"
	DirUse   = "use"
)

type FedIdentity struct {
	InstanceID   string `json:"instanceId"`
	PublicKey    string `json:"publicKey"`
	EncPublicKey string `json:"encPublicKey"`
	CreatedAt    string `json:"createdAt"`
}

type FedPeer struct {
	ID         int64  `json:"id"`
	InstanceID string `json:"instanceId"`
	Name       string `json:"name"`
	// Label is a local display name and nothing else. Empty means the UI
	// shows Name, which is the slug of the name the peer advertised when
	// this instance paired with it.
	Label     string `json:"label,omitempty"`
	URL       string `json:"url,omitempty"`
	PublicKey string `json:"publicKey"`
	// EncPublicKey is the peer's encryption key, bound to PublicKey by its
	// signature: every body sent to the peer is sealed to it. Empty until
	// learned — at pairing, or from a signed exchange — and never changed
	// once set.
	EncPublicKey string `json:"encPublicKey,omitempty"`
	FogVersion   string `json:"fogVersion,omitempty"`
	// RemoteGrants and Directory are JSON blobs from the peer's last
	// heartbeat: what they advertise granting us, and their exposed apps.
	// Display data only — never an input to enforcement.
	RemoteGrants json.RawMessage `json:"remoteGrants" ts:"string[]"`
	Directory    json.RawMessage `json:"directory" ts:"FedDirectoryApp[]"`
	// People is the peer's advertised household directory (chat): handles,
	// identity keys, devices. Display and key-pinning data; membership is
	// still decided by the hub's own checks.
	People     json.RawMessage `json:"people"`
	CreatedAt  string          `json:"createdAt"`
	LastSeenAt string          `json:"lastSeenAt,omitempty"`
	// UnpairedAt is set once the peer has refused enough consecutive signed
	// heartbeats with "unknown peer" to mean it: the other operator ended
	// the pairing. It is a display fact about them, not a change to what
	// they may do here — only removing the row does that.
	UnpairedAt string `json:"unpairedAt,omitempty"`
	// RejectStreak is how many of those refusals have arrived in a row.
	// Any exchange that works, in either direction, resets it to zero.
	RejectStreak int `json:"rejectStreak,omitempty"`
}

type FedCapability struct {
	PeerID     int64           `json:"peerId"`
	Capability string          `json:"capability"`
	Direction  string          `json:"direction" ts:"\"grant\" | \"use\""`
	Config     json.RawMessage `json:"config" ts:"Record<string, unknown>"`
}

type FedInvite struct {
	ID        int64  `json:"id"`
	Grants    string `json:"grants"`
	CreatedAt string `json:"createdAt"`
	ExpiresAt string `json:"expiresAt"`
	ClaimedBy string `json:"claimedBy,omitempty"`
}

// ---- identity ----

func (s *Store) FedIdentity() (*FedIdentity, error) {
	return ptr(queryOne(s.db, func(sc scanner) (FedIdentity, error) {
		var id FedIdentity
		err := sc.Scan(&id.InstanceID, &id.PublicKey, &id.EncPublicKey, &id.CreatedAt)
		return id, err
	}, `SELECT instance_id, public_key, enc_public_key, created_at FROM fed_identity WHERE id = 1`))
}

// SetFedIdentityEncKey records this instance's encryption public key.
func (s *Store) SetFedIdentityEncKey(pub string) error {
	_, err := s.db.Exec(`UPDATE fed_identity SET enc_public_key = ? WHERE id = 1`, pub)
	return err
}

func (s *Store) SetFedIdentity(instanceID, publicKey string) error {
	_, err := s.db.Exec(`INSERT INTO fed_identity (id, instance_id, public_key, created_at)
		VALUES (1, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET instance_id = excluded.instance_id, public_key = excluded.public_key`,
		instanceID, publicKey, now())
	return err
}

// RenewFedIdentity replaces this instance's identity row with a renewed
// one — new instance ID, identity key and encryption key — in one
// transaction, and carries the rows that name this instance as an author
// (its own contributions in peers' shares) over to the new ID.
func (s *Store) RenewFedIdentity(oldID, newID, publicKey, encPublicKey string) error {
	return s.withTx(func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE fed_identity SET instance_id = ?, public_key = ?, enc_public_key = ? WHERE id = 1 AND instance_id = ?`,
			newID, publicKey, encPublicKey, oldID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrNotFound
		}
		_, err = tx.Exec(`UPDATE file_fed_replicas SET origin = ? WHERE origin = ?`, newID, oldID)
		return err
	})
}

// ---- peers ----

// CreatePairedPeer stores a peer established by the pairing handshake — the
// only way a peer comes to exist. URL may be empty for a dial-in-only peer.
func (s *Store) CreatePairedPeer(instanceID, name, url, publicKey string) (*FedPeer, error) {
	res, err := s.db.Exec(`INSERT INTO fed_peers (instance_id, name, url, public_key, created_at)
		VALUES (?, ?, ?, ?, ?)`,
		instanceID, name, nullable(url), publicKey, now())
	if err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("a peer named %q or with this instance ID %w", name, ErrAlreadyExists)
		}
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.FedPeerByID(id)
}

// DisplayName is what a human should be shown for this peer. Everything
// machine-facing — audit records, capability lookups, the app-facing API —
// keeps using Name.
func (p FedPeer) DisplayName() string {
	if p.Label != "" {
		return p.Label
	}
	return p.Name
}

const fedPeerCols = `id, instance_id, name, label, url, public_key, enc_public_key, fog_version,
	remote_grants, directory, people, created_at, last_seen_at, unpaired_at, reject_streak`

func scanFedPeer(sc scanner) (FedPeer, error) {
	var p FedPeer
	var url, lastSeen, unpaired sql.NullString
	var grants, dir, people string
	err := sc.Scan(&p.ID, &p.InstanceID, &p.Name, &p.Label, &url, &p.PublicKey, &p.EncPublicKey, &p.FogVersion,
		&grants, &dir, &people, &p.CreatedAt, &lastSeen, &unpaired, &p.RejectStreak)
	if err != nil {
		return p, err
	}
	p.URL, p.LastSeenAt, p.UnpairedAt = url.String, lastSeen.String, unpaired.String
	p.RemoteGrants, p.Directory = json.RawMessage(grants), json.RawMessage(dir)
	if people == "" {
		people = "[]"
	}
	p.People = json.RawMessage(people)
	return p, nil
}

func (s *Store) FedPeerByID(id int64) (*FedPeer, error) {
	return ptr(queryOne(s.db, scanFedPeer, `SELECT `+fedPeerCols+` FROM fed_peers WHERE id = ?`, id))
}

func (s *Store) FedPeerByInstanceID(instanceID string) (*FedPeer, error) {
	return ptr(queryOne(s.db, scanFedPeer, `SELECT `+fedPeerCols+` FROM fed_peers WHERE instance_id = ?`, instanceID))
}

// FedPeersByInstanceIDs returns the paired peers among instance ids, in no
// particular order; ids with no peer are skipped.
func (s *Store) FedPeersByInstanceIDs(ids []string) ([]FedPeer, error) {
	var out []FedPeer
	for chunk := range slices.Chunk(ids, inChunk) {
		in, args := inList(chunk)
		peers, err := queryAll(s.db, scanFedPeer, `SELECT `+fedPeerCols+` FROM fed_peers WHERE instance_id IN (`+in+`)`, args...)
		if err != nil {
			return nil, err
		}
		out = append(out, peers...)
	}
	return out, nil
}

func (s *Store) ListFedPeers() ([]FedPeer, error) {
	return queryAll(s.db, scanFedPeer, `SELECT `+fedPeerCols+` FROM fed_peers ORDER BY name`)
}

// SetFedPeerLabel sets or (with an empty label) clears a peer's local
// display name. Unlike the name it shadows, a label is free-form and needs
// no uniqueness: it addresses nothing.
func (s *Store) SetFedPeerLabel(id int64, label string) error {
	_, err := s.db.Exec(`UPDATE fed_peers SET label = ? WHERE id = ?`, label, id)
	return err
}

// ErrPeerKeyMismatch: a pairing named an instance already held here, under
// a different key. A peer's key never changes after pairing — fog has no
// rotation — so this is someone else claiming that household's name.
var ErrPeerKeyMismatch = errors.New("that instance is already paired here under a different key")

// RefreshPairedPeer is what re-pairing an instance already held does: the
// row, its grants and its history stay, and a non-empty url becomes its dial
// address. It applies only while publicKey is the key the row was paired
// under, and is ErrPeerKeyMismatch otherwise — the key itself is never
// written here (and migration 0070 refuses any statement that would).
func (s *Store) RefreshPairedPeer(id int64, publicKey, url string) error {
	res, err := s.db.Exec(`UPDATE fed_peers SET url = COALESCE(?, url) WHERE id = ? AND public_key = ?`,
		nullable(url), id, publicKey)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrPeerKeyMismatch
	}
	return nil
}

// ErrPeerEncKeyMismatch: an encryption key arrived for a peer that already
// holds a different one. Like its identity key, a peer's encryption key never
// changes once known.
var ErrPeerEncKeyMismatch = errors.New("that peer already has a different encryption key")

// SetFedPeerEncKey records a peer's encryption key the first time it is
// learned. The same key again is a no-op; a different one is
// ErrPeerEncKeyMismatch and changes nothing (migration 0072 refuses it too).
func (s *Store) SetFedPeerEncKey(id int64, key string) error {
	res, err := s.db.Exec(`UPDATE fed_peers SET enc_public_key = ? WHERE id = ? AND enc_public_key = ''`, key, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return nil
	}
	var have string
	if err := s.db.QueryRow(`SELECT enc_public_key FROM fed_peers WHERE id = ?`, id).Scan(&have); err != nil {
		return err
	}
	if have != key {
		return ErrPeerEncKeyMismatch
	}
	return nil
}

// MarkFedPeerPaired records what a completed handshake proved: the peer is
// live, knows us, and runs fogVersion. Nothing else it holds about the peer
// moves until a verified heartbeat says so.
func (s *Store) MarkFedPeerPaired(id int64, fogVersion string) error {
	_, err := s.db.Exec(`UPDATE fed_peers SET last_seen_at = ?, fog_version = ?, unpaired_at = NULL,
		reject_streak = 0 WHERE id = ?`, now(), fogVersion, id)
	return err
}

// UpdateFedPeerURL moves a peer's dial address — what a heartbeat carrying
// a changed advertised URL does.
func (s *Store) UpdateFedPeerURL(id int64, url string) error {
	_, err := s.db.Exec(`UPDATE fed_peers SET url = ? WHERE id = ?`, nullable(url), id)
	return err
}

// FedPeerEdit is one operator edit of a peer, applied whole or not at
// all. Nil fields are left alone.
type FedPeerEdit struct {
	Name, Label, URL *string
	Capabilities     []FedCapabilityEdit
}

// FedCapabilityEdit sets (Enabled) or removes one capability row.
type FedCapabilityEdit struct {
	Capability, Direction, Config string
	Enabled                       bool
}

// EditFedPeer applies e in one transaction. Removing feedback.ingest/use
// also drops the reports still queued for the peer: they were written
// under the consent just withdrawn.
func (s *Store) EditFedPeer(id int64, e FedPeerEdit) error {
	return s.withTx(func(tx *sql.Tx) error {
		if e.Label != nil {
			if _, err := tx.Exec(`UPDATE fed_peers SET label = ? WHERE id = ?`, *e.Label, id); err != nil {
				return err
			}
		}
		if e.URL != nil {
			if _, err := tx.Exec(`UPDATE fed_peers SET url = ? WHERE id = ?`, nullable(*e.URL), id); err != nil {
				return err
			}
		}
		for _, c := range e.Capabilities {
			var prev sql.NullString
			if err := tx.QueryRow(`SELECT config FROM fed_capabilities WHERE peer_id = ? AND capability = ? AND direction = ?`,
				id, c.Capability, c.Direction).Scan(&prev); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			var err error
			config := c.Config
			if c.Enabled {
				if config == "" {
					config = "{}"
				}
				_, err = tx.Exec(`INSERT INTO fed_capabilities (peer_id, capability, direction, config)
					VALUES (?, ?, ?, ?)
					ON CONFLICT(peer_id, capability, direction) DO UPDATE SET config = excluded.config`,
					id, c.Capability, c.Direction, config)
			} else {
				_, err = tx.Exec(`DELETE FROM fed_capabilities WHERE peer_id = ? AND capability = ? AND direction = ?`,
					id, c.Capability, c.Direction)
			}
			if err == nil && c.Direction == DirUse {
				err = settleQueuedUnderConsent(tx, id, c.Capability, c.Enabled, prev, config)
			}
			if err != nil {
				return err
			}
		}
		if e.Name != nil {
			if _, err := tx.Exec(`UPDATE fed_peers SET name = ? WHERE id = ?`, *e.Name, id); err != nil {
				if isUniqueViolation(err) {
					return fmt.Errorf("a peer named %q %w", *e.Name, ErrAlreadyExists)
				}
				return err
			}
		}
		return nil
	})
}

// settleQueuedUnderConsent keeps what is already queued for a peer within
// the consent the operator just gave. Queued rows were prepared under the
// consent in force when they were written — a flare row scrubbed for the
// mode and app filter of the time, a report marked to carry its dossier — so
// withdrawing or narrowing a use row has to reach them too, or turning
// "full" off (or back on in "redacted") would still ship what full allowed.
//
//   - feedback.ingest: withdrawn drops every queued report; narrowed to
//     "note", queued reports go as notes. Widening never upgrades a report
//     written under narrower consent.
//   - flare.ingest: withdrawn or changed in any way drops the queued
//     events. They are a courtesy stream; the local copy is the record.
func settleQueuedUnderConsent(tx *sql.Tx, peerID int64, capability string, enabled bool, prev sql.NullString, config string) error {
	switch capability {
	case CapFeedbackIngest:
		if !enabled {
			_, err := tx.Exec(`DELETE FROM feedback_deliveries WHERE peer_id = ? AND state = 'queued'`, peerID)
			return err
		}
		var c struct {
			Mode string `json:"mode"`
		}
		if json.Unmarshal([]byte(config), &c) == nil && c.Mode == "note" {
			_, err := tx.Exec(`UPDATE feedback_deliveries SET mode = 'note' WHERE peer_id = ? AND state = 'queued'`, peerID)
			return err
		}
	case CapFlareIngest:
		if !enabled || !prev.Valid || prev.String != config {
			_, err := tx.Exec(`DELETE FROM fed_outbox WHERE peer_id = ? AND kind = 'flare'`, peerID)
			return err
		}
	}
	return nil
}

// DeleteFedPeer revokes access and cascades the peer's capabilities, queued
// outbox rows, and error groups (and their events) away.
func (s *Store) DeleteFedPeer(id int64) error {
	_, err := s.db.Exec(`DELETE FROM fed_peers WHERE id = ?`, id)
	return err
}

// TouchFedPeerSeen marks a peer fresh from an inbound signed request. A peer
// that dials us is a peer that still holds a row for us, so this clears any
// refusal history too — evidence of the pairing arrives in both directions.
func (s *Store) TouchFedPeerSeen(id int64) error {
	_, err := s.db.Exec(`UPDATE fed_peers SET last_seen_at = ?, unpaired_at = NULL,
		reject_streak = 0 WHERE id = ?`, now(), id)
	return err
}

// UpdateFedPeerHeartbeat records what the last heartbeat exchange carried.
// An exchange that got this far is proof the peer still knows us, so it
// clears any refusal history in the same statement.
func (s *Store) UpdateFedPeerHeartbeat(id int64, fogVersion, remoteGrants, directory, people string) error {
	if people == "" {
		people = "[]"
	}
	_, err := s.db.Exec(`UPDATE fed_peers SET last_seen_at = ?, fog_version = ?,
		remote_grants = ?, directory = ?, people = ?, unpaired_at = NULL, reject_streak = 0 WHERE id = ?`,
		now(), fogVersion, remoteGrants, directory, people, id)
	return err
}

// RecordFedPeerRejection counts one "unknown peer" answer from this peer,
// and stamps the row unpaired once threshold of them have arrived in a row.
//
// The threshold is the whole point: a single refusal is as consistent with
// a peer mid-restore, mid-migration, or briefly serving an empty database
// as it is with a deliberate unpair. Only a peer that keeps saying it is
// saying it.
func (s *Store) RecordFedPeerRejection(id int64, threshold int) error {
	_, err := s.db.Exec(`UPDATE fed_peers
		SET reject_streak = reject_streak + 1,
		    unpaired_at = CASE
		        WHEN unpaired_at IS NULL AND reject_streak + 1 >= ? THEN ?
		        ELSE unpaired_at END
		WHERE id = ?`, threshold, now(), id)
	return err
}

// ---- capabilities ----

// SetFedCapability upserts one capability row. Presence of a row means
// enabled; RemoveFedCapability disables.
func (s *Store) SetFedCapability(peerID int64, capability, direction, config string) error {
	if config == "" {
		config = "{}"
	}
	_, err := s.db.Exec(`INSERT INTO fed_capabilities (peer_id, capability, direction, config)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(peer_id, capability, direction) DO UPDATE SET config = excluded.config`,
		peerID, capability, direction, config)
	return err
}

func (s *Store) RemoveFedCapability(peerID int64, capability, direction string) error {
	_, err := s.db.Exec(`DELETE FROM fed_capabilities WHERE peer_id = ? AND capability = ? AND direction = ?`,
		peerID, capability, direction)
	return err
}

// FedCapabilityConfig returns the config JSON for one capability row and
// whether it exists. This is the enforcement primitive: handlers ask for the
// grant row of the authenticated peer before serving anything.
func (s *Store) FedCapabilityConfig(peerID int64, capability, direction string) (string, bool, error) {
	var config string
	err := s.db.QueryRow(`SELECT config FROM fed_capabilities WHERE peer_id = ? AND capability = ? AND direction = ?`,
		peerID, capability, direction).Scan(&config)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	return config, err == nil, err
}

func (s *Store) ListFedCapabilities(peerID int64) ([]FedCapability, error) {
	return queryAll(s.db, func(sc scanner) (FedCapability, error) {
		var c FedCapability
		var config string
		err := sc.Scan(&c.PeerID, &c.Capability, &c.Direction, &config)
		c.Config = json.RawMessage(config)
		return c, err
	}, `SELECT peer_id, capability, direction, config FROM fed_capabilities
		WHERE peer_id = ? ORDER BY capability, direction`, peerID)
}

// AllFedCapabilities returns every capability row, grouped by peer: one
// query for list views that would otherwise ask once per peer.
func (s *Store) AllFedCapabilities() (map[int64][]FedCapability, error) {
	out := map[int64][]FedCapability{}
	err := queryEach(s.db, func(sc scanner) error {
		var c FedCapability
		var config string
		if err := sc.Scan(&c.PeerID, &c.Capability, &c.Direction, &config); err != nil {
			return err
		}
		c.Config = json.RawMessage(config)
		out[c.PeerID] = append(out[c.PeerID], c)
		return nil
	}, `SELECT peer_id, capability, direction, config FROM fed_capabilities
		ORDER BY peer_id, capability, direction`)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// FedCapabilityConfigs returns the config of one capability row for every
// peer that holds it, keyed by peer id. A peer missing from the map has no
// such row.
func (s *Store) FedCapabilityConfigs(capability, direction string) (map[int64]string, error) {
	out := map[int64]string{}
	err := queryEach(s.db, func(sc scanner) error {
		var id int64
		var config string
		if err := sc.Scan(&id, &config); err != nil {
			return err
		}
		out[id] = config
		return nil
	}, `SELECT peer_id, config FROM fed_capabilities WHERE capability = ? AND direction = ?`,
		capability, direction)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// FedPeersWithCapability lists peers holding one capability row — e.g. every
// peer this instance forwards errors to (flare.ingest, use).
func (s *Store) FedPeersWithCapability(capability, direction string) ([]FedPeer, error) {
	return queryAll(s.db, scanFedPeer, `SELECT `+fedPeerCols+` FROM fed_peers
		WHERE id IN (SELECT peer_id FROM fed_capabilities WHERE capability = ? AND direction = ?)
		ORDER BY name`, capability, direction)
}

// ---- invites ----

func (s *Store) CreateFedInvite(secretDigest, grants, expiresAt string) (*FedInvite, error) {
	if grants == "" {
		grants = "[]"
	}
	n := now()
	res, err := s.db.Exec(`INSERT INTO fed_invites (secret_digest, grants, created_at, expires_at)
		VALUES (?, ?, ?, ?)`, secretDigest, grants, n, expiresAt)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &FedInvite{ID: id, Grants: grants, CreatedAt: n, ExpiresAt: expiresAt}, nil
}

// ClaimFedInvite atomically burns an unclaimed, unexpired invite matching
// the digest, returning it. A second claim — or a wrong secret — gets
// ErrNotFound, indistinguishably.
func (s *Store) ClaimFedInvite(secretDigest, claimedBy string) (*FedInvite, error) {
	return ptr(queryOne(s.db, func(sc scanner) (FedInvite, error) {
		inv := FedInvite{ClaimedBy: claimedBy}
		err := sc.Scan(&inv.ID, &inv.Grants, &inv.CreatedAt, &inv.ExpiresAt)
		return inv, err
	}, `UPDATE fed_invites SET claimed_by = ?
		WHERE secret_digest = ? AND claimed_by IS NULL AND expires_at > ?
		RETURNING id, grants, created_at, expires_at`,
		claimedBy, secretDigest, now()))
}

// FedInviteOpen reports whether an unclaimed, unexpired invite matches the
// digest, without claiming it.
func (s *Store) FedInviteOpen(secretDigest string) bool {
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM fed_invites WHERE secret_digest = ? AND claimed_by IS NULL AND expires_at > ?`,
		secretDigest, now()).Scan(&n)
	return n > 0
}

// ListOpenFedInvites returns unclaimed, unexpired invites (for the UI's
// pending list). Expired and claimed rows are swept opportunistically.
func (s *Store) ListOpenFedInvites() ([]FedInvite, error) {
	// The sweep is housekeeping, but its failure is a real signal (locked or
	// corrupt database) that the SELECT below might not reproduce — and a
	// listing the sweep didn't clean would show stale invites as open.
	if _, err := s.db.Exec(`DELETE FROM fed_invites WHERE claimed_by IS NOT NULL OR expires_at <= ?`, now()); err != nil {
		return nil, err
	}
	return queryAll(s.db, func(sc scanner) (FedInvite, error) {
		var inv FedInvite
		err := sc.Scan(&inv.ID, &inv.Grants, &inv.CreatedAt, &inv.ExpiresAt)
		return inv, err
	}, `SELECT id, grants, created_at, expires_at FROM fed_invites ORDER BY id`)
}

func (s *Store) DeleteFedInvite(id int64) error {
	_, err := s.db.Exec(`DELETE FROM fed_invites WHERE id = ?`, id)
	return err
}

func nullable(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// ---- replay record ----

// ClaimFedRequest records one accepted request signature (digest) until
// expiresAt, reporting false when it was already recorded: a replay.
// Expired entries are swept in the same transaction (an indexed delete that
// finds nothing almost every time).
// ReleaseFedRequest gives up a claim whose request never verified: a
// reservation made when its headers arrived, for a body that didn't come
// or wasn't the one they signed.
func (s *Store) ReleaseFedRequest(digest string) error {
	_, err := s.db.Exec(`DELETE FROM fed_seen_requests WHERE digest = ?`, digest)
	return err
}

// FedRequestExpiry is when the replay record lets digest go.
func (s *Store) FedRequestExpiry(digest string) (time.Time, bool) {
	var at string
	if err := s.db.QueryRow(`SELECT expires_at FROM fed_seen_requests WHERE digest = ?`, digest).Scan(&at); err != nil {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, at)
	return t, err == nil
}

// FedRequestHeld reports whether the replay record still holds digest.
func (s *Store) FedRequestHeld(digest string) bool {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM fed_seen_requests WHERE digest = ?`, digest).Scan(&n)
	return err == nil && n > 0
}

// CountFileFedOrigins counts the contributions recorded here from a peer,
// and those in one share, from anyone.
func (s *Store) CountFileFedOrigins(peerID, shareID int64) (perPeer, perShare int, err error) {
	err = s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM file_fed_origins WHERE peer_id = ?),
		(SELECT COUNT(*) FROM file_fed_origins WHERE share_id = ?)`, peerID, shareID).Scan(&perPeer, &perShare)
	return
}

func (s *Store) ClaimFedRequest(digest string, expiresAt time.Time) (bool, error) {
	fresh := false
	err := s.withTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM fed_seen_requests WHERE expires_at < ?`, time.Now().UTC().Format(time.RFC3339)); err != nil {
			return err
		}
		res, err := tx.Exec(`INSERT INTO fed_seen_requests (digest, expires_at) VALUES (?, ?) ON CONFLICT(digest) DO NOTHING`,
			digest, expiresAt.UTC().Format(time.RFC3339))
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		fresh = n == 1
		return err
	})
	return fresh, err
}
