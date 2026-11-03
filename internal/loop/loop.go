// Package loop runs fogd's periodic background work: a sweep, a check, a
// sync that fires once after a boot delay and then on a cadence until the
// daemon stops.
//
// The cadence is measured from the end of one pass to the start of the
// next. A pass that overruns its interval therefore never queues another
// one behind it, and a slow Docker or network can't make passes pile up.
package loop

import (
	"context"
	"time"
)

// Every calls fn after first, then every interval after each call returns,
// until ctx is done. A first of 0 runs fn straight away.
func Every(ctx context.Context, first, interval time.Duration, fn func(context.Context)) {
	Run(ctx, first, nil, func(ctx context.Context) time.Duration {
		fn(ctx)
		return interval
	})
}

// Run is Every for loops that choose their own next wait — a short retry
// while something is still coming up, the regular cadence once it's healthy
// — and that can be woken early. Each call to fn returns how long to wait
// before the next. A receive on kick (nil for none) runs fn at once instead
// of waiting the wait out.
func Run(ctx context.Context, first time.Duration, kick <-chan struct{}, fn func(context.Context) time.Duration) {
	timer := time.NewTimer(first)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-kick:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
		if ctx.Err() != nil {
			return
		}
		timer.Reset(fn(ctx))
	}
}
