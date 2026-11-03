// The gate: what stands in front of an app route marked auth: true, for
// apps that cannot sign people in themselves (docs/signin-contract.md).
//
// fog's own session cookie never leaves fog.<domain>, so a gated host gets
// a cookie of its own, the way oauth2-proxy and Cloudflare Access do it:
//
//  1. Caddy's forward_auth asks /api/authgate about every request to the
//     host. No gate cookie → a redirect to fog.<domain>/gate/authorize,
//     and a random binding value in __Host-fog_gate_bind on the host, which
//     no sibling host can set; the redirect carries only its digest.
//  2. /gate/authorize reads fog's session, asks internal/identity whether
//     this person may use this app, and sends the browser back to
//     https://<host>/.fog/gate/callback with a single-use code bound to
//     the host and to that digest.
//  3. Caddy routes /.fog/gate/* on the host to fogd, which redeems the code
//     only alongside the binding cookie — a callback link handed to another
//     browser signs nobody in (login CSRF) — and sets __Host-fog_gate,
//     host-only, so no other app ever sees it.
//  4. From then on /api/authgate resolves the cookie, re-checks the policy,
//     and answers 204 with who passed: Remote-* headers for apps that read
//     the Authelia convention, and Fog-Assertion, a 60-second ES256 JWT any
//     app can verify against /oauth2/jwks.
//
// Caddy deletes client-supplied Remote-*/Fog-* headers before any of this
// and strips fog's cookies from what reaches the app (internal/proxy).
package server

import (
	"crypto/subtle"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/auth"
	"gitea.lan/matthew/fog/internal/identity"
	"gitea.lan/matthew/fog/internal/oidc"
	"gitea.lan/matthew/fog/internal/proxy"
	"gitea.lan/matthew/fog/internal/randx"
	"gitea.lan/matthew/fog/internal/store"
)

const (
	// gateCodeTTL bounds the redirect leg: long enough for a slow phone,
	// short enough that a code in a log is worthless by the time it is read.
	gateCodeTTL = time.Minute
	// assertionTTL is how long a Fog-Assertion verifies: one request, with
	// room for clock skew between fogd and the app.
	assertionTTL = time.Minute
	// gatePrefix is the path Caddy hands to fogd on every gated host.
	gatePrefix = "/.fog/gate/"
)

// gateCookieName is the gate cookie on an https host, and a plain name for
// the http-only LAN setup where the __Host- prefix cannot be set.
func gateCookieName(secure bool) string {
	if secure {
		return "__Host-fog_gate"
	}
	return "fog_gate"
}

// gateBindTTL is how long a handshake's binding cookie lives: the time to
// sign in to fog on the way, with room to spare.
const gateBindTTL = 10 * time.Minute

// gateBindCookie names the browser's handshake binding on the gated host.
// On https it is __Host-fog_gate_bind: the prefix is the browser's promise
// that this host set it — never a sibling host, which may set an ordinary
// cookie for the whole domain — and it demands Path=/, so the app on the
// host sees it too (it is a binding, not a credential: knowing it signs no
// one in). One binding per browser, reused while it lasts, so handshakes in
// two tabs share it. Plain http (a LAN address) has no prefix to give, so
// there a sibling could plant fog_gate_bind; the contract says so.
func gateBindCookie(secure bool) string {
	if secure {
		return "__Host-fog_gate_bind"
	}
	return "fog_gate_bind"
}

// gateApps maps every gated hostname to its app. Read on every gated
// request, so it is computed at most every few seconds.
func (s *Server) gateApps() map[string]int64 {
	return s.gateHosts.get(15*time.Second, func() map[string]int64 {
		out := map[string]int64{}
		domain, _ := s.store.GetSetting(store.SettingBaseDomain)
		list, err := apps.WithSpecs(s.store)
		if err != nil {
			return out
		}
		for _, a := range list {
			spec := a.Spec
			if spec == nil {
				continue
			}
			for _, rt := range spec.Routes {
				if !rt.Auth {
					continue
				}
				if h := apps.RouteHost(a.Name, rt, domain); h != "" {
					out[strings.ToLower(h)] = a.ID
				}
			}
		}
		return out
	})
}

func (s *Server) gateApp(host string) (int64, bool) {
	id, ok := s.gateApps()[strings.ToLower(stripPort(host))]
	return id, ok
}

func stripPort(host string) string {
	if i := strings.LastIndexByte(host, ':'); i > 0 && !strings.Contains(host[i:], "]") {
		return host[:i]
	}
	return host
}

// decideFor is the policy question for one person, one app, one session.
// client carries an OIDC app's vocabulary and assurance; nil for the gate,
// whose apps have neither.
func (s *Server) decideFor(appID int64, p *store.Person, sess *store.PersonSession, req identity.Request, client *oidc.Client) (identity.Decision, error) {
	pol, err := s.store.SigninPolicyFor(appID)
	if err != nil {
		return identity.Decision{}, err
	}
	policy := identity.Policy{Audience: pol.Audience, People: pol.People}
	if client != nil {
		policy.RoleMap, policy.Roles, policy.ACR = client.RoleMap, client.Roles, client.ACR
	}
	return identity.Decide(
		identity.Person{ID: p.ID, Role: p.Role, Status: p.Status},
		policy,
		identity.Assurance{AMR: sess.AMR, AuthTime: sess.AuthTime},
		req, time.Now(),
	), nil
}

// ---- step 1 and 4: forward_auth ----

// handleAuthGate backs Caddy's forward_auth for gated routes. 204 lets the
// request through with the identity headers; a browser without a gate
// session is sent to fog to get one.
func (s *Server) handleAuthGate(w http.ResponseWriter, r *http.Request) {
	host := stripPort(r.Header.Get("X-Forwarded-Host"))
	appID, ok := s.gateApp(host)
	if !ok {
		// Not a gated host: Caddy only asks about hosts it gates, so this
		// is a stale route table or a request that did not come from Caddy.
		forbidden(w, "this host is not gated by fog")
		return
	}
	secure := forwardedSecure(r)
	if c, err := r.Cookie(gateCookieName(secure)); err == nil && c.Value != "" {
		sess, p, err := s.store.GateSessionPerson(auth.DigestToken(c.Value), host)
		if err == nil {
			d, err := s.decideFor(appID, p, sess, identity.Request{}, nil)
			if err != nil {
				writeError(w, err)
				return
			}
			if d.Outcome == identity.Allow {
				if err := s.writeGateIdentity(w, host, p, sess, d); err != nil {
					writeError(w, err)
					return
				}
				w.WriteHeader(http.StatusNoContent)
				return
			}
			// Signed in, and no longer allowed: the operator narrowed the
			// app's audience since. Say so rather than loop through fog.
			s.renderGateRefusal(w, host, d)
			return
		}
	}
	if m := r.Header.Get("X-Forwarded-Method"); m != "" && m != http.MethodGet && m != http.MethodHead {
		unauthorized(w)
		return
	}
	domain, _ := s.store.GetSetting(store.SettingBaseDomain)
	if domain == "" {
		unauthorized(w)
		return
	}
	scheme := "http"
	if secure {
		scheme = "https"
	}
	orig := scheme + "://" + host + r.Header.Get("X-Forwarded-Uri")
	// Bind the handshake to this browser: Caddy hands this response, cookie
	// and all, back to it on the gated host.
	bind := randx.Token()
	if c, err := r.Cookie(gateBindCookie(secure)); err == nil && len(c.Value) >= 32 && len(c.Value) <= 128 {
		bind = c.Value // a handshake already under way in another tab
	}
	digest := auth.DigestToken(bind)
	http.SetCookie(w, &http.Cookie{Name: gateBindCookie(secure), Value: bind, Path: "/", HttpOnly: true, Secure: secure,
		SameSite: http.SameSiteLaxMode, MaxAge: int(gateBindTTL.Seconds())})
	w.Header().Set("Cache-Control", "no-store")
	q := url.Values{"host": {host}, "next": {orig}, "bind": {digest}}
	http.Redirect(w, r, scheme+"://fog."+domain+"/gate/authorize?"+q.Encode(), http.StatusFound)
}

// writeGateIdentity sets who passed the gate on the 204 Caddy copies onto
// the proxied request.
func (s *Server) writeGateIdentity(w http.ResponseWriter, host string, p *store.Person, sess *store.PersonSession, d identity.Decision) error {
	assertion, err := s.gateAssertion(host, p, sess, d)
	if err != nil {
		return err
	}
	h := w.Header()
	h.Set("Remote-User", p.Handle)
	h.Set("Remote-Name", headerSafe(displayOr(p)))
	h.Set("Remote-Groups", d.Role)
	h.Set("Fog-Assertion", assertion)
	h.Set("Cache-Control", "no-store")
	return nil
}

func displayOr(p *store.Person) string {
	if p.DisplayName != "" {
		return p.DisplayName
	}
	return p.Handle
}

// headerSafe drops what cannot ride in a header value.
func headerSafe(v string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, v)
}

// gateScheme is the scheme everything the gate signs is named with: https
// when Caddy serves the household's hosts over TLS (it does exactly when a
// DNS token is set, internal/proxy), http when it serves plain http. One
// fact about the setup, not the request's transport — a tunnel can carry
// https to Caddy's http — so what fog signs, what /app/v1/gate advertises,
// and what fog and fog-client accept are always the same strings.
func (s *Server) gateScheme() string {
	return s.gateSchemeCache.get(15*time.Second, func() string {
		if tok, err := s.secrets().Get(0, proxy.SettingDNSToken); err == nil && tok != "" {
			return "https"
		}
		return "http"
	})
}

// gateIssuer is the iss of every gate assertion: scheme://fog.<domain>.
func (s *Server) gateIssuer() string {
	domain, _ := s.store.GetSetting(store.SettingBaseDomain)
	return s.gateScheme() + "://fog." + domain
}

// gateAudience is the aud an assertion for host carries.
func (s *Server) gateAudience(host string) string {
	return s.gateScheme() + "://" + strings.ToLower(host)
}

// gateAudiences are the auds meant for one app: its own gated hosts.
func (s *Server) gateAudiences(appID int64) []string {
	out := []string{}
	for host, id := range s.gateApps() {
		if id == appID {
			out = append(out, s.gateAudience(host))
		}
	}
	slices.Sort(out)
	return out
}

// gateAssertion is the signed statement of who passed the gate, for this
// host, for one minute.
func (s *Server) gateAssertion(host string, p *store.Person, sess *store.PersonSession, d identity.Decision) (string, error) {
	id, err := s.fed.Identity()
	if err != nil {
		return "", err
	}
	now := time.Now()
	return s.keys.Sign(map[string]any{
		"iss":                s.gateIssuer(),
		"aud":                s.gateAudience(host),
		"sub":                identity.Address(id.InstanceID, p.UID),
		"preferred_username": p.Handle,
		"name":               displayOr(p),
		"roles":              []string{d.Role},
		"fog_home":           id.InstanceID,
		"sid":                sess.SID,
		"amr":                nonNilStrings(sess.AMR),
		"acr":                identity.Level(sess.AMR),
		"iat":                now.Unix(),
		"exp":                now.Add(assertionTTL).Unix(),
	}, "JWT")
}

func nonNilStrings(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// ---- step 2: fog decides ----

// handleGateAuthorize runs on fog.<domain>: the one place fog's session
// cookie is readable. It decides, and hands the gated host a code.
func (s *Server) handleGateAuthorize(w http.ResponseWriter, r *http.Request) {
	host := stripPort(r.URL.Query().Get("host"))
	appID, ok := s.gateApp(host)
	if !ok {
		badRequest(w, "that host is not gated by fog")
		return
	}
	secure := forwardedSecure(r)
	next := sameHostNext(r.URL.Query().Get("next"), host, secure)
	scheme := "http"
	if secure {
		scheme = "https"
	}
	bind := r.URL.Query().Get("bind")
	if !isHexDigest(bind) {
		// A handshake the host did not begin — an old link, or one made by
		// hand. Starting over at the host binds a fresh one.
		http.Redirect(w, r, scheme+"://"+host+"/", http.StatusFound)
		return
	}
	pc, ok := s.resolvePerson(r)
	if !ok {
		http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
		return
	}
	d, err := s.decideFor(appID, pc.Person, pc.Session, identity.Request{}, nil)
	if err != nil {
		writeError(w, err)
		return
	}
	if d.Outcome != identity.Allow {
		s.renderGateRefusal(w, host, d)
		return
	}
	code, digest := auth.NewToken()
	if err := s.store.CreateGateCode(digest, host, pc.Session.ID, next, bind, gateCodeTTL); err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, scheme+"://"+host+gatePrefix+"callback?code="+url.QueryEscape(code), http.StatusFound)
}

// isHexDigest says whether v is a SHA-256 digest as auth.DigestToken
// writes one.
func isHexDigest(v string) bool {
	if len(v) != 64 {
		return false
	}
	for _, c := range v {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

// sameHostNext keeps a return address only when it is on the gated host;
// anything else — another host, another scheme, junk — returns to the
// host's root. The gate is never an open redirect.
func sameHostNext(next, host string, secure bool) string {
	scheme := "http"
	if secure {
		scheme = "https"
	}
	root := scheme + "://" + host + "/"
	u, err := url.Parse(next)
	if err != nil || u.Scheme != scheme || !strings.EqualFold(u.Host, host) || u.User != nil {
		return root
	}
	if !strings.HasPrefix(u.Path, "/") {
		return root
	}
	u.Fragment = ""
	return u.String()
}

// ---- step 3: the host gets its cookie ----

// handleGateCallback runs on the gated host (Caddy routes /.fog/gate/* there
// to fogd): it redeems the code and sets the host's own cookie.
func (s *Server) handleGateCallback(w http.ResponseWriter, r *http.Request) {
	host := stripPort(r.Header.Get("X-Forwarded-Host"))
	if host == "" {
		host = stripPort(r.Host)
	}
	if _, ok := s.gateApp(host); !ok {
		notFound(w, "not a gated host")
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		badRequest(w, "missing code")
		return
	}
	// Wrong codes are charged to the real address they came from; a good
	// handshake spends nothing (signin_handlers_limits.go).
	limits := s.credentialBuckets(r, "")
	if refuseLimited(w, "too many sign-ins; wait a minute", limits...) {
		return
	}
	secure := forwardedSecure(r)
	gc, err := s.store.RedeemGateCode(auth.DigestToken(code), host)
	if err != nil {
		// Spent, expired, or minted for a different host. Starting over
		// is the fix, and the root of the host starts it.
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "this sign-in link has expired; open the app again", http.StatusBadRequest)
		return
	}
	// The code is spent either way: one that arrives without the browser
	// that began its handshake was carried here by someone else.
	bound := false
	if isHexDigest(gc.Binding) {
		if c, err := r.Cookie(gateBindCookie(secure)); err == nil {
			bound = subtle.ConstantTimeCompare([]byte(auth.DigestToken(c.Value)), []byte(gc.Binding)) == 1
		}
	}
	if !bound {
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "this sign-in didn't start in this browser; open the app again", http.StatusBadRequest)
		return
	}
	refund(limits...)
	sess, _, err := s.store.SessionByID(gc.SessionID)
	if err != nil {
		unauthorized(w)
		return
	}
	token, digest := auth.NewToken()
	if err := s.store.CreateGateSession(digest, host, gc.SessionID); err != nil {
		writeError(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     gateCookieName(secure),
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(time.Until(sess.ExpiresAt).Seconds()),
	})
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, sameHostNext(gc.Next, host, secure), http.StatusFound)
}

// renderGateRefusal says why someone signed in to fog cannot use this app.
func (s *Server) renderGateRefusal(w http.ResponseWriter, host string, d identity.Decision) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusForbidden)
	msg := "This app isn't shared with you."
	if d.Reason == identity.ReasonInactive {
		msg = "Your account is suspended."
	} else if d.Reason == identity.ReasonGuest {
		msg = "Guests can't sign in to apps yet."
	}
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>Not shared</title>
<body style="font:15px/1.5 system-ui,sans-serif;max-width:32rem;margin:15vh auto;padding:0 1rem">
<p><strong>%s</strong></p><p>%s Ask the person who runs fog for %s to share it with you.</p>`,
		html.EscapeString(host), html.EscapeString(msg), html.EscapeString(s.householdName()))
}

// ---- what a gated app verifies ----

// gateInfo is what an app behind the gate needs to verify a Fog-Assertion
// itself, rather than trust Remote-User: who signs them and the audiences
// meant for it — its own gated hosts, and no other app's.
type gateInfo struct {
	Issuer    string   `json:"issuer"`
	Audiences []string `json:"audiences"`
	JWKSPath  string   `json:"jwksPath"`
}

// handleAppGate answers /app/v1/gate for the app whose FOG_API_TOKEN asks.
// fog-client reads it once and verifies every request's assertion against
// fog's keys (/oauth2/jwks, through FOG_API_URL), so nothing it trusts rests
// on a header a container on the same network could forge, or on a Host
// header naming another app.
func (s *Server) handleAppGate(w http.ResponseWriter, r *http.Request) {
	app, ok := s.appAuthed(w, r)
	if !ok {
		return
	}
	domain, _ := s.store.GetSetting(store.SettingBaseDomain)
	if domain == "" {
		writeErr(w, http.StatusServiceUnavailable, "the gate needs a domain")
		return
	}
	// Exactly what gateAssertion signs for this app, scheme included.
	writeJSON(w, http.StatusOK, gateInfo{Issuer: s.gateIssuer(), Audiences: s.gateAudiences(app.ID), JWKSPath: "/oauth2/jwks"})
}

// ---- keys ----

// handleJWKS serves the published signing keys: what an app verifies a
// Fog-Assertion (and later an ID token) against.
func (s *Server) handleJWKS(w http.ResponseWriter, r *http.Request) {
	set, err := s.keys.JWKS()
	if err != nil {
		writeError(w, err)
		return
	}
	if len(set.Keys) == 0 {
		// A fresh box publishes its first key on first use rather than at
		// boot; asking for the set is a use.
		if err := s.keys.Reconcile(); err != nil {
			writeError(w, err)
			return
		}
		if set, err = s.keys.JWKS(); err != nil {
			writeError(w, err)
			return
		}
	}
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Header().Set("Content-Type", "application/jwk-set+json")
	writeJSON(w, http.StatusOK, set)
}
