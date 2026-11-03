package server

import (
	"context"
	"net/http"
	"time"

	"gitea.lan/matthew/fog/internal/dockerx"
	"gitea.lan/matthew/fog/internal/pg"
)

// GET /api/system/dependencies: the state of the things fog runs on but
// does not own — the two containers it supervises for every app (the
// shared Postgres and the Caddy proxy) and the Docker engine under all of
// them. Each container answers with what it is, how long it has been up,
// how often Docker has had to restart it, and what it is using right now,
// so the Dependencies page can put status, performance and the levers for
// each on one screen. The settings that shape them still come from their
// own endpoints; this is the live reading.

// proxyContainerName is the name the proxy's compose file gives the Caddy
// container (internal/proxy). Its stats are read by name, so a rename
// there must land here too.
const proxyContainerName = "caddy"

type containerFacts struct {
	Found        bool     `json:"found"`
	Running      bool     `json:"running"`
	Image        string   `json:"image,omitempty"`
	StartedAt    string   `json:"startedAt,omitempty"`
	RestartCount int      `json:"restartCount"`
	MemUsage     *uint64  `json:"memUsage,omitempty"`
	CPUPercent   *float64 `json:"cpuPercent,omitempty"`
}

func (s *Server) handleDependencies(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	docker := s.engine.Docker
	out := map[string]any{
		"postgres": containerFacts{},
		"proxy":    containerFacts{},
	}
	if docker == nil || !docker.Available(ctx) {
		out["docker"] = map[string]any{"available": false}
		writeJSON(w, http.StatusOK, out)
		return
	}
	out["postgres"] = factsFor(ctx, docker, pg.ContainerName)
	out["proxy"] = factsFor(ctx, docker, proxyContainerName)
	d := map[string]any{"available": true}
	if du, err := docker.DiskUsage(ctx); err == nil {
		d["imagesBytes"] = du.Bytes
		d["reclaimableBytes"] = du.Reclaimable
	}
	out["docker"] = d
	writeJSON(w, http.StatusOK, out)
}

// factsFor reads one container by name. A container that isn't there is
// an answer (found=false), not an error: the proxy doesn't exist until a
// domain is set up, and Postgres not until the first app that needs it.
func factsFor(ctx context.Context, docker *dockerx.Client, name string) containerFacts {
	d, err := docker.Inspect(ctx, name)
	if err != nil {
		return containerFacts{}
	}
	f := containerFacts{
		Found:        true,
		Running:      d.State.Running,
		Image:        d.Config.Image,
		StartedAt:    d.State.StartedAt,
		RestartCount: d.RestartCount,
	}
	if d.State.Running {
		if st, err := docker.StatsOnce(ctx, name); err == nil {
			mem, cpu := st.MemUsage, st.CPUPercent
			f.MemUsage, f.CPUPercent = &mem, &cpu
		}
	}
	return f
}
