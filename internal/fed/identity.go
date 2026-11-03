// Package fed is the federation plane: this instance's identity, the
// signed-request wire discipline peers use to talk to each other, and (in
// later phases) the pairing handshake and heartbeat loop.
// See docs/federation-contract.md.
package fed

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hpke"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"gitea.lan/matthew/fog/internal/store"
	"gitea.lan/matthew/fog/internal/vault"
)

var ecdhX25519 = ecdh.X25519()

// SecretIdentityKey holds the ed25519 seed, vault-encrypted, in the system
// secret namespace. The public half lives plainly in fed_identity.
const SecretIdentityKey = "_fed_identity_key"

// Identity is this instance's name in the federation: a stable ULID plus the
// keypair every outbound request is signed with.
type Identity struct {
	InstanceID string
	Public     ed25519.PublicKey
	private    ed25519.PrivateKey

	// The encryption key every body sent to this instance is sealed to
	// (seal.go), and its binding: the identity key's signature over it.
	encPriv    hpke.PrivateKey
	EncPublic  []byte
	EncBinding string
}

// SecretEncryptionKey holds the seed of the encryption key, vault-encrypted
// like the identity seed. The key pair is HPKE's DeriveKeyPair of it.
const SecretEncryptionKey = "_fed_encryption_key"

// encBindingLabel is the transcript an identity key signs over its
// encryption key: "fogfed1-enckey\n<instance id>\n<sha256hex(key)>".
const encBindingLabel = "fogfed1-enckey"

// EncPublicB64 is the encryption key as the instance document carries it.
func (i *Identity) EncPublicB64() string { return base64.StdEncoding.EncodeToString(i.EncPublic) }

// VerifyEncBinding checks that the holder of identity key pub, as instance
// instanceID, bound the encryption key encB64 with sig.
func VerifyEncBinding(pub ed25519.PublicKey, instanceID, encB64, sig string) error {
	raw, err := decodeKey(encB64)
	if err != nil {
		return err
	}
	if _, err := sealKEM.NewPublicKey(raw); err != nil {
		return errors.New("invalid encryption key")
	}
	if !VerifyTranscript(pub, encBindingLabel, instanceID, raw, sig) {
		return errors.New("encryption key is not bound to that identity")
	}
	return nil
}

// decodeKey reads a 32-byte key as std base64.
func decodeKey(b64 string) ([]byte, error) {
	raw, err := base64.StdEncoding.Strict().DecodeString(b64)
	if err != nil || len(raw) != 32 {
		return nil, errors.New("invalid key encoding")
	}
	return raw, nil
}

// loadEncKey attaches the encryption key to id, minting it on first use:
// an identity from before bodies were sealed gets one the first time it
// loads. Secret first, then row, as with the identity itself.
func loadEncKey(st *store.Store, vlt *vault.Vault, id *Identity, rowEnc string) error {
	secrets := vault.Secrets{Store: st, Vault: vlt}
	seedB64, err := secrets.Get(0, SecretEncryptionKey)
	switch {
	case errors.Is(err, store.ErrNotFound):
		if rowEnc != "" {
			return errors.New("federation encryption key row exists but its private key is gone")
		}
		seed := make([]byte, 32)
		if _, err := rand.Read(seed); err != nil {
			return err
		}
		seedB64 = base64.StdEncoding.EncodeToString(seed)
		if err := secrets.Put(0, SecretEncryptionKey, seedB64); err != nil {
			return err
		}
	case err != nil:
		return fmt.Errorf("reading federation encryption key: %w", err)
	}
	seed, err := base64.StdEncoding.Strict().DecodeString(seedB64)
	if err != nil || len(seed) != 32 {
		return errors.New("federation encryption key secret is corrupt")
	}
	priv, err := sealKEM.DeriveKeyPair(seed)
	if err != nil {
		return err
	}
	pub := priv.PublicKey().Bytes()
	pubB64 := base64.StdEncoding.EncodeToString(pub)
	if rowEnc == "" {
		if err := st.SetFedIdentityEncKey(pubB64); err != nil {
			return err
		}
	} else if rowEnc != pubB64 {
		return errors.New("federation encryption keypair mismatch between vault and fed_identity")
	}
	id.encPriv, id.EncPublic = priv, pub
	id.EncBinding = id.SignTranscript(encBindingLabel, id.InstanceID, pub)
	return nil
}

func (i *Identity) PublicKeyB64() string {
	return base64.StdEncoding.EncodeToString(i.Public)
}

// EnsureIdentity loads the instance identity, minting one on first use.
// Mint order is secret-then-row: a crash between the two leaves an orphan
// secret that the next call simply overwrites, never a row whose private key
// is missing.
func EnsureIdentity(st *store.Store, vlt *vault.Vault) (*Identity, error) {
	row, err := st.FedIdentity()
	if err == nil {
		return loadIdentity(st, vlt, row)
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}

	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return nil, err
	}
	// The instance ID is the key's: nobody else can hold a key that names
	// this ID, so no other household can claim it (DeriveInstanceID).
	instanceID := DeriveInstanceID(pub)
	if err := (vault.Secrets{Store: st, Vault: vlt}).Put(0, SecretIdentityKey, base64.StdEncoding.EncodeToString(priv.Seed())); err != nil {
		return nil, err
	}
	if err := st.SetFedIdentity(instanceID, base64.StdEncoding.EncodeToString(pub)); err != nil {
		return nil, err
	}
	id := &Identity{InstanceID: instanceID, Public: pub, private: priv}
	if err := loadEncKey(st, vlt, id, ""); err != nil {
		return nil, err
	}
	return id, nil
}

func loadIdentity(st *store.Store, vlt *vault.Vault, row *store.FedIdentity) (*Identity, error) {
	secrets := vault.Secrets{Store: st, Vault: vlt}
	// A renewal that stopped between rewriting the row and moving its
	// seeds into place is finished first (RenewIdentity).
	resumeRenewal(secrets, row.PublicKey)
	seedB64, err := secrets.Get(0, SecretIdentityKey)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("federation identity row exists but its private key is gone: %w", err)
	}
	if err != nil {
		return nil, fmt.Errorf("reading federation key: %w", err)
	}
	seed, err := base64.StdEncoding.Strict().DecodeString(seedB64)
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, errors.New("federation key secret is corrupt")
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	// The stored public key is authoritative for what peers know; verify the
	// private key still matches it rather than silently diverging.
	if row.PublicKey != base64.StdEncoding.EncodeToString(pub) {
		return nil, errors.New("federation keypair mismatch between vault and fed_identity")
	}
	id := &Identity{InstanceID: row.InstanceID, Public: pub, private: priv}
	if err := loadEncKey(st, vlt, id, row.EncPublicKey); err != nil {
		return nil, err
	}
	return id, nil
}

// Self-certifying instance IDs.
//
// An instance ID names a household in every peer's database, in every
// person address (person:<instance>/<ulid>), and in every room a household's
// people share. An ID that is merely random proves nothing about who holds
// it: a household one invites could claim the ID of a real household one
// isn't paired with, and be that household here — its people's addresses,
// their keys in rooms — from then on. So an instance ID is derived from the
// identity key, and checked against it wherever a key is taken in for an ID:
//
//	ID = Crockford base32 (26 characters) of 0b10 || first 128 bits of
//	     sha256("fogfed1-instance\n" || public key)
//
// The marker bits make a derived ID start with one of G H J K M N P Q, where a ULID —
// the random IDs instances were minted with before — starts with 0–7; both
// share the alphabet and length, so every place that reads an ID as a
// ULID-shaped name keeps working.

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// DeriveInstanceID is the instance ID that belongs to an identity key.
func DeriveInstanceID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(append([]byte("fogfed1-instance\n"), pub...))
	// 130 bits: the marker 0b10, then 128 bits of the digest.
	var bits [17]byte // 136 bits, the top 130 used
	bits[0] = 0x80    // 1 0 ...
	for i := 0; i < 16; i++ {
		bits[i] |= sum[i] >> 2
		bits[i+1] = sum[i] << 6
	}
	out := make([]byte, 26)
	for i := range out {
		bit := i * 5
		v := (uint16(bits[bit/8])<<8 | uint16(bits[bit/8+1])) >> (11 - bit%8) & 0x1f
		out[i] = crockford[v]
	}
	return string(out)
}

// ErrInstanceNotBound: an instance ID in the self-certifying form that its
// key doesn't derive — somebody else's ID, claimed.
var ErrInstanceNotBound = errors.New("refused: that instance ID belongs to another key")

// IsDerivedInstanceID reports whether id is in the self-certifying form.
func IsDerivedInstanceID(id string) bool {
	return len(id) == 26 && strings.ContainsRune("GHJKMNPQ", rune(id[0])) && strings.Trim(id, crockford) == ""
}

// IsLegacyInstanceID reports whether id is a random ULID, minted before
// instance IDs were derived from keys.
func IsLegacyInstanceID(id string) bool {
	return len(id) == 26 && id[0] >= '0' && id[0] <= '7' && strings.Trim(id, crockford) == ""
}

// CheckInstanceID checks an instance ID against the identity key presented
// for it: a derived ID must be that key's; a legacy one can't be checked and
// is taken as given (see the contract's known gaps); anything else is not an
// instance ID.
func CheckInstanceID(id, pubB64 string) error {
	pub, err := ParsePublicKey(pubB64)
	if err != nil {
		return err
	}
	switch {
	case IsDerivedInstanceID(id):
		if id != DeriveInstanceID(pub) {
			return ErrInstanceNotBound
		}
		return nil
	case IsLegacyInstanceID(id):
		return nil
	}
	return errors.New("not an instance ID")
}

// ParsePublicKey decodes a peer's base64 ed25519 public key.
func ParsePublicKey(b64 string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.Strict().DecodeString(b64)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, errors.New("invalid ed25519 public key")
	}
	return ed25519.PublicKey(raw), nil
}

// Fingerprint is how a key reads on a screen: the SHA-256 of the raw
// ed25519 public key as colon-separated hex. Short is the first four
// bytes, enough to tell two peers apart at a glance; Full is the whole
// digest, for comparing over the phone. An unparsable key fingerprints
// as empty rather than as a digest of garbage.
func Fingerprint(b64 string) (short, full string) {
	pub, err := ParsePublicKey(b64)
	if err != nil {
		return "", ""
	}
	sum := sha256.Sum256(pub)
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02x", b)
	}
	return strings.Join(parts[:4], ":"), strings.Join(parts, ":")
}

// ---- pairing only with key-bound IDs ----

// ErrLegacySelf: this fog's own instance ID predates key-bound IDs, so it
// can't pair with a household it isn't already paired with until it renews
// its identity. Pairings made before keep working.
var ErrLegacySelf = errors.New("this fog's identity predates key-bound instance IDs; renew it on the Connect page to pair with a new household " +
	"(households already paired here keep working)")

// ErrLegacyPeer: the other side's instance ID predates key-bound IDs, so
// nothing proves it is that household's: a new pairing needs it renewed.
var ErrLegacyPeer = errors.New("refused: the other fog's identity predates key-bound instance IDs; it must renew its identity " +
	"(on its Connect page) before it can pair with a new household")

// LegacyIdentity reports whether this fog's own instance ID is a legacy,
// random one rather than its key's.
func (s *Service) LegacyIdentity() bool {
	id, err := s.Identity()
	return err == nil && !IsDerivedInstanceID(id.InstanceID)
}

// CanPairNew says whether a new pairing — a peer row that doesn't exist
// yet — may be made with the household named other: both this fog's ID and
// theirs must be key-bound. (That their ID is their key's is checked with
// the key, by InstanceDoc.CheckEncBinding.) A household already held here
// re-pairs under the rules it was paired under.
func (s *Service) CanPairNew(other string) error {
	id, err := s.Identity()
	if err != nil {
		return err
	}
	if !IsDerivedInstanceID(id.InstanceID) {
		return ErrLegacySelf
	}
	if !IsDerivedInstanceID(other) {
		return ErrLegacyPeer
	}
	return nil
}

// CheckPeerRow checks a stored peer row's instance ID against its key, as
// CheckInstanceID does a presented one. A row is checked again wherever it
// stands for a household — authenticating it, naming a household's people
// — because a fog older than key-bound IDs took any ID it was given: a row
// whose ID has the derived form but is another key's is somebody else's
// name, claimed before the check existed, and stands for nobody.
//
// Only an ID in the derived form can be checked; any other row is left to
// the checks it was paired under (a legacy ID was taken as given).
func CheckPeerRow(p *store.FedPeer) error {
	if p == nil {
		return store.ErrNotFound
	}
	if !IsDerivedInstanceID(p.InstanceID) {
		return nil
	}
	return CheckInstanceID(p.InstanceID, p.PublicKey)
}

// PeerForHousehold is the peer row that stands for the household named by
// instanceID — a person address's home, a room's hub — when there is one
// and its ID checks out against its key; store.ErrNotFound otherwise.
func PeerForHousehold(st *store.Store, instanceID string) (*store.FedPeer, error) {
	p, err := st.FedPeerByInstanceID(instanceID)
	if err != nil {
		return nil, err
	}
	if CheckPeerRow(p) != nil {
		return nil, store.ErrNotFound
	}
	return p, nil
}

// ---- renewing a legacy identity ----

// The renewal's new seeds wait under these names until the identity row
// names the new key; loading finishes a swap a crash interrupted.
const (
	secretIdentityNext   = "_fed_identity_key_next"
	secretEncryptionNext = "_fed_encryption_key_next"
)

// ErrNotLegacy: the identity is already key-bound; there is nothing to renew.
var ErrNotLegacy = errors.New("this fog's identity is already key-bound; there is nothing to renew")

// RenewIdentity replaces a legacy identity with a new, key-bound one: new
// identity and encryption keys, and the instance ID derived from them. It
// is the operator's explicit act — every household paired with this fog
// knows it by the old ID and key and will not recognise the new one, and
// whatever named this household by its old ID (chat addresses among them)
// names nobody here afterwards. The caller says so first. Peer rows are
// kept, so pairing again with a household refreshes its row in place.
func (s *Service) RenewIdentity() (oldID, newID string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.id
	if cur == nil {
		if cur, err = EnsureIdentity(s.Store, s.Vault); err != nil {
			return "", "", err
		}
	}
	if IsDerivedInstanceID(cur.InstanceID) {
		return "", "", ErrNotLegacy
	}
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return "", "", err
	}
	encSeed := make([]byte, 32)
	if _, err := rand.Read(encSeed); err != nil {
		return "", "", err
	}
	encPriv, err := sealKEM.DeriveKeyPair(encSeed)
	if err != nil {
		return "", "", err
	}
	secrets := vault.Secrets{Store: s.Store, Vault: s.Vault}
	seedB64, encB64 := base64.StdEncoding.EncodeToString(priv.Seed()), base64.StdEncoding.EncodeToString(encSeed)
	if err := secrets.Put(0, secretIdentityNext, seedB64); err != nil {
		return "", "", err
	}
	if err := secrets.Put(0, secretEncryptionNext, encB64); err != nil {
		return "", "", err
	}
	newID = DeriveInstanceID(pub)
	if err := s.Store.RenewFedIdentity(cur.InstanceID, newID, base64.StdEncoding.EncodeToString(pub),
		base64.StdEncoding.EncodeToString(encPriv.PublicKey().Bytes())); err != nil {
		return "", "", err
	}
	if err := finishRenewal(secrets); err != nil {
		return "", "", err
	}
	s.id = nil // the next Identity() loads the new one
	return cur.InstanceID, newID, nil
}

// finishRenewal moves a renewal's seeds into place and clears the staged
// copies. Idempotent: run again after a crash, it lands the same state.
func finishRenewal(secrets vault.Secrets) error {
	seed, err := secrets.Get(0, secretIdentityNext)
	if err != nil {
		return err
	}
	enc, err := secrets.Get(0, secretEncryptionNext)
	if err != nil {
		return err
	}
	if err := secrets.Put(0, SecretIdentityKey, seed); err != nil {
		return err
	}
	if err := secrets.Put(0, SecretEncryptionKey, enc); err != nil {
		return err
	}
	if err := secrets.Put(0, secretIdentityNext, ""); err != nil {
		return err
	}
	return secrets.Put(0, secretEncryptionNext, "")
}

// resumeRenewal finishes a renewal interrupted after the identity row was
// rewritten: the row names the staged key, not the stored one.
func resumeRenewal(secrets vault.Secrets, rowPub string) bool {
	next, err := secrets.Get(0, secretIdentityNext)
	if err != nil || next == "" {
		return false
	}
	if enc, err := secrets.Get(0, secretEncryptionNext); err != nil || enc == "" {
		return false
	}
	seed, err := base64.StdEncoding.Strict().DecodeString(next)
	if err != nil || len(seed) != ed25519.SeedSize {
		return false
	}
	pub := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	if base64.StdEncoding.EncodeToString(pub) != rowPub {
		return false
	}
	return finishRenewal(secrets) == nil
}
