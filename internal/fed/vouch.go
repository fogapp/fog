// The vouch: a fog-signed attestation that an app in this household may
// connect to a named app in a paired household, so an app inherits the
// peering the operator already made instead of establishing trust itself.
// The minting fog signs a short-lived vouch with its instance key and relays
// it over the federation channel; the receiving fog verifies it against the
// key it pinned for that peer at pairing — no app-level pairing, no per-app
// bearer to store.
package fed

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"
)

const vouchVersion = "fogvouch2"

// VouchTTL bounds how long a minted vouch is valid — long enough to present
// it on a fresh connection, short enough that a captured one is soon useless.
const VouchTTL = 5 * time.Minute

// Vouch is fog's attestation. From/To are instance ULIDs, App is the target
// app's name, FromApp is the requesting app's public URL, and FromKey is the
// requesting app's own public key — signed in, so the receiver takes the key
// from the verified vouch and a captured vouch cannot be presented with a
// swapped key. Nonce makes each vouch one connection: the receiver answers
// a replay from its cache instead of minting again.
type Vouch struct {
	From      string
	To        string
	App       string
	FromApp   string
	FromKey   string
	Nonce     string
	IssuedAt  string
	ExpiresAt string
}

func vouchString(v Vouch) []byte {
	return []byte(vouchVersion + "\n" + v.From + "\n" + v.To + "\n" + v.App + "\n" + v.FromApp + "\n" + v.FromKey + "\n" + v.Nonce + "\n" + v.IssuedAt + "\n" + v.ExpiresAt)
}

// SignVouch signs a vouch with the minting instance's key.
func SignVouch(id *Identity, v Vouch) string {
	return base64.RawURLEncoding.EncodeToString(ed25519.Sign(id.private, vouchString(v)))
}

// VerifyVouch checks a vouch's signature against the minter's public key and
// its expiry.
func VerifyVouch(pub ed25519.PublicKey, v Vouch, sigB64 string, now time.Time) error {
	exp, err := time.Parse(time.RFC3339, v.ExpiresAt)
	if err != nil {
		return errors.New("invalid vouch expiry")
	}
	if now.After(exp) {
		return errors.New("vouch expired")
	}
	sig, err := base64.RawURLEncoding.Strict().DecodeString(sigB64)
	if err != nil {
		return errors.New("malformed vouch signature")
	}
	if !ed25519.Verify(pub, vouchString(v), sig) {
		return errors.New("vouch signature does not verify")
	}
	return nil
}

type vouchWire struct {
	From      string `json:"from"`
	To        string `json:"to"`
	App       string `json:"app"`
	FromApp   string `json:"fromApp"`
	FromKey   string `json:"fromKey"`
	Nonce     string `json:"nonce"`
	IssuedAt  string `json:"iat"`
	ExpiresAt string `json:"exp"`
	Sig       string `json:"sig"`
}

// EncodeVouch packs a vouch and its signature into one opaque token fog
// relays to the peer and hands to the granting app.
func EncodeVouch(v Vouch, sig string) string {
	b, _ := json.Marshal(vouchWire{
		From: v.From, To: v.To, App: v.App, FromApp: v.FromApp,
		FromKey: v.FromKey, Nonce: v.Nonce, IssuedAt: v.IssuedAt, ExpiresAt: v.ExpiresAt, Sig: sig,
	})
	return base64.RawURLEncoding.EncodeToString(b)
}

// DecodeVouch unpacks a token back into a vouch and its signature.
func DecodeVouch(token string) (Vouch, string, error) {
	raw, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil {
		return Vouch{}, "", errors.New("malformed vouch")
	}
	var w vouchWire
	if err := json.Unmarshal(raw, &w); err != nil {
		return Vouch{}, "", errors.New("malformed vouch")
	}
	return Vouch{From: w.From, To: w.To, App: w.App, FromApp: w.FromApp, FromKey: w.FromKey, Nonce: w.Nonce, IssuedAt: w.IssuedAt, ExpiresAt: w.ExpiresAt}, w.Sig, nil
}
