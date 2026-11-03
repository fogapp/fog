// fog's OpenID provider (docs/signin-contract.md): discovery, pushed
// authorization requests, authorize, token, userinfo, revocation and
// RP-initiated logout. The protocol rules are internal/oidc's and the policy
// internal/identity's; this file is the shell — parse, decide, persist,
// respond.
package server

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gitea.lan/matthew/fog/internal/auth"
	"gitea.lan/matthew/fog/internal/identity"
	"gitea.lan/matthew/fog/internal/oidc"
	"gitea.lan/matthew/fog/internal/store"
)

// parURNPrefix is the request_uri namespace of RFC 9126 §2.2.
const parURNPrefix = "urn:ietf:params:oauth:request_uri:"

// issuer is fog's OIDC issuer, https://fog.<domain>, or "" without a
// domain: OIDC needs a stable https origin, as passkeys already do (§8).
func (s *Server) issuer() string { return s.fogURL() }

func (s *Server) requireIssuer(w http.ResponseWriter) (string, bool) {
	iss := s.issuer()
	if iss == "" {
		writeErr(w, http.StatusServiceUnavailable, "sign-in with fog needs a domain; set one under Settings › Network › Domain & HTTPS")
		return "", false
	}
	return iss, true
}

func toOIDCClient(c *store.OIDCClient) *oidc.Client {
	return &oidc.Client{
		ID: c.ClientID, AppID: c.AppID,
		RedirectURIs: c.RedirectURIs, PostLogoutRedirectURIs: c.PostLogoutURIs,
		RequirePAR: c.RequirePAR,
		Scopes:     c.Scopes, ACR: c.RequiredACR, Roles: c.Roles, RoleMap: c.RoleMap,
	}
}

func (s *Server) lookupClient(id string) (*store.OIDCClient, *oidc.Client) {
	if id == "" {
		return nil, nil
	}
	c, err := s.store.OIDCClient(id)
	if err != nil {
		return nil, nil
	}
	return c, toOIDCClient(c)
}

// writeOAuthError answers a back-channel endpoint with the RFC 6749 §5.2
// error body.
func writeOAuthError(w http.ResponseWriter, e *oidc.Error) {
	status := e.Status
	if status == 0 {
		status = http.StatusBadRequest
	}
	if e.Code == oidc.ErrInvalidClient {
		w.Header().Set("WWW-Authenticate", `Basic realm="fog"`)
		status = http.StatusUnauthorized
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, map[string]string{"error": string(e.Code), "error_description": e.Description})
}

// authClient authenticates the client at a back-channel endpoint:
// client_secret_basic or client_secret_post, never both (RFC 6749 §2.3).
// The secret is compared as a digest, in constant time, against what the
// client may use now (signin.Registry.Authenticate). A failure is charged
// to the caller's address, among strangers (signin_handlers_limits.go); a
// client that proves itself is charged to its own bucket, which only its
// secret reaches — so a stranger spraying guesses at one client cannot
// spend the budget of the app itself.
func (s *Server) authClient(r *http.Request) (*store.OIDCClient, *oidc.Error) {
	bad := &oidc.Error{Code: oidc.ErrInvalidClient, Description: "client authentication failed", Status: http.StatusUnauthorized}
	id, secret, basic := r.BasicAuth()
	if basic {
		if r.PostForm.Has("client_secret") {
			return nil, &oidc.Error{Code: oidc.ErrInvalidRequest, Description: "use one client authentication method", Status: 400}
		}
		var err1, err2 error
		id, err1 = url.QueryUnescape(id)
		secret, err2 = url.QueryUnescape(secret)
		if err1 != nil || err2 != nil {
			return nil, bad
		}
	} else {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if id == "" || secret == "" {
		return nil, bad
	}
	// The limiters' keys are built from what fog checked — the caller's
	// address, a registered client id — never from the raw id a request
	// sent, which could be as long as its body.
	// The check is a digest compare, cheap enough to make first: a client
	// that proves itself never touches the strangers' limiter.
	slow := &oidc.Error{Code: oidc.ErrInvalidRequest, Description: "too many requests; slow down", Status: http.StatusTooManyRequests}
	failed := func() *oidc.Error {
		if !s.strangerAllowed(r, "oidc-client") {
			return slow
		}
		return bad
	}
	c, err := s.store.OIDCClient(id)
	if err != nil {
		return nil, failed()
	}
	ok, err := s.signin.Authenticate(c, secret, time.Now())
	if err != nil {
		return nil, &oidc.Error{Code: oidc.ErrServerError, Description: "could not check the client", Status: http.StatusInternalServerError}
	}
	if !ok {
		return nil, failed()
	}
	if !s.appAPILimiter.Allow("oidc-client:" + c.ClientID) {
		return nil, slow
	}
	return c, nil
}

// maxSigninForm bounds a sign-in endpoint's form body: every parameter fog
// takes fits many times over, and nothing a stranger sends becomes megabytes
// of memory (a key, a stored request) before it is checked.
const maxSigninForm = 64 << 10

// limitForm caps r's body at maxSigninForm, before it is parsed.
func limitForm(w http.ResponseWriter, r *http.Request) {
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, maxSigninForm)
	}
}

// ---- discovery ----

func (s *Server) handleOIDCDiscovery(w http.ResponseWriter, r *http.Request) {
	iss, ok := s.requireIssuer(w)
	if !ok {
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=300")
	writeJSON(w, http.StatusOK, oidc.Discovery(iss))
}

// ---- pushed authorization requests (RFC 9126) ----

func (s *Server) handlePAR(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireIssuer(w); !ok {
		return
	}
	limitForm(w, r)
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, &oidc.Error{Code: oidc.ErrInvalidRequest, Description: "malformed body"})
		return
	}
	sc, oerr := s.authClient(r)
	if oerr != nil {
		writeOAuthError(w, oerr)
		return
	}
	form := cloneValues(r.PostForm)
	form.Del("client_secret")
	if !form.Has("client_id") {
		form.Set("client_id", sc.ClientID)
	}
	req, perr := oidc.ParseAuthorize(form, toOIDCClient(sc), true)
	if perr != nil {
		writeOAuthError(w, perr)
		return
	}
	req.Began = time.Now().UnixMilli()
	token, digest := auth.NewToken()
	if err := s.store.CreateOIDCRequest(digest, store.OIDCRequestPAR, sc.ClientID, 0, mustMarshal(req), "", oidc.PARTTL); err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, map[string]any{"request_uri": parURNPrefix + token, "expires_in": int(oidc.PARTTL.Seconds())})
}

func cloneValues(v url.Values) url.Values {
	out := url.Values{}
	for k, vs := range v {
		out[k] = append([]string(nil), vs...)
	}
	return out
}

func mustMarshal(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// ---- authorize ----

// handleAuthorize is the front channel: the browser arrives with a request
// (pushed, or in the query), fog decides for the person signed in, and the
// browser leaves with a code — or is sent to sign in or confirm first and
// returns here to the same stored request.
func (s *Server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	iss := s.issuer()
	if iss == "" {
		s.renderSigninPage(w, http.StatusServiceUnavailable, "Sign-in needs a domain", "fog signs people in to apps once it has a domain with https. The person who runs fog can set one under Settings › Network › Domain & HTTPS.")
		return
	}
	q := r.URL.Query()
	// Who is asking, for the limit: the person signed in here, or the
	// address of someone who isn't. A request link (request_uri) is a
	// bearer secret, and a signed-in person can mint codes; neither may be
	// hammered.
	// Someone signed out is limited by their address bucket
	// (signin_handlers_limits.go: an address or /64, a tunnel visitor's own,
	// or the one shared stranger bucket). A request link is a 256-bit secret
	// either way.
	// A signed-in person's bucket is in a limiter only fog-checked keys
	// reach; a signed-out caller's is among strangers.
	//
	// Only a page load is charged. A browser says what a request is for
	// (Sec-Fetch-Mode, Sec-Fetch-Dest): a sibling app's subresource — an
	// image, a fetch — pointed here is never a sign-in, so it is refused
	// before anything is charged or done, and can't spend a person's
	// budget for them.
	if !authorizeNavigation(r) {
		s.renderSigninPage(w, http.StatusBadRequest, "Open this sign-in in your browser", "Go back to the app and sign in again.")
		return
	}
	pc, signedIn := s.resolvePerson(r)
	allowed := false
	if signedIn {
		allowed = s.appAPILimiter.Allow("authorize:person:" + strconv.FormatInt(pc.Person.ID, 10))
	} else {
		allowed = s.strangerAllowed(r, "authorize")
	}
	if !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(int(s.appAPILimiter.Window().Seconds())))
		s.renderSigninPage(w, http.StatusTooManyRequests, "Too many sign-ins", "Wait a minute, then go back to the app and sign in again.")
		return
	}
	_, client := s.lookupClient(q.Get("client_id"))
	var req oidc.AuthorizeRequest
	txn := ""
	if raw := q.Get("request_uri"); raw != "" {
		token, ok := strings.CutPrefix(raw, parURNPrefix)
		if client == nil || !ok {
			s.renderSigninPage(w, http.StatusBadRequest, "This sign-in link isn't valid", "Go back to the app and sign in again.")
			return
		}
		row, err := s.store.PeekOIDCRequest(auth.DigestToken(token), store.OIDCRequestPAR)
		if err != nil || row.ClientID != client.ID || json.Unmarshal([]byte(row.Params), &req) != nil {
			s.renderSigninPage(w, http.StatusBadRequest, "This sign-in link has expired", "Go back to the app and sign in again.")
			return
		}
		txn = auth.DigestToken(token)
	} else {
		var perr *oidc.Error
		req, perr = oidc.ParseAuthorize(q, client, false)
		if perr != nil {
			if !perr.RedirectSafe {
				s.renderSigninPage(w, perr.Status, "This sign-in request isn't valid", perr.Description)
				return
			}
			http.Redirect(w, r, oidc.ErrorRedirect(req.RedirectURI, iss, req.State, perr), http.StatusFound)
			return
		}
	}
	fail := func(code oidc.Code, desc string) {
		http.Redirect(w, r, oidc.ErrorRedirect(req.RedirectURI, iss, req.State, &oidc.Error{Code: code, Description: desc}), http.StatusFound)
	}
	// interact sends the browser to sign in or to confirm, holding the
	// request server-side so it returns to exactly this request — and so
	// "sign in again" can be told apart from "signed in long ago" by when
	// the request began.
	hold := func() (string, bool) {
		if txn == "" {
			req.Began = time.Now().UnixMilli()
			token, digest := auth.NewToken()
			if err := s.store.CreateOIDCRequest(digest, store.OIDCRequestPAR, client.ID, 0, mustMarshal(req), "", oidc.PARTTL); err != nil {
				writeError(w, err)
				return "", false
			}
			txn = parURNPrefix + token
		} else {
			txn = q.Get("request_uri")
		}
		return "/oauth2/authorize?" + url.Values{"client_id": {client.ID}, "request_uri": {txn}}.Encode(), true
	}
	interact := func(page string, extra url.Values) {
		back, ok := hold()
		if !ok {
			return
		}
		v := url.Values{"next": {back}, "app": {client.ID}}
		for k, vs := range extra {
			v[k] = vs
		}
		if req.LoginHint != "" {
			v.Set("hint", req.LoginHint)
		}
		http.Redirect(w, r, page+"?"+v.Encode(), http.StatusFound)
	}

	if !signedIn {
		// A visitor from a paired household (visit_handlers.go): a person
		// signed in here always wins over one.
		if v, ok := s.resolveVisitor(r); ok {
			s.authorizeVisitor(w, r, iss, client, req, v, txn, fail, hold)
			return
		}
		if req.Prompt == "none" {
			fail(oidc.ErrLoginRequired, "no one is signed in to fog")
			return
		}
		interact("/login", nil)
		return
	}
	stale := staleFor(req, pc.Session.AuthTime)
	d, err := s.decideFor(client.AppID, pc.Person, pc.Session, identity.Request{ACR: req.ACR}, client)
	if err != nil {
		writeError(w, err)
		return
	}
	switch {
	case d.Outcome == identity.Deny:
		if req.Prompt == "none" {
			fail(oidc.ErrAccessDenied, d.Reason.String())
			return
		}
		s.renderSigninRefusal(w, client.ID, d)
		return
	case req.Prompt == "none" && (stale || d.Outcome == identity.StepUp):
		fail(oidc.ErrInteractionRequired, "the app asked fog to confirm it's you")
		return
	case d.Outcome == identity.StepUp:
		interact("/confirm", url.Values{"with": {"passkey"}})
		return
	case stale:
		interact("/confirm", nil)
		return
	}

	grant := oidc.Grant{Request: req, Role: d.Role, SID: pc.Session.SID, AMR: pc.Session.AMR, ACR: d.ACR, AuthTime: pc.Session.AuthTime}
	s.issueCode(w, r, iss, client, req, grant, pc.Session.ID, txn)
}

// staleFor says whether a sign-in at authTime is too old for req:
// prompt=login wants one proven after the request began — signed in or
// confirmed on the way here — and max_age one at most that old.
func staleFor(req oidc.AuthorizeRequest, authTime time.Time) bool {
	if req.Prompt != "login" && req.MaxAge == nil {
		return false
	}
	fresh := req.Began > 0 && authTime.After(time.UnixMilli(req.Began))
	if !fresh && req.Prompt != "login" && req.MaxAge != nil {
		fresh = !authTime.IsZero() && time.Since(authTime) <= req.MaxAge.Std()
	}
	return !fresh
}

// issueCode ends an authorize request fog said yes to: the code the app
// redeems for grant, the held request (txn, its digest; "" when none) spent
// so its link can't be used twice, and the browser sent back to the app.
// sessionID is the fog session the code hangs off; 0 for a visitor.
func (s *Server) issueCode(w http.ResponseWriter, r *http.Request, iss string, client *oidc.Client, req oidc.AuthorizeRequest, grant oidc.Grant, sessionID int64, txn string) {
	code, digest := auth.NewToken()
	if err := s.store.CreateOIDCRequest(digest, store.OIDCRequestCode, client.ID, sessionID, mustMarshal(req), mustMarshal(grant), oidc.CodeTTL); err != nil {
		writeError(w, err)
		return
	}
	if txn != "" {
		if _, err := s.store.SpendOIDCRequest(txn, store.OIDCRequestPAR); err != nil {
			s.renderSigninPage(w, http.StatusBadRequest, "This sign-in link was already used", "Go back to the app and sign in again.")
			return
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, oidc.Redirect(req.RedirectURI, iss, req.State, url.Values{"code": {code}}), http.StatusFound)
}

// ---- token ----

func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	iss, ok := s.requireIssuer(w)
	if !ok {
		return
	}
	limitForm(w, r)
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, &oidc.Error{Code: oidc.ErrInvalidRequest, Description: "malformed body"})
		return
	}
	sc, oerr := s.authClient(r)
	if oerr != nil {
		writeOAuthError(w, oerr)
		return
	}
	form := cloneValues(r.PostForm)
	form.Del("client_id")
	form.Del("client_secret")
	if form.Get("grant_type") == oidc.DeviceGrantType && sc.DeviceGrant {
		s.tokenForDevice(w, iss, sc, form)
		return
	}
	tr, perr := oidc.ParseTokenRequest(form)
	if perr != nil {
		writeOAuthError(w, perr)
		return
	}
	invalid := &oidc.Error{Code: oidc.ErrInvalidGrant, Description: "the code is invalid, expired, or was issued to another request"}
	codeDigest := auth.DigestToken(tr.Code)
	row, err := s.store.SpendOIDCRequest(codeDigest, store.OIDCRequestCode)
	if errors.Is(err, store.ErrReplayed) {
		s.audit(r, "oidc.code.replayed", sc.ClientID, nil)
		writeOAuthError(w, invalid)
		return
	}
	if err != nil || row.ClientID != sc.ClientID {
		writeOAuthError(w, invalid)
		return
	}
	var req oidc.AuthorizeRequest
	var grant oidc.Grant
	if json.Unmarshal([]byte(row.Params), &req) != nil || json.Unmarshal([]byte(row.Grant), &grant) != nil {
		writeOAuthError(w, invalid)
		return
	}
	if perr := oidc.CheckRedemption(tr, sc.ClientID, req); perr != nil {
		writeOAuthError(w, perr)
		return
	}
	s.issueTokens(w, iss, sc, row.SessionID, grant, codeDigest)
}

// oidcPrincipal is who an app is told signed in: a person of this
// household, or a visitor from a paired one (visit_handlers.go). The token
// and userinfo endpoints answer the same for both; what differs is where
// the access token is kept, what a sign-in records, and how the audit log
// names them.
type oidcPrincipal interface {
	// subject is the principal as claims describe them. An *oidc.Error
	// means they can no longer be described — gone, not fog failing.
	subject() (oidc.Subject, error)
	// createAccessToken keeps an access token's digest for userinfo;
	// codeDigest is the code it was redeemed from, which a replay revokes.
	createAccessToken(digest, clientID, codeDigest string, grant oidc.Grant) error
	recordSignIn(clientID string) error
	// sessionExpires is when the fog session behind the sign-in ends: the
	// ID token's fog_session_exp, which the app's session must not outlive.
	sessionExpires() time.Time
	// stillAllowed re-asks policy at the token endpoint, so a change made
	// between authorize and redemption is not signed over.
	stillAllowed(sc *store.OIDCClient, grant oidc.Grant) *oidc.Error
	// auditActor and auditDetail are how the sign-in reads in the audit log.
	auditActor() string
	auditDetail(grant oidc.Grant) map[string]any
}

// personPrincipal is a person signed in to fog.
type personPrincipal struct {
	s    *Server
	sess *store.PersonSession
	p    *store.Person
}

func (pp personPrincipal) subject() (oidc.Subject, error) { return pp.s.subjectFor(pp.p) }

func (pp personPrincipal) createAccessToken(digest, clientID, codeDigest string, grant oidc.Grant) error {
	return pp.s.store.CreateOIDCAccessToken(digest, clientID, pp.sess.ID, codeDigest, mustMarshal(grant), oidc.AccessTokenTTL)
}

func (pp personPrincipal) recordSignIn(clientID string) error {
	return pp.s.store.RecordOIDCSignIn(pp.sess.ID, clientID, pp.p.ID)
}

func (pp personPrincipal) sessionExpires() time.Time { return pp.sess.ExpiresAt }

// stillAllowed refuses a grant the person's access no longer covers: refused
// now, or decided for a role the policy no longer gives them.
func (pp personPrincipal) stillAllowed(sc *store.OIDCClient, grant oidc.Grant) *oidc.Error {
	d, err := pp.s.decideFor(sc.AppID, pp.p, pp.sess, identity.Request{}, toOIDCClient(sc))
	if err != nil || d.Outcome == identity.Deny || d.Role != grant.Role {
		return &oidc.Error{Code: oidc.ErrInvalidGrant, Description: "what this person may do in the app changed; sign in again"}
	}
	return nil
}

func (pp personPrincipal) auditActor() string { return pp.p.Handle }

func (pp personPrincipal) auditDetail(grant oidc.Grant) map[string]any {
	return map[string]any{"role": grant.Role, "acr": grant.ACR}
}

// personForSession is the person behind a fog session, while they are
// still signed in to fog and active.
func (s *Server) personForSession(id int64) (personPrincipal, *oidc.Error) {
	sess, p, err := s.store.SessionByID(id)
	if err != nil || p.Status != store.PersonStatusActive {
		return personPrincipal{}, &oidc.Error{Code: oidc.ErrInvalidGrant, Description: "the person is no longer signed in to fog"}
	}
	return personPrincipal{s: s, sess: sess, p: p}, nil
}

// issueTokens answers a token request whose grant has been checked: an
// access token for userinfo, an ID token, and a note of who signed in where.
// The person must still be signed in to fog and active (a visitor, still
// signed in and still welcome); codeDigest is what a replay revokes.
func (s *Server) issueTokens(w http.ResponseWriter, iss string, sc *store.OIDCClient, sessionID int64, grant oidc.Grant, codeDigest string) {
	var pr oidcPrincipal
	var gone *oidc.Error
	if grant.VisitorSessionID != 0 {
		pr, gone = s.visitorForGrant(sc, grant)
	} else {
		pr, gone = s.personForSession(sessionID)
	}
	if gone != nil {
		writeOAuthError(w, gone)
		return
	}
	if gone = pr.stillAllowed(sc, grant); gone != nil {
		writeOAuthError(w, gone)
		return
	}
	subject, err := pr.subject()
	if errors.As(err, &gone) {
		writeOAuthError(w, gone)
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	access, digest := auth.NewToken()
	if err := pr.createAccessToken(digest, sc.ClientID, codeDigest, grant); err != nil {
		writeError(w, err)
		return
	}
	idToken, err := s.keys.Sign(oidc.IDToken(iss, subject, grant, access, pr.sessionExpires(), time.Now()), "JWT")
	if err != nil {
		writeError(w, err)
		return
	}
	if err := pr.recordSignIn(sc.ClientID); err != nil {
		writeError(w, err)
		return
	}
	s.auditAs(pr.auditActor(), "oidc.signin", sc.ClientID, pr.auditDetail(grant))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": access,
		"token_type":   "Bearer",
		"expires_in":   int(oidc.AccessTokenTTL.Seconds()),
		"id_token":     idToken,
		"scope":        strings.Join(grant.Request.Scope, " "),
	})
}

// subjectFor is the person as claims describe them.
func (s *Server) subjectFor(p *store.Person) (oidc.Subject, error) {
	inst, err := s.fed.Identity()
	if err != nil {
		return oidc.Subject{}, err
	}
	return oidc.Subject{
		Address: identity.Address(inst.InstanceID, p.UID),
		Home:    inst.InstanceID,
		Handle:  p.Handle,
		Name:    displayOr(p),
		Email:   p.Email,
	}, nil
}

// authorizeNavigation says whether an authorize request is a top-level
// page load: a browser marks one navigate/document; a frame, an image, a
// fetch are marked otherwise and refused. A client that sends neither
// marker — not a browser — is taken at its word.
func authorizeNavigation(r *http.Request) bool {
	if m := r.Header.Get("Sec-Fetch-Mode"); m != "" && m != "navigate" {
		return false
	}
	if d := r.Header.Get("Sec-Fetch-Dest"); d != "" && d != "document" {
		return false
	}
	return true
}

// ---- userinfo ----

func (s *Server) handleUserInfo(w http.ResponseWriter, r *http.Request) {
	refuse := func() {
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		writeErr(w, http.StatusUnauthorized, "invalid access token")
	}
	// The token is checked first (a digest lookup): a request without a
	// live one is charged to its address, among strangers; one with a token
	// to the token's client, in a limiter strangers never reach
	// (signin_handlers_limits.go).
	refuse = func() {
		if !s.limitStranger(w, r, "userinfo", "too many requests; slow down") {
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		writeErr(w, http.StatusUnauthorized, "invalid access token")
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		refuse()
		return
	}
	pr, rawGrant, clientID, ok := s.accessTokenPrincipal(auth.DigestToken(token))
	if !ok {
		refuse()
		return
	}
	if !limit(w, s.appAPILimiter, "userinfo-client:"+clientID, "too many requests; slow down") {
		return
	}
	var grant oidc.Grant
	if json.Unmarshal([]byte(rawGrant), &grant) != nil {
		refuse()
		return
	}
	// The same question the token endpoint asked: an access token is no
	// better than the access it was minted under.
	sc, err := s.store.OIDCClient(clientID)
	if err != nil || pr.stillAllowed(sc, grant) != nil {
		refuse()
		return
	}
	subject, err := pr.subject()
	var gone *oidc.Error
	if errors.As(err, &gone) {
		refuse()
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, oidc.UserInfo(subject, grant))
}

// accessTokenPrincipal finds who an access token was issued to — a person,
// or failing that a visitor — the grant it carries, and the client it was
// issued to. ok is false for a token fog doesn't know, or one whose holder
// is no longer signed in.
func (s *Server) accessTokenPrincipal(digest string) (oidcPrincipal, string, string, bool) {
	if at, err := s.store.OIDCAccessTokenByDigest(digest); err == nil {
		pp, gone := s.personForSession(at.SessionID)
		return pp, at.Grant, at.ClientID, gone == nil
	}
	at, err := s.store.VisitorAccessTokenByDigest(digest)
	if err != nil {
		return nil, "", "", false
	}
	vp, gone := s.visitorForSession(at.VisitorSessionID)
	return vp, at.Grant, at.ClientID, gone == nil
}

// ---- revocation (RFC 7009) ----

func (s *Server) handleRevoke(w http.ResponseWriter, r *http.Request) {
	limitForm(w, r)
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, &oidc.Error{Code: oidc.ErrInvalidRequest, Description: "malformed body"})
		return
	}
	sc, oerr := s.authClient(r)
	if oerr != nil {
		writeOAuthError(w, oerr)
		return
	}
	if t := r.PostForm.Get("token"); t != "" {
		if err := s.store.RevokeOIDCAccessToken(auth.DigestToken(t), sc.ClientID); err != nil {
			writeError(w, err)
			return
		}
		if err := s.store.RevokeVisitorAccessToken(auth.DigestToken(t), sc.ClientID); err != nil {
			writeError(w, err)
			return
		}
	}
	// 200 whether or not the token existed (§2.2): a probe learns nothing.
	w.WriteHeader(http.StatusOK)
}

// ---- RP-initiated logout ----

// handleEndSession signs the person out of fog at an app's request. With an
// ID token fog signed for the session signed in now, it happens at once;
// otherwise the person is asked first — a link alone must not be able to
// sign anyone out (OIDC RP-Initiated Logout §2).
func (s *Server) handleEndSession(w http.ResponseWriter, r *http.Request) {
	iss, ok := s.requireIssuer(w)
	if !ok {
		return
	}
	limitForm(w, r)
	if err := r.ParseForm(); err != nil {
		badRequest(w, "malformed request")
		return
	}
	v := r.Form
	var hint struct {
		Aud string `json:"aud"`
		Azp string `json:"azp"`
		Sid string `json:"sid"`
		Iss string `json:"iss"`
	}
	hinted := false
	if raw := v.Get("id_token_hint"); raw != "" {
		// Only an ID token fog issued to a registered client: a gate
		// assertion, a logout token or a proof is signed by the same keys
		// and may name the same sid, and none of them may sign anyone out
		// without asking. An expired ID token is still a fine hint.
		if payload, err := s.keys.VerifyAs(raw, "JWT"); err == nil && json.Unmarshal(payload, &hint) == nil && hint.Iss == iss &&
			(hint.Azp == "" || hint.Azp == hint.Aud) {
			if _, c := s.lookupClient(hint.Aud); c != nil {
				hinted = true
			}
		}
	}
	clientID := v.Get("client_id")
	if hinted {
		if clientID != "" && clientID != hint.Aud {
			badRequest(w, "client_id does not match the id_token_hint")
			return
		}
		clientID = hint.Aud
	}
	redirect := v.Get("post_logout_redirect_uri")
	if redirect != "" {
		_, c := s.lookupClient(clientID)
		if c == nil || !c.AllowsPostLogoutRedirect(redirect) {
			s.renderSigninPage(w, http.StatusBadRequest, "This sign-out link isn't valid", "The app asked fog to send you somewhere it never registered.")
			return
		}
	}
	pc, signedIn := s.resolvePerson(r)
	visitor, visiting := s.resolveVisitor(r)
	sid := ""
	switch {
	case signedIn:
		sid = pc.Session.SID
	case visiting:
		sid = visitor.SID
	}
	confirmed := r.Method == http.MethodPost && v.Get("confirm") == "1" && sameHost(r.Header.Get("Origin"), r.Host)
	matches := hinted && sid != "" && subtle.ConstantTimeCompare([]byte(hint.Sid), []byte(sid)) == 1
	if sid != "" && !matches && !confirmed {
		s.renderEndSessionConfirm(w, v, clientID)
		return
	}
	if signedIn {
		s.endFogSession(w, r, pc)
	}
	if visiting {
		if c, err := r.Cookie(visitorCookie); err == nil {
			s.store.DeleteVisitorSessionByDigest(auth.DigestToken(c.Value))
		}
		s.clearVisitorCookie(w)
		s.kickLogouts()
	}
	if redirect != "" {
		u, _ := url.Parse(redirect)
		q := u.Query()
		if st := v.Get("state"); st != "" {
			q.Set("state", st)
		}
		u.RawQuery = q.Encode()
		http.Redirect(w, r, u.String(), http.StatusFound)
		return
	}
	s.renderSigninPage(w, http.StatusOK, "Signed out", "You're signed out of fog.")
}

// endFogSession ends the browser's fog session: the row, the cookie, and
// with them every gate session and code that hung off it.
func (s *Server) endFogSession(w http.ResponseWriter, r *http.Request, pc *personCtx) {
	if token := s.sessionToken(r); token != "" {
		s.store.DeletePersonSessionByDigest(auth.DigestToken(token))
	}
	s.clearSessionCookie(w, r)
	s.endChatCredentials(w, r, 0)
	s.kickLogouts()
	s.auditAs(pc.Person.Handle, "people.logout", pc.Person.Handle, map[string]any{"via": "end_session"})
}

// ---- pages ----

// renderSigninPage is fog's plain page for the sign-in flow's dead ends:
// served before any SPA loads, readable without JavaScript.
func (s *Server) renderSigninPage(w http.ResponseWriter, status int, title, body string, extra ...string) {
	s.renderSigninPageTo(w, status, title, body, "", extra...)
}

// renderSigninPageTo is renderSigninPage for a form that posts to fog and is
// then redirected to redirectTo, an address fog has already checked (a
// client's registered post-logout URI, a visit's pushed redirect URI).
// Browsers apply form-action to that redirect as well as to the post
// (Chromium blocks it, verified), so the policy names exactly fog and that
// one origin: no other page, and no other target, gets a form that can send
// anywhere else. A page with no form allows none.
func (s *Server) renderSigninPageTo(w http.ResponseWriter, status int, title, body, redirectTo string, extra ...string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'; form-action "+signinFormAction(len(extra) > 0, redirectTo))
	w.WriteHeader(status)
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>%s</title>
<body style="font:15px/1.5 system-ui,sans-serif;max-width:32rem;margin:15vh auto;padding:0 1rem;color:#1d2329;background:#eef1f4">
<p><strong>%s</strong></p><p>%s</p>%s`, html.EscapeString(title), html.EscapeString(title), html.EscapeString(body), strings.Join(extra, ""))
}

// signinFormOrigin is a CSP source naming exactly one http(s) origin.
var signinFormOrigin = regexp.MustCompile(`^https?://[A-Za-z0-9.\-]+(:[0-9]{1,5})?$`)

// signinFormAction is a sign-in page's form-action sources.
func signinFormAction(hasForm bool, redirectTo string) string {
	if !hasForm {
		return "'none'"
	}
	if u, err := url.Parse(redirectTo); err == nil && redirectTo != "" {
		if origin := u.Scheme + "://" + u.Host; signinFormOrigin.MatchString(origin) {
			return "'self' " + origin
		}
	}
	return "'self'"
}

// renderSigninRefusal says an app isn't shared with this person, and offers
// to ask the operator.
func (s *Server) renderSigninRefusal(w http.ResponseWriter, app string, d identity.Decision) {
	msg := fmt.Sprintf("%s isn't shared with you yet.", app)
	ask := fmt.Sprintf(`<form method="post" action="/oauth2/ask"><input type="hidden" name="app" value="%s"><button type="submit">Ask for access</button></form>`, html.EscapeString(app))
	switch d.Reason {
	case identity.ReasonInactive:
		msg, ask = "Your account is suspended.", ""
	case identity.ReasonGuest:
		msg, ask = "Guests can't sign in to apps yet.", ""
	case identity.ReasonNoRole:
		msg = fmt.Sprintf("You don't have a role in %s.", app)
	}
	s.renderSigninPage(w, http.StatusForbidden, "Not shared", msg, ask)
}

func (s *Server) renderEndSessionConfirm(w http.ResponseWriter, v url.Values, clientID string) {
	var hidden strings.Builder
	for _, k := range []string{"id_token_hint", "post_logout_redirect_uri", "state", "client_id"} {
		if val := v.Get(k); val != "" {
			fmt.Fprintf(&hidden, `<input type="hidden" name="%s" value="%s">`, k, html.EscapeString(val))
		}
	}
	who := "An app"
	if clientID != "" {
		who = clientID
	}
	form := fmt.Sprintf(`<form method="post" action="/oauth2/end_session">%s<input type="hidden" name="confirm" value="1"><button type="submit">Sign out</button></form>`, hidden.String())
	s.renderSigninPageTo(w, http.StatusOK, "Sign out of fog?", who+" asked to sign you out of fog, which signs you out of every app.", v.Get("post_logout_redirect_uri"), form)
}

// handleSigninAsk files "Kim asked to use Manor" in the operator's Inbox.
func (s *Server) handleSigninAsk(w http.ResponseWriter, r *http.Request) {
	pc, ok := s.resolvePerson(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	if !sameHost(r.Header.Get("Origin"), r.Host) {
		forbidden(w, "cross-origin request rejected")
		return
	}
	if !limit(w, s.chatLimiter, "ask:"+pc.Person.Handle, "too many requests; slow down") {
		return
	}
	limitForm(w, r)
	app := r.FormValue("app")
	if _, err := s.store.AppByName(app); err != nil {
		badRequest(w, "no such app")
		return
	}
	name := displayOr(pc.Person)
	if err := s.store.CreateNotice("signin-ask/"+pc.Person.Handle, app,
		fmt.Sprintf("%s asked to use %s", name, app),
		fmt.Sprintf("%s (%s) tried to sign in to %s, which isn't shared with them. Share it from the app's Sign-in tab.", name, pc.Person.Handle, app)); err != nil {
		writeError(w, err)
		return
	}
	s.renderSigninPage(w, http.StatusOK, "Asked", fmt.Sprintf("%s will see your request in fog's Inbox.", s.householdName()))
}

// handleAppSigninProof is how an app proves to itself that it runs on fog
// (docs/signin-contract.md): authenticated by the app's own
// FOG_API_TOKEN, fog signs — with the key that signs the app's ID tokens —
// the issuer and client id the app is configured with. An app that can
// verify that against its OIDC_ISSUER's keys knows the two are one fog, and
// only then switches its own local sign-in off.
func (s *Server) handleAppSigninProof(w http.ResponseWriter, r *http.Request) {
	app, ok := s.appAuthed(w, r)
	if !ok {
		return
	}
	iss, ok := s.requireIssuer(w)
	if !ok {
		return
	}
	c, err := s.store.OIDCClientForApp(app.ID)
	if err != nil {
		notFound(w, app.Name+" does not declare fog-signin/1")
		return
	}
	proof, err := s.keys.Sign(map[string]any{"issuer": iss, "client_id": c.ClientID, "app": app.Name, "iat": time.Now().Unix()}, "fog-signin-proof+jwt")
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"proof": proof, "issuer": iss, "clientId": c.ClientID})
}
