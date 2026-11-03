package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"time"

	"gitea.lan/matthew/fog/internal/randx"
)

const (
	// CookieName is the session cookie on a plain-http address (a bare
	// LAN IP, dev), where the browser refuses the __Host- prefix.
	CookieName = "fog_session"
	// SecureCookieName is the session cookie behind Caddy's TLS. The
	// __Host- prefix makes the browser refuse a Domain attribute, so the
	// cookie can only ever be sent back to fog.<domain> itself — never to
	// an app's hostname (docs/signin-contract.md).
	SecureCookieName = "__Host-fog_session"
	// SessionTTL is how long a sign-in lasts: both the session row's expiry
	// and the cookie's Max-Age, so the browser and the database agree.
	SessionTTL = 30 * 24 * time.Hour
)

// NewToken returns an opaque session token and the digest to persist. Only
// the SHA-256 digest is stored; a database leak doesn't leak live sessions.
func NewToken() (token, digest string) {
	token = randx.Token()
	return token, DigestToken(token)
}

func DigestToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
