package apps

import (
	"context"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"gitea.lan/matthew/fog/internal/dockerx"
	"gitea.lan/matthew/fog/internal/loop"
)

// Docker computes CPU percentage as a delta between two internal samples, so
// every /containers/{id}/stats call blocks for roughly a second regardless of
// how fast the daemon is. Collecting them inline made /api/apps?stats=1 cost
// one second per running app — and the dashboard polls it every five. Past a
// handful of apps the requests overlapped continuously and the numbers on
// screen were structurally stale.
//
// So they are collected in the background instead, in parallel, and served
// from memory. The API call becomes O(1) and the readings are never older
// than one sampling interval. The sampler itself takes one-shot snapshots,
// which skip Docker's one-second wait, and computes CPU between its own
// readings — otherwise a pass over forty containers took most of the
// interval it ran in.
const (
	// statsConcurrency bounds the fan-out. The work is a blocking read per
	// container, not CPU, but an unbounded burst against the Docker socket on
	// a small box is its own kind of rude.
	statsConcurrency = 8
	// statsMaxAge is how long a reading may be served after it was taken.
	// Beyond this the sampler is presumed stuck or stopped, and callers are
	// told the figure is unknown rather than shown a stale one.
	statsMaxAge = 90 * time.Second
)

type statsEntry struct {
	stats dockerx.Stats
	at    time.Time
	// Rates over the previous sample of the same container. Nil until a
	// second reading exists: Docker's counters are cumulative, and one
	// reading says how much has moved, never how fast.
	rates *Rates
}

// Rates is what the cumulative counters become between two samples, in
// bytes per second.
type Rates struct {
	NetIn     float64
	NetOut    float64
	DiskRead  float64
	DiskWrite float64
}

// containerStats returns the cached reading for a container, and whether one
// is available and fresh enough to quote.
func (e *Engine) containerStats(id string) (dockerx.Stats, *Rates, bool) {
	e.statsMu.RLock()
	defer e.statsMu.RUnlock()
	entry, ok := e.stats[id]
	if !ok || time.Since(entry.at) > statsMaxAge {
		return dockerx.Stats{}, nil, false
	}
	return entry.stats, entry.rates, true
}

// StatsLoop samples container resource usage until ctx ends. It takes a first
// pass immediately so a dashboard opened right after boot fills in quickly.
func (e *Engine) StatsLoop(ctx context.Context, interval time.Duration) {
	loop.Every(ctx, 0, interval, e.sampleStats)
}

func (e *Engine) sampleStats(ctx context.Context) {
	containers, err := e.AppContainers(ctx)
	if err != nil {
		return // Docker down: existing readings age out on their own
	}
	// The host's RAM caps an app's summed memory limits. Asked once: it
	// doesn't change while the daemon runs.
	e.statsMu.RLock()
	known := e.hostMem > 0
	e.statsMu.RUnlock()
	if !known {
		if info, err := e.Docker.Info(ctx); err == nil && info.MemTotal > 0 {
			e.statsMu.Lock()
			e.hostMem = uint64(info.MemTotal)
			e.statsMu.Unlock()
		}
	}

	var (
		mu    sync.Mutex
		fresh = make(map[string]statsEntry, len(containers))
		g     errgroup.Group
	)
	g.SetLimit(statsConcurrency)
	for _, c := range containers {
		if c.State != "running" {
			continue
		}
		g.Go(func() error {
			if ctx.Err() != nil {
				return nil
			}
			st, err := e.Docker.StatsSnapshot(ctx, c.ID)
			if err != nil {
				return nil
			}
			mu.Lock()
			fresh[c.ID] = statsEntry{stats: *st, at: time.Now()}
			mu.Unlock()
			return nil
		})
	}
	g.Wait()

	// Wholesale replacement, so containers that went away don't linger. The
	// previous reading of each survivor is what turns counters into rates;
	// a container restarted between samples resets its counters, and a
	// negative delta is that restart, not a flow backwards.
	e.statsMu.Lock()
	for id, cur := range fresh {
		prev, ok := e.stats[id]
		if !ok {
			continue
		}
		dt := cur.at.Sub(prev.at).Seconds()
		if dt <= 0 {
			continue
		}
		rate := func(now, then uint64) float64 {
			if now < then {
				return 0
			}
			return float64(now-then) / dt
		}
		// CPU the same way: a one-shot snapshot carries counters, not a
		// percentage, and the delta over the whole interval is a steadier
		// figure than Docker's own one-second one.
		cur.stats.CPUPercent = dockerx.CPUPercent(prev.stats.CPUTotal, prev.stats.SystemCPU,
			cur.stats.CPUTotal, cur.stats.SystemCPU, cur.stats.OnlineCPUs)
		cur.rates = &Rates{
			NetIn:     rate(cur.stats.NetRx, prev.stats.NetRx),
			NetOut:    rate(cur.stats.NetTx, prev.stats.NetTx),
			DiskRead:  rate(cur.stats.BlkRead, prev.stats.BlkRead),
			DiskWrite: rate(cur.stats.BlkWrite, prev.stats.BlkWrite),
		}
		fresh[id] = cur
	}
	e.stats = fresh
	e.statsMu.Unlock()
}

// AppUsage is what all of an app's running containers — main, workers,
// aux sidecars — are measured to be using together. Immich is its server
// plus its database, cache and machine learning, and both a capacity
// question and the dashboard's row want the whole of it: Stream's
// transcoder can hold every core while its web container idles.
type AppUsage struct {
	Running    bool
	Containers int
	// Measured is whether any running container had a fresh reading. Until
	// one does, the figures below are unknown rather than zero.
	Measured bool
	MemUsage uint64
	// MemLimit sums each container's ceiling, capped at the host's RAM: a
	// container without a limit reports the whole host, and three of them
	// cannot share three hosts' worth.
	MemLimit uint64
	// CPUPercent is a share of the whole host, 0–100 — the same scale as the
	// host CPU figure above the list. Docker's own figure is per core, so a
	// container pinning four cores reads 400%, which beside a host reading
	// of 96% says the wrong thing.
	CPUPercent float64
	// Summed over the containers that have two readings; nil until one does.
	Rates *Rates
}

// AppUsages sums the cached readings per app.
func (e *Engine) AppUsages(ctx context.Context) (map[string]AppUsage, error) {
	containers, err := e.AppContainers(ctx)
	if err != nil {
		return nil, err
	}
	return e.appUsages(containers), nil
}

// appUsages sums the cached readings of the given containers per app.
// Containers without a fresh reading contribute nothing rather than a stale
// figure, so a total may undercount briefly after a start; it never
// overcounts.
func (e *Engine) appUsages(containers []dockerx.ContainerSummary) map[string]AppUsage {
	type reading struct {
		stats dockerx.Stats
		rates *Rates
	}
	e.statsMu.RLock()
	hostMem := e.hostMem
	e.statsMu.RUnlock()
	readings := map[string]reading{}
	for _, c := range containers {
		if c.State != "running" {
			continue
		}
		if st, rates, ok := e.containerStats(c.ID); ok {
			readings[c.ID] = reading{st, rates}
		}
	}

	out := map[string]AppUsage{}
	for _, c := range containers {
		name := c.Labels["fog.app"]
		u := out[name]
		if c.State == "running" {
			u.Running = true
			u.Containers++
			if r, ok := readings[c.ID]; ok {
				u.Measured = true
				u.MemUsage += r.stats.MemUsage
				u.MemLimit += r.stats.MemLimit
				if hostMem > 0 {
					u.MemLimit = min(u.MemLimit, hostMem)
				}
				cpus := r.stats.OnlineCPUs
				if cpus == 0 {
					cpus = 1
				}
				u.CPUPercent += r.stats.CPUPercent / float64(cpus)
				if r.rates != nil {
					if u.Rates == nil {
						u.Rates = &Rates{}
					}
					u.Rates.NetIn += r.rates.NetIn
					u.Rates.NetOut += r.rates.NetOut
					u.Rates.DiskRead += r.rates.DiskRead
					u.Rates.DiskWrite += r.rates.DiskWrite
				}
			}
		}
		out[name] = u
	}
	return out
}
