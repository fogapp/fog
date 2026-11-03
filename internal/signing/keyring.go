package signing

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"

	"gitea.lan/matthew/fog/internal/randx"
	"gitea.lan/matthew/fog/internal/store"
	"gitea.lan/matthew/fog/internal/vault"
)

// Alg is the one algorithm fog signs with. ES256 is the widest-supported
// asymmetric JOSE algorithm among OIDC clients; RS256-only clients get a
// key of their own when one arrives, never a weaker default.
const Alg = jose.ES256

// secretPrefix names a key's private half in the vault (system secrets,
// app_id 0).
const secretPrefix = "_signing_key_"

// ErrUnknownKey is a token naming a key fog does not publish.
var ErrUnknownKey = errors.New("signing: unknown or retired key")

// Keyring signs with the current key and publishes the key set. It owns the
// keys' private halves; the schedule is Plan's.
type Keyring struct {
	Store *store.Store
	Vault *vault.Vault
	Now   func() time.Time

	mu      sync.Mutex
	private map[string]*ecdsa.PrivateKey
	// cache is the signing_keys table, parsed: Sign runs on every gated
	// request and Verify on every app→fog call, so neither may touch the
	// database. The Keyring is the table's only writer, and each of its
	// writes drops the cache; nil means read it again.
	cache []cachedKey
}

// cachedKey is one signing_keys row with its public half parsed. A row whose
// JWK does not parse keeps the error, reported only while it is published.
type cachedKey struct {
	key    Key
	jwk    jose.JSONWebKey
	jwkErr error
}

func (k *Keyring) now() time.Time {
	if k.Now != nil {
		return k.Now()
	}
	return time.Now()
}

func (k *Keyring) secrets() vault.Secrets { return vault.Secrets{Store: k.Store, Vault: k.Vault} }

// cached returns the parsed table, reading it if a write dropped it. k.mu
// must be held.
func (k *Keyring) cached() ([]cachedKey, error) {
	if k.cache != nil {
		return k.cache, nil
	}
	rows, err := k.Store.ListSigningKeys()
	if err != nil {
		return nil, err
	}
	c := make([]cachedKey, len(rows))
	for i, r := range rows {
		c[i].key = Key{KID: r.KID, PublishAt: r.PublishAt, SignFrom: r.SignFrom, SignUntil: r.SignUntil, UnpublishAt: r.UnpublishAt}
		if err := json.Unmarshal([]byte(r.PublicJWK), &c[i].jwk); err != nil {
			c[i].jwkErr = fmt.Errorf("signing key %s: %w", r.KID, err)
		}
	}
	k.cache = c
	return c, nil
}

// ring is the keys' schedules. k.mu must be held.
func (k *Keyring) ring() ([]Key, error) {
	c, err := k.cached()
	if err != nil {
		return nil, err
	}
	ring := make([]Key, len(c))
	for i := range c {
		ring[i] = c[i].key
	}
	return ring, nil
}

// Reconcile carries out Plan for now. Safe to call from a ticker and on
// demand; a plan that asks for nothing touches nothing.
func (k *Keyring) Reconcile() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.reconcileLocked()
}

func (k *Keyring) reconcileLocked() error {
	ring, err := k.ring()
	if err != nil {
		return err
	}
	for _, a := range Plan(ring, k.now()) {
		switch a.Op {
		case Drop:
			k.cache = nil
			if err := k.Store.DeleteSigningKey(a.Key.KID); err != nil {
				return err
			}
			if err := k.Store.DeleteSecret(0, secretPrefix+a.Key.KID); err != nil {
				return err
			}
			delete(k.private, a.Key.KID)
		case Create:
			if err := k.create(a.Key); err != nil {
				return err
			}
		}
	}
	return nil
}

// create mints a P-256 key on schedule s. The private half is written to the
// vault before the public row exists, so a key the ring lists can always
// sign.
func (k *Keyring) create(s Key) error {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return err
	}
	kid := randx.ULID()
	if err := k.secrets().Put(0, secretPrefix+kid, base64.StdEncoding.EncodeToString(der)); err != nil {
		return err
	}
	pub, err := json.Marshal(jose.JSONWebKey{Key: &priv.PublicKey, KeyID: kid, Algorithm: string(Alg), Use: "sig"})
	if err != nil {
		return err
	}
	k.cache = nil
	if err := k.Store.InsertSigningKey(store.SigningKey{
		KID: kid, Alg: string(Alg), PublicJWK: string(pub),
		PublishAt: s.PublishAt, SignFrom: s.SignFrom, SignUntil: s.SignUntil, UnpublishAt: s.UnpublishAt,
	}); err != nil {
		return err
	}
	if k.private == nil {
		k.private = map[string]*ecdsa.PrivateKey{}
	}
	k.private[kid] = priv
	return nil
}

func (k *Keyring) privateKey(kid string) (*ecdsa.PrivateKey, error) {
	if p := k.private[kid]; p != nil {
		return p, nil
	}
	v, err := k.secrets().Get(0, secretPrefix+kid)
	if err != nil {
		return nil, fmt.Errorf("signing key %s: %w", kid, err)
	}
	der, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return nil, err
	}
	parsed, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, err
	}
	p, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("signing key %s is not ECDSA", kid)
	}
	if k.private == nil {
		k.private = map[string]*ecdsa.PrivateKey{}
	}
	k.private[kid] = p
	return p, nil
}

// Sign serializes claims as a compact JWS signed by the current key, with
// typ in the header ("JWT", or "logout+jwt" for a logout token).
func (k *Keyring) Sign(claims any, typ string) (string, error) {
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	ring, err := k.ring()
	if err != nil {
		return "", err
	}
	cur, ok := Current(ring, k.now())
	if !ok {
		if err := k.reconcileLocked(); err != nil {
			return "", err
		}
		if ring, err = k.ring(); err != nil {
			return "", err
		}
		if cur, ok = Current(ring, k.now()); !ok {
			return "", errors.New("signing: no key signs now")
		}
	}
	priv, err := k.privateKey(cur.KID)
	if err != nil {
		return "", err
	}
	opts := (&jose.SignerOptions{}).WithType(jose.ContentType(typ)).WithHeader(jose.HeaderKey("kid"), cur.KID)
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: Alg, Key: priv}, opts)
	if err != nil {
		return "", err
	}
	obj, err := signer.Sign(payload)
	if err != nil {
		return "", err
	}
	return obj.CompactSerialize()
}

// JWKS is the published key set: what /oauth2/jwks serves.
func (k *Keyring) JWKS() (jose.JSONWebKeySet, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	c, err := k.cached()
	if err != nil {
		return jose.JSONWebKeySet{}, err
	}
	// Which keys are published is a matter of the clock, not the table, so
	// it is decided per call.
	set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{}}
	now := k.now()
	for i := range c {
		if !c[i].key.Published(now) {
			continue
		}
		if c[i].jwkErr != nil {
			return jose.JSONWebKeySet{}, c[i].jwkErr
		}
		set.Keys = append(set.Keys, c[i].jwk)
	}
	return set, nil
}

// Verify checks a compact JWS fog signed: the algorithm is ES256 and nothing
// else, the key is one fog publishes now, and the signature holds. It
// returns the payload; claims are the caller's to check.
func (k *Keyring) Verify(compact string) ([]byte, error) {
	set, err := k.JWKS()
	if err != nil {
		return nil, err
	}
	return verifyWith(set, compact)
}

// VerifyAs is Verify for a token of one purpose: its protected typ header
// must be typ ("JWT" for an ID token), so a logout token, a proof or a
// visit token can never stand in for it.
func (k *Keyring) VerifyAs(compact, typ string) ([]byte, error) {
	set, err := k.JWKS()
	if err != nil {
		return nil, err
	}
	payload, got, err := VerifyTyped(set, compact)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(got, typ) {
		return nil, fmt.Errorf("signing: a %q token, not %q", got, typ)
	}
	return payload, nil
}

// VerifyTyped is verifyWith, also returning the protected typ header, so a
// caller can refuse a token signed for another purpose.
func VerifyTyped(set jose.JSONWebKeySet, compact string) ([]byte, string, error) {
	payload, err := verifyWith(set, compact)
	if err != nil {
		return nil, "", err
	}
	obj, _ := jose.ParseSigned(compact, []jose.SignatureAlgorithm{Alg})
	typ, _ := obj.Signatures[0].Protected.ExtraHeaders[jose.HeaderType].(string)
	return payload, typ, nil
}

// verifyWith checks a compact JWS against a key set — another fog's, as its
// pairing delivered it: ES256 only, exactly one signature, a key in the set.
func verifyWith(set jose.JSONWebKeySet, compact string) ([]byte, error) {
	obj, err := jose.ParseSigned(compact, []jose.SignatureAlgorithm{Alg})
	if err != nil {
		return nil, err
	}
	if len(obj.Signatures) != 1 {
		return nil, errors.New("signing: want exactly one signature")
	}
	keys := set.Key(obj.Signatures[0].Header.KeyID)
	if len(keys) != 1 {
		return nil, ErrUnknownKey
	}
	return obj.Verify(keys[0].Key)
}

// RetireAll ends every key now — the emergency rotation — and reconciles,
// so a fresh key signs immediately and everything signed before stops
// verifying.
func (k *Keyring) RetireAll() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	rows, err := k.Store.ListSigningKeys()
	if err != nil {
		return err
	}
	now := k.now()
	k.cache = nil
	for _, r := range rows {
		ret := Retire(Key{PublishAt: r.PublishAt, SignFrom: r.SignFrom, SignUntil: r.SignUntil, UnpublishAt: r.UnpublishAt}, now)
		r.PublishAt, r.SignFrom, r.SignUntil, r.UnpublishAt = ret.PublishAt, ret.SignFrom, ret.SignUntil, ret.UnpublishAt
		if err := k.Store.UpdateSigningKeySchedule(r); err != nil {
			return err
		}
	}
	return k.reconcileLocked()
}
