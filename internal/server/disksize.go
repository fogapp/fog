package server

import (
	"context"
	"sync"
	"time"

	"gitea.lan/matthew/fog/internal/fsx"
	"gitea.lan/matthew/fog/internal/nas"
)

// The usage breakdown on a disk row — how much of the used space is app
// data, the Drive, backups, Postgres, fog's own bookkeeping, Docker's
// image store — comes from walking the directories fog owns and asking
// Docker to size what it holds. That walk is the one expensive thing on
// the Storage page (a media library on a NAS can be terabytes over NFS;
// Docker's answer means a pass over every layer), so it never runs on a
// request: the page's poll answers from the last completed pass and
// starts a fresh one in the background once the figures are older than
// sizeTTL. Until the first pass lands there is no breakdown, and the row
// shows the plain used-of-total bar — a reading, not a guess.

const sizeTTL = 10 * time.Minute

// dockerTimeout bounds Docker's own sizing inside a pass. The daemon walks
// its whole store to answer; a wedged daemon must not hold the pass open
// forever and leave the page without figures.
const dockerTimeout = 2 * time.Minute

// dockerUsage is what Docker reported holding, and where. Docker's
// writable world — image layers, the build cache, container scratch —
// can't be walked by path: with the containerd image store it isn't even
// under data-root. Docker's own accounting is the only honest reading.
type dockerUsage struct {
	root        string // the daemon's data-root; the disk it fills
	bytes       int64
	reclaimable int64 // what a prune would free: unused images, idle cache
}

// sizingPass is one completed pass: every walked path's bytes, and what
// Docker said (nil when it couldn't answer — then its bytes stay in
// "other", as they always were).
type sizingPass struct {
	sizes  map[string]int64
	docker *dockerUsage
	at     time.Time
}

// diskSizer is the single-flight cache of directory sizes.
type diskSizer struct {
	mu      sync.Mutex
	last    *sizingPass
	running bool
	// walk is the sizer; a var so tests can hand in a fake.
	walk func(ctx context.Context, path string) int64
	// docker sizes Docker's store; nil when there is no daemon to ask.
	docker func(ctx context.Context) (*dockerUsage, error)
}

// sizes answers the last completed pass for the given paths and kicks off
// a new one through spawn when the pass is missing or stale. Paths that
// the last pass didn't cover (a share added since) are absent from the
// map until the next pass includes them. Nil until the first pass lands.
// The pass runs under ctx — the server's background context, so Shutdown
// ends a walk that would otherwise outlive the store.
func (d *diskSizer) sizes(ctx context.Context, paths []string, spawn func(func())) *sizingPass {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.running && (d.last == nil || time.Since(d.last.at) > sizeTTL || d.missing(paths)) {
		d.running = true
		ps := append([]string(nil), paths...)
		spawn(func() { d.run(ctx, ps) })
	}
	return d.last
}

// dockerRoot is the data-root the last pass learned from Docker, or ""
// when no pass has, so the caller can leave it out of the walk list.
func (d *diskSizer) dockerRoot() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.last != nil && d.last.docker != nil {
		return d.last.docker.root
	}
	return ""
}

func (d *diskSizer) missing(paths []string) bool {
	for _, p := range paths {
		if _, ok := d.last.sizes[p]; !ok {
			return true
		}
	}
	return false
}

func (d *diskSizer) run(ctx context.Context, paths []string) {
	walk := d.walk
	if walk == nil {
		// One disk at a time: a mount inside a walked tree — a NAS share, or
		// Apps or Content bound from another disk — is sized as its own
		// path and charged to its own disk, not to the tree around it.
		mounts := nas.MountedPoints()
		walk = func(ctx context.Context, p string) int64 { return fsx.DirSizeExcept(ctx, p, mounts) }
	}
	pass := &sizingPass{sizes: make(map[string]int64, len(paths))}
	if d.docker != nil {
		dctx, cancel := context.WithTimeout(ctx, dockerTimeout)
		if u, err := d.docker(dctx); err == nil && u != nil {
			pass.docker = u
		}
		cancel()
	}
	for _, p := range paths {
		// Docker's data-root is Docker's to size: on an installed box it
		// sits inside fog's data directory, where a walk would count it
		// as fog's own and count it twice. Left out of the map, the next
		// poll drops it from the list once it knows the root.
		if pass.docker != nil && p == pass.docker.root {
			continue
		}
		pass.sizes[p] = walk(ctx, p)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.running = false
	// A cancelled pass stopped partway, so its figures are short: keep the
	// last whole one rather than show a breakdown that undercounts.
	if ctx.Err() != nil {
		return
	}
	pass.at = time.Now()
	d.last = pass
}
