// Cookies a neighbour can plant (docs/signin-contract.md "Sessions and
// logout"). Where fog's session cookie is a plain fog_session — an http
// setup, a bare address — the browser also returns to fog a same-named
// cookie anything sharing the host set: an app on another port of the same
// address (cookies ignore ports), or a sibling host for the whole domain. A
// cookie with a longer Path is sent first, so "the first one that resolves"
// would be the planted one.
//
// So, on top of the pin (resolveSession):
//
//   - Beside every cookie session fog sets a browser cookie, fog_browser, a
//     fresh random value per sign-in, and records its digest on the session.
//     When more than one cookie session would count, only one bound to a
//     browser cookie this browser carries does — fog's own, not one planted
//     without fog's browser cookie beside it — and if that still leaves more
//     than one, none does: fog can't tell which it set, so it trusts
//     neither.
//   - Whenever a request carries a name of fog's more than once, every copy
//     that could be a plant is expired: at each path the request's own path
//     lies under — every Path a cookie sent with it can have — host-only
//     except "/", and domain-wide at all of them.
package server

import (
	"net/http"
	"strings"

	"gitea.lan/matthew/fog/internal/auth"
	"gitea.lan/matthew/fog/internal/randx"
)

// maxCookieDepth bounds how many path segments expirePlantedCookies covers.
const maxCookieDepth = 12

const (
	browserCookie       = "fog_browser"
	secureBrowserCookie = "__Host-fog_browser"
)

// browserCookieName is the browser cookie's name on r, chosen as the
// session cookie's is (hostOnlySessions).
func (s *Server) browserCookieName(r *http.Request) string {
	if s.hostOnlySessions(r) {
		return secureBrowserCookie
	}
	return browserCookie
}

// bindBrowser sets a fresh browser cookie beside a session cookie just set
// and binds the session to it.
func (s *Server) bindBrowser(w http.ResponseWriter, r *http.Request, token string) {
	nonce := randx.Token()
	c := &http.Cookie{
		Name:     browserCookie,
		Value:    nonce,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(auth.SessionTTL.Seconds()),
	}
	if s.hostOnlySessions(r) {
		c.Name, c.Secure = secureBrowserCookie, true
	}
	if err := s.store.SetSessionBrowser(auth.DigestToken(token), auth.DigestToken(nonce)); err != nil {
		s.log.Warn("binding a session to its browser", "err", err)
		return
	}
	http.SetCookie(w, c)
}

// browserDigests are the digests of every browser cookie r carries.
func (s *Server) browserDigests(r *http.Request) map[string]bool {
	out := map[string]bool{}
	for _, v := range cookieValues(r, s.browserCookieName(r)) {
		out[auth.DigestToken(v)] = true
	}
	return out
}

// clearBrowserCookie expires the browser cookie with the session cookie.
func (s *Server) clearBrowserCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: browserCookie, Value: "", Path: "/", HttpOnly: true, MaxAge: -1})
	if s.hostOnlySessions(r) {
		http.SetCookie(w, &http.Cookie{Name: secureBrowserCookie, Value: "", Path: "/", HttpOnly: true, Secure: true, MaxAge: -1})
	}
}

// expirePlantedCookies expires every copy of fog's plain cookie names a
// request carries more than one of, wherever it could have been set (see
// the file comment). fog sets its own at "/" host-only, and that one is
// left alone; a plant at "/" host-only replaced it outright and fails the
// browser binding.
func (s *Server) expirePlantedCookies(w http.ResponseWriter, r *http.Request) {
	var dup []string
	for _, name := range []string{auth.CookieName, browserCookie, "fog_chat"} {
		if len(cookieValues(r, name)) > 1 {
			dup = append(dup, name)
		}
	}
	// A single fog_chat that isn't this browser's is a plant too.
	if len(cookieValues(r, "fog_chat")) == 1 && s.chatCredentialPlanted(r) {
		dup = append(dup, "fog_chat")
	}
	if len(dup) == 0 {
		return
	}
	domain := s.sessionCookieDomain(r)
	for _, name := range dup {
		for _, p := range cookiePaths(r.URL.Path) {
			if p != "/" {
				http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: p, HttpOnly: true, MaxAge: -1})
			}
			if domain != "" {
				http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: p, Domain: domain, HttpOnly: true, MaxAge: -1})
			}
		}
	}
}

// cookiePaths is every Path a cookie sent with a request for path can have
// (RFC 6265 §5.1.4): "/" and each leading run of segments, with and without
// its trailing slash — the first maxCookieDepth of them. A plant deeper
// than that is still never used (resolveSession); it just isn't expired.
func cookiePaths(path string) []string {
	out := []string{"/"}
	for i, depth := 1, 0; i < len(path) && depth < maxCookieDepth; i++ {
		if path[i] == '/' {
			out = append(out, path[:i], path[:i+1])
			depth++
		}
	}
	if path != "/" && strings.HasPrefix(path, "/") && !strings.HasSuffix(path, "/") {
		out = append(out, path)
	}
	return out
}
