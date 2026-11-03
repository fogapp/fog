package oidc

import (
	"crypto/sha256"
	"encoding/base64"
	"slices"
	"time"
)

// Lifetimes.
const (
	CodeTTL        = time.Minute
	PARTTL         = 5 * time.Minute // long enough to sign in in the middle
	IDTokenTTL     = 5 * time.Minute
	AccessTokenTTL = 10 * time.Minute
)

// Subject is everything the claims say about the person.
type Subject struct {
	Address     string // sub: person:<instance>/<uid>
	Home        string // fog_home: the instance ULID
	Handle      string
	Name        string
	Email       string
	EmailVerify bool
	// Visitor: a paired household's person, signed in by their own fog
	// (docs/signin-contract.md). Claimed as fog_visitor, so an app
	// never has to infer it from addresses.
	Visitor bool
}

// Grant is what one authorization decided.
type Grant struct {
	Request  AuthorizeRequest
	Role     string
	SID      string
	AMR      []string
	ACR      string
	AuthTime time.Time
	// VisitorSessionID marks a visitor's grant (docs/signin-contract.md): the
	// session is a visitor_sessions row, not a person's.
	VisitorSessionID int64 `json:",omitempty"`
}

// ClaimSessionExpires is when the fog session behind a sign-in ends, in
// unix seconds: a person's fog session (30 days from sign-in, never
// sliding), or a visitor's (12 hours, and never past their home's). An app
// ends its own session no later (docs/signin-contract.md); fog also sends
// a back-channel logout then, for apps that keep sessions longer.
const ClaimSessionExpires = "fog_session_exp"

// IDToken builds the ID token's claims (OIDC Core §2, §5.1). Claims for a
// scope the client did not ask for are left out. sessionExp is when the fog
// session behind the grant ends.
func IDToken(issuer string, s Subject, g Grant, accessToken string, sessionExp, now time.Time) map[string]any {
	c := map[string]any{
		"iss":       issuer,
		"sub":       s.Address,
		"aud":       g.Request.ClientID,
		"azp":       g.Request.ClientID,
		"iat":       now.Unix(),
		"exp":       now.Add(IDTokenTTL).Unix(),
		"auth_time": g.AuthTime.Unix(),
		"sid":       g.SID,
		"acr":       g.ACR,
		"amr":       nonNil(g.AMR),
		"fog_home":  s.Home,
	}
	if !sessionExp.IsZero() {
		c[ClaimSessionExpires] = sessionExp.Unix()
	}
	if g.Request.Nonce != "" {
		c["nonce"] = g.Request.Nonce
	}
	if accessToken != "" {
		c["at_hash"] = halfHash(accessToken)
	}
	for k, v := range UserInfo(s, g) {
		c[k] = v
	}
	return c
}

// UserInfo is the scope-filtered profile, as /oauth2/userinfo serves it and
// the ID token carries it.
func UserInfo(s Subject, g Grant) map[string]any {
	c := map[string]any{"sub": s.Address}
	if s.Visitor {
		c["fog_visitor"] = true
		c["fog_home"] = s.Home
	}
	if slices.Contains(g.Request.Scope, ScopeProfile) {
		c["preferred_username"] = s.Handle
		c["name"] = s.Name
	}
	if slices.Contains(g.Request.Scope, ScopeEmail) && s.Email != "" {
		c["email"] = s.Email
		c["email_verified"] = s.EmailVerify
	}
	if slices.Contains(g.Request.Scope, ScopeRoles) {
		c["roles"] = []string{g.Role}
	}
	return c
}

// halfHash is at_hash: the left half of SHA-256 (ES256's hash), base64url.
func halfHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(sum[:len(sum)/2])
}

func nonNil(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// LogoutToken builds a back-channel logout token's claims (OIDC Back-Channel
// Logout 1.0 §2.4). One of sid or sub at least; never a nonce.
func LogoutToken(issuer, clientID, sub, sid, jti string, now time.Time) map[string]any {
	c := map[string]any{
		"iss":    issuer,
		"aud":    clientID,
		"iat":    now.Unix(),
		"exp":    now.Add(2 * time.Minute).Unix(),
		"jti":    jti,
		"events": map[string]any{"http://schemas.openid.net/event/backchannel-logout": map[string]any{}},
	}
	if sub != "" {
		c["sub"] = sub
	}
	if sid != "" {
		c["sid"] = sid
	}
	return c
}

// EventSessionsRevoked is fog's own logout-token event: every session the app
// started from a sign-in fog issued before its "before" (unix seconds) must
// end. fog sends it after an emergency key rotation, when a leaked key could
// have minted an ID token for anyone — a subject no app has seen, a visitor
// from anywhere — so ending sessions by sub or sid can't reach them all.
const EventSessionsRevoked = "urn:fog:event:sessions-revoked"

// AllSessionsSID is the sid such a token names: Back-Channel Logout wants a
// sub or a sid, and no session carries this one, so a relying party that
// doesn't know the event ends nothing and still answers 200.
const AllSessionsSID = "urn:fog:sid:all"

// LogoutAllToken builds the claims of a logout token that ends every session
// started from a sign-in issued before before.
func LogoutAllToken(issuer, clientID, jti string, before, now time.Time) map[string]any {
	c := LogoutToken(issuer, clientID, "", AllSessionsSID, jti, now)
	c["events"] = map[string]any{
		"http://schemas.openid.net/event/backchannel-logout": map[string]any{},
		EventSessionsRevoked: map[string]any{"before": before.Unix()},
	}
	return c
}
