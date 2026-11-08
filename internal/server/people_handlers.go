// People: the household directory (docs/chat-contract.md). Three
// audiences share this file — the operator administering people and invites
// (session-authed, /api/people/*), a person joining or signing in (public,
// /api/people/join and /api/people/login), and a signed-in person managing
// their own credentials (/api/people/me/*). Chat-facing identity and device
// endpoints are in chatme_handlers.go.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"gitea.lan/matthew/fog/internal/auth"
	"gitea.lan/matthew/fog/internal/people"
	"gitea.lan/matthew/fog/internal/randx"
	"gitea.lan/matthew/fog/internal/store"
)

// personCtx is what the session middlewares resolve: who is speaking, from
// which device, and whether they administer this fog.
type personCtx struct {
	Person  *store.Person
	Session *store.PersonSession
	// Operator says this person administers the box. It is the role, read
	// once at resolve time, so a handler never re-derives it.
	Operator bool
}

const ctxPerson ctxKey = 2

// currentPerson returns the resolved person, or nil outside requirePerson.
func (s *Server) currentPerson(r *http.Request) *personCtx {
	p, _ := r.Context().Value(ctxPerson).(*personCtx)
	return p
}

// actor names who is making the request, for the audit log. Empty on an
// unauthenticated one, which no audited handler is.
func (s *Server) actor(r *http.Request) string {
	if pc := s.currentPerson(r); pc != nil {
		return pc.Person.Handle
	}
	return ""
}

// tokenSource says how a request carried its session token.
type tokenSource int

const (
	fromNowhere         tokenSource = iota
	fromHostCookie                  // __Host-fog_session: only fog's own host can hold it
	fromPlainCookie                 // fog_session, on a plain-http bare address
	fromBearer                      // Authorization: Bearer, a native chat client
	fromQuery                       // ?token= on the chat stream
	fromChatHostCookie              // __Host-fog_chat: a browser's chat device
	fromChatPlainCookie             // fog_chat, on an http setup or a bare address
)

// tokenCandidate is one token a request carries, and how.
type tokenCandidate struct {
	token  string
	source tokenSource
}

// sessionCandidates are the tokens a request carries in the three shapes fog
// accepts: the session cookie — every value of it, since a host-only cookie
// and a planted domain-wide one share a name — an Authorization header
// (native clients), and, for the chat stream alone, a query parameter,
// because a browser cannot set headers on a WebSocket and a native client
// has no cookie.
func (s *Server) sessionCandidates(r *http.Request) []tokenCandidate {
	var out []tokenCandidate
	// Only the cookie fog sets on this kind of request: never a plain
	// fog_session where fog uses __Host- (server.go, hostOnlySessions).
	for _, name := range s.sessionCookieNames(r) {
		source := fromPlainCookie
		if name == auth.SecureCookieName {
			source = fromHostCookie
		}
		for _, v := range cookieValues(r, name) {
			out = append(out, tokenCandidate{v, source})
		}
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		out = append(out, tokenCandidate{strings.TrimPrefix(h, "Bearer "), fromBearer})
	}
	if r.URL.Path == "/api/chat/stream" {
		if t := r.URL.Query().Get("token"); t != "" {
			out = append(out, tokenCandidate{t, fromQuery})
		}
	}
	return out
}

// sessionToken is the token of the request's session — the one that
// resolves through every check any request meets (resolveSession: the pin,
// the browser binding, app networks) — or "". A token that would be no one
// on any other request can't end a session either.
func (s *Server) sessionToken(r *http.Request) string {
	if _, tok, ok := s.resolveSession(r); ok {
		return tok
	}
	return ""
}

// resolvePerson finds the person behind a request without failing it. One
// session table, one answer: the role on the row says what they may do, and
// a suspended person resolves to nothing at all.
//
// No app may use a person's session (docs/signin-contract.md), even where an
// app can see the token — a plain fog_session on a bare address or an http
// setup is sent to every port of that address, a direct-port app's included.
// How a session may be used follows from how fog handed it out (IssuedAs,
// 0075), never from the name or header a request presents it under:
//   - plain (a plain cookie, a web chat device's included): works only from
//     the address it was signed in from (an IPv6 /64), however presented.
//     An app replaying it from its container, anyone it sent it to, a
//     cookie a sibling planted, and the same value renamed __Host- are all
//     someone else.
//   - host (__Host-fog_session on an https setup): only fog's own host ever
//     holds it, so presented as __Host-fog_session it is not pinned; a
//     browser moves between networks. Presented any other way — a plain
//     cookie on a bare address, a bearer — it has left that cookie, and is
//     pinned like any other.
//   - device (a native chat client's bearer token): never accepted as a
//     cookie, and not pinned, so the client keeps working as it moves.
//
// When more than one cookie session would count — one planted beside fog's
// — the one bound to the browser cookie fog set with it wins
// (signin_handlers_cookies.go); if that doesn't settle it, none counts: fog
// never guesses which of two it set.
//
// And no session of any kind is accepted from an app container's network:
// apps are never people.
func (s *Server) resolvePerson(r *http.Request) (*personCtx, bool) {
	pc, _, ok := s.resolveSession(r)
	return pc, ok
}

// resolveSession is resolvePerson, also returning the token that resolved.
func (s *Server) resolveSession(r *http.Request) (*personCtx, string, bool) {
	return s.resolveSessionFor(r, deviceTokenReaches(r))
}

// resolveSessionFor is resolveSession, with chat saying whether a chat
// device's credential may count: on chat's own routes, a browser's chat
// cookie comes first (signin_handlers_device.go); elsewhere, and when the
// browser's own session is wanted (browserSession), it is never read.
func (s *Server) resolveSessionFor(r *http.Request, chat bool) (*personCtx, string, bool) {
	if s.fromContainerNet(r) {
		return nil, "", false
	}
	if chat {
		if pc, tok, ok := s.pickSession(r, s.chatCandidates(r), true); ok {
			return pc, tok, true
		}
	}
	return s.pickSession(r, s.sessionCandidates(r), chat)
}

// pickSession is the session among cands that counts on r (see
// resolvePerson), chat saying whether r is on chat's routes.
func (s *Server) pickSession(r *http.Request, cands []tokenCandidate, chat bool) (*personCtx, string, bool) {
	type counted struct {
		pc    *personCtx
		token string
	}
	var cookies []counted
	for _, c := range cands {
		asCookie := c.source != fromBearer && c.source != fromQuery
		if !asCookie && len(cookies) > 0 {
			break // a cookie session counts before any other kind
		}
		sess, p, err := s.store.SessionPerson(auth.DigestToken(c.token))
		if err != nil || p.Status != store.PersonStatusActive {
			continue
		}
		chatCookie := c.source == fromChatHostCookie || c.source == fromChatPlainCookie
		// A session tied to a chat device — a native token, a browser's
		// chat credential, one from before either — reaches chat alone,
		// by its record, however it is presented.
		isChat := store.IsChatCredential(sess.IssuedAs)
		if (sess.DeviceID != 0 || sess.IssuedAs == store.SessionDevice || isChat) && !chat {
			continue
		}
		switch {
		case isChat && !chatCookie:
			continue // a chat credential is its cookie's alone
		case !isChat && chatCookie:
			continue
		case isChat && !s.chatCredentialFits(r, sess):
			continue // not this browser's (signin_handlers_device.go)
		case sess.IssuedAs == store.SessionDevice && asCookie:
			continue // a native token is a bearer's alone
		case sess.IssuedAs == store.SessionDevice:
		case sess.IssuedAs == store.SessionHost && c.source == fromHostCookie:
		case sess.IssuedAs == store.SessionChatHost && c.source == fromChatHostCookie:
		default: // plain, a session moved out of its cookie, anything unknown
			if !sameClient(sess.RemoteAddr, s.clientIP(r)) {
				continue
			}
		}
		pc := &personCtx{Person: p, Session: sess, Operator: p.Role == store.PersonRoleOperator}
		if !asCookie {
			return pc, c.token, true
		}
		if !slices.ContainsFunc(cookies, func(k counted) bool { return k.pc.Session.ID == sess.ID }) {
			cookies = append(cookies, counted{pc, c.token})
		}
	}
	if len(cookies) > 1 {
		// Several would count: only those bound to a browser cookie this
		// browser carries, and only if that leaves one.
		browsers := s.browserDigests(r)
		bound := cookies[:0:0]
		for _, c := range cookies {
			if d := c.pc.Session.BrowserDigest; d != "" && browsers[d] {
				bound = append(bound, c)
			}
		}
		cookies = bound
	}
	if len(cookies) == 1 {
		return cookies[0].pc, cookies[0].token, true
	}
	return nil, "", false
}

// sameClient says whether a request from now comes from the client a
// session was signed in from: the same IPv4 address, or the same IPv6 /64
// (privacy addresses rotate within it), zones and IPv4-mapping aside. A
// session with no recorded address predates recording one and is not
// pinned.
func sameClient(signedInFrom, now string) bool {
	if signedInFrom == "" {
		return true
	}
	a, okA := parseClientAddr(signedInFrom)
	b, okB := parseClientAddr(now)
	return okA && okB && addrKey(a) == addrKey(b)
}

// requirePerson gates a route on any signed-in person — a member's session
// or the operator's. Suspended people are refused as if signed out.
func (s *Server) requirePerson(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pc, ok := s.resolvePerson(r)
		if !ok {
			unauthorized(w)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxPerson, pc)))
	})
}

// chatLimited charges a chat call to its person's budget: one budget per
// person across every chat call, so a runaway client (or a person's
// compromised device) cannot starve the household. Sits inside
// requirePerson, which put the person on the request.
func (s *Server) chatLimited(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pc := s.currentPerson(r)
		if pc != nil && !limit(w, s.chatLimiter, "person:"+strconv.FormatInt(pc.Person.ID, 10), "too many chat requests; slow down") {
			return
		}
		next(w, r)
	}
}

// mintPersonSession creates a session for a person and returns the plaintext
// token; the caller decides cookie or bearer.
//
// amr records how the person proved themselves, in RFC 8176's words: an app
// that asks for a passkey (docs/signin-contract.md) reads it.
//
// How the token will be handed out is recorded with it (0075), because it
// decides how the token may be used (resolveSession): a native chat
// device's bearer token (clientKind "native" with a device), or a browser
// cookie — __Host-fog_session where this request gets that
// (hostOnlySessions), else the plain fog_session. Every caller hands the
// token out exactly that way: native as a token in the body, the rest
// through setSessionCookie on this same request.
func (s *Server) mintPersonSession(r *http.Request, personID, deviceID int64, clientKind string, amr ...string) (string, int64, error) {
	// A person's interactive sign-ins are minted at a bounded rate and kept
	// to a bounded number (signin_handlers_limits.go): a loop of sign-ins
	// can't grow the table without end. Native device tokens are minted by
	// mintDeviceToken, on a budget of the minting session's own, so no
	// session can spend this one.
	if !s.signinLimits().sessionMint.Allow(strconv.FormatInt(personID, 10)) {
		return "", 0, errSessionRate
	}
	token, digest := auth.NewToken()
	id, err := s.store.CreatePersonSession(personID, deviceID, digest, clientKind, auth.SessionTTL, r.UserAgent(), s.clientIP(r), amr)
	if err != nil {
		return "", 0, err
	}
	kind := store.SessionPlain
	switch {
	case clientKind == "native" && deviceID != 0:
		kind = store.SessionDevice
	case s.hostOnlySessions(r):
		kind = store.SessionHost
	}
	if err := s.store.SetSessionIssuedAs(id, kind); err != nil {
		s.store.DeletePersonSessionByDigest(digest)
		return "", 0, err
	}
	if n, err := s.store.PrunePersonSessions(personID, maxSessionsPerPerson, id, sessionInUse); err == nil && n > 0 {
		s.kickLogouts()
	}
	return token, id, nil
}

func (s *Server) householdName() string { return householdNameOf(s.store) }

// householdNameOf is the server name, or a stand-in before one is set.
func householdNameOf(st *store.Store) string {
	name, _ := st.GetSetting(store.SettingServerName)
	if name == "" {
		name = "this household"
	}
	return name
}

// ---- operator: directory and invites ----

type personAdminView struct {
	store.Person
	Devices         int      `json:"devices"`
	VerifiedDevices int      `json:"verifiedDevices"`
	Credentials     []string `json:"credentials"`
	HasIdentity     bool     `json:"hasIdentity"`
}

func (s *Server) personAdminView(p store.Person) personAdminView {
	v := personAdminView{Person: p, Credentials: []string{}, HasIdentity: len(p.IdentityKey) > 0}
	v.Person.IdentityKey = nil
	if devs, err := s.store.PersonDevices(p.ID); err == nil {
		v.Devices = len(devs)
		for _, d := range devs {
			if d.Verified() {
				v.VerifiedDevices++
			}
		}
	}
	if creds, err := s.store.PersonCredentials(p.ID); err == nil {
		seen := map[string]bool{}
		for _, c := range creds {
			if !seen[c.Kind] {
				seen[c.Kind] = true
				v.Credentials = append(v.Credentials, c.Kind)
			}
		}
	}
	return v
}

func (s *Server) handleListPeople(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListPeople()
	if err != nil {
		writeError(w, err)
		return
	}
	out := make([]personAdminView, 0, len(list))
	for _, p := range list {
		out = append(out, s.personAdminView(p))
	}
	invites, _ := s.store.ListPersonInvites()
	if invites == nil {
		invites = []store.PersonInvite{}
	}
	pending := invites[:0:0]
	nowS := time.Now().UTC().Format(time.RFC3339)
	for _, i := range invites {
		if i.UsedAt == "" && i.ExpiresAt > nowS {
			pending = append(pending, i)
		}
	}
	writeJSON(w, http.StatusOK, peopleDirectory{Invites: pending, People: out})
}

// peopleDirectory is Settings › People & access › People: everyone, and the invites not yet
// used or expired.
type peopleDirectory struct {
	Invites []store.PersonInvite `json:"invites"`
	People  []personAdminView    `json:"people"`
}

// personSignedIn answers a join or a passkey sign-in with who it made.
// Person is null only when the row could not be read back after a join.
type personSignedIn struct {
	Person *store.Person `json:"person"`
}

// personLogin answers a password sign-in. Username and Role repeat the
// person's handle and role.
type personLogin struct {
	Person   *store.Person `json:"person"`
	Role     string        `json:"role"`
	Username string        `json:"username"`
}

// passkeyCeremony starts a WebAuthn ceremony: its id, to hand back with
// the finish, and go-webauthn's options for the browser.
type passkeyCeremony struct {
	Ceremony string `json:"ceremony"`
	Options  any    `json:"options"`
}

// passkeyAdded is the credential a passkey registration stored.
type passkeyAdded struct {
	ID    int64  `json:"id"`
	Label string `json:"label"`
}

func (s *Server) handleCreatePersonInvite(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Handle      string `json:"handle"`
		DisplayName string `json:"displayName"`
		Role        string `json:"role"`
		TTLHours    int    `json:"ttlHours"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	role, err := people.ValidRole(req.Role)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	hint := ""
	if strings.TrimSpace(req.Handle) != "" {
		hint, err = people.NormalizeHandle(req.Handle)
		if err != nil {
			badRequest(w, err.Error())
			return
		}
		if _, err := s.store.PersonByHandle(hint); err == nil {
			badRequest(w, "that handle is taken")
			return
		}
	}
	ttl := people.InviteTTL
	if req.TTLHours > 0 && req.TTLHours <= 24*30 {
		ttl = time.Duration(req.TTLHours) * time.Hour
	}
	secret, digest := auth.NewToken()
	by := s.actor(r)
	id, err := s.store.CreatePersonInvite(digest, hint, people.ValidDisplayName(req.DisplayName), role, by, ttl)
	if err != nil {
		writeError(w, err)
		return
	}
	s.auditAs(by, "people.invite", hint, map[string]any{"role": role})
	writeJSON(w, http.StatusOK, personInviteCreated{
		ExpiresAt: time.Now().UTC().Add(ttl).Format(time.RFC3339),
		ID:        id,
		Path:      people.JoinPath(secret),
		URL:       s.publicURL(r) + people.JoinPath(secret),
	})
}

// personInviteCreated carries the join link, the only time its secret is
// ever shown. Its fields are in alphabetical order, as they came out when
// this was built as a map.
type personInviteCreated struct {
	ExpiresAt string `json:"expiresAt"`
	ID        int64  `json:"id"`
	Path      string `json:"path"`
	URL       string `json:"url"`
}

// publicURL is the address a join link should carry: the configured domain
// when there is one, else the address the operator is using right now.
func (s *Server) publicURL(r *http.Request) string {
	if u := s.fogURL(); u != "" {
		return u
	}
	return requestOrigin(r)
}

func (s *Server) handleDeletePersonInvite(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	if err := s.store.DeletePersonInvite(id); err != nil {
		writeError(w, err)
		return
	}
	writeOK(w)
}

func (s *Server) personFromPath(w http.ResponseWriter, r *http.Request) *store.Person {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return nil
	}
	p, err := s.store.PersonByID(id)
	if errors.Is(err, store.ErrNotFound) {
		notFound(w, "no such person")
		return nil
	}
	if err != nil {
		writeError(w, err)
		return nil
	}
	return p
}

func (s *Server) handleUpdatePerson(w http.ResponseWriter, r *http.Request) {
	p := s.personFromPath(w, r)
	if p == nil {
		return
	}
	var req struct {
		DisplayName *string `json:"displayName"`
		Role        *string `json:"role"`
		Status      *string `json:"status"`
		// Email is optional and unverified (docs/signin-contract.md);
		// "" clears it.
		Email *string `json:"email"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Email != nil {
		email, err := people.ValidEmail(*req.Email)
		if err != nil {
			badRequest(w, err.Error())
			return
		}
		if err := s.store.SetPersonEmail(p.ID, email); err != nil {
			writeError(w, err)
			return
		}
	}
	display, role, status := p.DisplayName, p.Role, p.Status
	if req.DisplayName != nil {
		display = people.ValidDisplayName(*req.DisplayName)
		if display == "" {
			display = p.Handle
		}
	}
	if p.Role == store.PersonRoleOperator {
		if (req.Role != nil && *req.Role != p.Role) || (req.Status != nil && *req.Status != p.Status) {
			badRequest(w, "the operator's role and status cannot change")
			return
		}
	} else {
		if req.Role != nil {
			var err error
			if role, err = people.ValidRole(*req.Role); err != nil {
				badRequest(w, err.Error())
				return
			}
		}
		if req.Status != nil {
			switch *req.Status {
			case store.PersonStatusActive, store.PersonStatusSuspended:
				status = *req.Status
			default:
				badRequest(w, "status must be active or suspended")
				return
			}
		}
	}
	if err := s.store.UpdatePerson(p.ID, display, role, status); err != nil {
		writeError(w, err)
		return
	}
	if status == store.PersonStatusSuspended && p.Status != status {
		s.store.DeletePersonSessions(p.ID)
		// Every app that saw them is owed a logout (0052's triggers).
	}
	np, err := s.store.PersonByID(p.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	// A demotion — member to guest — keeps their fog session (a guest still
	// uses fog), so no trigger fires: each app where they may now do less
	// is told here, and so is each household they were visiting.
	if err := s.endReducedAccess(p, np); err != nil {
		writeError(w, err)
		return
	}
	s.audit(r, "people.update", p.Handle, map[string]any{"role": role, "status": status})
	writeJSON(w, http.StatusOK, s.personAdminView(*np))
}

func (s *Server) handleDeletePerson(w http.ResponseWriter, r *http.Request) {
	p := s.personFromPath(w, r)
	if p == nil {
		return
	}
	if p.Role == store.PersonRoleOperator {
		badRequest(w, "the operator cannot be removed")
		return
	}
	if err := s.store.DeletePerson(p.ID); err != nil {
		writeError(w, err)
		return
	}
	s.chat.PersonDeleted(p) // out of every room; nothing left for the handle's next holder (internal/chat/identity.go)
	// The delete's triggers owe every app a logout and every SCIM app an
	// active: false (0052, 0073); send them now rather than at the next tick.
	s.kickLogouts()
	s.audit(r, "people.remove", p.Handle, nil)
	writeOK(w)
}

// handleResetPersonIdentity is the operator-assisted recovery: the person
// lost every device and the recovery phrase. Their identity key is cleared
// and every device revoked; the next device to sign in starts a new
// identity, which every room shows as a reset to verify again.
func (s *Server) handleResetPersonIdentity(w http.ResponseWriter, r *http.Request) {
	p := s.personFromPath(w, r)
	if p == nil {
		return
	}
	// Revoking someone's devices is a credential change: the operator's own
	// browser session, freshly proven — and never on their own account, as
	// with a password (handleSetPersonPassword): a stolen operator session
	// must not be able to swap the operator's chat identity for one it holds.
	pc := s.currentPerson(r)
	if p.ID == pc.Person.ID {
		forbidden(w, "your own chat identity is reset from your own devices, not the People page")
		return
	}
	if !s.credentialSession(w, r, pc, true) {
		return
	}
	if err := s.store.ResetPersonIdentity(p.ID); err != nil {
		writeError(w, err)
		return
	}
	s.chat.IdentityReset(p) // out of every room, with a warning there (internal/chat/identity.go)
	s.audit(r, "people.identity.reset", p.Handle, nil)
	writeOK(w)
}

// handleSetPersonPassword lets the operator hand a member a temporary
// password — the fallback for a box without passkeys, or a locked-out member.
// It is a credential change like any other: the operator's browser session,
// proven within minutes (credentialSession), and never their own account,
// which goes through Settings › Your sign-in like everyone's.
func (s *Server) handleSetPersonPassword(w http.ResponseWriter, r *http.Request) {
	p := s.personFromPath(w, r)
	if p == nil {
		return
	}
	pc := s.currentPerson(r)
	if !s.credentialSession(w, r, pc, true) {
		return
	}
	if p.ID == pc.Person.ID {
		forbidden(w, "change your own password under Settings › Your sign-in")
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if err := auth.ValidatePassword(req.Password); err != nil {
		badRequest(w, err.Error())
		return
	}
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.store.SetPersonPassword(p.ID, hash); err != nil {
		writeError(w, err)
		return
	}
	// A password the operator hands out is a recovery: whatever the old one
	// opened — someone else's session, if that is why — ends with it.
	if err := s.store.DeletePersonSessions(p.ID); err != nil {
		writeError(w, err)
		return
	}
	s.kickLogouts()
	s.audit(r, "people.password.set", p.Handle, map[string]any{"sessionsEnded": true})
	writeOK(w)
}

// ---- joining ----

func (s *Server) handleJoinPreview(w http.ResponseWriter, r *http.Request) {
	inv, err := s.store.PersonInviteByDigest(auth.DigestToken(r.PathValue("secret")))
	if errors.Is(err, store.ErrNotFound) {
		notFound(w, "this invite is not valid — it may have expired or already been used")
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	_, pkErr := people.RelyingParty(r, s.baseDomain(), s.householdName())
	writeJSON(w, http.StatusOK, joinPreview{
		DisplayName:       inv.DisplayHint,
		ExpiresAt:         inv.ExpiresAt,
		Handle:            inv.HandleHint,
		Household:         s.householdName(),
		PasskeysAvailable: pkErr == nil,
		Role:              inv.Role,
	})
}

// joinPreview is what an invite says before it is spent: whose household,
// and the handle and name the operator suggested. Its fields are in
// alphabetical order, as they came out when this was built as a map.
type joinPreview struct {
	DisplayName       string `json:"displayName"`
	ExpiresAt         string `json:"expiresAt"`
	Handle            string `json:"handle"`
	Household         string `json:"household"`
	PasskeysAvailable bool   `json:"passkeysAvailable"`
	Role              string `json:"role"`
}

func (s *Server) baseDomain() string {
	d, _ := s.store.GetSetting(store.SettingBaseDomain)
	return d
}

func (s *Server) handleJoin(w http.ResponseWriter, r *http.Request) {
	if s.refuseSignInHere(w, r) {
		return
	}
	// A wrong invite link is charged to the address it came from
	// (signin_handlers_limits.go); a good one spends nothing.
	limits := s.credentialBuckets(r, "")
	if refuseLimited(w, "too many attempts; try again later", limits...) {
		return
	}
	digest := auth.DigestToken(r.PathValue("secret"))
	inv, err := s.store.PersonInviteByDigest(digest)
	if errors.Is(err, store.ErrNotFound) {
		notFound(w, "this invite is not valid — it may have expired or already been used")
		return
	}
	refund(limits...)
	if err != nil {
		writeError(w, err)
		return
	}
	var req struct {
		Handle      string `json:"handle"`
		DisplayName string `json:"displayName"`
		Password    string `json:"password"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	handle := req.Handle
	if handle == "" {
		handle = inv.HandleHint
	}
	handle, err = people.NormalizeHandle(handle)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	if inv.HandleHint != "" && handle != inv.HandleHint {
		badRequest(w, "this invite is for @"+inv.HandleHint)
		return
	}
	display := people.ValidDisplayName(req.DisplayName)
	if display == "" {
		display = inv.DisplayHint
	}
	if display == "" {
		display = handle
	}
	var hash string
	if req.Password != "" {
		if err := auth.ValidatePassword(req.Password); err != nil {
			badRequest(w, err.Error())
			return
		}
		if hash, err = auth.HashPassword(req.Password); err != nil {
			writeError(w, err)
			return
		}
	}
	// One transaction: a taken handle, a failed password write or a lost
	// race for the same link leaves no person behind and the link as it was.
	id, _, err := s.store.JoinPerson(digest, handle, display, hash)
	switch {
	case errors.Is(err, store.ErrAlreadyExists):
		badRequest(w, "that handle is taken")
		return
	case errors.Is(err, store.ErrNotFound):
		notFound(w, "this invite was just used")
		return
	case err != nil:
		writeError(w, err)
		return
	}
	// Joining proves possession of the invite and, when one was set, the
	// password; a joiner who chose a passkey registers it next, signed in.
	var amr []string
	if hash != "" {
		amr = []string{"pwd"}
	}
	if err := s.startWebSession(w, r, id, amr...); err != nil {
		writeError(w, err)
		return
	}
	s.auditAs(handle, "people.join", handle, map[string]any{"role": inv.Role})
	p, _ := s.store.PersonByID(id)
	writeJSON(w, http.StatusOK, personSignedIn{Person: p})
}

// ---- signing in ----

// handleLogin is fog's only password sign-in. It does not ask which kind of
// account you have — it looks up the person, checks the password, and the
// role on the row decides what the session can reach. `handle` and
// `username` both name the same field: the older operator client sent one,
// the member client the other, and neither has to be rewritten.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Handle   string `json:"handle"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	typed := req.Handle
	if typed == "" {
		typed = req.Username
	}
	// One refusal for an unknown handle, a wrong password and a suspended
	// account alike: which of the three it was is not the sign-in page's to
	// tell.
	fail := func() {
		writeErr(w, http.StatusUnauthorized, "invalid handle or password")
	}
	if s.refuseSignInHere(w, r) {
		return
	}
	// Who the guess is at decides its account bucket: an active person's
	// own, or — for a handle no one active holds — the one shared bucket
	// for all of those (signin_handlers_limits.go). A made-up handle never
	// makes a key of its own.
	var p *store.Person
	if handle, err := people.NormalizeHandle(typed); err == nil {
		if found, err := s.store.PersonByHandle(handle); err == nil && found.Status == store.PersonStatusActive {
			p = found
		}
	}
	account := unknownAccount
	if p != nil {
		account = loginAccountKey(p.Handle)
	}
	// A wrong guess is charged to the real address it came from, then to
	// the account it aimed at; a right password is charged to the
	// account's and the address's success budgets instead
	// (signin_handlers_limits.go), and a passkey has buckets of its own.
	limits := s.credentialBuckets(r, account)
	if refuseLimited(w, "too many attempts; try again later", limits...) {
		return
	}
	keepSuccess := func() {}
	if p != nil {
		// A right password is reserved now, beside the attempt, and given
		// back on every way out but a session.
		release, ok := s.reserveSuccess(w, r, account, limits...)
		if !ok {
			return
		}
		defer func() {
			if release != nil {
				release()
			}
		}()
		keepSuccess = func() { release = nil }
	}
	// The attempt is charged now (refuseLimited); only a right password gives
	// it back. One account holds at most one place in the hashing line.
	caller := s.hashCaller(r, p != nil)
	if p != nil {
		caller.Account = account
	}
	if p == nil {
		// The same work as a wrong password, so how long a refusal takes
		// does not say whether the handle exists.
		if _, err := auth.CheckPasswordAs(req.Password, dummyPasswordHash(), caller); busyHashing(w, err, limits...) {
			return
		}
		fail()
		return
	}
	creds, _ := s.store.PersonCredentials(p.ID)
	ok, tried := false, false
	for _, c := range creds {
		if c.Kind != store.CredentialPassword {
			continue
		}
		tried = true
		good, err := auth.CheckPasswordAs(req.Password, c.Data, caller)
		if busyHashing(w, err, limits...) {
			return
		}
		if good {
			ok = true
			s.store.UpdatePersonCredential(c.ID, c.Data)
			break
		}
	}
	if !tried {
		if _, err := auth.CheckPasswordAs(req.Password, dummyPasswordHash(), caller); busyHashing(w, err, limits...) {
			return
		}
	}
	if !ok {
		fail()
		return
	}
	refund(limits...)
	keepSuccess()
	s.clearAccount(account)
	if err := s.startWebSession(w, r, p.ID, "pwd"); err != nil {
		writeError(w, err)
		return
	}
	s.store.TouchPersonSeen(p.ID)
	s.auditAs(p.Handle, "auth.login", p.Handle, map[string]any{"role": p.Role})
	writeJSON(w, http.StatusOK, personLogin{Person: p, Role: p.Role, Username: p.Handle})
}

// loginAccountKey is the account bucket for password guesses at one
// handle, wherever they come from.
func loginAccountKey(handle string) string { return "login-account:" + handle }

// dummyPasswordHash is an argon2id hash of nothing anyone knows, verified
// against when there is no real one, so a refusal costs the same either way.
var dummyPasswordHash = sync.OnceValue(func() string {
	h, err := auth.HashPassword(randx.Token())
	if err != nil {
		panic(err)
	}
	return h
})

// startWebSession signs a browser in: a fresh session and token (never one
// the browser brought, so no session can be fixed in advance), and the
// session the browser held before — someone else's, on a shared computer —
// ended, so the apps it signed in to are told.
func (s *Server) startWebSession(w http.ResponseWriter, r *http.Request, personID int64, amr ...string) error {
	token, _, err := s.mintPersonSession(r, personID, 0, "web", amr...)
	if err != nil {
		return err
	}
	// Only the session the browser was really using: a value planted beside
	// it names someone else's session, which isn't the browser's to end.
	if _, prev, ok := s.resolveSessionFor(r, false); ok && prev != token {
		s.store.DeletePersonSessionByDigest(auth.DigestToken(prev))
		s.kickLogouts()
	}
	// A chat credential this browser holds for someone else ends too.
	s.endChatCredentials(w, r, personID)
	s.setSessionCookie(w, r, token)
	return nil
}

// handleLogout ends the session the request arrived on, whichever of the
// accepted shapes carried it, and clears both cookie names — the one fog
// issues now and the one a member's browser may still hold.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	// Only a session this request could use ends: a token that is no one
	// here — replayed from elsewhere, from an app's network — is refused,
	// not obeyed. The browser's own cookies are cleared either way.
	token := s.sessionToken(r)
	s.clearSessionCookie(w, r)
	s.endChatCredentials(w, r, 0) // this browser's chat goes with it
	if token == "" {
		unauthorized(w)
		return
	}
	s.store.DeletePersonSessionByDigest(auth.DigestToken(token))
	// The apps this session signed in to are owed a logout (0052).
	s.kickLogouts()
	writeOK(w)
}

func (s *Server) handlePersonMe(w http.ResponseWriter, r *http.Request) {
	pc := s.currentPerson(r)
	creds, _ := s.store.PersonCredentials(pc.Person.ID)
	if creds == nil {
		creds = []store.PersonCredential{}
	}
	_, pkErr := people.RelyingParty(r, s.baseDomain(), s.householdName())
	activity, err := s.accountActivity(pc.Person)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, personMe{
		Activity:          activity,
		Credentials:       creds,
		Household:         s.householdName(),
		Operator:          pc.Operator,
		PasskeysAvailable: pkErr == nil,
		Person:            *pc.Person,
	})
}

// personMe is /api/people/me: the person view of whoever is signed in,
// under the same session as /api/auth/me. Its fields are in alphabetical
// order, as they came out when this was built as a map.
type personMe struct {
	// Activity is what recently happened to this person's sign-in —
	// passwords, passkeys, sessions — newest first.
	Activity          []accountEvent           `json:"activity"`
	Credentials       []store.PersonCredential `json:"credentials"`
	Household         string                   `json:"household"`
	Operator          bool                     `json:"operator"`
	PasskeysAvailable bool                     `json:"passkeysAvailable"`
	Person            store.Person             `json:"person"`
}

// handleSetOwnPassword lets a person set or change their password. Someone
// who already has one must present it; a passkey-only person is adding a
// fallback and has nothing to present.
func (s *Server) handleSetOwnPassword(w http.ResponseWriter, r *http.Request) {
	pc := s.currentPerson(r)
	var req struct {
		Current  string `json:"current"`
		Password string `json:"password"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	// Someone with a password proves it below. Someone adding a first one
	// has nothing to present, so the session itself must have been proven
	// just now (credentialSession).
	hasPassword := false
	if creds, err := s.store.PersonCredentials(pc.Person.ID); err == nil {
		for _, c := range creds {
			hasPassword = hasPassword || c.Kind == store.CredentialPassword
		}
	}
	if !s.credentialSession(w, r, pc, !hasPassword) {
		return
	}
	// Checking the current password is a password check like any other: a
	// stolen session must not be a place to guess it.
	key := "password-check:" + strconv.FormatInt(pc.Person.ID, 10)
	limits := s.credentialBuckets(r, key)
	if refuseLimited(w, "too many attempts; try again later", limits...) {
		return
	}
	creds, _ := s.store.PersonCredentials(pc.Person.ID)
	for _, c := range creds {
		if c.Kind != store.CredentialPassword {
			continue
		}
		caller := s.hashCaller(r, true)
		caller.Account = key
		ok, err := auth.CheckPasswordAs(req.Current, c.Data, caller)
		if busyHashing(w, err, limits...) {
			return
		}
		if !ok {
			forbidden(w, "current password is wrong")
			return
		}
	}
	refund(limits...)
	s.clearAccount(key)
	if err := auth.ValidatePassword(req.Password); err != nil {
		badRequest(w, err.Error())
		return
	}
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.store.SetPersonPassword(pc.Person.ID, hash); err != nil {
		writeError(w, err)
		return
	}
	// Whatever the old password opened elsewhere ends with it; the session
	// that changed it stays.
	ended, err := s.store.DeletePersonSessionsExcept(pc.Person.ID, pc.Session.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	if ended > 0 {
		s.kickLogouts()
	}
	// The same entry the operator's reset writes, marked as the person's own.
	s.audit(r, "people.password.set", pc.Person.Handle, map[string]any{"self": true, "sessionsEnded": ended})
	writeOK(w)
}

// credentialProofAge is how recently a session must have been proven to
// add or remove a way in: long enough to confirm and then add a passkey,
// short enough that a session left open, stolen, or held by a chat device
// can't quietly add a credential of its own.
const credentialProofAge = 10 * time.Minute

// credentialSession says whether this request may change how its person
// signs in. Only a browser's own fog session may — never a bearer token,
// which is what a chat device signs in with — and, when fresh is set, only
// one proven within credentialProofAge (a sign-in, a join, or /confirm).
// A stale one is answered 409 reauth_required, so the page sends the person
// to confirm and back.
func (s *Server) credentialSession(w http.ResponseWriter, r *http.Request, pc *personCtx, fresh bool) bool {
	if pc.Session.DeviceID != 0 || pc.Session.IssuedAs == store.SessionDevice || !s.sessionFromCookie(r) {
		forbidden(w, "a device's sign-in can't change how you sign in; use fog in your browser")
		return false
	}
	if fresh && (pc.Session.AuthTime.IsZero() || time.Since(pc.Session.AuthTime) > credentialProofAge) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "confirm it's you first", "code": "reauth_required"})
		return false
	}
	return true
}

// sessionFromCookie says whether the session that resolved this request
// came from fog's session cookie, not an Authorization header or a query
// token.
func (s *Server) sessionFromCookie(r *http.Request) bool {
	// The browser's own session, never its chat credential, on any route.
	_, tok, ok := s.resolveSessionFor(r, false)
	if !ok {
		return false
	}
	for _, name := range s.sessionCookieNames(r) {
		for _, v := range cookieValues(r, name) {
			if v == tok {
				return true
			}
		}
	}
	return false
}

// ---- passkeys ----

// waUser adapts a person to the WebAuthn library's user model. The handle
// authenticators see is a stable digest of the id: opaque, never a name.
type waUser struct {
	p     *store.Person
	creds []webauthn.Credential
}

func personWebAuthnID(id int64) []byte {
	return []byte(auth.DigestToken("fog-person/1:" + strconv.FormatInt(id, 10)))[:32]
}

func (u waUser) WebAuthnID() []byte                         { return personWebAuthnID(u.p.ID) }
func (u waUser) WebAuthnName() string                       { return u.p.Handle }
func (u waUser) WebAuthnDisplayName() string                { return u.p.DisplayName }
func (u waUser) WebAuthnCredentials() []webauthn.Credential { return u.creds }

func (s *Server) webAuthnUser(p *store.Person) (waUser, []store.PersonCredential) {
	creds, _ := s.store.PersonCredentials(p.ID)
	u := waUser{p: p}
	for _, c := range creds {
		if c.Kind != store.CredentialPasskey {
			continue
		}
		var wc webauthn.Credential
		if json.Unmarshal([]byte(c.Data), &wc) == nil {
			u.creds = append(u.creds, wc)
		}
	}
	return u, creds
}

func (s *Server) handlePasskeyRegisterBegin(w http.ResponseWriter, r *http.Request) {
	pc := s.currentPerson(r)
	if !s.credentialSession(w, r, pc, true) {
		return
	}
	rp, err := people.RelyingParty(r, s.baseDomain(), s.householdName())
	if err != nil {
		writeErr(w, http.StatusPreconditionFailed, err.Error())
		return
	}
	u, _ := s.webAuthnUser(pc.Person)
	creation, sess, err := rp.BeginRegistration(u,
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired),
		webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			ResidentKey:      protocol.ResidentKeyRequirementRequired,
			UserVerification: protocol.VerificationPreferred,
		}))
	if err != nil {
		writeError(w, err)
		return
	}
	id := s.ceremonies.Put(people.CeremonyRegister, sess, pc.Person.ID)
	writeJSON(w, http.StatusOK, passkeyCeremony{Ceremony: id, Options: creation})
}

func (s *Server) handlePasskeyRegisterFinish(w http.ResponseWriter, r *http.Request) {
	pc := s.currentPerson(r)
	if !s.credentialSession(w, r, pc, true) {
		return
	}
	var req struct {
		Ceremony string          `json:"ceremony"`
		Label    string          `json:"label"`
		Response json.RawMessage `json:"response"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	sess, personID, ok := s.ceremonies.Take(people.CeremonyRegister, req.Ceremony)
	if !ok || personID != pc.Person.ID {
		badRequest(w, "this passkey ceremony expired; start again")
		return
	}
	rp, err := people.RelyingParty(r, s.baseDomain(), s.householdName())
	if err != nil {
		writeErr(w, http.StatusPreconditionFailed, err.Error())
		return
	}
	parsed, err := protocol.ParseCredentialCreationResponseBody(bytes.NewReader(req.Response))
	if err != nil {
		badRequest(w, "malformed passkey response")
		return
	}
	u, _ := s.webAuthnUser(pc.Person)
	cred, err := rp.CreateCredential(u, sess, parsed)
	if err != nil {
		badRequest(w, "passkey could not be verified: "+err.Error())
		return
	}
	data, _ := json.Marshal(cred)
	label := people.ValidDisplayName(req.Label)
	if label == "" {
		label = "Passkey"
	}
	id, err := s.store.AddPersonCredential(pc.Person.ID, store.CredentialPasskey, string(data), label)
	if err != nil {
		writeError(w, err)
		return
	}
	s.audit(r, "people.passkey.add", pc.Person.Handle, nil)
	writeJSON(w, http.StatusOK, passkeyAdded{ID: id, Label: label})
}

func (s *Server) handleDeleteOwnCredential(w http.ResponseWriter, r *http.Request) {
	pc := s.currentPerson(r)
	// Taking a way in away locks its owner out as surely as adding one lets
	// someone else in: the same fresh proof.
	if !s.credentialSession(w, r, pc, true) {
		return
	}
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	// Read the credential first so the audit entry names what went; a
	// delete of one that isn't theirs (or is already gone) still answers ok
	// and records nothing, since nothing changed.
	kind := ""
	creds, _ := s.store.PersonCredentials(pc.Person.ID)
	for _, c := range creds {
		if c.ID == id {
			kind = c.Kind
		}
	}
	// The last way in is not removable: a person with no password and no
	// passkey is locked out until the operator steps in, and the operator
	// locked out of their own box has no one to step in.
	if kind != "" && len(creds) == 1 {
		writeErr(w, http.StatusConflict, "this is your only way to sign in — add another passkey or a password before removing it")
		return
	}
	if err := s.store.DeletePersonCredential(pc.Person.ID, id); err != nil {
		writeError(w, err)
		return
	}
	if kind != "" {
		s.audit(r, "people.credential.remove", pc.Person.Handle, map[string]any{"credential": id, "kind": kind})
	}
	writeOK(w)
}

func (s *Server) handlePasskeyLoginBegin(w http.ResponseWriter, r *http.Request) {
	if s.refuseSignInHere(w, r) {
		return
	}
	// Unauthenticated, and each call holds a challenge for minutes: bounded
	// per real address, in the passkey bucket — never the password one, so
	// password guesses can't keep a passkey out.
	if !chargeAttempt(w, "too many attempts; try again later", s.passkeyBuckets(r)...) {
		return
	}
	rp, err := people.RelyingParty(r, s.baseDomain(), s.householdName())
	if err != nil {
		writeErr(w, http.StatusPreconditionFailed, err.Error())
		return
	}
	assertion, sess, err := rp.BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationPreferred))
	if err != nil {
		writeError(w, err)
		return
	}
	id := s.ceremonies.Put(people.CeremonyLogin, sess, 0)
	writeJSON(w, http.StatusOK, passkeyCeremony{Ceremony: id, Options: assertion})
}

func (s *Server) handlePasskeyLoginFinish(w http.ResponseWriter, r *http.Request) {
	if s.refuseSignInHere(w, r) {
		return
	}
	// A failed assertion is charged to the passkey bucket (the begin already
	// paid for the ceremony); a good one spends nothing.
	limits := s.passkeyBuckets(r)
	if refuseLimited(w, "too many attempts; try again later", limits...) {
		return
	}
	var req struct {
		Ceremony string          `json:"ceremony"`
		Response json.RawMessage `json:"response"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	sess, _, ok := s.ceremonies.Take(people.CeremonyLogin, req.Ceremony)
	if !ok {
		badRequest(w, "this sign-in expired; start again")
		return
	}
	rp, err := people.RelyingParty(r, s.baseDomain(), s.householdName())
	if err != nil {
		writeErr(w, http.StatusPreconditionFailed, err.Error())
		return
	}
	parsed, err := protocol.ParseCredentialRequestResponseBody(bytes.NewReader(req.Response))
	if err != nil {
		badRequest(w, "malformed passkey response")
		return
	}
	var matched *store.Person
	var matchedCred *store.PersonCredential
	handler := func(rawID, userHandle []byte) (webauthn.User, error) {
		list, err := s.store.ListPeople()
		if err != nil {
			return nil, err
		}
		for i := range list {
			if !bytes.Equal(personWebAuthnID(list[i].ID), userHandle) {
				continue
			}
			u, creds := s.webAuthnUser(&list[i])
			for j := range creds {
				var wc webauthn.Credential
				if creds[j].Kind == store.CredentialPasskey && json.Unmarshal([]byte(creds[j].Data), &wc) == nil && bytes.Equal(wc.ID, rawID) {
					matchedCred = &creds[j]
				}
			}
			matched = &list[i]
			return u, nil
		}
		return nil, errors.New("unknown passkey")
	}
	cred, err := rp.ValidateDiscoverableLogin(handler, sess, parsed)
	if err != nil || matched == nil {
		writeErr(w, http.StatusUnauthorized, "passkey not recognised")
		return
	}
	if matched.Status != store.PersonStatusActive {
		writeErr(w, http.StatusUnauthorized, "this account is suspended")
		return
	}
	refund(limits...)
	if matchedCred != nil {
		if data, err := json.Marshal(cred); err == nil {
			s.store.UpdatePersonCredential(matchedCred.ID, string(data))
		}
	}
	// "user" only when the authenticator verified the person (a PIN, a
	// fingerprint): a passkey a tap alone unlocked proves possession, not
	// who held it.
	amr := []string{"pop"}
	if cred.Flags.UserVerified {
		amr = append(amr, "user")
	}
	if err := s.startWebSession(w, r, matched.ID, amr...); err != nil {
		writeError(w, err)
		return
	}
	s.store.TouchPersonSeen(matched.ID)
	s.auditAs(matched.Handle, "people.login", matched.Handle, map[string]any{"via": "passkey"})
	writeJSON(w, http.StatusOK, personSignedIn{Person: matched})
}
