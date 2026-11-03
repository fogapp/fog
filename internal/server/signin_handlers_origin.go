// Where a browser's request may come from (docs/signin-contract.md "Sessions
// and logout"). protect already refuses a mutation whose Origin names
// another host. This adds what a browser says about itself:
//
//   - A state-changing request a browser sends from anywhere but fog's own
//     page — Sec-Fetch-Site cross-site, or same-site (a sibling app's page,
//     or another port of fog's address) — is refused, and so is one carrying
//     the browser's Sec-Fetch-Site but no Origin.
//   - The requests that make or claim a sign-in — setup, its restores and
//     claim, password and passkey sign-in, joining — are refused from a
//     browser when the Host they name isn't this box's (auth.HostAllowed): a
//     page whose DNS name was rebound to fog's address sends its own name,
//     and the same-origin check alone would pass it.
//
// Clients that aren't browsers send none of these markers and are judged by
// what they authenticate with: a web session presented any way but
// __Host-fog_session works only from the address it was signed in from
// (resolvePerson).
package server

import (
	"crypto/subtle"
	"net/http"
	"net/url"
	"strings"

	"gitea.lan/matthew/fog/internal/auth"
	"gitea.lan/matthew/fog/internal/proxy"
	"gitea.lan/matthew/fog/internal/store"
)

// hostSensitive are the routes that create or claim a session: what a DNS
// rebinding page would aim at.
func hostSensitive(r *http.Request) bool {
	p := r.URL.Path
	switch {
	case strings.HasPrefix(p, "/api/setup"):
		return r.Method != http.MethodGet || p == "/api/setup/claim"
	case p == "/api/auth/login", p == "/api/people/login", strings.HasPrefix(p, "/api/people/login/"):
		return true
	case strings.HasPrefix(p, "/api/join/"):
		return r.Method != http.MethodGet
	}
	return false
}

// browserMarked says whether a request carries what only a browser sends:
// Origin, or Sec-Fetch-Site. (Not Sec-Fetch-Mode: Node's fetch sends it on
// every request.)
func browserMarked(r *http.Request) bool {
	return r.Header.Get("Sec-Fetch-Site") != "" || r.Header.Get("Origin") != ""
}

// boxNames are the names, beyond its own addresses and hostname, this box
// answers to: fog.<domain> and the household's hosts, and the address it
// advertises to paired households (a tunnel's included).
func (s *Server) boxNames() []string {
	names := []string{"host.docker.internal"}
	if domain, _ := s.store.GetSetting(store.SettingBaseDomain); domain != "" {
		names = append(names, domain, "*."+domain)
	}
	if s.fed != nil {
		if u, err := url.Parse(s.fed.AdvertisedURL()); err == nil && u.Host != "" {
			names = append(names, u.Host)
		}
	}
	return names
}

// browserRequestAllowed applies the rules above, answering a refusal itself.
func (s *Server) browserRequestAllowed(w http.ResponseWriter, r *http.Request) bool {
	path := r.URL.Path
	mutation := r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions
	guarded := strings.HasPrefix(path, "/api/") || strings.HasPrefix(path, "/oauth2/") || strings.HasPrefix(path, "/visit/")
	if mutation && guarded {
		switch site := r.Header.Get("Sec-Fetch-Site"); site {
		case "", "same-origin", "none":
		default:
			http.Error(w, "cross-site request rejected", http.StatusForbidden)
			return false
		}
		if r.Header.Get("Origin") == "" && r.Header.Get("Sec-Fetch-Site") != "" {
			http.Error(w, "a browser's request must say where it comes from", http.StatusForbidden)
			return false
		}
	}
	if hostSensitive(r) && browserMarked(r) && !auth.HostAllowed(r.Host, s.boxNames()...) {
		http.Error(w, "this request named a host that isn't this fog", http.StatusMisdirectedRequest)
		return false
	}
	return true
}

// forwardingHeaders are what a proxy writes about the request it forwards.
var forwardingHeaders = []string{
	"X-Forwarded-Proto", "X-Forwarded-Host", "X-Forwarded-Uri", "X-Forwarded-Method",
	"X-Forwarded-For", "X-Forwarded-Port", "X-Forwarded-Prefix", "Forwarded", proxy.HeaderClientIP,
	proxy.HeaderVia,
}

// fromProxy says whether a request came from fog's own Caddy: it carries the
// forward secret only Caddy's environment holds. Without a proxy wired
// (tests, a development fogd) there is nobody to prove it, and the headers
// stand as sent.
func (s *Server) fromProxy(r *http.Request) bool {
	if s.proxy == nil {
		return true
	}
	got := r.Header.Get(proxy.HeaderSecret)
	want, err := s.proxy.ForwardSecret()
	return got != "" && err == nil && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// dropUnprovenForwarding removes X-Forwarded-* and its kin from a request that
// didn't come from Caddy, before anything reads them: the scheme decides
// which session cookie fog reads and how it pins it, the host which gated
// app and which passkey origin, the client address every limit and pin.
// Any peer — an app's container, a LAN machine at fogd's port — could
// otherwise say it was https, or someone else.
func (s *Server) dropUnprovenForwarding(r *http.Request) {
	if s.fromProxy(r) {
		return
	}
	for _, h := range forwardingHeaders {
		r.Header.Del(h)
	}
}
