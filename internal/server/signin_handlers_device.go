// Chat devices and sessions (docs/signin-contract.md "Sessions and logout").
// Only the genuine sign-in endpoints — password, passkey, joining, setup —
// mint a browser session, and opening chat never changes it:
//
//   - A browser's chat device gets a credential of its own, issued as
//     `chat`: its own cookie (fog_chat, __Host-fog_chat where the session
//     is __Host-), read only on chat's routes, pinned unless it is
//     __Host-. The browser's session stays the person's full session.
//   - A native client's device gets a token of its own, issued as
//     `device`: an unpinned bearer, so minted only from a browser session
//     proven within minutes (credentialSession).
//   - Either is minted only from the browser's own session — a cookie, not
//     tied to any device — on a budget of that session's own. A bearer, a
//     chat credential or a device's token mints and binds nothing. A device
//     holds one credential at a time.
//   - A session tied to a chat device reaches chat alone, by its record
//     (pickSession), however it is presented; revoking the device ends it.
package server

import (
	"net/http"
	"strconv"
	"strings"

	"gitea.lan/matthew/fog/internal/auth"
	"gitea.lan/matthew/fog/internal/people"
	"gitea.lan/matthew/fog/internal/store"
)

const (
	// deviceTokenPrefix is where a native device's token is accepted.
	deviceTokenPrefix = "/api/chat/"
	// deviceMints is native tokens one browser session may mint per
	// failureWindow.
	deviceMints = 10
	// maxDevicesPerPerson is how many live chat devices one person holds.
	maxDevicesPerPerson = 20
)

// deviceTokenReaches says whether a native device's token counts on r.
func deviceTokenReaches(r *http.Request) bool {
	return strings.HasPrefix(r.URL.Path, deviceTokenPrefix)
}

// chatCookieName is the chat credential's cookie on r, chosen as the
// session cookie's is (hostOnlySessions).
func (s *Server) chatCookieName(r *http.Request) string {
	if s.hostOnlySessions(r) {
		return "__Host-fog_chat"
	}
	return "fog_chat"
}

// chatCandidates are the chat credentials a request carries.
func (s *Server) chatCandidates(r *http.Request) []tokenCandidate {
	name := s.chatCookieName(r)
	source := fromChatPlainCookie
	if strings.HasPrefix(name, "__Host-") {
		source = fromChatHostCookie
	}
	var out []tokenCandidate
	for _, v := range cookieValues(r, name) {
		out = append(out, tokenCandidate{v, source})
	}
	return out
}

func (s *Server) setChatCookie(w http.ResponseWriter, r *http.Request, token string) {
	c := &http.Cookie{Name: s.chatCookieName(r), Value: token, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: int(auth.SessionTTL.Seconds())}
	if strings.HasPrefix(c.Name, "__Host-") {
		c.Secure = true
	}
	http.SetCookie(w, c)
}

// clearChatCookie expires the chat credential's cookie, under both names.
func clearChatCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: "fog_chat", Value: "", Path: "/", HttpOnly: true, MaxAge: -1})
	http.SetCookie(w, &http.Cookie{Name: "__Host-fog_chat", Value: "", Path: "/", HttpOnly: true, Secure: true, MaxAge: -1})
}

// endChatCredentials ends the chat credentials this browser carries that
// aren't keep's person's (keep 0: all of them): signing out, or someone
// else signing in on a shared computer, takes chat with it.
func (s *Server) endChatCredentials(w http.ResponseWriter, r *http.Request, keep int64) {
	ended := false
	for _, name := range []string{"fog_chat", "__Host-fog_chat"} {
		for _, v := range cookieValues(r, name) {
			sess, _, err := s.store.SessionPerson(auth.DigestToken(v))
			if err != nil || !store.IsChatCredential(sess.IssuedAs) || sess.PersonID == keep {
				continue
			}
			s.store.DeletePersonSessionByDigest(auth.DigestToken(v))
			ended = true
		}
	}
	if ended || keep == 0 {
		clearChatCookie(w)
	}
}

// chatCredentialFits says whether a chat credential belongs to this browser:
// bound to a fog_browser cookie it carries (when it carries any — a
// browser always does once signed in), and the same person as the
// browser's own session when there is one. A single planted fog_chat —
// someone else's, or another browser's — is no one.
func (s *Server) chatCredentialFits(r *http.Request, sess *store.PersonSession) bool {
	if browsers := s.browserDigests(r); len(browsers) > 0 && (sess.BrowserDigest == "" || !browsers[sess.BrowserDigest]) {
		return false
	}
	if own, _, ok := s.browserSession(r); ok && own.Person.ID != sess.PersonID {
		return false
	}
	return true
}

// chatCredentialPlanted says whether r carries a fog_chat that is not this
// browser's (chatCredentialFits): expirePlantedCookies expires it wherever
// it could have been set.
func (s *Server) chatCredentialPlanted(r *http.Request) bool {
	for _, c := range s.chatCandidates(r) {
		sess, _, err := s.store.SessionPerson(auth.DigestToken(c.token))
		if err != nil || !store.IsChatCredential(sess.IssuedAs) || !s.chatCredentialFits(r, sess) {
			return true
		}
	}
	return false
}

// freshBrowser answers for a request that changes something beyond chat
// from a chat route — where the person's alerts go — unless the browser's
// own session, proven within minutes, is making it. A chat credential or a
// device's token alone never can.
func (s *Server) freshBrowser(w http.ResponseWriter, r *http.Request) bool {
	own, _, ok := s.browserSession(r)
	if !ok {
		forbidden(w, "only your browser's own sign-in can change where your alerts go; use fog in your browser")
		return false
	}
	return s.credentialSession(w, r, own, true)
}

// mayChangePush answers for a request that adds or removes a device's push
// target unless the browser's own session is making it, or the device
// itself is: a chat credential or a device's token changes its own
// device's target alone, never another's — so a stolen one can't silence
// the alerts the person's other devices get.
func (s *Server) mayChangePush(w http.ResponseWriter, r *http.Request, deviceID int64) bool {
	if _, _, ok := s.browserSession(r); ok {
		return true
	}
	if pc, _, ok := s.resolveSession(r); ok && pc.Session.DeviceID == deviceID {
		return true
	}
	forbidden(w, "only that device, or your browser's own sign-in, can change its notifications")
	return false
}

// visibleSessions is what a chat route shows of a person's sessions: all of
// them to the browser's own session, and to a chat credential or a
// device's token alone, only the devices' — never a browser session, which
// they may neither list nor end.
func (s *Server) visibleSessions(r *http.Request, rows []store.PersonSessionView) []store.PersonSessionView {
	if _, _, ok := s.browserSession(r); ok {
		return rows
	}
	out := rows[:0:0]
	for _, v := range rows {
		if v.DeviceID != 0 {
			out = append(out, v)
		}
	}
	return out
}

// browserSession is the browser's own session on r — a cookie, never a
// chat credential, a bearer or a device's — or false.
func (s *Server) browserSession(r *http.Request) (*personCtx, string, bool) {
	pc, tok, ok := s.resolveSessionFor(r, false)
	if !ok || pc.Session.DeviceID != 0 || pc.Session.IssuedAs == store.SessionDevice || store.IsChatCredential(pc.Session.IssuedAs) {
		return nil, "", false
	}
	for _, name := range s.sessionCookieNames(r) {
		for _, v := range cookieValues(r, name) {
			if v == tok {
				return pc, tok, true
			}
		}
	}
	return nil, "", false
}

// mayAttachDevice reports whether this request may attach a chat device for
// pc's person — a browser's chat credential or a native token, minted from
// the browser's own session — answering for it when it may not, and
// returns that session. Checked before anything is written.
func (s *Server) mayAttachDevice(w http.ResponseWriter, r *http.Request, pc *personCtx, clientKind string, adding bool) (*personCtx, bool) {
	minter, _, ok := s.browserSession(r)
	if !ok || minter.Person.ID != pc.Person.ID {
		forbidden(w, "only your browser's own sign-in can add or bind a device; use fog in your browser")
		return nil, false
	}
	if clientKind == "native" && !s.credentialSession(w, r, minter, true) {
		return nil, false
	}
	if s.signinLimits().deviceMint.Exhausted(minterKey(minter)) {
		w.Header().Set("Retry-After", strconv.Itoa(int(failureWindow.Seconds())))
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many devices signed in from this browser lately; wait a few minutes", "code": "rate_limited"})
		return nil, false
	}
	if adding {
		live := 0
		if list, err := s.store.PersonDevices(pc.Person.ID); err == nil {
			for _, d := range list {
				if d.RevokedAt == "" {
					live++
				}
			}
		}
		if live >= maxDevicesPerPerson {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "you have " + strconv.Itoa(live) + " chat devices; remove one first", "code": "too_many_devices"})
			return nil, false
		}
	}
	return minter, true
}

func minterKey(pc *personCtx) string { return strconv.FormatInt(pc.Session.ID, 10) }

// attachDevice mints the device's credential from minter (mayAttachDevice
// ran first): a browser's chat cookie, or a native token in out. The
// device's earlier credentials end.
func (s *Server) attachDevice(w http.ResponseWriter, r *http.Request, minter *personCtx, deviceID int64, clientKind string, out map[string]any) bool {
	if !s.signinLimits().deviceMint.Allow(minterKey(minter)) {
		w.Header().Set("Retry-After", strconv.Itoa(int(failureWindow.Seconds())))
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many devices signed in from this browser lately; wait a few minutes", "code": "rate_limited"})
		return false
	}
	kind, issued := "native", store.SessionDevice
	if clientKind != "native" {
		// Recorded as the cookie it is set as, never read from the name a
		// request presents it under.
		kind, issued = "web", store.SessionChat
		if strings.HasPrefix(s.chatCookieName(r), "__Host-") {
			issued = store.SessionChatHost
		}
	}
	token, digest := auth.NewToken()
	id, err := s.store.CreatePersonSession(minter.Person.ID, deviceID, digest, kind, auth.SessionTTL, r.UserAgent(), s.clientIP(r), []string{"pop"})
	if err == nil {
		err = s.store.SetSessionIssuedAs(id, issued)
	}
	if err == nil && store.IsChatCredential(issued) && minter.Session.BrowserDigest != "" {
		// The browser's binding (signin_handlers_cookies.go) holds for its
		// chat credential too.
		err = s.store.SetSessionBrowser(digest, minter.Session.BrowserDigest)
	}
	if err != nil {
		s.store.DeletePersonSessionByDigest(digest)
		writeError(w, err)
		return false
	}
	ended, _ := s.store.DeletePersonSessionsOfDevice(deviceID, id)
	if store.IsChatCredential(issued) {
		// The chat credential this browser held before, whichever device.
		for _, v := range cookieValues(r, s.chatCookieName(r)) {
			if sess, _, err := s.store.SessionPerson(auth.DigestToken(v)); err == nil && store.IsChatCredential(sess.IssuedAs) && sess.ID != id {
				s.store.DeletePersonSessionByDigest(auth.DigestToken(v))
				ended++
			}
		}
		s.setChatCookie(w, r, token)
	} else {
		out["token"] = token
	}
	if ended > 0 {
		s.kickLogouts()
	}
	return true
}

// maxKeyPackagesPerRegistration bounds what one registration writes.
const maxKeyPackagesPerRegistration = people.MaxKeyPackages
