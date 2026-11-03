package server

import "net/http"

func (s *Server) handleTunnelStatus(w http.ResponseWriter, r *http.Request) {
	st := s.tunnel.Status()
	st.Service = s.tunnelService() // signin_handlers_tunnel.go
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleTunnelStart(w http.ResponseWriter, r *http.Request) {
	if err := s.tunnel.Start(); err != nil {
		badRequest(w, err.Error())
		return
	}
	s.audit(r, "tunnel.start", "", nil)
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}

func (s *Server) handleTunnelStop(w http.ResponseWriter, r *http.Request) {
	s.tunnel.Stop()
	s.audit(r, "tunnel.stop", "", nil)
	writeOK(w)
}
