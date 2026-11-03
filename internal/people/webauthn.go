package people

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"

	"gitea.lan/matthew/fog/internal/ttlmap"
)

// Passkeys need a Relying Party ID that is the site's registrable domain and
// origins that match the page. Fog derives both from the request: on
// fog.<domain> behind Caddy that is the domain with an https origin; on a
// bare LAN address WebAuthn is unavailable (browsers require a secure
// context) and callers should offer the password fallback instead.

// ErrNoSecureContext says passkeys cannot work at this address.
var ErrNoSecureContext = errors.New("passkeys need https and a domain; use a password here")

// ErrNotFogsOrigin refuses a passkey ceremony on any host of the household's
// domain but fog's own.
var ErrNotFogsOrigin = errors.New("passkeys for fog work only at fog's own address")

// RelyingParty builds a WebAuthn instance for the request's origin.
func RelyingParty(r *http.Request, baseDomain, displayName string) (*webauthn.WebAuthn, error) {
	host := r.Host
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	if fh := r.Header.Get("X-Forwarded-Host"); fh != "" {
		host = fh
	}
	hostOnly := host
	if i := strings.LastIndex(hostOnly, ":"); i > 0 && !strings.Contains(hostOnly, "]") {
		hostOnly = hostOnly[:i]
	}
	if scheme != "https" && hostOnly != "localhost" {
		return nil, ErrNoSecureContext
	}
	rpID := hostOnly
	origin := (&url.URL{Scheme: scheme, Host: host}).String()
	if baseDomain != "" && (strings.EqualFold(hostOnly, baseDomain) || strings.HasSuffix(strings.ToLower(hostOnly), "."+strings.ToLower(baseDomain))) {
		// Under the household's domain the RP ID is the domain itself, so
		// any app host on it may ask a browser for a fog passkey. Only fog's
		// own page may spend one: the origin is fixed to fog.<domain>, never
		// read from the request, so an assertion a sibling app collected —
		// its clientData names the app's origin — is refused however the
		// request is dressed.
		fog := "fog." + strings.ToLower(baseDomain)
		if !strings.EqualFold(hostOnly, fog) {
			return nil, ErrNotFogsOrigin
		}
		rpID, origin = baseDomain, "https://"+fog
	}
	if displayName == "" {
		displayName = "fog"
	}
	return webauthn.New(&webauthn.Config{
		RPID:          rpID,
		RPDisplayName: displayName,
		RPOrigins:     []string{origin},
	})
}

// Ceremonies keeps the challenge state between the begin and finish halves
// of a WebAuthn ceremony where no stranger can touch it: sealed into the
// ceremony id the browser is handed and echoes back (AES-256-GCM under a key
// this process holds), not in a shared table. A table had to be capped, and
// a stranger beginning enough sign-ins — from an IPv6 /64, or through the
// host's own address — evicted everyone else's challenge mid-ceremony.
// Sealed, there is nothing to evict: a ceremony lives or dies with the
// browser holding it.
//
// Each is bound to its purpose (signing in, adding a passkey, confirming),
// so one can never finish another, and to the person who began it (0 when
// signing in, before anyone is known). Each is good once: finishing records
// it as spent until it would have expired. Only finished ceremonies are
// recorded, and finishing one takes a begin, which is rate limited; a
// restart forgets the key, which only expires what was in flight.
type Ceremonies struct {
	aead cipher.AEAD
	now  func() time.Time

	mu    sync.Mutex // makes "not spent, now spent" one step
	spent *ttlmap.Map[string, struct{}]
}

// CeremonyPurpose says what a ceremony was begun for.
type CeremonyPurpose string

const (
	CeremonyLogin    CeremonyPurpose = "login"
	CeremonyRegister CeremonyPurpose = "register"
	CeremonyConfirm  CeremonyPurpose = "confirm"
)

const ceremonyTTL = 5 * time.Minute

// sealedCeremony is what a ceremony id carries.
type sealedCeremony struct {
	Purpose  CeremonyPurpose      `json:"p"`
	PersonID int64                `json:"u"`
	Expires  int64                `json:"x"`
	Data     webauthn.SessionData `json:"d"`
}

func NewCeremonies() *Ceremonies {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic(err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		panic(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		panic(err)
	}
	return &Ceremonies{aead: aead, spent: ttlmap.New[string, struct{}](ceremonyTTL+time.Minute, 0), now: time.Now}
}

func ceremonyAAD(p CeremonyPurpose) []byte { return []byte("fog-ceremony/1 " + string(p)) }

// Put seals session data for purpose and returns the ceremony id the client
// echoes back with the finish.
func (c *Ceremonies) Put(purpose CeremonyPurpose, data *webauthn.SessionData, personID int64) string {
	plain, err := json.Marshal(sealedCeremony{Purpose: purpose, PersonID: personID, Expires: c.now().Add(ceremonyTTL).Unix(), Data: *data})
	if err != nil {
		panic(err) // SessionData always marshals
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(c.aead.Seal(nonce, nonce, plain, ceremonyAAD(purpose)))
}

// Take opens a ceremony id for purpose: its session data and the person who
// began it, once. A forged, altered, expired, spent or other-purpose id is
// not ok.
func (c *Ceremonies) Take(purpose CeremonyPurpose, id string) (webauthn.SessionData, int64, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(id)
	ns := c.aead.NonceSize()
	if err != nil || len(raw) < ns+c.aead.Overhead() {
		return webauthn.SessionData{}, 0, false
	}
	plain, err := c.aead.Open(nil, raw[:ns], raw[ns:], ceremonyAAD(purpose))
	if err != nil {
		return webauthn.SessionData{}, 0, false
	}
	var sc sealedCeremony
	if json.Unmarshal(plain, &sc) != nil || sc.Purpose != purpose || c.now().Unix() > sc.Expires {
		return webauthn.SessionData{}, 0, false
	}
	key := string(raw[:ns])
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, used := c.spent.Get(key); used {
		return webauthn.SessionData{}, 0, false
	}
	c.spent.Put(key, struct{}{})
	return sc.Data, sc.PersonID, true
}
