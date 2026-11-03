// Health monitoring: probe each app's health endpoint on an interval and
// notify on transitions.
package apps

import (
	"context"
	"fmt"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"gitea.lan/matthew/fog/internal/dockerx"
	"gitea.lan/matthew/fog/internal/loop"
	"gitea.lan/matthew/fog/internal/ring"
	"gitea.lan/matthew/fog/internal/store"
)

// ---- health monitoring ----

// probeConcurrency bounds simultaneous health probes. Each can block for the
// full probe timeout, so these are latency-bound rather than CPU-bound.
const probeConcurrency = 8

// HealthProbe is one result from the monitor loop.
type HealthProbe struct {
	At time.Time `json:"at"`
	OK bool      `json:"ok"`
}

// healthKeep is how much probe history is retained per app.
const healthKeep = 24 * time.Hour

// healthRingSize is one probe per HealthInterval across healthKeep. A ring
// rather than a trimmed slice: trimming copied the whole day of history on
// every probe once a day of it existed.
func healthRingSize() int {
	if HealthInterval <= 0 {
		return 1
	}
	return int(healthKeep / HealthInterval)
}

func (e *Engine) recordHealth(name string, healthy bool, at time.Time) {
	// Load first: LoadOrStore alone would allocate a day-sized ring on
	// every probe only to throw it away.
	v, ok := e.healthHistory.Load(name)
	if !ok {
		v, _ = e.healthHistory.LoadOrStore(name, ring.New[HealthProbe](healthRingSize()))
	}
	v.(*ring.Ring[HealthProbe]).Add(HealthProbe{At: at, OK: healthy})
}

// probeHistory returns the retained probe results for an app, oldest
// first. Empty for an app without a health check, or one fogd has not
// probed since it started.
func (e *Engine) probeHistory(name string) []HealthProbe {
	v, ok := e.healthHistory.Load(name)
	if !ok {
		return nil
	}
	probes := v.(*ring.Ring[HealthProbe]).All()
	// The ring counts probes, not time: an app whose checks paused (stopped,
	// or its health check removed for a while) still holds probes from
	// before the gap. Keep only the day before the newest.
	if len(probes) == 0 {
		return probes
	}
	cutoff := probes[len(probes)-1].At.Add(-healthKeep)
	i := 0
	for i < len(probes) && probes[i].At.Before(cutoff) {
		i++
	}
	return probes[i:]
}

func (e *Engine) healthSnapshot() map[string]bool {
	m := map[string]bool{}
	e.healthState.Range(func(k, v any) bool {
		m[k.(string)] = v.(bool)
		return true
	})
	return m
}

// MonitorLoop probes app health endpoints and notifies on transitions.
// Probes hit the container IP on the app's own network — host-routable on Linux.
func (e *Engine) MonitorLoop(ctx context.Context, interval time.Duration) {
	loop.Every(ctx, interval, interval, e.probeAll)
}

func (e *Engine) probeAll(ctx context.Context) {
	list, err := e.Store.ListApps()
	if err != nil {
		return
	}
	containers, err := e.AppContainers(ctx)
	if err != nil {
		return
	}
	// One map for the whole tick: building it per app made each tick
	// quadratic in the number of containers.
	mains := dockerx.MainContainers(containers)
	// All specs in one query, before the fan-out — per-goroutine LatestSpec
	// calls were one SQL round-trip each, every tick, all contending for the
	// store at once.
	specs, err := LatestSpecs(e.Store)
	if err != nil {
		return
	}
	// Each probe can burn the full HTTP timeout, so probing serially meant a
	// handful of unreachable apps could stretch one pass past the tick that
	// scheduled it. Fan out, then apply transitions in list order so
	// notifications arrive in a stable sequence.
	type probe struct {
		app     store.App
		checked bool // false = no health spec, or spec unreadable
		healthy bool
	}
	probes := make([]probe, len(list))
	var g errgroup.Group
	g.SetLimit(probeConcurrency)
	for i, a := range list {
		probes[i].app = a
		if a.DesiredState != "running" {
			continue
		}
		g.Go(func() error {
			if ctx.Err() != nil {
				return nil
			}
			spec := specs[a.ID]
			if spec == nil || spec.Health == nil {
				return nil
			}
			probes[i].checked = true
			// The listing carries each container's address; no inspect.
			ip := mainIPIn(mains, a.Name)
			if ip == "" {
				return nil
			}
			port := spec.HealthPort()
			if port == 0 {
				return nil
			}
			probes[i].healthy = dockerx.ProbeHTTP(ctx, fmt.Sprintf("http://%s:%d%s", ip, port, spec.Health.Path))
			return nil
		})
	}
	g.Wait()

	now := time.Now()
	for _, p := range probes {
		if !p.checked {
			e.healthState.Delete(p.app.Name)
			continue
		}
		e.recordHealth(p.app.Name, p.healthy, now)
		prev, had := e.healthState.Load(p.app.Name)
		e.healthState.Store(p.app.Name, p.healthy)
		if had && prev.(bool) != p.healthy {
			if p.healthy {
				e.notify("Healthy again: "+p.app.Name, "health check is passing", true)
			} else {
				e.notify("Unhealthy: "+p.app.Name, "health check is failing", false)
			}
		}
	}
	e.pruneAppState(list)
}

// pruneAppState drops per-app bookkeeping for apps that no longer exist.
//
// All three maps are keyed by app name and were only ever added to. The health
// map is the one that could actually mislead — an app recreated under a
// deleted one's name inherited its health verdict until the first probe — but
// the deploy locks and live-deploy buffers leaked memory on the same schedule,
// and the live buffer can hold up to its 256 KB cap.
func (e *Engine) pruneAppState(list []store.App) {
	live := make(map[string]bool, len(list))
	for _, a := range list {
		live[a.Name] = true
	}
	dead := func(k any) bool {
		name, ok := k.(string)
		return ok && !live[name]
	}
	e.healthState.Range(func(k, _ any) bool {
		if dead(k) {
			e.healthState.Delete(k)
		}
		return true
	})
	e.healthHistory.Range(func(k, _ any) bool {
		if dead(k) {
			e.healthHistory.Delete(k)
		}
		return true
	})
	e.liveDeploys.Range(func(k, v any) bool {
		// Never drop one that is mid-operation: the app row can disappear
		// while a delete is still running, and CurrentDeploy is what the UI
		// polls to know it is still working.
		if lv, ok := v.(*liveDeploy); dead(k) && ok && !lv.busy() {
			e.liveDeploys.Delete(k)
		}
		return true
	})
	e.locks.Range(func(k, v any) bool {
		mu, ok := v.(*sync.Mutex)
		if !dead(k) || !ok {
			return true
		}
		// Only reclaim a lock nobody holds. TryLock is the whole test: if it
		// fails, an operation is in flight and the entry must stay.
		if mu.TryLock() {
			e.locks.Delete(k)
			mu.Unlock()
		}
		return true
	})
}
