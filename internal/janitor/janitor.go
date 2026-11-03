// Package janitor reclaims disk the rest of Fog no longer references:
// dangling images superseded by re-pulls, unbounded build cache, abandoned
// backup uploads, and orphaned directories left behind by deleted apps or
// edited specs.
// Deletion of orphans is never automatic — the janitor reports, the
// operator decides.
package janitor

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/backup"
	"gitea.lan/matthew/fog/internal/execx"
	"gitea.lan/matthew/fog/internal/fsx"
	"gitea.lan/matthew/fog/internal/loop"
	"gitea.lan/matthew/fog/internal/store"
)

// buildCacheCap bounds docker's buildkit cache. Git-app builds accumulate
// cache forever without one; 5GB keeps recent layers hot on a small disk.
const buildCacheCap = "5GB"

type Janitor struct {
	Store       *store.Store
	DataDir     string
	BackupsRoot func() string // backup manager owns the layout (it's a setting)
	Log         *slog.Logger
}

// sweepTimeout bounds one sweep: a wedged docker daemon must not block
// CombinedOutput forever and silently stop the Loop from ever sweeping again.
const sweepTimeout = 5 * time.Minute

// Loop sweeps docker-side garbage on an interval. The first sweep fires a
// few minutes after boot — a daily interval on a box that restarts daily
// would otherwise never reach its first tick, and never prune.
func (j *Janitor) Loop(ctx context.Context, interval time.Duration) {
	loop.Every(ctx, 3*time.Minute, interval, j.sweep)
}

func (j *Janitor) sweep(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, sweepTimeout)
	defer cancel()
	// Dangling images: what a re-pulled :latest/:release tag leaves behind.
	// Docker refuses to touch images containers still use.
	if _, err := execx.Output(ctx, "docker", "image", "prune", "-f"); err != nil {
		j.Log.Warn("image prune", "err", err)
	}
	// Docker 29 renamed --keep-storage to --max-used-space; try new, then old.
	if _, err := execx.Output(ctx, "docker", "builder", "prune", "-f", "--all", "--max-used-space", buildCacheCap); err != nil {
		if _, err2 := execx.Output(ctx, "docker", "builder", "prune", "-f", "--all", "--keep-storage", buildCacheCap); err2 != nil {
			j.Log.Warn("builder prune", "err", err, "fallbackErr", err2)
		}
	}
	j.sweepUploads()
	// A count is enough for the log. Sizing every orphan walks whole trees —
	// backup trees on the NAS among them — and only the UI, which shows the
	// sizes, needs to pay for that.
	count := 0
	if err := j.scan(func(string, string, string) { count++ }); err == nil && count > 0 {
		j.Log.Info("orphaned directories present", "count", count)
	}
}

// uploadMaxAge bounds how long a backup upload may sit unconsumed. The
// upload endpoint streams GB-scale artifacts into <data>/uploads, and only
// the create/restore that consumes one deletes it — an upload followed by a
// closed tab would otherwise leak the file forever.
const uploadMaxAge = 24 * time.Hour

// sweepUploads removes abandoned backup uploads: plain files directly under
// <data>/uploads whose mtime is older than uploadMaxAge. Only what the
// upload handler itself writes is eligible — directories and anything
// nested are left alone, and an upload still being streamed keeps a fresh
// mtime, so in-flight files survive.
func (j *Janitor) sweepUploads() {
	dir := filepath.Join(j.DataDir, "uploads")
	ents, err := os.ReadDir(dir)
	if err != nil {
		return // no uploads dir yet — nothing to leak
	}
	cutoff := time.Now().Add(-uploadMaxAge)
	for _, e := range ents {
		if !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if err := os.Remove(path); err != nil {
			j.Log.Warn("upload sweep", "path", path, "err", err)
		} else {
			j.Log.Info("removed abandoned upload", "path", path, "bytes", info.Size())
		}
	}
}

// Orphan is a directory no current app accounts for.
type Orphan struct {
	Path      string `json:"path"`
	Kind      string `json:"kind"` // data | volume | backups | builds | postgres
	App       string `json:"app"`  // the (possibly deleted) app it belonged to
	SizeBytes int64  `json:"sizeBytes"`
}

// Orphans scans Fog's directories against the store and sizes each orphan
// tree for the UI. The sizing walks are the expensive half — an orphan can
// be a NAS-resident backup tree — so callers that only need the paths (see
// Reclaim) use scan directly.
func (j *Janitor) Orphans() ([]Orphan, error) {
	var out []Orphan
	err := j.scan(func(path, kind, app string) {
		out = append(out, Orphan{Path: path, Kind: kind, App: app, SizeBytes: fsx.DirSize(context.Background(), path)})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// scan enumerates orphaned directories against the store — app dirs without
// an app, volume subdirs no longer in their app's spec, backup and build
// trees for apps that are gone — calling add for each. Enumeration only:
// what an orphan costs on disk is the caller's business.
func (j *Janitor) scan(add func(path, kind, app string)) error {
	list, err := apps.WithSpecs(j.Store)
	if err != nil {
		return err
	}
	specs := map[string]*apps.AppSpec{}
	for _, a := range list {
		// A spec that won't parse yields an empty one on purpose: the app still
		// exists, so its directories must not be reported as orphans and
		// offered for deletion.
		spec := a.Spec
		if spec == nil {
			spec = &apps.AppSpec{}
		}
		specs[a.Name] = spec
	}

	// data/<app>: volume data for deleted apps, then per-volume subdirs
	// dropped from a live app's spec.
	for _, ent := range readDir(filepath.Join(j.DataDir, "data")) {
		name := ent.Name()
		spec, live := specs[name]
		if !live {
			add(filepath.Join(j.DataDir, "data", name), "data", name)
			continue
		}
		managed := map[string]bool{}
		for _, v := range spec.AllVolumes() {
			if v.Managed() {
				managed[v.Name] = true
			}
		}
		for _, sub := range readDir(filepath.Join(j.DataDir, "data", name)) {
			if !managed[sub.Name()] {
				add(filepath.Join(j.DataDir, "data", name, sub.Name()), "volume", name)
			}
		}
	}

	// builds/<app>: checkouts for apps that are gone or no longer git-built.
	for _, ent := range readDir(filepath.Join(j.DataDir, "builds")) {
		name := ent.Name()
		if spec, live := specs[name]; !live || spec.Source.Type != "git" {
			add(filepath.Join(j.DataDir, "builds", name), "builds", name)
		}
	}

	// postgres/data.pg<N>: the cluster a Postgres major upgrade set aside.
	// Kept until the operator is satisfied with the migrated data; this is
	// the only door that removes it.
	for _, ent := range readDir(filepath.Join(j.DataDir, "postgres")) {
		if strings.HasPrefix(ent.Name(), "data.pg") {
			add(filepath.Join(j.DataDir, "postgres", ent.Name()), "postgres", "fog-postgres")
		}
	}

	// backups/<app>: snapshots for apps that are gone. The recovery kits and
	// Files snapshots share the root but are no app's, so never orphans.
	if j.BackupsRoot != nil {
		root := j.BackupsRoot()
		for _, ent := range readDir(root) {
			name := ent.Name()
			if backup.IsReservedDir(name) {
				continue
			}
			if _, live := specs[name]; !live {
				add(filepath.Join(root, name), "backups", name)
			}
		}
	}
	return nil
}

// Reclaim deletes one reported orphan. The path must match a fresh scan —
// this endpoint can only ever remove what the orphan scan itself identifies.
// Membership only: validating one path must not re-walk the size of every
// orphan tree (some live on the NAS).
func (j *Janitor) Reclaim(path string) error {
	known := false
	if err := j.scan(func(p, _, _ string) {
		if p == path {
			known = true
		}
	}); err != nil {
		return err
	}
	if !known {
		return fmt.Errorf("%q is not a known orphan", path)
	}
	return os.RemoveAll(path)
}

// readDir lists subdirectories, tolerating a missing parent.
func readDir(dir string) []os.DirEntry {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var dirs []os.DirEntry
	for _, e := range ents {
		if e.IsDir() {
			dirs = append(dirs, e)
		}
	}
	return dirs
}
