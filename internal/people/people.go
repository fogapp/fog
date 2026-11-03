// Package people holds the rules of the household directory: what a handle
// may look like, how a device proves it belongs to a person, how an invite
// is encoded. Storage is internal/store; HTTP is internal/server. Nothing in
// here touches a key fog could use to read a conversation — fog stores public
// keys and signatures over them, and verifies; it never signs on a person's
// behalf.
package people

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"net/mail"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"gitea.lan/matthew/fog/internal/ttlmap"
)

const (
	// InviteTTL is how long a join link stays live by default.
	InviteTTL = 7 * 24 * time.Hour
	// MaxKeyPackages bounds how many unconsumed KeyPackages a device may keep
	// published; more than this is a client bug, not a need.
	MaxKeyPackages = 100
	// MaxBackupBytes caps a person's client-encrypted backup blob.
	MaxBackupBytes = 64 << 20
)

var handleRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{1,31}$`)

// reserved handles are ones fog itself uses or that would confuse addresses.
var reserved = map[string]bool{"fog": true, "system": true, "everyone": true, "here": true, "channel": true, "me": true, "join": true, "login": true}

var (
	ErrBadHandle      = errors.New("handle must be 2–32 characters: lowercase letters, digits, '.', '_' or '-'")
	ErrReservedHandle = errors.New("that handle is reserved")
	ErrBadRole        = errors.New("role must be member or guest")
	ErrBadAttestation = errors.New("attestation does not verify against the identity key")
	ErrBadKey         = errors.New("key must be a 32-byte ed25519 public key")
)

// NormalizeHandle lowercases and validates a handle.
func NormalizeHandle(h string) (string, error) {
	h = strings.ToLower(strings.TrimSpace(h))
	if !handleRe.MatchString(h) {
		return "", ErrBadHandle
	}
	if reserved[h] {
		return "", ErrReservedHandle
	}
	return h, nil
}

// LookupHandle normalizes a handle for lookup only: syntax is checked, but
// reserved names pass, since fog's own bot is one of them.
func LookupHandle(h string) (string, error) {
	h = strings.ToLower(strings.TrimSpace(h))
	if !handleRe.MatchString(h) {
		return "", ErrBadHandle
	}
	return h, nil
}

// ValidDisplayName trims and bounds a display name; empty falls back to the
// handle at the call site.
func ValidDisplayName(n string) string {
	n = strings.TrimSpace(n)
	if utf8.RuneCountInString(n) > 64 {
		r := []rune(n)
		n = string(r[:64])
	}
	return n
}

// ValidEmail trims and checks an optional email address; "" is no address.
// It is shown as unverified until fog can send mail, and never used to find
// or link anyone (docs/signin-contract.md).
func ValidEmail(e string) (string, error) {
	e = strings.TrimSpace(e)
	if e == "" {
		return "", nil
	}
	a, err := mail.ParseAddress(e)
	if err != nil || a.Address != e || len(e) > 254 {
		return "", errors.New("that doesn't look like an email address")
	}
	return e, nil
}

// ValidRole accepts the roles the operator may hand out. Operator is never
// assignable: there is exactly one, and it is whoever set the box up.
func ValidRole(r string) (string, error) {
	switch r {
	case "", "member":
		return "member", nil
	case "guest":
		return "guest", nil
	}
	return "", ErrBadRole
}

// attestationPrefix domain-separates the device attestation from any other
// signature a person's identity key might ever make.
const attestationPrefix = "fog-device-attest/1\n"

// AttestationMessage is what the person's identity key signs to vouch for a
// device key. Exposed so clients and tests build exactly the same bytes.
func AttestationMessage(deviceKey []byte) []byte {
	return append([]byte(attestationPrefix), deviceKey...)
}

// VerifyAttestation checks that identityKey signed deviceKey.
func VerifyAttestation(identityKey, deviceKey, attestation []byte) error {
	if len(identityKey) != ed25519.PublicKeySize || len(deviceKey) != ed25519.PublicKeySize {
		return ErrBadKey
	}
	if len(attestation) != ed25519.SignatureSize {
		return ErrBadAttestation
	}
	if !ed25519.Verify(ed25519.PublicKey(identityKey), AttestationMessage(deviceKey), attestation) {
		return ErrBadAttestation
	}
	return nil
}

// deviceSessionPrefix domain-separates the possession proof a device makes
// to bind a fresh session to itself.
const deviceSessionPrefix = "fog-device-session/1\n"

// DeviceSessionMessage is what a device signs to prove it holds its key.
func DeviceSessionMessage(nonce string) string { return deviceSessionPrefix + nonce }

// VerifyDeviceSession checks a device's possession proof.
func VerifyDeviceSession(deviceKey []byte, nonce string, sig []byte) bool {
	if len(deviceKey) != ed25519.PublicKeySize || len(sig) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(ed25519.PublicKey(deviceKey), []byte(DeviceSessionMessage(nonce)), sig)
}

// Nonces is a small TTL set of outstanding device challenges.
type Nonces struct {
	items *ttlmap.Map[string, struct{}]
}

const nonceTTL = 5 * time.Minute

func NewNonces() *Nonces { return &Nonces{items: ttlmap.New[string, struct{}](nonceTTL, 0)} }

func (n *Nonces) Put(deviceID int64, nonce string) {
	n.items.Put(nonceKey(deviceID, nonce), struct{}{})
}

// Take consumes a nonce; true when it was outstanding and unexpired.
func (n *Nonces) Take(deviceID int64, nonce string) bool {
	_, ok := n.items.Take(nonceKey(deviceID, nonce))
	return ok
}

func nonceKey(deviceID int64, nonce string) string { return fmt.Sprintf("%d:%s", deviceID, nonce) }

// ValidPublicKey accepts a raw 32-byte ed25519 public key.
func ValidPublicKey(k []byte) error {
	if len(k) != ed25519.PublicKeySize {
		return ErrBadKey
	}
	return nil
}

// DecodeKey accepts base64 (standard or URL, padded or not).
func DecodeKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	for _, enc := range []*base64.Encoding{base64.RawURLEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.StdEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, errors.New("malformed base64")
}

// EncodeKey is the one encoding fog emits: base64url, unpadded.
func EncodeKey(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// JoinPath is where an invite link lands in the SPA.
func JoinPath(secret string) string { return "/join/" + secret }
