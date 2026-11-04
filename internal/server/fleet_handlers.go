// The fleet views: what every app's backups and updates look like at once,
// and the one-shot notification test. Each is a read over stored facts —
// the verbs (back up now, check for updates) live with their apps.
package server

import (
	"net/http"
	"time"

	"gitea.lan/matthew/fog/internal/backup"
	"gitea.lan/matthew/fog/internal/notify"
	"gitea.lan/matthew/fog/internal/updates"
)

func (s *Server) handleBackupsStatus(w http.ResponseWriter, r *http.Request) {
	rows, err := backup.FleetStatus(s.store, time.Now().UTC())
	if err != nil {
		writeError(w, err)
		return
	}
	out := map[string]any{"apps": rows, "files": s.files.BackupStatus(time.Now().UTC())}
	if s.backup != nil {
		out["store"] = s.backup.Summary()
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleUpdatesStatus(w http.ResponseWriter, r *http.Request) {
	rows, err := updates.FleetStatus(s.store)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"apps": rows})
}

// handleNotifyTest sends one message over the configured channel and
// reports whether it arrived — the only way to know a topic is right short
// of waiting for something to break.
func (s *Server) handleNotifyTest(w http.ResponseWriter, r *http.Request) {
	n := &notify.Notifier{Store: s.store, Log: s.log}
	if err := n.SendNow(r.Context(), "Fog test notification", "If you can read this, notifications work.", true); err != nil {
		if err == notify.ErrNotConfigured {
			badRequest(w, err.Error())
			return
		}
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeOK(w)
}
