package server

import (
	"net/http"
	"runtime"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/catalog"
)

// GET /api/capacity: what this machine has, what the apps on it are using,
// and how every catalog entry would fit — assessed here, once, so the
// store's cards, the listing and Settings › Storage all read the same
// verdict and the arithmetic lives where it is tested. Host memory comes
// from the metrics sampler's newest reading, so a machine that has only
// just booted reports no memory figures rather than guessed ones.
func (s *Server) handleCapacity(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	host := catalog.Host{Cores: runtime.NumCPU()}
	var measuredAt int64
	if s.metrics != nil {
		if smp, ok := s.metrics.Latest(); ok {
			host.MemTotal, host.MemUsed = smp.MemTotal, smp.MemUsed
			measuredAt = smp.T
		}
	}
	if total, free, err := diskUsage(s.cfg.DataDir); err == nil {
		host.DiskTotal, host.DiskFree = total, free
	}

	entries := s.catalogEntries()
	list, err := s.store.ListApps()
	if err != nil {
		writeError(w, err)
		return
	}
	specs, _ := apps.LatestSpecs(s.store)
	usages, _ := s.engine.AppUsages(ctx) // Docker down: every app reads as not running
	installed := make([]catalog.Installed, 0, len(list))
	for _, a := range list {
		entry := catalog.EntryForSpec(entries, a.Name, specs[a.ID])
		u := usages[a.Name]
		installed = append(installed, catalog.NewInstalled(a.Name, entry, u.Running, u.MemUsage))
	}

	fits := make(map[string]catalog.Fit, len(entries))
	for _, e := range entries {
		fits[e.ID] = catalog.AssessFit(host, installed, e)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"host":       host,
		"measuredAt": measuredAt,
		"apps":       installed,
		"fits":       fits,
	})
}
