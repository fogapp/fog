package server

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// dockerStatusCache holds the Docker part of /api/system: one probe at a
// time, answered from the last view while one is in flight. It has its own
// lock, apart from warningsCache's: the probe can spend ~30s of client
// timeouts behind a sick daemon, and nothing else on /api/system may queue
// behind that.
type dockerStatusCache struct {
	mu   sync.Mutex
	view *dockerStatusView
	at   time.Time
	// probing is non-nil while one goroutine is probing the daemon; it
	// closes when the probe lands. Concurrent callers reuse the previous
	// view instead of piling up behind the probe.
	probing chan struct{}

	// installInvalidated: the currently-Done install run has already had its
	// one invalidate. The install status endpoint is polled, and invalidating
	// on every poll would defeat the TTL forever after.
	installInvalidated atomic.Bool
}

// get returns a view younger than ttl, running probe for a new one when
// there isn't. probe runs outside the lock and detached from ctx's
// cancellation: its answer is shared state, and a caller hanging up
// mid-probe must not cache "unavailable" for the TTL.
func (c *dockerStatusCache) get(ctx context.Context, ttl time.Duration, probe func(context.Context) *dockerStatusView) *dockerStatusView {
	for {
		c.mu.Lock()
		if c.view != nil && time.Since(c.at) < ttl {
			d := c.view
			c.mu.Unlock()
			return d
		}
		if wait := c.probing; wait != nil {
			// Someone else is already probing. A slightly stale view now
			// beats a fresh one after ~30s of client timeouts behind a sick
			// daemon — return the previous answer and let the poller pick up
			// the new one next tick. With no previous answer (first request
			// after startup or an invalidate), wait for the probe: guessing
			// would mean claiming a state nobody has observed.
			cached := c.view
			c.mu.Unlock()
			if cached != nil {
				return cached
			}
			<-wait
			continue
		}
		wait := make(chan struct{})
		c.probing = wait
		c.mu.Unlock()

		d := probe(context.WithoutCancel(ctx))
		c.mu.Lock()
		c.view, c.at = d, time.Now()
		c.probing = nil
		c.mu.Unlock()
		close(wait)
		return d
	}
}

// invalidate drops the view so the next get probes again.
func (c *dockerStatusCache) invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.view = nil
}

// ttlCache keeps one computed value for a while; compute runs under the
// lock, so concurrent callers share one computation.
type ttlCache[T any] struct {
	mu  sync.Mutex
	val T
	ok  bool
	at  time.Time
}

func (c *ttlCache[T]) get(ttl time.Duration, compute func() T) T {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ok && time.Since(c.at) < ttl {
		return c.val
	}
	c.val, c.ok, c.at = compute(), true, time.Now()
	return c.val
}
