package oidc

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

var stream = &Client{
	ID:           "stream",
	RedirectURIs: []string{"https://stream.home.test/auth/oidc/callback"},
	Scopes:       DefaultScopes,
}

const verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"

func good() url.Values {
	return url.Values{
		"client_id":             {"stream"},
		"redirect_uri":          {"https://stream.home.test/auth/oidc/callback"},
		"response_type":         {"code"},
		"scope":                 {"openid profile"},
		"state":                 {"s"},
		"nonce":                 {"n"},
		"code_challenge":        {ChallengeFor(verifier)},
		"code_challenge_method": {"S256"},
	}
}

func with(k, v string) url.Values {
	q := good()
	if v == "" {
		q.Del(k)
	} else {
		q.Set(k, v)
	}
	return q
}

func TestParseAuthorizeAcceptsTheProfile(t *testing.T) {
	req, err := ParseAuthorize(good(), stream, false)
	if err != nil {
		t.Fatal(err)
	}
	if req.ClientID != "stream" || req.Nonce != "n" || strings.Join(req.Scope, " ") != "openid profile" {
		t.Fatalf("req = %+v", req)
	}
}

func TestParseAuthorizeRefusals(t *testing.T) {
	par := *stream
	par.RequirePAR = true
	cases := []struct {
		name         string
		q            url.Values
		c            *Client
		code         Code
		redirectSafe bool
	}{
		{"unknown client", good(), nil, ErrInvalidClient, false},
		{"client_id mismatch", with("client_id", "hub"), stream, ErrInvalidRequest, false},
		{"unregistered redirect", with("redirect_uri", "https://evil.example/cb"), stream, ErrInvalidRequest, false},
		{"redirect differs by a slash", with("redirect_uri", "https://stream.home.test/auth/oidc/callback/"), stream, ErrInvalidRequest, false},
		{"redirect differs by case", with("redirect_uri", "https://Stream.home.test/auth/oidc/callback"), stream, ErrInvalidRequest, false},
		{"no redirect", with("redirect_uri", ""), stream, ErrInvalidRequest, false},
		{"implicit", with("response_type", "id_token"), stream, ErrUnsupportedResponseType, true},
		{"hybrid", with("response_type", "code id_token"), stream, ErrUnsupportedResponseType, true},
		{"fragment mode", with("response_mode", "fragment"), stream, ErrInvalidRequest, true},
		{"no openid", with("scope", "profile"), stream, ErrInvalidScope, true},
		{"unregistered scope", with("scope", "openid email"), stream, ErrInvalidScope, true},
		{"unknown scope", with("scope", "openid offline_access"), stream, ErrInvalidScope, true},
		{"no PKCE", with("code_challenge", ""), stream, ErrInvalidRequest, true},
		{"plain PKCE", with("code_challenge_method", "plain"), stream, ErrInvalidRequest, true},
		{"short challenge", with("code_challenge", "abc"), stream, ErrInvalidRequest, true},
		{"prompt consent", with("prompt", "consent"), stream, ErrInvalidRequest, true},
		{"prompt pair", with("prompt", "login consent"), stream, ErrInvalidRequest, true},
		{"negative max_age", with("max_age", "-1"), stream, ErrInvalidRequest, true},
		{"request object", with("request", "eyJ"), stream, ErrInvalidRequest, true},
		{"PAR required", good(), &par, ErrInvalidRequest, true},
		{"repeated parameter", func() url.Values { q := good(); q.Add("scope", "openid"); return q }(), stream, ErrInvalidRequest, false},
	}
	for _, c := range cases {
		_, err := ParseAuthorize(c.q, c.c, false)
		if err == nil {
			t.Errorf("%s: accepted", c.name)
			continue
		}
		if err.Code != c.code || err.RedirectSafe != c.redirectSafe {
			t.Errorf("%s: got %s (redirectSafe=%v), want %s (redirectSafe=%v)", c.name, err.Code, err.RedirectSafe, c.code, c.redirectSafe)
		}
	}
}

func TestParseAuthorizeOptionalParameters(t *testing.T) {
	q := good()
	q.Set("prompt", "none")
	q.Set("max_age", "0")
	q.Set("acr_values", "urn:example:unknown "+ACRPhishingResistant)
	req, err := ParseAuthorize(q, stream, false)
	if err != nil {
		t.Fatal(err)
	}
	if req.Prompt != "none" || req.MaxAge == nil || *req.MaxAge != 0 || req.ACR != ACRPhishingResistant {
		t.Fatalf("req = %+v", req)
	}
}

func TestPKCE(t *testing.T) {
	// RFC 7636 Appendix B.
	if got := ChallengeFor(verifier); got != "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" {
		t.Fatalf("challenge = %s", got)
	}
	if !VerifyPKCE("E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM", verifier) {
		t.Fatal("RFC vector does not verify")
	}
	for _, bad := range []string{"", "short", verifier + "x", strings.Repeat("a", 129), verifier[:42] + "!"} {
		if VerifyPKCE(ChallengeFor(verifier), bad) {
			t.Errorf("verifier %q accepted", bad)
		}
	}
}

func TestCheckRedemption(t *testing.T) {
	a, _ := ParseAuthorize(good(), stream, false)
	tr := TokenRequest{Code: "c", RedirectURI: a.RedirectURI, CodeVerifier: verifier}
	if err := CheckRedemption(tr, "stream", a); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*TokenRequest) string{
		"other client":   func(*TokenRequest) string { return "hub" },
		"other redirect": func(t *TokenRequest) string { t.RedirectURI += "x"; return "stream" },
		"wrong verifier": func(t *TokenRequest) string { t.CodeVerifier = strings.Repeat("b", 43); return "stream" },
	} {
		tr := tr
		client := mutate(&tr)
		if err := CheckRedemption(tr, client, a); err == nil || err.Code != ErrInvalidGrant {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestClaimsFollowScope(t *testing.T) {
	s := Subject{Address: "person:I/U", Home: "I", Handle: "kim", Name: "Kim", Email: "kim@example.com"}
	now := time.Unix(1_800_000_000, 0)
	g := Grant{Request: AuthorizeRequest{ClientID: "stream", Scope: []string{"openid"}, Nonce: "n"}, Role: "member", SID: "sid1", AuthTime: now}
	c := IDToken("https://fog.home.test", s, g, "at", time.Time{}, now)
	for _, k := range []string{"preferred_username", "email", "roles"} {
		if _, ok := c[k]; ok {
			t.Errorf("openid alone released %s", k)
		}
	}
	if c["nonce"] != "n" || c["aud"] != "stream" || c["sid"] != "sid1" || c["at_hash"] == nil {
		t.Errorf("claims = %v", c)
	}
	g.Request.Scope = []string{"openid", "profile", "roles"}
	c = IDToken("https://fog.home.test", s, g, "", time.Time{}, now)
	if c["preferred_username"] != "kim" || c["roles"].([]string)[0] != "member" {
		t.Errorf("claims = %v", c)
	}
	if _, ok := c["email"]; ok {
		t.Error("email released without the email scope")
	}
}

// TestIDTokenCarriesTheSessionExpiry: an app learns when fog's session
// ends, so it can end its own no later.
func TestIDTokenCarriesTheSessionExpiry(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	exp := now.Add(29 * 24 * time.Hour)
	g := Grant{Request: AuthorizeRequest{ClientID: "stream", Scope: []string{"openid"}}, AuthTime: now}
	c := IDToken("https://fog.home.test", Subject{Address: "person:I/U"}, g, "", exp, now)
	if c[ClaimSessionExpires] != exp.Unix() {
		t.Fatalf("%s = %v, want %d", ClaimSessionExpires, c[ClaimSessionExpires], exp.Unix())
	}
}

// TestDiscoveryClaimsAreWhatIsIssued: claims_supported lists every claim an
// ID token or userinfo answer can carry, and nothing they never do — the
// fullest subject, every scope, a visitor and a person alike.
func TestDiscoveryClaimsAreWhatIsIssued(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	issued := map[string]bool{}
	for _, visitor := range []bool{false, true} {
		s := Subject{Address: "person:I/U", Home: "I", Handle: "kim", Name: "Kim", Email: "kim@example.com", Visitor: visitor}
		g := Grant{Request: AuthorizeRequest{ClientID: "stream", Scope: []string{"openid", "profile", "email", "roles"}, Nonce: "n"},
			Role: "member", SID: "sid", AMR: []string{"pwd"}, ACR: ACRAny, AuthTime: now}
		for k := range IDToken("https://fog.home.test", s, g, "at", now.Add(time.Hour), now) {
			issued[k] = true
		}
		for k := range UserInfo(s, g) {
			issued[k] = true
		}
	}
	listed := map[string]bool{}
	for _, k := range Discovery("https://fog.home.test")["claims_supported"].([]string) {
		listed[k] = true
		if !issued[k] {
			t.Errorf("claims_supported lists %q, which fog never issues", k)
		}
	}
	for k := range issued {
		if !listed[k] {
			t.Errorf("fog issues %q, which claims_supported omits", k)
		}
	}
}

func TestRedirectKeepsQueryAndAddsIss(t *testing.T) {
	got := Redirect("https://a.test/cb?x=1", "https://fog.test", "st", url.Values{"code": {"c"}})
	u, _ := url.Parse(got)
	if u.Query().Get("x") != "1" || u.Query().Get("code") != "c" || u.Query().Get("state") != "st" || u.Query().Get("iss") != "https://fog.test" {
		t.Fatalf("redirect = %s", got)
	}
}

// FuzzParseAuthorize: whatever arrives, the parser never panics, and never
// lets a request through — or an error be redirected — to a URI the client
// did not register.
func FuzzParseAuthorize(f *testing.F) {
	f.Add("stream", "https://stream.home.test/auth/oidc/callback", "code", "openid profile", "S256", ChallengeFor(verifier), "none", "0")
	f.Add("stream", "https://evil.test/", "code id_token", "openid", "plain", "x", "consent", "-1")
	f.Fuzz(func(t *testing.T, client, redirect, rt, scope, method, challenge, prompt, maxAge string) {
		q := url.Values{
			"client_id": {client}, "redirect_uri": {redirect}, "response_type": {rt}, "scope": {scope},
			"code_challenge_method": {method}, "code_challenge": {challenge}, "prompt": {prompt}, "max_age": {maxAge},
		}
		req, err := ParseAuthorize(q, stream, false)
		registered := stream.AllowsRedirect(redirect) && client == "stream"
		if err == nil && !registered {
			t.Fatalf("accepted an unregistered redirect %q", redirect)
		}
		if err != nil && err.RedirectSafe && !registered {
			t.Fatalf("would redirect an error to %q", redirect)
		}
		if err == nil && (req.CodeChallenge == "" || len(req.Scope) == 0) {
			t.Fatal("accepted without PKCE or scope")
		}
	})
}
