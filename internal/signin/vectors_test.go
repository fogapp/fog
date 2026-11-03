package signin

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"

	"gitea.lan/matthew/fog/internal/oidc"
	"gitea.lan/matthew/fog/internal/signing"
	"gitea.lan/matthew/fog/internal/store"
	"gitea.lan/matthew/fog/internal/vault"
)

// The cross-language vectors: tokens fog signs, with the verdict every
// relying-party implementation must reach. fog-client's Ruby tests read
// clients/fog-client/test/vectors/signin.json; this test checks the
// checked-in file against go-jose, and FOG_WRITE_VECTORS=1 regenerates it
// from a real Keyring.

const vectorsPath = "../../clients/fog-client/test/vectors/signin.json"

type vector struct {
	Name  string   `json:"name"`
	Token string   `json:"token"`
	Valid bool     `json:"valid"`
	Nonce string   `json:"nonce,omitempty"`
	Sub   string   `json:"sub,omitempty"`
	Sid   string   `json:"sid,omitempty"`
	Roles []string `json:"roles,omitempty"`
	// Visitor: the token is a visitor's (fog_visitor).
	Visitor bool `json:"visitor,omitempty"`
	// SessionExp is the fog_session_exp a valid ID token carries: when the
	// fog session behind it ends, which an app's session must not outlive.
	SessionExp int64 `json:"session_exp,omitempty"`
	// AllBefore is a sessions-revoked logout's "before": every session from a
	// sign-in issued earlier ends.
	AllBefore int64 `json:"all_before,omitempty"`
}

type vectors struct {
	Issuer       string             `json:"issuer"`
	ClientID     string             `json:"client_id"`
	Now          int64              `json:"now"`
	JWKS         jose.JSONWebKeySet `json:"jwks"`
	IDTokens     []vector           `json:"id_tokens"`
	LogoutTokens []vector           `json:"logout_tokens"`
	// Proofs are /app/v1/signin answers: an app is provably on fog.
	Proofs []vector `json:"proofs"`
}

func TestWriteSigninVectors(t *testing.T) {
	if os.Getenv("FOG_WRITE_VECTORS") == "" {
		t.Skip("set FOG_WRITE_VECTORS=1 to regenerate")
	}
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	vlt, err := vault.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	keys := &signing.Keyring{Store: st, Vault: vlt, Now: func() time.Time { return now }}
	const iss, client = "https://fog.home.test", "stream"
	subject := oidc.Subject{Address: "person:01J9HOUSEHOLD0000000000000/01J9PERSON00000000000000000", Home: "01J9HOUSEHOLD0000000000000", Handle: "kim", Name: "Kim"}
	visitor := oidc.Subject{Address: "person:01J9AWAY000000000000000000/01J9DAVE00000000000000000", Home: "01J9AWAY000000000000000000",
		Handle: "dave", Name: "Dave", Visitor: true}
	grant := func(nonce string) oidc.Grant {
		return oidc.Grant{Request: oidc.AuthorizeRequest{ClientID: client, Scope: []string{"openid", "profile", "roles"}, Nonce: nonce},
			Role: "member", SID: "sid-1", AMR: []string{"pop", "user"}, ACR: oidc.ACRPhishingResistant, AuthTime: now}
	}
	sign := func(c map[string]any, typ string) string {
		tok, err := keys.Sign(c, typ)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	// A person's fog session began a day ago and runs 30; a visitor's
	// began now and runs 12 hours.
	personExp, visitorExp := now.Add(29*24*time.Hour), now.Add(12*time.Hour)
	idt := func(mut func(map[string]any)) string {
		c := oidc.IDToken(iss, subject, grant("n-1"), "", personExp, now)
		if mut != nil {
			mut(c)
		}
		return sign(c, "JWT")
	}
	good := idt(nil)
	parts := strings.Split(good, ".")
	tampered := parts[0] + "." + parts[1] + "." + flip(parts[2])

	v := vectors{Issuer: iss, ClientID: client, Now: now.Unix()}
	v.IDTokens = []vector{
		{Name: "valid", Token: good, Valid: true, Nonce: "n-1", Sub: subject.Address, Roles: []string{"member"}, SessionExp: personExp.Unix()},
		{Name: "wrong nonce", Token: good, Nonce: "n-2"},
		{Name: "tampered signature", Token: tampered, Nonce: "n-1"},
		{Name: "other audience", Token: idt(func(c map[string]any) { c["aud"] = "hub"; c["azp"] = "hub" }), Nonce: "n-1"},
		{Name: "other issuer", Token: idt(func(c map[string]any) { c["iss"] = "https://fog.elsewhere.test" }), Nonce: "n-1"},
		{Name: "expired", Token: idt(func(c map[string]any) { c["exp"] = now.Add(-10 * time.Minute).Unix() }), Nonce: "n-1"},
		{Name: "issued in the future", Token: idt(func(c map[string]any) { c["iat"] = now.Add(10 * time.Minute).Unix() }), Nonce: "n-1"},
		{Name: "alg none", Token: algNone(parts[1]), Nonce: "n-1"},
		{Name: "two audiences", Token: idt(func(c map[string]any) { c["aud"] = []string{client, "hub"} }), Nonce: "n-1"},
		{Name: "other authorized party", Token: idt(func(c map[string]any) { c["azp"] = "hub" }), Nonce: "n-1"},
		{Name: "typed as a logout token", Token: sign(oidc.IDToken(iss, subject, grant("n-1"), "", personExp, now), "logout+jwt"), Nonce: "n-1"},
		{Name: "unknown key", Token: rekid(good), Nonce: "n-1"},
		{Name: "no nonce when one was sent", Token: sign(oidc.IDToken(iss, subject, grant(""), "", personExp, now), "JWT"), Nonce: "n-1"},
		{Name: "visitor", Token: sign(oidc.IDToken(iss, visitor, grant("n-1"), "", visitorExp, now), "JWT"), Valid: true, Nonce: "n-1",
			Sub: visitor.Address, Roles: []string{"member"}, Visitor: true, SessionExp: visitorExp.Unix()},
		// Another provider, or an older fog: no fog_session_exp at all. Still
		// valid; the relying party caps the session itself.
		{Name: "no session expiry", Token: idt(func(c map[string]any) { delete(c, oidc.ClaimSessionExpires) }), Valid: true, Nonce: "n-1",
			Sub: subject.Address, Roles: []string{"member"}},
	}
	v.LogoutTokens = []vector{
		{Name: "valid", Token: sign(oidc.LogoutToken(iss, client, subject.Address, "sid-1", "jti-1", now), "logout+jwt"), Valid: true, Sid: "sid-1"},
		{Name: "carries a nonce", Token: sign(with(oidc.LogoutToken(iss, client, "", "sid-2", "jti-2", now), "nonce", "x"), "logout+jwt")},
		{Name: "no event", Token: sign(with(oidc.LogoutToken(iss, client, "", "sid-3", "jti-3", now), "events", map[string]any{}), "logout+jwt")},
		{Name: "other audience", Token: sign(oidc.LogoutToken(iss, "hub", "", "sid-4", "jti-4", now), "logout+jwt")},
		{Name: "stale", Token: sign(oidc.LogoutToken(iss, client, "", "sid-5", "jti-5", now.Add(-time.Hour)), "logout+jwt")},
		{Name: "subject only", Token: sign(oidc.LogoutToken(iss, client, subject.Address, "", "jti-6", now), "logout+jwt"), Valid: true, Sub: subject.Address},
		{Name: "typed as an ID token", Token: sign(oidc.LogoutToken(iss, client, "", "sid-7", "jti-7", now), "JWT")},
		{Name: "every session before a key rotation", Token: sign(oidc.LogoutAllToken(iss, client, "jti-8", now, now), "logout+jwt"),
			Valid: true, Sid: oidc.AllSessionsSID, AllBefore: now.Unix()},
		{Name: "sessions revoked, but naming a subject", Token: sign(with(oidc.LogoutAllToken(iss, client, "jti-9", now, now), "sub", subject.Address), "logout+jwt")},
	}
	proof := func(clientID string, at time.Time, typ string) string {
		return sign(map[string]any{"issuer": iss, "client_id": clientID, "app": clientID, "iat": at.Unix()}, typ)
	}
	v.Proofs = []vector{
		{Name: "valid", Token: proof(client, now, "fog-signin-proof+jwt"), Valid: true},
		{Name: "another app's", Token: proof("hub", now, "fog-signin-proof+jwt")},
		{Name: "stale", Token: proof(client, now.Add(-time.Hour), "fog-signin-proof+jwt")},
		{Name: "typed as an ID token", Token: proof(client, now, "JWT")},
	}
	if v.JWKS, err = keys.JWKS(); err != nil {
		t.Fatal(err)
	}
	b, _ := json.MarshalIndent(v, "", "  ")
	if err := os.MkdirAll(filepath.Dir(vectorsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(vectorsPath, append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestSigninVectorsVerifyInGo holds the checked-in file to go-jose: what Go
// says verifies must be what Ruby says verifies.
func TestSigninVectorsVerifyInGo(t *testing.T) {
	raw, err := os.ReadFile(vectorsPath)
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	for _, x := range v.IDTokens {
		ok := verifies(v, x.Token, "JWT", func(c map[string]any) bool {
			if exp, has := c[oidc.ClaimSessionExpires].(float64); x.Valid && x.SessionExp != 0 && (!has || int64(exp) != x.SessionExp) {
				return false
			}
			azp, hasAzp := c["azp"]
			return c["iss"] == v.Issuer && c["aud"] == v.ClientID && (!hasAzp || azp == v.ClientID) && c["nonce"] == x.Nonce &&
				int64(c["exp"].(float64)) > v.Now && int64(c["iat"].(float64)) <= v.Now+60
		})
		if ok != x.Valid {
			t.Errorf("id token %q: Go says %v, the vector says %v", x.Name, ok, x.Valid)
		}
	}
	for _, x := range v.LogoutTokens {
		ok := verifies(v, x.Token, "logout+jwt", func(c map[string]any) bool {
			ev, _ := c["events"].(map[string]any)
			_, hasEvent := ev["http://schemas.openid.net/event/backchannel-logout"]
			_, hasNonce := c["nonce"]
			iat := int64(c["iat"].(float64))
			if all, revoked := ev[oidc.EventSessionsRevoked].(map[string]any); revoked {
				_, hasSub := c["sub"]
				before, _ := all["before"].(float64)
				if c["sid"] != oidc.AllSessionsSID || hasSub || int64(before) != x.AllBefore {
					return false
				}
			}
			return c["iss"] == v.Issuer && c["aud"] == v.ClientID && hasEvent && !hasNonce && iat > v.Now-300 && iat < v.Now+300
		})
		if ok != x.Valid {
			t.Errorf("logout token %q: Go says %v, the vector says %v", x.Name, ok, x.Valid)
		}
	}
	for _, x := range v.Proofs {
		ok := verifies(v, x.Token, "fog-signin-proof+jwt", func(c map[string]any) bool {
			iat := int64(c["iat"].(float64))
			return c["issuer"] == v.Issuer && c["client_id"] == v.ClientID && iat > v.Now-300 && iat < v.Now+300
		})
		if ok != x.Valid {
			t.Errorf("proof %q: Go says %v, the vector says %v", x.Name, ok, x.Valid)
		}
	}
	if len(v.Proofs) == 0 {
		t.Error("the vectors carry no proofs")
	}
}

func verifies(v vectors, token, typ string, claims func(map[string]any) bool) bool {
	obj, err := jose.ParseSigned(token, []jose.SignatureAlgorithm{jose.ES256})
	if err != nil || len(obj.Signatures) != 1 {
		return false
	}
	if got, _ := obj.Signatures[0].Protected.ExtraHeaders[jose.HeaderType].(string); !strings.EqualFold(got, typ) {
		return false
	}
	keys := v.JWKS.Key(obj.Signatures[0].Header.KeyID)
	if len(keys) != 1 {
		return false
	}
	payload, err := obj.Verify(keys[0].Key)
	if err != nil {
		return false
	}
	var c map[string]any
	return json.Unmarshal(payload, &c) == nil && claims(c)
}

func with(c map[string]any, k string, v any) map[string]any { c[k] = v; return c }

func flip(sig string) string {
	b := []byte(sig)
	if b[3] == 'A' {
		b[3] = 'B'
	} else {
		b[3] = 'A'
	}
	return string(b)
}

func algNone(payload string) string {
	return "eyJhbGciOiJub25lIn0." + payload + "."
}

// rekid re-signs nothing: it names a key the set does not hold, which a
// verifier must refuse before trying any signature.
func rekid(token string) string {
	parts := strings.Split(token, ".")
	h, _ := base64.RawURLEncoding.DecodeString(parts[0])
	var m map[string]any
	json.Unmarshal(h, &m)
	m["kid"] = "not-a-key-fog-holds"
	nh, _ := json.Marshal(m)
	return base64.RawURLEncoding.EncodeToString(nh) + "." + parts[1] + "." + parts[2]
}
