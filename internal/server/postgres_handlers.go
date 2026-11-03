// The shared Postgres cluster's major and the one-button move to a newer
// one. The data directory decides what runs (pg.Inspect); this surface
// reports it and starts the upgrade (pg.Upgrader), which stops the apps,
// dumps, restores into a fresh cluster, verifies every row, and rolls back
// on any mismatch.
package server

import (
	"context"
	"errors"
	"net/http"

	"gitea.lan/matthew/fog/internal/pg"
)

func (s *Server) handlePostgresStatus(w http.ResponseWriter, r *http.Request) {
	if s.pgUpgrader == nil {
		writeErr(w, http.StatusServiceUnavailable, "the managed Postgres is not wired")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"cluster": s.pgUpgrader.PG.ClusterStatus(),
		"upgrade": s.pgUpgrader.Status(),
	})
}

func (s *Server) handlePostgresUpgrade(w http.ResponseWriter, r *http.Request) {
	if s.pgUpgrader == nil {
		writeErr(w, http.StatusServiceUnavailable, "the managed Postgres is not wired")
		return
	}
	if s.docker == nil || !s.docker.Available(r.Context()) {
		badRequest(w, "Docker is not reachable")
		return
	}
	// Background context: the upgrade must outlive this request.
	if err := s.pgUpgrader.Start(context.Background()); err != nil {
		if errors.Is(err, pg.ErrUpgradeRunning) {
			conflict(w, err.Error())
			return
		}
		writeError(w, err)
		return
	}
	s.audit(r, "postgres.upgrade", "", nil)
	s.log.Info("postgres upgrade requested", "user", s.actor(r))
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}

// handlePostgresUpgradeCancel stops a running upgrade: before the old
// cluster is retired the apps come back on it; after, it rolls back.
func (s *Server) handlePostgresUpgradeCancel(w http.ResponseWriter, r *http.Request) {
	if s.pgUpgrader == nil {
		writeErr(w, http.StatusServiceUnavailable, "the managed Postgres is not wired")
		return
	}
	if err := s.pgUpgrader.Cancel(); err != nil {
		if errors.Is(err, pg.ErrNoUpgrade) {
			conflict(w, err.Error())
			return
		}
		writeError(w, err)
		return
	}
	s.audit(r, "postgres.upgrade.cancel", "", nil)
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}
