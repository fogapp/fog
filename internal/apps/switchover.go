package apps

import (
	"context"
	"sync"
)

// switchGate admits one holder at a time and remembers who it is, so a
// deploy waiting at the gate can say what it is waiting for.
//
// It exists for the shared Postgres cluster. A blue-green swap runs the old
// and new replicas side by side, and both hold their connection pools; a
// release command opens its own on top. One app doing that is well inside
// max_connections. Several doing it at once — a burst of pushes, or every
// app auto-updating after a fog release — stacked their overlaps past the
// limit, and every app on the cluster, deploying or not, was refused new
// connections ("remaining connection slots are reserved for ... SUPERUSER",
// seen on feed, hook, stream and office on 2026-10-19 and 20).
//
// Only the overlap is gated: fetches and builds, which hold no connections,
// still run side by side.
type switchGate struct {
	once   sync.Once
	slot   chan struct{}
	mu     sync.Mutex
	holder string
}

func (g *switchGate) init() { g.once.Do(func() { g.slot = make(chan struct{}, 1) }) }

// acquire blocks until name holds the gate or ctx ends. waiting is called
// once, with the current holder, if the gate is taken on arrival. The
// returned function releases it.
func (g *switchGate) acquire(ctx context.Context, name string, waiting func(holder string)) (func(), error) {
	g.init()
	select {
	case g.slot <- struct{}{}:
	default:
		if waiting != nil {
			waiting(g.current())
		}
		select {
		case g.slot <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	g.mu.Lock()
	g.holder = name
	g.mu.Unlock()
	return func() {
		g.mu.Lock()
		g.holder = ""
		g.mu.Unlock()
		<-g.slot
	}, nil
}

func (g *switchGate) current() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.holder
}
