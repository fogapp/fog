package apps

import (
	"context"
	"slices"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"gitea.lan/matthew/fog/internal/dockerx"
)

// containerListTTL is how long one listing of every app container answers
// the read paths. The dashboard polls Statuses every few seconds per open
// tab while the stats sampler and health monitor tick on their own, and all
// of them want the same list; two seconds merges them without anyone seeing
// a state older than a poll interval.
const containerListTTL = 2 * time.Second

// containerCache is the shared, short-lived fog.app container listing.
type containerCache struct {
	group singleflight.Group

	mu  sync.Mutex
	at  time.Time
	gen uint64 // bumped by forget; a fetch begun before it is not stored
	cs  []dockerx.ContainerSummary
}

// AppContainers lists every app container (label fog.app), shared across
// callers for containerListTTL and coalesced while a fetch is in flight.
// For reads only: deploy and converge list exactly what they need, fresh.
func (e *Engine) AppContainers(ctx context.Context) ([]dockerx.ContainerSummary, error) {
	c := &e.containers
	c.mu.Lock()
	if !c.at.IsZero() && time.Since(c.at) < containerListTTL {
		cs := slices.Clone(c.cs)
		c.mu.Unlock()
		return cs, nil
	}
	gen := c.gen
	c.mu.Unlock()

	v, err, _ := c.group.Do("list", func() (any, error) {
		// Shared by every caller waiting on it, so one tab closing must not
		// fail the others; the Docker client's own timeout bounds it.
		cs, err := e.Docker.ListByLabel(context.WithoutCancel(ctx), "fog.app")
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		if c.gen == gen {
			c.cs, c.at = cs, time.Now()
		}
		c.mu.Unlock()
		return cs, nil
	})
	if err != nil {
		return nil, err
	}
	return slices.Clone(v.([]dockerx.ContainerSummary)), nil
}

// forgetContainers drops the shared listing, so the next read after a
// change to an app's containers sees the change.
func (e *Engine) forgetContainers() {
	c := &e.containers
	c.mu.Lock()
	c.at, c.cs = time.Time{}, nil
	c.gen++
	c.mu.Unlock()
}
