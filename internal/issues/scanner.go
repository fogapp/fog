// Package issues is fog's configuration scanner: it routinely re-derives
// what should be true (specs, contracts, mounts, versions) against what is
// true, and files discrepancies as persistent, acknowledgeable issues —
// the goal being to catch a misconfiguration between failures instead of
// during one. Lifecycle rules live in store.SyncIssues; this package owns
// the checks and the sweep.
package issues

import (
	"context"
	"gitea.lan/matthew/fog/internal/compute"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/backup"
	"gitea.lan/matthew/fog/internal/dockerx"
	"gitea.lan/matthew/fog/internal/loop"
	"gitea.lan/matthew/fog/internal/nas"
	"gitea.lan/matthew/fog/internal/pg"
	"gitea.lan/matthew/fog/internal/store"
)

// checkTimeout bounds one check; a probe wedged on a dead container must
// not stall the sweep behind it.
const checkTimeout = 60 * time.Second

// checkConcurrency mirrors updates.CheckAll: enough parallelism to hide
// probe latency, little enough to not hammer docker or the apps.
const checkConcurrency = 4

type Scanner struct {
	Store  *store.Store
	Log    *slog.Logger
	Notify func(title, msg string, ok bool) // nil-checked
	Docker *dockerx.Client
	// DockerAvailable answers whether the daemon is reachable at all — the
	// difference between "no containers" and "no Docker". nil skips the check.
	DockerAvailable func(ctx context.Context) bool
	PG              *pg.Manager
	Nas             *nas.Manager
	DataDir         string

	// Function fields for cross-subsystem reach (the janitor's BackupsRoot
	// idiom): each is exactly what a check needs, and each is fakeable.
	Deploying func(name string) bool
	ExecInApp func(ctx context.Context, name string, timeout time.Duration, argv ...string) (string, error)
	// AppContainers lists every app's containers — the engine's short-lived
	// shared listing, so a sweep doesn't add its own daemon round-trip to
	// the dashboard's. nil lists through Docker directly.
	AppContainers   func(ctx context.Context) ([]dockerx.ContainerSummary, error)
	FetchDescriptor func(ctx context.Context, app *store.App, spec *apps.AppSpec) (*backup.Descriptor, error)
	BackupDir       func() string
	// FilesRoot is where Files lives, for resolving a spec's Files volumes.
	FilesRoot func() string
	// FilesOwner is the uid:gid fog gives what it creates in Files — the
	// identity an app's container writes as. ok is false when fog leaves
	// ownership alone, and there is then nothing to verify.
	FilesOwner  func() (uid, gid int, ok bool)
	SecretValue func(appID int64, key string) (string, error)
	// ComputeStuck is each machine online but stuck behind fog's release
	// of fog-node. nil skips the check.
	ComputeStuck func() []compute.Machine
	// FogdPort is the port fogd listens on (0 = unknown), which no app may
	// publish (check_fogports.go).
	FogdPort func() int
	// RulesRefused names the apps fog couldn't bring up under its current
	// rules, with why (check_fogports.go).
	RulesRefused func() map[string]string
	// SecretsMissing names the apps started without secrets their running
	// revision lists (check_fogports.go).
	SecretsMissing func() map[string][]string
	// Waiting names the apps left stopped until their storage mounts
	// (check_fogports.go).
	Waiting func() map[string]string

	// checksOverride replaces the registry in tests.
	checksOverride []check

	mu       sync.Mutex   // one sweep at a time; a manual scan can't stack
	lastScan atomic.Value // RFC3339 of the last completed sweep
}

// Loop runs sweeps forever. The first fires a few minutes after boot —
// late enough for NAS mounts and the boot reconcile to settle, early
// enough that the Inbox isn't empty all morning.
func (sc *Scanner) Loop(ctx context.Context, interval time.Duration) {
	loop.Every(ctx, 3*time.Minute, interval, sc.Sweep)
}

// LastScan reports when the last sweep completed ("" before the first).
func (sc *Scanner) LastScan() string {
	if v := sc.lastScan.Load(); v != nil {
		return v.(string)
	}
	return ""
}

// Sweep runs every check once against a single consistent snapshot and
// reconciles the results into the issues table. Also invoked directly by
// POST /api/issues/scan.
func (sc *Scanner) Sweep(ctx context.Context) {
	if !sc.mu.TryLock() {
		return
	}
	defer sc.mu.Unlock()

	sn, err := sc.buildSnapshot(ctx)
	if err != nil {
		sc.Log.Warn("issues sweep: snapshot", "err", err)
		return
	}

	registry := sc.checks()
	if sc.checksOverride != nil {
		registry = sc.checksOverride
	}

	var wg sync.WaitGroup
	sem := make(chan struct{}, checkConcurrency)
	var resMu sync.Mutex
	var total store.IssueSyncResult
	for _, ck := range registry {
		wg.Add(1)
		go func(ck check) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			// A panicking or failing check logs and syncs nothing — its
			// existing issues survive rather than resolving on missing
			// evidence.
			defer func() {
				if r := recover(); r != nil {
					sc.Log.Warn("issues check panicked", "check", ck.kind, "panic", r)
				}
			}()
			cctx, cancel := context.WithTimeout(ctx, checkTimeout)
			defer cancel()
			rep := ck.run(cctx, sn)
			if len(rep.evaluated) == 0 {
				return
			}
			// Each kind syncs separately — SyncIssues reconciles one kind's
			// rows — over the scopes the run evaluated, so a kind with no
			// findings this time resolves what it filed before.
			byKind := map[string][]store.IssueFinding{}
			for _, f := range rep.findings {
				byKind[f.Kind] = append(byKind[f.Kind], f)
			}
			for _, kind := range ck.kinds() {
				res, err := sc.Store.SyncIssues(kind, rep.evaluated, byKind[kind])
				if err != nil {
					sc.Log.Warn("issues sync", "check", kind, "err", err)
					continue
				}
				resMu.Lock()
				total.New = append(total.New, res.New...)
				total.Recurred = append(total.Recurred, res.Recurred...)
				total.Reopened = append(total.Reopened, res.Reopened...)
				total.Resolved = append(total.Resolved, res.Resolved...)
				resMu.Unlock()
			}
		}(ck)
	}
	wg.Wait()

	sc.report(&total)
	// Resolved rows age out in internal/retention, on the scan's hourly
	// cadence.
	sc.lastScan.Store(time.Now().UTC().Format(time.RFC3339))
}
