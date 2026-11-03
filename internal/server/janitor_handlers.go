// Reclaiming disk: directories no current app accounts for. The janitor
// reports and the operator decides; nothing here deletes on its own.
package server

import (
	"net/http"

	"gitea.lan/matthew/fog/internal/janitor"
)

// handleListOrphans reports directories no current app accounts for —
// deleted apps' kept data, dropped volumes, stray backups and checkouts.
func (s *Server) handleListOrphans(w http.ResponseWriter, r *http.Request) {
	orphans, err := s.janitor.Orphans()
	if err != nil {
		writeError(w, err)
		return
	}
	if orphans == nil {
		orphans = []janitor.Orphan{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"orphans": orphans})
}

// handleReclaimOrphan deletes one reported orphan path. The janitor
// re-scans before removing, so only paths it identifies are deletable.
func (s *Server) handleReclaimOrphan(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if err := s.janitor.Reclaim(req.Path); err != nil {
		badRequest(w, err.Error())
		return
	}
	s.audit(r, "system.reclaim", req.Path, nil)
	writeOK(w)
}
