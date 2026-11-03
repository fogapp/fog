// fog's public addresses: the one the configured domain names, and the one
// a request arrived at.
package server

import (
	"net/http"
	"strings"

	"gitea.lan/matthew/fog/internal/store"
)

// forwardedSecure reports whether the browser's request reached Caddy over
// https.
func forwardedSecure(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// requestOrigin is the scheme and host the browser used for this request.
func requestOrigin(r *http.Request) string {
	if forwardedSecure(r) {
		return "https://" + r.Host
	}
	return "http://" + r.Host
}

// fogURL is fog's own address under the configured domain,
// https://fog.<domain>, or "" without a domain.
func (s *Server) fogURL() string {
	domain, _ := s.store.GetSetting(store.SettingBaseDomain)
	if domain == "" {
		return ""
	}
	return "https://fog." + domain
}
