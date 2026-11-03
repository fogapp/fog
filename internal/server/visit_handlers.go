// Visitors (docs/signin-contract.md): a paired household's person
// signs in to one of our apps, authenticated by their own fog. The flow is
// OIDC between the two fogs, pinned to the pairing: the back channel is the
// signed /fed/v1 plane, so neither side registers, types a secret or trusts
// DNS, and the ID token verifies only against keys the pairing delivered.
//
// The home decides who (its people.visit grant to the host); the host
// decides what (a visitor grant per app, capped at the app's declared
// visitors.role). Visitors are never people here: a visitor_sessions row
// carries who they are and the sid the apps are told.
//
//	host /visit/start ──PAR (fed)──▶ home
//	browser ──▶ home /visit/authorize ── code ──▶ host /visit/callback
//	host ──token (fed)──▶ home; host verifies against the home's keys (fed)
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-jose/go-jose/v4"

	"gitea.lan/matthew/fog/internal/auth"
	"gitea.lan/matthew/fog/internal/identity"
	"gitea.lan/matthew/fog/internal/oidc"
	"gitea.lan/matthew/fog/internal/randx"
	"gitea.lan/matthew/fog/internal/signing"
	"gitea.lan/matthew/fog/internal/store"
)

const (
	// visitorCookie holds a visitor's session on the host's fog origin.
	visitorCookie = "__Host-fog_visitor"
	// visitTxnCookie binds a visit in flight to the browser that began it:
	// the callback's state must match it, so a code can't be planted.
	visitTxnCookie = "__Host-fog_visit"
	// visitorSessionTTL is how long a visit lasts before the visitor goes
	// home to prove themselves again. Shorter than a person's session: the
	// host can't see the home's suspensions except through its word.
	visitorSessionTTL = 12 * time.Hour
	// visitTokenTyp marks the home's ID token, so nothing else the home
	// signs can stand in for one.
	visitTokenTyp = "fog-visit+jwt"
)

// fogIssuer is how one fog names another in the visit flow: by instance,
// which the pairing pinned, never by hostname.
func fogIssuer(instance string) string { return "fog:" + instance }

func peerName(p *store.FedPeer) string {
	if p.Label != "" {
		return p.Label
	}
	return p.Name
}

// visitParams is what the host pushes to the home, and the home keeps.
type visitParams struct {
	RedirectURI   string `json:"redirectUri"`
	State         string `json:"state"`
	Nonce         string `json:"nonce"`
	CodeChallenge string `json:"codeChallenge"`
	// App is the host's app, for the home's pages ("Stream at …").
	App string `json:"app"`
	// Prompt "login" and ACR phr are the host's step-up, carried home.
	Prompt string `json:"prompt,omitempty"`
	ACR    string `json:"acr,omitempty"`
	Began  int64  `json:"began"`
}

// visitClaims is the home's ID token for a visit.
type visitClaims struct {
	Iss      string   `json:"iss"`
	Aud      string   `json:"aud"`
	Sub      string   `json:"sub"`
	Exp      int64    `json:"exp"`
	Iat      int64    `json:"iat"`
	Nonce    string   `json:"nonce"`
	SID      string   `json:"sid"`
	AMR      []string `json:"amr"`
	ACR      string   `json:"acr"`
	AuthTime int64    `json:"auth_time"`
	Handle   string   `json:"preferred_username"`
	Name     string   `json:"name"`
	Home     string   `json:"fog_home"`
	// SessionExp is when the home session ends: the host's visit never
	// outlasts it (unix seconds; absent from an older home).
	SessionExp int64 `json:"fog_session_exp,omitempty"`
}

// ---- home side ----

// visitAllows is the home's outbound rule for one of its people visiting
// peer: people.visit granted (with people.directory, without which the peer
// doesn't know them), the person named by its config, and a member.
func (s *Server) visitAllows(peerID int64, p *store.Person) bool {
	if p.Status != store.PersonStatusActive || (p.Role != store.PersonRoleOperator && p.Role != store.PersonRoleMember) {
		return false
	}
	if _, ok, _ := s.store.FedCapabilityConfig(peerID, store.CapPeopleDirectory, store.DirGrant); !ok {
		return false
	}
	cfg, ok, _ := s.store.FedCapabilityConfig(peerID, store.CapPeopleVisit, store.DirGrant)
	return ok && peopleConfigAllows(cfg, p.Handle)
}

// peopleConfigAllows reads {"people": "members" | ["handle", …]}. It fails
// closed: a config that is missing, unreadable, or names no people in a form
// it knows lets nobody visit — the home's word is given only as written.
func peopleConfigAllows(cfg, handle string) bool {
	var c struct {
		People json.RawMessage `json:"people"`
	}
	if json.Unmarshal([]byte(cfg), &c) != nil || len(c.People) == 0 {
		return false
	}
	var list []string
	if json.Unmarshal(c.People, &list) == nil {
		for _, h := range list {
			if h == handle {
				return true
			}
		}
		return false
	}
	var all string
	return json.Unmarshal(c.People, &all) == nil && (all == "members" || all == "all")
}

// handleFedVisitPAR takes the host's pushed request.
func (s *Server) handleFedVisitPAR(w http.ResponseWriter, r *http.Request) {
	peer, body, ok := s.fedAuthed(w, r, 1<<14)
	if !ok {
		return
	}
	iss := s.issuer()
	if iss == "" {
		writeErr(w, http.StatusServiceUnavailable, "this fog has no domain, so its people can't sign in elsewhere")
		return
	}
	if _, granted, _ := s.store.FedCapabilityConfig(peer.ID, store.CapPeopleVisit, store.DirGrant); !granted {
		fedReject(w, "forbidden", "this household doesn't let its people visit yours")
		return
	}
	var p visitParams
	if err := json.Unmarshal(body, &p); err != nil || p.State == "" || p.Nonce == "" || len(p.CodeChallenge) != 43 {
		badRequest(w, "invalid visit request")
		return
	}
	// The browser goes back only to the peer itself: its /visit/callback, on
	// the address the pairing knows it by — never a host the request names.
	u, err := url.Parse(p.RedirectURI)
	if err != nil || u.Scheme != "https" || u.Path != "/visit/callback" || u.RawQuery != "" || u.Fragment != "" || u.User != nil ||
		!peerOwnsHost(peer, u.Host) {
		badRequest(w, "redirectUri must be https://…/visit/callback on the paired fog's own address")
		return
	}
	if p.Prompt != "" && p.Prompt != "login" {
		badRequest(w, "prompt must be login or absent")
		return
	}
	if p.ACR != "" && p.ACR != identity.ACRPhishingResistant {
		badRequest(w, "acr must be "+identity.ACRPhishingResistant+" or absent")
		return
	}
	if len([]rune(p.App)) > 60 {
		p.App = string([]rune(p.App)[:60])
	}
	p.Began = time.Now().UnixMilli()
	token := randx.Token()
	if err := s.store.CreateVisitRequest(auth.DigestToken(token), store.VisitPAR, peer.ID, 0, mustMarshal(p), "", oidc.PARTTL); err != nil {
		writeError(w, err)
		return
	}
	s.store.TouchFedPeerSeen(peer.ID)
	s.writeFedSignedJSON(w, r, map[string]any{
		"authorize": iss + "/visit/authorize?" + url.Values{"request_uri": {token}}.Encode(),
		"expiresIn": int(oidc.PARTTL.Seconds()),
	})
}

// peerOwnsHost says whether host (with any port) is the address the pairing
// knows peer by: its URL, delivered at pairing and moved only by its signed
// heartbeats.
func peerOwnsHost(peer *store.FedPeer, host string) bool {
	pu, err := url.Parse(peer.URL)
	if err != nil || pu.Hostname() == "" || host == "" {
		return false
	}
	return strings.EqualFold(stripPort(host), pu.Hostname())
}

// handleVisitAuthorize is the home's front channel: its own person, signed
// in here with their own passkey, is sent back to the host with a code —
// once they have said so. The GET asks; only a same-origin POST answers, so
// a paired host navigating someone's browser here learns nothing about who
// they are without them.
func (s *Server) handleVisitAuthorize(w http.ResponseWriter, r *http.Request) {
	limitForm(w, r)
	if err := r.ParseForm(); err != nil {
		badRequest(w, "malformed request")
		return
	}
	token := r.Form.Get("request_uri")
	digest := auth.DigestToken(token)
	row, err := s.store.PeekVisitRequest(digest, store.VisitPAR)
	var p visitParams
	if token == "" || err != nil || json.Unmarshal([]byte(row.Params), &p) != nil {
		s.renderSigninPage(w, http.StatusBadRequest, "This sign-in link has expired", "Go back to the app and sign in again.")
		return
	}
	peer, err := s.store.FedPeerByID(row.PeerID)
	if err != nil {
		s.renderSigninPage(w, http.StatusBadRequest, "This sign-in link has expired", "Go back to the app and sign in again.")
		return
	}
	where := p.App + " at " + peerName(peer)
	back := "/visit/authorize?" + url.Values{"request_uri": {token}}.Encode()
	pc, signedIn := s.resolvePerson(r)
	if !signedIn {
		http.Redirect(w, r, "/login?"+url.Values{"next": {back}, "app": {where}}.Encode(), http.StatusFound)
		return
	}
	if !s.visitAllows(peer.ID, pc.Person) {
		s.renderSigninPage(w, http.StatusForbidden, "Not shared", fmt.Sprintf("%s doesn't let you sign in at %s.", s.householdName(), peerName(peer)))
		return
	}
	// The host's step-up, judged here, where the person can meet it.
	fresh := p.Began > 0 && pc.Session.AuthTime.After(time.UnixMilli(p.Began))
	switch {
	case p.ACR == identity.ACRPhishingResistant && identity.Level(pc.Session.AMR) != identity.ACRPhishingResistant:
		http.Redirect(w, r, "/confirm?"+url.Values{"next": {back}, "app": {where}, "with": {"passkey"}}.Encode(), http.StatusFound)
		return
	case p.Prompt == "login" && !fresh:
		http.Redirect(w, r, "/confirm?"+url.Values{"next": {back}, "app": {where}}.Encode(), http.StatusFound)
		return
	}
	if r.Method != http.MethodPost || r.Form.Get("confirm") != "1" || !sameHost(r.Header.Get("Origin"), r.Host) {
		form := fmt.Sprintf(`<form method="post" action="/visit/authorize"><input type="hidden" name="request_uri" value="%s"><input type="hidden" name="confirm" value="1"><button type="submit">Continue as %s</button></form>`,
			html.EscapeString(token), html.EscapeString(displayOr(pc.Person)))
		s.renderSigninPageTo(w, http.StatusOK, "Sign in at "+peerName(peer)+"?",
			fmt.Sprintf("%s will learn your name and handle, and that you sign in at %s. Signing out here signs you out there too.", where, s.householdName()),
			p.RedirectURI, form)
		return
	}
	if _, err := s.store.SpendVisitRequest(digest, store.VisitPAR); err != nil {
		s.renderSigninPage(w, http.StatusBadRequest, "This sign-in link was already used", "Go back to the app and sign in again.")
		return
	}
	code := randx.Token()
	if err := s.store.CreateVisitRequest(auth.DigestToken(code), store.VisitCode, peer.ID, pc.Session.ID, mustMarshal(p), "", oidc.CodeTTL); err != nil {
		writeError(w, err)
		return
	}
	// Whom to tell when this session ends: the host, which then tells its
	// apps.
	if err := s.store.RecordVisitOut(pc.Session.ID, peer.ID); err != nil {
		writeError(w, err)
		return
	}
	s.audit(r, "visit.signin", peerName(peer), map[string]any{"app": p.App})
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, oidc.Redirect(p.RedirectURI, fogIssuer(s.fed.InstanceID()), p.State, url.Values{"code": {code}}), http.StatusFound)
}

// handleFedVisitToken redeems a code for the home's word on who visited.
func (s *Server) handleFedVisitToken(w http.ResponseWriter, r *http.Request) {
	peer, body, ok := s.fedAuthed(w, r, 1<<12)
	if !ok {
		return
	}
	var req struct {
		Code         string `json:"code"`
		CodeVerifier string `json:"codeVerifier"`
	}
	invalid := func() { writeJSON(w, http.StatusBadRequest, map[string]string{"error": string(oidc.ErrInvalidGrant)}) }
	if json.Unmarshal(body, &req) != nil || req.Code == "" {
		invalid()
		return
	}
	row, err := s.store.SpendVisitCode(auth.DigestToken(req.Code))
	if errors.Is(err, store.ErrReplayed) {
		// A code redeemed twice was seen by someone it wasn't for: whatever
		// the first redemption became at the host ends (RFC 6749 §4.1.2).
		s.recallReplayedVisit(row)
		invalid()
		return
	}
	var p visitParams
	if err != nil || row.PeerID != peer.ID || json.Unmarshal([]byte(row.Params), &p) != nil || !oidc.VerifyPKCE(p.CodeChallenge, req.CodeVerifier) {
		invalid()
		return
	}
	sess, person, err := s.store.SessionByID(row.SessionID)
	if err != nil || !s.visitAllows(peer.ID, person) {
		invalid()
		return
	}
	inst := s.fed.InstanceID()
	now := time.Now()
	c := visitClaims{
		Iss: fogIssuer(inst), Aud: fogIssuer(peer.InstanceID), Sub: identity.Address(inst, person.UID),
		Iat: now.Unix(), Exp: now.Add(oidc.IDTokenTTL).Unix(), Nonce: p.Nonce,
		SID: sess.SID, AMR: sess.AMR, ACR: identity.Level(sess.AMR), AuthTime: sess.AuthTime.Unix(),
		Handle: person.Handle, Name: displayOr(person), Home: inst, SessionExp: sess.ExpiresAt.Unix(),
	}
	if c.AMR == nil {
		c.AMR = []string{}
	}
	tok, err := s.keys.Sign(c, visitTokenTyp)
	if err != nil {
		writeError(w, err)
		return
	}
	s.writeFedSignedJSON(w, r, map[string]any{"idToken": tok})
}

// recallReplayedVisit ends, at the peer the code was minted for, every visit
// the code's session became there, and says so in the audit log.
func (s *Server) recallReplayedVisit(row *store.VisitRequest) {
	sid := ""
	if sess, _, err := s.store.SessionByID(row.SessionID); err == nil {
		sid = sess.SID
	}
	peer := strconv.FormatInt(row.PeerID, 10)
	if p, err := s.store.FedPeerByID(row.PeerID); err == nil {
		peer = peerName(p)
	}
	s.auditAs("fog", "visit.code.replayed", peer, nil)
	if sid == "" {
		return // the session is gone, and its end was owed to the peer then
	}
	if err := s.store.EnqueueVisitLogout(row.PeerID, sid); err != nil {
		s.log.Warn("visit: recall a replayed code", "err", err)
		return
	}
	s.kickLogouts()
}

// handleFedVisitKeys serves the home's signing keys to a paired fog, signed
// by the pairing: what a visit's ID token must verify against.
func (s *Server) handleFedVisitKeys(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.fedAuthed(w, r, 0); !ok {
		return
	}
	set, err := s.keys.JWKS()
	if err != nil {
		writeError(w, err)
		return
	}
	s.writeFedSignedJSON(w, r, set)
}

// ---- host side ----

// handleVisitPeers lists the households whose people may visit an app: the
// login page's "From another household?".
func (s *Server) handleVisitPeers(w http.ResponseWriter, r *http.Request) {
	out := []map[string]any{}
	app := r.URL.Query().Get("app")
	if cap, err := s.store.ClientVisitorRole(app); err == nil && cap != "" {
		grants, err := s.store.VisitorGrants(app)
		if err != nil {
			writeError(w, err)
			return
		}
		for _, g := range grants {
			if p, err := s.store.FedPeerByID(g.PeerID); err == nil {
				out = append(out, map[string]any{"id": p.ID, "name": peerName(p)})
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// visitTxn is what the host remembers while the browser is at the home.
type visitTxn struct {
	Nonce    string `json:"nonce"`
	Verifier string `json:"verifier"`
	Next     string `json:"next"`
}

// handleVisitStart sends a visitor home to sign in: next is the
// authorization request they return to.
func (s *Server) handleVisitStart(w http.ResponseWriter, r *http.Request) {
	iss, ok := s.requireIssuer(w)
	if !ok {
		return
	}
	q := r.URL.Query()
	next := q.Get("next")
	nextURL, err := url.Parse(next)
	if err != nil || nextURL.Path != "/oauth2/authorize" || nextURL.Host != "" || nextURL.Scheme != "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		s.renderSigninPage(w, http.StatusBadRequest, "This sign-in link isn't valid", "Go back to the app and sign in again.")
		return
	}
	clientID := nextURL.Query().Get("client_id")
	peerID, _ := strconv.ParseInt(q.Get("peer"), 10, 64)
	peer, err := s.store.FedPeerByID(peerID)
	if err != nil {
		s.renderSigninPage(w, http.StatusBadRequest, "That household isn't paired", "Go back to the app and sign in again.")
		return
	}
	// The home sends the visitor back only to the address it knows this fog
	// by, so that address must be where the visit returns: fog's own origin.
	if adv, err := url.Parse(s.fed.AdvertisedURL()); err != nil || !strings.EqualFold(adv.Hostname(), strings.TrimPrefix(iss, "https://")) {
		s.renderSigninPage(w, http.StatusConflict, "Visitors can't sign in here yet",
			fmt.Sprintf("Other households know this fog by %s, not %s, where sign-in lives. Set this fog's federation address to %s under Settings › Federation.", s.fed.AdvertisedURL(), iss, iss))
		return
	}
	cap, _ := s.store.ClientVisitorRole(clientID)
	if granted, err := s.store.VisitorGrant(clientID, peer.ID); err != nil || cap == "" || granted == "" {
		s.renderSigninPage(w, http.StatusForbidden, "Not open to visitors", fmt.Sprintf("%s isn't open to people from %s.", clientID, peerName(peer)))
		return
	}
	// Each start holds a request at the home: bounded per real address
	// (signin_handlers_limits.go), never by a bucket everyone shares.
	if !s.limitStranger(w, r, "visit", "too many sign-ins; wait a minute") {
		return
	}
	state, nonce, verifier := randx.Token(), randx.Token(), randx.Token()
	p := visitParams{
		RedirectURI: iss + "/visit/callback", State: state, Nonce: nonce,
		CodeChallenge: oidc.ChallengeFor(verifier), App: clientID,
	}
	if q.Get("prompt") == "login" {
		p.Prompt = "login"
	}
	if q.Get("acr") == identity.ACRPhishingResistant {
		p.ACR = identity.ACRPhishingResistant
	}
	resp, err := s.fed.SignedPostVerified(r.Context(), peer, "/fed/v1/signin/par", "application/json", []byte(mustMarshal(p)))
	var par struct {
		Authorize string `json:"authorize"`
	}
	if err == nil {
		err = json.Unmarshal(resp, &par)
	}
	au, perr := url.Parse(par.Authorize)
	if err != nil || perr != nil || au.Scheme != "https" || au.Path != "/visit/authorize" {
		s.log.Warn("visit: home refused", "peer", peer.Name, "err", err)
		s.renderSigninPage(w, http.StatusBadGateway, "Couldn't reach "+peerName(peer), fmt.Sprintf("%s didn't take the sign-in. It may be offline, or may not let its people sign in here.", peerName(peer)))
		return
	}
	if err := s.store.CreateVisitRequest(auth.DigestToken(state), store.VisitTxn, peer.ID, 0,
		mustMarshal(visitTxn{Nonce: nonce, Verifier: verifier, Next: next}), "", oidc.PARTTL); err != nil {
		writeError(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: visitTxnCookie, Value: state, Path: "/", HttpOnly: true, Secure: true,
		SameSite: http.SameSiteLaxMode, MaxAge: int(oidc.PARTTL.Seconds())})
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, au.String(), http.StatusFound)
}

// handleVisitCallback is the home's answer: verify it against the keys the
// pairing vouches for, open a visitor session, and resume the app's sign-in.
func (s *Server) handleVisitCallback(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireIssuer(w); !ok {
		return
	}
	limits := s.credentialBuckets(r, "")
	if refuseLimited(w, "too many sign-ins; wait a minute", limits...) {
		return
	}
	q := r.URL.Query()
	state := q.Get("state")
	c, cerr := r.Cookie(visitTxnCookie)
	http.SetCookie(w, &http.Cookie{Name: visitTxnCookie, Value: "", Path: "/", HttpOnly: true, Secure: true, MaxAge: -1})
	if state == "" || cerr != nil || subtle.ConstantTimeCompare([]byte(c.Value), []byte(state)) != 1 {
		s.renderSigninPage(w, http.StatusBadRequest, "This sign-in didn't start here", "Go back to the app and sign in again.")
		return
	}
	row, err := s.store.SpendVisitRequest(auth.DigestToken(state), store.VisitTxn)
	var txn visitTxn
	if err != nil || json.Unmarshal([]byte(row.Params), &txn) != nil {
		s.renderSigninPage(w, http.StatusBadRequest, "This sign-in has expired", "Go back to the app and sign in again.")
		return
	}
	peer, err := s.store.FedPeerByID(row.PeerID)
	if err != nil {
		s.renderSigninPage(w, http.StatusBadRequest, "That household isn't paired any more", "Go back to the app.")
		return
	}
	// RFC 9207: the answer must say it is from the home we sent them to.
	if q.Get("iss") != fogIssuer(peer.InstanceID) {
		s.renderSigninPage(w, http.StatusBadRequest, "This sign-in came back from the wrong place", "Go back to the app and sign in again.")
		return
	}
	if q.Get("code") == "" {
		s.renderSigninPage(w, http.StatusForbidden, "Not signed in", peerName(peer)+" didn't sign you in.")
		return
	}
	claims, err := s.redeemVisit(r, peer, q.Get("code"), txn)
	if err != nil {
		s.log.Warn("visit: callback", "peer", peer.Name, "err", err)
		s.renderSigninPage(w, http.StatusBadGateway, "Couldn't finish signing in", fmt.Sprintf("%s's answer didn't check out. Try again.", peerName(peer)))
		return
	}
	uid := strings.TrimPrefix(claims.Sub, "person:"+peer.InstanceID+"/")
	// A new visit replaces the browser's last one, rather than leaving it to
	// run out beside it.
	if old, err := r.Cookie(visitorCookie); err == nil && old.Value != "" {
		s.store.DeleteVisitorSessionByDigest(auth.DigestToken(old.Value))
	}
	// A visit lasts twelve hours, and never past the home session it came
	// from: an app capping its session at the visit's end is capped at
	// the home's too.
	expires := time.Now().Add(visitorSessionTTL)
	if claims.SessionExp > 0 && time.Unix(claims.SessionExp, 0).Before(expires) {
		expires = time.Unix(claims.SessionExp, 0)
	}
	token := randx.Token()
	_, err = s.store.CreateVisitorSession(auth.DigestToken(token), randx.Token(), store.VisitorSession{
		PeerID: peer.ID, Address: identity.Address(peer.InstanceID, uid), Handle: claims.Handle, DisplayName: claims.Name,
		HomeSID: claims.SID, AMR: claims.AMR, AuthTime: time.Unix(claims.AuthTime, 0), ExpiresAt: expires,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	refund(limits...)
	s.auditAs(claims.Handle+"@"+peer.Name, "visit.arrive", txn.Next, nil)
	http.SetCookie(w, &http.Cookie{Name: visitorCookie, Value: token, Path: "/", HttpOnly: true, Secure: true,
		SameSite: http.SameSiteLaxMode, MaxAge: int(time.Until(expires).Seconds())})
	http.Redirect(w, r, txn.Next, http.StatusFound)
}

// redeemVisit trades the code for the home's ID token and checks it: signed
// by a key the home serves over the pairing, for us, from the home, about
// one of the home's own people — never a third household's — and fresh.
func (s *Server) redeemVisit(r *http.Request, peer *store.FedPeer, code string, txn visitTxn) (*visitClaims, error) {
	resp, err := s.fed.SignedPostVerified(r.Context(), peer, "/fed/v1/signin/token", "application/json",
		[]byte(mustMarshal(map[string]string{"code": code, "codeVerifier": txn.Verifier})))
	if err != nil {
		return nil, err
	}
	var tr struct {
		IDToken string `json:"idToken"`
	}
	if err := json.Unmarshal(resp, &tr); err != nil {
		return nil, err
	}
	var set jose.JSONWebKeySet
	if _, err := s.fed.SignedGetJSON(r.Context(), peer, "/fed/v1/signin/keys", "", &set); err != nil {
		return nil, err
	}
	payload, typ, err := signing.VerifyTyped(set, tr.IDToken)
	if err != nil {
		return nil, err
	}
	var c visitClaims
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	switch {
	case typ != visitTokenTyp:
		return nil, fmt.Errorf("token type %q", typ)
	case c.Iss != fogIssuer(peer.InstanceID) || c.Home != peer.InstanceID:
		return nil, fmt.Errorf("issued by %q, not the paired home", c.Iss)
	case c.Aud != fogIssuer(s.fed.InstanceID()):
		return nil, fmt.Errorf("issued for %q, not us", c.Aud)
	case c.Exp < now || c.Iat > now+60:
		return nil, fmt.Errorf("expired or from the future")
	case subtle.ConstantTimeCompare([]byte(c.Nonce), []byte(txn.Nonce)) != 1:
		return nil, fmt.Errorf("nonce mismatch")
	case !strings.HasPrefix(c.Sub, "person:"+peer.InstanceID+"/") || !isPersonULID(strings.TrimPrefix(c.Sub, "person:"+peer.InstanceID+"/")):
		return nil, fmt.Errorf("subject %q isn't one of the home's people", c.Sub)
	case c.SID == "" || c.Handle == "":
		return nil, fmt.Errorf("token lacks sid or handle")
	}
	return &c, nil
}

// resolveVisitor finds the visitor behind a request on fog's origin.
func (s *Server) resolveVisitor(r *http.Request) (*store.VisitorSession, bool) {
	c, err := r.Cookie(visitorCookie)
	if err != nil || c.Value == "" {
		return nil, false
	}
	v, err := s.store.VisitorSessionByDigest(auth.DigestToken(c.Value))
	if err != nil {
		return nil, false
	}
	return v, true
}

func (s *Server) clearVisitorCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: visitorCookie, Value: "", Path: "/", HttpOnly: true, Secure: true, MaxAge: -1})
}

// decideVisitor is policy for a visitor at one app.
func (s *Server) decideVisitor(v *store.VisitorSession, client *oidc.Client, req identity.Request) (identity.Decision, error) {
	cap, err := s.store.ClientVisitorRole(client.ID)
	if err != nil {
		return identity.Decision{}, err
	}
	granted, err := s.store.VisitorGrant(client.ID, v.PeerID)
	if err != nil && err != store.ErrNotFound {
		return identity.Decision{}, err
	}
	return identity.DecideVisitor(identity.Visitor{Granted: granted, Cap: cap, Roles: client.Roles, ACR: client.ACR},
		identity.Assurance{AMR: v.AMR, AuthTime: v.AuthTime}, req, time.Now()), nil
}

// visitorSubject is a visitor as claims describe them: their home's
// address and the name they go by there; never an email.
func (s *Server) visitorSubject(v *store.VisitorSession) (oidc.Subject, error) {
	peer, err := s.store.FedPeerByID(v.PeerID)
	if err != nil {
		return oidc.Subject{}, err
	}
	name := v.DisplayName
	if name == "" {
		name = v.Handle
	}
	return oidc.Subject{Address: v.Address, Home: peer.InstanceID, Handle: v.Handle, Name: name, Visitor: true}, nil
}

// visitorPrincipal is a visitor signed in here, as oidcPrincipal.
type visitorPrincipal struct {
	s *Server
	v *store.VisitorSession
}

func (vp visitorPrincipal) subject() (oidc.Subject, error) {
	sub, err := vp.s.visitorSubject(vp.v)
	if err != nil {
		return oidc.Subject{}, &oidc.Error{Code: oidc.ErrInvalidGrant, Description: "that household isn't paired any more"}
	}
	return sub, nil
}

// createAccessToken keeps codeDigest with the token, so a replay of the code
// revokes a visitor's tokens as it does a person's.
func (vp visitorPrincipal) createAccessToken(digest, clientID, codeDigest string, grant oidc.Grant) error {
	return vp.s.store.CreateVisitorAccessToken(digest, clientID, vp.v.ID, codeDigest, mustMarshal(grant), oidc.AccessTokenTTL)
}

func (vp visitorPrincipal) recordSignIn(clientID string) error {
	return vp.s.store.RecordVisitorSignIn(vp.v.ID, clientID)
}

func (vp visitorPrincipal) sessionExpires() time.Time { return vp.v.ExpiresAt }

// stillAllowed refuses a grant the host no longer gives this household's
// visitors at this app: withdrawn, or another role.
func (vp visitorPrincipal) stillAllowed(sc *store.OIDCClient, grant oidc.Grant) *oidc.Error {
	d, err := vp.s.decideVisitor(vp.v, toOIDCClient(sc), identity.Request{})
	if err != nil || d.Outcome == identity.Deny || d.Role != grant.Role {
		return &oidc.Error{Code: oidc.ErrInvalidGrant, Description: "visitors from that household are no longer welcome here as that"}
	}
	return nil
}

func (vp visitorPrincipal) auditActor() string { return vp.v.Handle + "@visitor" }

func (vp visitorPrincipal) auditDetail(grant oidc.Grant) map[string]any {
	return map[string]any{"role": grant.Role, "acr": grant.ACR, "visitor": vp.v.Address}
}

// visitorForSession is the visitor behind a visitor session, while it lasts.
func (s *Server) visitorForSession(id int64) (visitorPrincipal, *oidc.Error) {
	v, err := s.store.VisitorSessionByID(id)
	if err != nil {
		return visitorPrincipal{}, &oidc.Error{Code: oidc.ErrInvalidGrant, Description: "the visitor is no longer signed in"}
	}
	return visitorPrincipal{s: s, v: v}, nil
}

// visitorForGrant is visitorForSession at the token endpoint, where the
// host may have withdrawn its grant between the code and now.
func (s *Server) visitorForGrant(sc *store.OIDCClient, grant oidc.Grant) (visitorPrincipal, *oidc.Error) {
	vp, gone := s.visitorForSession(grant.VisitorSessionID)
	if gone != nil {
		return vp, gone
	}
	if d, err := s.decideVisitor(vp.v, toOIDCClient(sc), identity.Request{}); err != nil || d.Outcome == identity.Deny {
		return visitorPrincipal{}, &oidc.Error{Code: oidc.ErrInvalidGrant, Description: "visitors from that household are no longer welcome here"}
	}
	return vp, nil
}

// handleFedVisitLogout is the home saying one of its sessions ended: every
// visit it became here ends, and the apps hear through the outbox.
func (s *Server) handleFedVisitLogout(w http.ResponseWriter, r *http.Request) {
	peer, body, ok := s.fedAuthed(w, r, 1<<12)
	if !ok {
		return
	}
	var req struct {
		SID string `json:"sid"`
	}
	if json.Unmarshal(body, &req) != nil || req.SID == "" {
		badRequest(w, "sid is required")
		return
	}
	n, err := s.store.EndVisitorSessions(peer.ID, req.SID)
	if err != nil {
		writeError(w, err)
		return
	}
	if n > 0 {
		s.kickLogouts()
	}
	s.writeFedSignedJSON(w, r, map[string]any{"ok": true, "ended": n})
}

// sendVisitLogout is the outbox's delivery of a visit_logout: tell the peer
// that hosted one of our sessions that it ended.
func (s *Server) sendVisitLogout(ctx context.Context, peerID int64, sid string) error {
	peer, err := s.store.FedPeerByID(peerID)
	if err != nil {
		return nil // unpaired: its visitor sessions went with the pairing
	}
	_, err = s.fed.SignedPostVerified(ctx, peer, "/fed/v1/signin/logout", "application/json", []byte(mustMarshal(map[string]string{"sid": sid})))
	return err
}

// authorizeVisitor is handleAuthorize for a visitor: the host's policy on
// the home's word. A step-up sends them home once; arriving back still
// short of it is a refusal, not a loop. txn is the held request's digest,
// "" when the request came in the query.
func (s *Server) authorizeVisitor(w http.ResponseWriter, r *http.Request, iss string, client *oidc.Client, req oidc.AuthorizeRequest, v *store.VisitorSession,
	txn string, fail func(oidc.Code, string), hold func() (string, bool)) {
	d, err := s.decideVisitor(v, client, identity.Request{ACR: req.ACR})
	if err != nil {
		writeError(w, err)
		return
	}
	held := txn != ""
	stale := staleFor(req, v.AuthTime)
	switch {
	case d.Outcome == identity.Deny:
		if req.Prompt == "none" {
			fail(oidc.ErrAccessDenied, d.Reason.String())
			return
		}
		s.renderSigninPage(w, http.StatusForbidden, "Not shared", fmt.Sprintf("%s isn't open to you as a visitor.", client.ID))
		return
	case req.Prompt == "none" && (stale || d.Outcome == identity.StepUp):
		fail(oidc.ErrInteractionRequired, "the app asked to confirm it's you")
		return
	case d.Outcome == identity.StepUp || stale:
		// Been home for this request already and still short: the home
		// couldn't give what the app asks.
		if held && req.Began > 0 && v.AuthTime.After(time.UnixMilli(req.Began)) && d.Outcome == identity.StepUp {
			s.renderSigninPage(w, http.StatusForbidden, "A passkey is needed", fmt.Sprintf("%s asks for a passkey, and your household's fog didn't confirm one.", client.ID))
			return
		}
		back, ok := hold()
		if !ok {
			return
		}
		q := url.Values{"peer": {strconv.FormatInt(v.PeerID, 10)}, "next": {back}, "prompt": {"login"}}
		if d.Outcome == identity.StepUp && d.ACR == identity.ACRPhishingResistant {
			q.Set("acr", identity.ACRPhishingResistant)
		}
		http.Redirect(w, r, "/visit/start?"+q.Encode(), http.StatusFound)
		return
	}
	grant := oidc.Grant{Request: req, Role: d.Role, SID: v.SID, AMR: v.AMR, ACR: d.ACR, AuthTime: v.AuthTime, VisitorSessionID: v.ID}
	s.issueCode(w, r, iss, client, req, grant, 0, txn)
}

// ---- the Sign-in tab: which households' people may visit an app ----

// visitorGrantsView is an app's visitor settings: the most it lets a
// visitor be, the roles at or below that, and every paired household with
// its grant ("" = not open to them).
type visitorGrantsView struct {
	Cap    string             `json:"cap"`
	Roles  []string           `json:"roles"`
	Grants []visitorGrantView `json:"grants"`
}

type visitorGrantView struct {
	PeerID int64  `json:"peerId"`
	Name   string `json:"name"`
	Role   string `json:"role"`
	// HomeAllows: the peer's heartbeat says it lets its people visit us.
	// Display only; the home enforces its own rule.
	HomeAllows bool `json:"homeAllows"`
}

func (s *Server) visitorClient(w http.ResponseWriter, r *http.Request) (*store.OIDCClient, string, bool) {
	a, ok := s.appByPath(w, r)
	if !ok {
		return nil, "", false
	}
	c, err := s.store.OIDCClientForApp(a.ID)
	if err != nil {
		notFound(w, a.Name+" doesn't sign in with fog")
		return nil, "", false
	}
	cap, err := s.store.ClientVisitorRole(c.ClientID)
	if err != nil {
		writeError(w, err)
		return nil, "", false
	}
	return c, cap, true
}

func (s *Server) handleGetVisitorGrants(w http.ResponseWriter, r *http.Request) {
	c, cap, ok := s.visitorClient(w, r)
	if !ok {
		return
	}
	view := visitorGrantsView{Cap: cap, Roles: []string{}, Grants: []visitorGrantView{}}
	if cap != "" {
		for _, role := range c.Roles {
			if identity.VisitorRoleAllowed(c.Roles, cap, role) {
				view.Roles = append(view.Roles, role)
			}
		}
	}
	grants, err := s.store.VisitorGrants(c.ClientID)
	if err != nil {
		writeError(w, err)
		return
	}
	byPeer := map[int64]string{}
	for _, g := range grants {
		byPeer[g.PeerID] = g.Role
	}
	peers, err := s.store.ListFedPeers()
	if err != nil {
		writeError(w, err)
		return
	}
	for _, p := range peers {
		var remote []string
		json.Unmarshal(p.RemoteGrants, &remote)
		view.Grants = append(view.Grants, visitorGrantView{PeerID: p.ID, Name: peerName(&p), Role: byPeer[p.ID],
			HomeAllows: slices.Contains(remote, store.CapPeopleVisit)})
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) handlePutVisitorGrants(w http.ResponseWriter, r *http.Request) {
	c, cap, ok := s.visitorClient(w, r)
	if !ok {
		return
	}
	var req struct {
		Grants []store.VisitorGrant `json:"grants"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	seen := map[int64]bool{}
	grants := []store.VisitorGrant{}
	for _, g := range req.Grants {
		if g.Role == "" {
			continue
		}
		if cap == "" {
			badRequest(w, c.ClientID+" doesn't host visitors: its fog.yaml declares no signin.visitors")
			return
		}
		if !identity.VisitorRoleAllowed(c.Roles, cap, g.Role) {
			badRequest(w, fmt.Sprintf("%q is more than %s lets a visitor be (at most %s)", g.Role, c.ClientID, cap))
			return
		}
		if _, err := s.store.FedPeerByID(g.PeerID); err != nil || seen[g.PeerID] {
			badRequest(w, "unknown or repeated household")
			return
		}
		seen[g.PeerID] = true
		grants = append(grants, g)
	}
	if err := s.store.PutVisitorGrants(c.ClientID, grants); err != nil {
		writeError(w, err)
		return
	}
	s.kickLogouts()
	s.audit(r, "signin.visitors", c.ClientID, map[string]any{"grants": grants})
	s.handleGetVisitorGrants(w, r)
}

// recallVisits ends, at the peer, the visits our people no longer may make
// there: the home withdrawing its word is as total as the host's.
func (s *Server) recallVisits(peerID int64) {
	err := s.store.RecallVisits(peerID, func(handle string) bool {
		p, err := s.store.PersonByHandle(handle)
		return err == nil && s.visitAllows(peerID, p)
	})
	if err != nil {
		s.log.Warn("visit: recall", "peer", peerID, "err", err)
		return
	}
	s.kickLogouts()
}
