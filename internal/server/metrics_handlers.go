package server

import (
	"net/http"

	"gitea.lan/matthew/fog/internal/metrics"
)

// handleSystemMetrics returns the sampler's retained host series, oldest
// first. An empty samples list means the sampler hasn't produced a reading
// yet (e.g. right after startup) — the UI says "collecting" rather than
// inventing a flat line.
func (s *Server) handleSystemMetrics(w http.ResponseWriter, r *http.Request) {
	samples := []metrics.Sample{}
	interval := 0
	if s.metrics != nil {
		samples = s.metrics.Series()
		interval = int(s.metrics.Interval().Seconds())
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"intervalSeconds": interval,
		"samples":         samples,
	})
}
