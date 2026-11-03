// Package netwatch says when this machine's network changed: a new DHCP
// lease, a cable moved to another router, an interface coming up. Loops
// that assert something about the network to the outside world (the DNS
// keeper) subscribe so a move heals in seconds rather than on the next
// scheduled tick.
//
// On Linux the source is the kernel's own route/address broadcast
// (netlink); elsewhere, and as a fallback when netlink is unavailable, a
// cheap poll of the default route's source address and gateway. Both
// feed one debounced callback: DHCP produces a burst of events (address,
// then route, then a rename) and the subscriber wants the settled state.
package netwatch

import (
	"context"
	"time"

	"gitea.lan/matthew/fog/internal/wan"
)

// Fingerprint is what "the network" means here: the address this box
// would use to reach the internet, and the gateway it would go through.
// Either changing is a move.
type Fingerprint struct {
	LocalIP string
	Gateway string
}

// Watcher runs Watch. Fields exist for tests; the zero value is the real
// thing.
type Watcher struct {
	// Poll overrides the polling interval (default 15s).
	Poll time.Duration
	// Relaxed is the polling interval while the kernel subscription is up
	// (default 2m): netlink already reports every change, so the poll is
	// only a backstop and needn't wake the machine (or, on a laptop
	// dev box, run `route`) four times a minute.
	Relaxed time.Duration
	// Debounce is how long to let a burst settle (default 3s).
	Debounce time.Duration
	// Snapshot overrides fingerprinting (tests).
	Snapshot func() Fingerprint
	// NoNetlink forces the poller even on Linux.
	NoNetlink bool
}

func (w *Watcher) poll() time.Duration {
	if w.Poll > 0 {
		return w.Poll
	}
	return 15 * time.Second
}

func (w *Watcher) relaxed() time.Duration {
	if w.Relaxed > 0 {
		return w.Relaxed
	}
	return 2 * time.Minute
}

// subscribe is subscribeKernel; a variable so tests can stand in for the
// kernel.
var subscribe = subscribeKernel

func (w *Watcher) debounce() time.Duration {
	if w.Debounce > 0 {
		return w.Debounce
	}
	return 3 * time.Second
}

func (w *Watcher) snapshot() Fingerprint {
	if w.Snapshot != nil {
		return w.Snapshot()
	}
	return Current()
}

// Current fingerprints the network as it is right now, from the same two
// readings the rest of fogd acts on (internal/wan), so a change seen here
// is a change to what they see.
func Current() Fingerprint {
	fp := Fingerprint{LocalIP: wan.LANIP()}
	if gw, err := wan.Gateway(); err == nil {
		fp.Gateway = gw.String()
	}
	return fp
}

// Watch calls onChange after the network settles into a different
// fingerprint than the last one reported. It returns when ctx ends.
//
// onChange runs on the watcher's own goroutine, so it must hand anything
// slow off: while it runs, later changes aren't seen, and a subscriber
// waiting minutes (a VPN re-join) would hold back one that needs seconds
// (the DNS keeper).
func (w *Watcher) Watch(ctx context.Context, onChange func(Fingerprint)) {
	events := make(chan struct{}, 1)
	notify := func() {
		select {
		case events <- struct{}{}:
		default:
		}
	}
	// live says whether the kernel subscription is up, so the poll can
	// relax while it is and tighten again if it drops.
	live := make(chan bool, 2)
	if !w.NoNetlink {
		go subscribe(ctx, notify, func(up bool) { // no-op where unsupported
			select {
			case live <- up:
			case <-ctx.Done():
			}
		})
	}

	last := w.snapshot()
	ticker := time.NewTicker(w.poll())
	defer ticker.Stop()
	var settle *time.Timer
	var settleC <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			if settle != nil {
				settle.Stop()
			}
			return
		case <-events:
			// The kernel said something moved; wait for the burst to end.
			if settle == nil {
				settle = time.NewTimer(w.debounce())
			} else {
				settle.Reset(w.debounce())
			}
			settleC = settle.C
			continue
		case up := <-live:
			if up {
				ticker.Reset(w.relaxed())
			} else {
				ticker.Reset(w.poll())
			}
			continue
		case <-settleC:
			settleC = nil
		case <-ticker.C:
		}
		if now := w.snapshot(); now != last {
			last = now
			onChange(now)
		}
	}
}
