package apps

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"gitea.lan/matthew/fog/internal/build"
	"gitea.lan/matthew/fog/internal/dockerx"
	"gitea.lan/matthew/fog/internal/pg"
	"gitea.lan/matthew/fog/internal/signin"
	"gitea.lan/matthew/fog/internal/store"
	"gitea.lan/matthew/fog/internal/vault"
)

// NetworkName is fog's shared bridge. Caddy and fog's Postgres live on it;
// apps used to, and are moved off it onto networks of their own
// (netwire.go).
const NetworkName = "fog"

// SealedNetworkName is the internal network sealed apps used to share. Caddy
// and fog's Postgres still join it; a sealed app now has an internal network
// of its own (docs/sealed-network.md).
const SealedNetworkName = "fog-sealed"

// Phase budgets, carved out of the caller's overall deploy context. Without
// them a single wedged step consumes the whole 30-minute allowance and the
// operator watches an app sit at one stage with no verdict; with them, a stuck
// phase fails as itself ("release command timed out") and the app lock comes
// back. Generous enough that slow-but-progressing work on a small server is
// never cut off.
const (
	releaseTimeout   = 15 * time.Minute
	convergeTimeout  = 15 * time.Minute
	lifecycleTimeout = 5 * time.Minute // start/stop/restart
	teardownTimeout  = 5 * time.Minute // compose down during delete
)

// Engine reconciles app specs into running containers. All mutating
// operations are serialized per app.
type Engine struct {
	Store   *store.Store
	Vault   *vault.Vault
	Docker  *dockerx.Client
	Mat     *Materializer
	Builder *build.Builder
	PG      *pg.Manager
	Log     *slog.Logger
	// DevPermissiveProbe relaxes swap health checks on dev machines where
	// container IPs aren't host-routable (Docker Desktop on macOS).
	DevPermissiveProbe bool
	// OnChange is called after any change that affects routing (create,
	// delete, spec update) so the proxy can regenerate its config. It runs
	// once the app lock is released: the proxy reads every app's routes from
	// the store under its own lock, so holding this app's lock adds nothing
	// but a deploy that stays "in progress" through a Caddy reload.
	OnChange func(ctx context.Context)
	// OnSpecChanged is called with the app's name after its spec is
	// created, revised, or deleted — not on a deploy — once the app lock is
	// released, so the wiring that follows it can revise other apps.
	OnSpecChanged func(ctx context.Context, name string)
	// RemoveBackups deletes an app's snapshot files on delete-with-data
	// (wired to the backup manager; the layout is its concern, not ours).
	RemoveBackups func(appName string) error
	// TrashDriveFolder sends an app's own folder in Drive — Apps/<App> — to
	// Drive's trash on delete-with-data, so data that lives there goes the
	// same way the managed data dir does, recoverable for as long as
	// anything else in the trash is. Optional (a fogd without Drive).
	TrashDriveFolder func(appName string) error
	// RestoreStaged reports whether an armed restore is waiting in the app's
	// volumes, so status can show it and the UI can offer to cancel it (wired
	// to the backup manager, which owns the staging layout).
	RestoreStaged func(appName string) bool
	// StageArtifact arms a delegated app's restore from an artifact file, to
	// run on its next boot (wired to the backup manager, like RestoreStaged).
	// DeployRecovered needs it; nothing else does.
	StageArtifact func(appName string, spec *AppSpec, artifactPath string, confirmIdentity, forgetSecrets bool, stagedBy string) error
	// Notify sends an operator notification (title, message, ok).
	Notify func(title, msg string, ok bool)
	// EnsureMounts makes sure the NAS shares behind the given host paths are
	// mounted before containers touch them (wired to the nas manager) —
	// otherwise a deploy against an unmounted share would bind an empty
	// directory and the app would quietly write to the local disk.
	EnsureMounts func(ctx context.Context, hostPaths []string) error
	// FogHostAddr is where apps reach fogd from inside their containers
	// ("host.docker.internal:8080"): the base of FOG_API_URL, FLARE_DSN and
	// the OIDC provider's internal URL. Set from the listen address in
	// main; empty disables injecting them.
	FogHostAddr string
	// Signin registers apps that declare fog-signin/1 as clients of fog's
	// OpenID provider (internal/signin). Nil disables it.
	Signin *signin.Registry

	containers  containerCache
	locks       sync.Map // app name -> *sync.Mutex
	liveDeploys sync.Map // app name -> *liveDeploy
	healthState sync.Map // app name -> bool; pruned against the app list
	// rulesRefused: app name -> why the rules as they stand didn't bring
	// it up (rerender.go), for the issue.
	rulesRefused sync.Map
	// secretsMissing: app name -> the secrets its running revision lists
	// that it was last started without (rerender.go).
	secretsMissing sync.Map
	// waiting: app name -> why it waits, stopped, for its storage to mount
	// (rerender.go).
	waiting sync.Map
	// grandfatherMu runs the one-time upgrade passes once, first.
	grandfatherMu sync.Mutex
	// bringUpSeam stands in for bringUp in tests that exercise which apps
	// are started, not how (fleet_test.go).
	bringUpSeam func(ctx context.Context, app *store.App, restart bool) error
	// healthHistory keeps a day of probe results per app so the app page can
	// draw what happened while nobody was looking. In memory only: a fogd
	// restart forgets it, and the gap shows as "no data" rather than as a
	// verdict.
	healthHistory sync.Map   // app name -> *ring.Ring[HealthProbe]
	deployKeyMu   sync.Mutex // the deploy keypair is minted and replaced as one
	// pgSwitch lets one Postgres-backed app at a time run its release and
	// converge, where its old and new replicas both hold connections (see
	// switchGate).
	pgSwitch switchGate

	statsMu sync.RWMutex
	stats   map[string]statsEntry // container id -> most recent reading
	hostMem uint64                // Docker's MemTotal; 0 until first asked
}

// RegenerateDeployKey mints a fresh keypair for an app (escape hatch for key
// trouble). The old key stops working immediately; the new public key must
// be added to the git host.
func (e *Engine) RegenerateDeployKey(name, actor string) (*GitInfo, error) {
	app, err := e.Store.AppByName(name)
	if err != nil {
		return nil, err
	}
	// The deletes must be checked: if one fails, ensureGitCredentials finds
	// the old key still stored and "regenerate" would hand back the old
	// keypair as if it were fresh — the operator's escape hatch silently
	// doing nothing.
	e.deployKeyMu.Lock()
	errPriv := e.Store.DeleteSecret(app.ID, SecretDeployKey)
	errPub := e.Store.DeleteSecret(app.ID, SecretDeployKeyPub)
	e.deployKeyMu.Unlock()
	if errPriv != nil {
		return nil, fmt.Errorf("removing old deploy key: %w", errPriv)
	}
	if errPub != nil {
		return nil, fmt.Errorf("removing old deploy key: %w", errPub)
	}
	if err := e.ensureGitCredentials(app.ID); err != nil {
		return nil, err
	}
	e.Store.Audit(actor, "app.deploykey.regenerate", name, "")
	return e.GitInfo(app.ID, app.Name)
}

func (e *Engine) lock(name string) func() {
	for {
		v, _ := e.locks.LoadOrStore(name, &sync.Mutex{})
		mu := v.(*sync.Mutex)
		mu.Lock()
		// pruneAppState may have evicted this entry between the LoadOrStore
		// and the Lock (TryLock succeeds on a mutex nobody holds yet), after
		// which another caller mints a replacement — and two goroutines would
		// hold "the" app lock at once. Holding only counts if the map still
		// (or again) points at the mutex held; otherwise defer to the
		// replacement and retry. Prune can't evict a locked mutex, so a
		// verified entry stays put until it is unlocked.
		if cur, _ := e.locks.LoadOrStore(name, mu); cur == mu {
			// Whatever ran under the lock may have changed the app's
			// containers; the shared listing must not outlive it.
			return func() {
				mu.Unlock()
				e.forgetContainers()
			}
		}
		mu.Unlock()
	}
}

// tryLock takes the app's lock if nobody holds it, as lock does.
func (e *Engine) tryLock(name string) (func(), bool) {
	v, _ := e.locks.LoadOrStore(name, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	if !mu.TryLock() {
		return nil, false
	}
	if cur, _ := e.locks.LoadOrStore(name, mu); cur != mu {
		mu.Unlock()
		return nil, false
	}
	return func() {
		mu.Unlock()
		e.forgetContainers()
	}, true
}

// WithLock runs fn holding the app's mutex — the same exclusion a deploy
// takes. Sibling subsystems use it so that work which rewrites an app's state
// on disk (a restore replacing volume contents, a snapshot reading them) can
// never interleave with a converge that is moving those same files.
func (e *Engine) WithLock(name string, fn func() error) error {
	defer e.lock(name)()
	return fn()
}

func (e *Engine) notify(title, msg string, ok bool) {
	if e.Notify != nil {
		e.Notify(title, msg, ok)
	}
}

func (e *Engine) onChange(ctx context.Context) {
	if e.OnChange != nil {
		e.OnChange(ctx)
	}
}

// specChanged is deferred ahead of lockThenChange, so it runs after the
// lock is released.
func (e *Engine) specChanged(ctx context.Context, name string, changed *bool) {
	if *changed && e.OnSpecChanged != nil {
		e.OnSpecChanged(ctx, name)
	}
}

// lockThenChange takes the app lock and returns the function to defer: it
// releases the lock and then, if *changed was set, runs OnChange — so the
// proxy converge happens outside the lock.
func (e *Engine) lockThenChange(ctx context.Context, name string, changed *bool) func() {
	unlock := e.lock(name)
	return func() {
		unlock()
		if *changed {
			e.onChange(ctx)
		}
	}
}

// filesRoot is where Files lives, for resolving a spec's Files volumes —
// the materializer's answer, or "" when it has none.
func (e *Engine) filesRoot() string {
	if e.Mat != nil && e.Mat.FilesRoot != nil {
		return e.Mat.FilesRoot()
	}
	return ""
}
