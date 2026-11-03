package server

import (
	"net/http"
	"strings"

	"gitea.lan/matthew/fog/internal/vpn"
)

func (s *Server) handleVPNStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.vpn.Status())
}

func (s *Server) handleVPNStart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Reauth bool `json:"reauth"`
	}
	if !readOptionalJSON(w, r, &body) {
		return
	}
	if err := s.vpn.Start(body.Reauth); err != nil {
		badRequest(w, err.Error())
		return
	}
	action := "vpn.start"
	if body.Reauth {
		action = "vpn.reauth"
	}
	s.audit(r, action, "", nil)
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}

func (s *Server) handleVPNStop(w http.ResponseWriter, r *http.Request) {
	s.vpn.Stop()
	s.audit(r, "vpn.stop", "", nil)
	writeOK(w)
}

// handleVPNRoutes saves an override for the advertised route. Validated
// here rather than at `tailscale up` time so a typo is refused while the
// operator is looking at the field, not two layers down in a log line.
func (s *Server) handleVPNRoutes(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Routes string `json:"routes"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	routes := strings.TrimSpace(body.Routes)
	if routes != "" {
		normalized, err := vpn.ParseRoutes(routes)
		if err != nil {
			badRequest(w, err.Error())
			return
		}
		routes = normalized
	}
	if err := s.store.SetSetting(vpn.SettingRoutes, routes); err != nil {
		writeError(w, err)
		return
	}
	s.audit(r, "vpn.routes", "", map[string]any{"routes": routes})
	// Re-advertise now: the answer the page shows must be what the daemon
	// was actually asked to do, not what it was doing a moment ago.
	s.vpn.Refresh()
	writeJSON(w, http.StatusOK, s.vpn.Status())
}
