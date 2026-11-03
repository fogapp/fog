// Package updates is the post-Watchtower answer for app updates: digest-aware
// checking against the registry (image apps) and remote-head checking against
// the git branch (git apps), per-app notify/auto policies, pre-update
// snapshots, and health-verified application.
package updates

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/backup"
	"gitea.lan/matthew/fog/internal/execx"
	"gitea.lan/matthew/fog/internal/loop"
	"gitea.lan/matthew/fog/internal/notify"
	"gitea.lan/matthew/fog/internal/store"
)

// pullTimeout bounds a registry pull. Large images on a domestic connection
// are slow but not unbounded; past this the app lock matters more than the
// pull, and the operator gets a reason instead of a spinner.
const pullTimeout = 30 * time.Minute

// checkConcurrency bounds how many registry/git lookups run at once. These are
// latency-bound, not CPU-bound, but an unbounded burst would hammer a single
// registry and, for git apps, fork a process per app simultaneously.
const checkConcurrency = 6

// checkTimeout bounds one app's update check. A git host that moved away —
// or worse, an address now owned by an unrelated machine that accepts the
// TCP connection and says nothing — must fail a check, not hang it.
const checkTimeout = 90 * time.Second

// failThreshold: consecutive failed checks before an app's broken update
// source is raised as a persistent notice (transient blips stay quiet).
const failThreshold = 3

// autoRetryCooldown: how long a failed auto-update stands down before the
// same remote version is attempted again. Without it, a persistently failing
// auto-update re-runs its pre-update backup + pull + deploy every enforce
// tick — four doomed update attempts a day against the same version.
const autoRetryCooldown = 24 * time.Hour

type Checker struct {
	Store  *store.Store
	Engine *apps.Engine
	Backup *backup.Manager
	Log    *slog.Logger
	Notify func(title, msg string, ok bool)

	// failing counts consecutive per-app check failures and owns the
	// "update-check-failing" notice lifecycle; built lazily so the zero-value
	// Checker keeps working. Access via failNotice.
	failing     *notify.FailStreak
	failingOnce sync.Once

	// checkMu keeps CheckAll runs from overlapping — the manual "check now"
	// endpoint can land mid-loop-tick, and two concurrent runs double-count
	// failure streaks and race their SetAppUpdate writes.
	checkMu sync.Mutex
}

// failNotice is the streak tracker behind noteFailure/noteSuccess (see
// notify.FailStreak for the raise/re-raise/dismiss cadence).
func (c *Checker) failNotice() *notify.FailStreak {
	c.failingOnce.Do(func() {
		c.failing = &notify.FailStreak{Store: c.Store, Kind: "update-check-failing", Threshold: failThreshold}
	})
	return c.failing
}

// noteFailure tracks an app's consecutive check failures and raises one
// persistent notice when the streak crosses the threshold. Update-check
// failures used to be a Debug line and nothing else — an app whose git host
// stayed behind in the old house simply stopped hearing about updates,
// forever, invisibly.
func (c *Checker) noteFailure(appName, errMsg, sourceRef string) {
	streak, raise := c.failNotice().Fail(appName)
	c.Log.Warn("update check failing", "app", appName, "streak", streak, "err", errMsg)
	if raise {
		c.Store.CreateNotice("update-check-failing", appName,
			appName+"'s update source is unreachable",
			fmt.Sprintf("Checking %s for updates has failed %d times in a row: %s\n\n"+
				"The app keeps running, but it can't hear about new versions and can't redeploy until this is fixed. "+
				"If this server (or the source host) moved networks, edit the app's repository/image address.",
				sourceRef, streak, errMsg))
	}
}

// noteSuccess clears an app's failure streak (and its notice, if one is up).
func (c *Checker) noteSuccess(appName string) {
	c.failNotice().Success(appName)
}

// remoteDigest resolves a tag's current manifest digest from its registry.
func remoteDigest(ctx context.Context, ref string) (string, error) {
	out, err := execx.Output(ctx, "docker", "buildx", "imagetools", "inspect", ref, "--format", "{{println .Manifest.Digest}}")
	if err != nil {
		return "", fmt.Errorf("registry lookup failed for %s: %w", ref, err)
	}
	return digestIn(out)
}

// digestIn picks the digest out of what imagetools printed. The output
// carries stderr too, so a warning line must not be mistaken for it.
func digestIn(out string) (string, error) {
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); strings.HasPrefix(line, "sha256:") {
			return line, nil
		}
	}
	return "", fmt.Errorf("registry lookup printed no digest: %q", out)
}

// CheckResult is what one app's check actually turned up. The UI reports it
// verbatim, so a check that finds an update can never be shown as "up to
// date" and a check that failed can never be shown as a clean bill of health.
type CheckResult struct {
	App             string `json:"app"`
	Source          string `json:"source"` // image | git
	UpdateAvailable bool   `json:"updateAvailable"`
	From            string `json:"from,omitempty"`
	To              string `json:"to,omitempty"`
	// Comparable is false when there is nothing to compare against yet (the
	// app never deployed from this source) — neither an update nor a clean
	// result, and the UI says so rather than claiming "up to date".
	Comparable bool   `json:"comparable"`
	Error      string `json:"error,omitempty"`
	// Deploying means the check was skipped because the app is mid-deploy —
	// its digest/commit is changing under the check, so any comparison would
	// report the version it's moving away from. Not an update, not clean.
	Deploying bool `json:"deploying,omitempty"`
}

// CheckApp refreshes update state for one app; returns true when an update
// is available.
func (c *Checker) CheckApp(ctx context.Context, app *store.App, spec *apps.AppSpec) (bool, error) {
	res := c.Check(ctx, app, spec)
	if res.Error != "" {
		return false, errors.New(res.Error)
	}
	return res.UpdateAvailable, nil
}

// Check refreshes update state for one app and reports the full outcome.
// An app that is mid-deploy is skipped, badge state untouched: comparing
// against a version that's being replaced right now would re-flag (and
// re-notify) the very update that's being applied.
func (c *Checker) Check(ctx context.Context, app *store.App, spec *apps.AppSpec) CheckResult {
	res := CheckResult{App: app.Name, Source: spec.Source.Type}
	if c.Engine.Deploying(app.Name) {
		res.Deploying = true
		return res
	}
	var local, remote string
	var err error
	switch spec.Source.Type {
	case "image":
		local, remote, err = c.checkImage(ctx, app, spec)
	case "git":
		local, remote, err = c.checkGit(ctx, app, spec)
	default:
		return res
	}
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.Comparable = local != "" && remote != ""
	res.From, res.To = apps.ShortRef(local), apps.ShortRef(remote)
	res.UpdateAvailable = updateAvailable(spec.Source.Type, local, remote)
	c.Store.SetAppUpdate(app.ID, local, remote, res.UpdateAvailable)
	return res
}

// updateAvailable is the verdict rule on its own. Anything it can't compare
// is neither an update nor a clean result — the caller reports "nothing to
// compare yet" rather than letting an unknown pass as "up to date".
func updateAvailable(sourceType, local, remote string) bool {
	if local == "" || remote == "" {
		return false
	}
	if sourceType == "git" {
		return commitOutdated(local, remote)
	}
	return local != remote
}

// checkImage resolves the tag's registry digest and the deployed one. An
// empty local digest means the app has never deployed this image — there is
// nothing to compare yet.
func (c *Checker) checkImage(ctx context.Context, app *store.App, spec *apps.AppSpec) (string, string, error) {
	remote, err := remoteDigest(ctx, spec.Source.Image)
	if err != nil {
		return "", "", err
	}
	local, err := c.deployedDigest(app)
	if err != nil {
		return "", "", err
	}
	return local, remote, nil
}

// deployedDigest is the digest of the image this app is actually running, or
// "" when it has never deployed an image.
//
// It is what fog recorded at deploy time, never the locally pulled tag: after
// a rollback the tag still resolves to the newer image that was pulled before
// the operator stepped back from it, and comparing that against the registry
// would report an app running last month's version as up to date.
func (c *Checker) deployedDigest(app *store.App) (string, error) {
	v, err := c.Store.CurrentAppVersion(app.ID)
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if v.Source != "image" {
		return "", nil
	}
	return v.Ref, nil
}

// checkGit resolves the deployed commit and the branch head on the remote.
// ls-remote only — nothing is fetched or built until Apply.
func (c *Checker) checkGit(ctx context.Context, app *store.App, spec *apps.AppSpec) (string, string, error) {
	branch := spec.Source.BranchOrDefault()
	key, err := c.Engine.DeployKey(app.ID)
	if err != nil {
		return "", "", fmt.Errorf("deploy key unavailable: %w", err)
	}
	remote, err := c.Engine.Builder.RemoteHead(ctx, app.Name, spec.Source.Repo, branch, key)
	if err != nil {
		return "", "", err
	}
	// An empty deployed commit means never successfully deployed — nothing
	// meaningful to compare.
	return app.DeployedCommit, remote, nil
}

// commitOutdated reports whether the deployed commit (possibly a 12-char
// short sha recovered from an image tag) differs from the remote head.
func commitOutdated(local, remote string) bool {
	return local != "" && remote != "" && !strings.HasPrefix(remote, local)
}

// ErrUpdateInProgress is Apply's refusal to stack a second update or deploy
// on an app that already has one in flight.
var ErrUpdateInProgress = errors.New("an update or deploy for this app is already in progress")

// ApplyOptions adjusts one update run without touching the app's spec.
type ApplyOptions struct {
	// SkipSnapshot leaves out the pre-update snapshot the spec asks for, for
	// this run only — the escape hatch when the snapshot itself is what
	// fails or hangs, and the operator would rather update without one than
	// not update at all.
	SkipSnapshot bool `json:"skipSnapshot,omitempty"`
}

// Apply updates an app to the latest version of its source: pull + redeploy
// for image apps, plain redeploy for git apps (Deploy re-fetches, rebuilds,
// runs the release phase, and blue-green swaps). Takes a pre-update snapshot
// when configured. The whole run — snapshot, pull, deploy — is one live
// operation: the app reads as deploying from the first moment, so the UI
// can't offer "Update" again mid-update and checks stand aside until done.
func (c *Checker) Apply(ctx context.Context, appName, actor string, opts ApplyOptions) error {
	return c.runOp(ctx, appName, "starting update", func(opCtx context.Context) (*apps.AppSpec, error) {
		return c.apply(opCtx, appName, actor, opts)
	})
}

// runOp holds one cancellable live operation around body, then re-checks
// the app once it has ended — after EndOp, or the check would skip the app
// as still deploying. An update moves the local version forward; a rollback
// moves it back, so the badge should name what was stepped away from.
func (c *Checker) runOp(ctx context.Context, appName, stage string, body func(context.Context) (*apps.AppSpec, error)) error {
	opCtx, ok := c.Engine.BeginCancellableOp(ctx, appName, stage)
	if !ok {
		return ErrUpdateInProgress
	}
	spec, err := body(opCtx)
	if by := c.Engine.CancelledBy(appName); err != nil && by != "" {
		// Whatever step was interrupted reports its own symptom; the cause
		// is the operator.
		err = &apps.CancelledError{By: by}
	}
	c.Engine.EndOp(appName)
	if err != nil {
		return err
	}
	if app, err := c.Store.AppByName(appName); err == nil {
		c.CheckApp(ctx, app, spec)
	}
	return nil
}

// snapshotsFirst reports whether an update or a rollback of this app
// starts with a snapshot: the spec's choice, else the fleet default.
func (c *Checker) snapshotsFirst(spec *apps.AppSpec) bool {
	global, _ := c.Store.GetSetting(backup.SettingBackupBeforeUpdate)
	return backup.SnapshotsBeforeUpdate(spec, global)
}

// preSnapshot takes the snapshot the spec (or the fleet default) asks for
// before an update or a rollback. failMsg prefixes the error that abandons
// the operation.
func (c *Checker) preSnapshot(ctx context.Context, appName string, spec *apps.AppSpec, stage, kind, failMsg string) error {
	if !c.snapshotsFirst(spec) {
		return nil
	}
	c.Engine.OpStage(appName, stage)
	if _, err := c.Backup.BackupApp(ctx, appName, kind); err != nil {
		return fmt.Errorf("%s: %w", failMsg, err)
	}
	return nil
}

// apply is Apply's body, run while the live operation is held.
func (c *Checker) apply(ctx context.Context, appName, actor string, opts ApplyOptions) (*apps.AppSpec, error) {
	app, err := c.Store.AppByName(appName)
	if err != nil {
		return nil, err
	}
	spec, err := apps.LatestSpec(c.Store, app.ID)
	if err != nil {
		return nil, err
	}
	if opts.SkipSnapshot {
		if c.snapshotsFirst(spec) {
			c.Engine.OpStage(appName, "skipping the pre-update snapshot, as asked")
		}
	} else if err := c.preSnapshot(ctx, appName, spec, "taking a pre-update snapshot", "pre-update",
		"pre-update backup failed, update aborted"); err != nil {
		return nil, err
	}
	if spec.Source.Type == "image" {
		c.Engine.OpStage(appName, "pulling the new image")
		pctx, cancel := context.WithTimeout(ctx, pullTimeout)
		_, err := execx.Run(c.Engine.Mat.ComposeCmd(pctx, appName, "pull"))
		cancel()
		if err != nil {
			if pctx.Err() != nil {
				return nil, fmt.Errorf("pulling the new image timed out after %s", pullTimeout)
			}
			return nil, fmt.Errorf("pull failed: %w", err)
		}
	}
	if err := c.Engine.Deploy(ctx, appName, 0, actor); err != nil {
		return nil, err
	}
	return spec, nil
}

// Rollback redeploys an app on a version it has run before: an exact image
// digest for image apps, an exact commit rebuilt from source for git ones.
// Like Apply it snapshots first when configured, and holds one live
// operation for the whole run so the UI reads as busy from the first moment
// rather than after the pull.
//
// The version has to be one fog recorded deploying. That is not paranoia
// about the API: a digest fog never ran is a digest fog cannot say anything
// true about — not what it contained, not whether this app's data survived
// it — and "roll back" would be a guess wearing a safe word.
func (c *Checker) Rollback(ctx context.Context, appName, ref, actor string) error {
	return c.runOp(ctx, appName, "starting rollback", func(opCtx context.Context) (*apps.AppSpec, error) {
		return c.rollback(opCtx, appName, ref, actor)
	})
}

// rollback is Rollback's body, run while the live operation is held.
func (c *Checker) rollback(ctx context.Context, appName, ref, actor string) (*apps.AppSpec, error) {
	app, err := c.Store.AppByName(appName)
	if err != nil {
		return nil, err
	}
	version, err := c.Store.AppVersionByRef(app.ID, ref)
	if err != nil {
		return nil, fmt.Errorf("%s has no record of running that version", appName)
	}
	spec, err := apps.LatestSpec(c.Store, app.ID)
	if err != nil {
		return nil, err
	}
	if spec.Source.Type != version.Source {
		return nil, fmt.Errorf("%s is built from %s now, so its old %s versions can no longer be deployed",
			appName, spec.Source.Type, version.Source)
	}
	// The same snapshot the update path takes, for the same reason and then
	// some: going back a version can meet data a newer version migrated
	// forward, and this is the copy that saves the day when it does.
	if err := c.preSnapshot(ctx, appName, spec, "taking a snapshot before rolling back", "pre-rollback",
		"the snapshot before rolling back failed, so the rollback was abandoned"); err != nil {
		return nil, err
	}
	c.Engine.OpStage(appName, "deploying "+apps.ShortRef(ref))
	if err := c.Engine.DeployVersion(ctx, appName, ref, actor); err != nil {
		return nil, err
	}
	// Still inside the live operation, so no enforce tick can land between
	// the rollback and the policy change: the check skips an app mid-op.
	c.pauseAutoUpdates(ctx, appName, spec, actor)
	return spec, nil
}

// pauseAutoUpdates turns an app that updates itself down to "notify" once a
// rollback has landed. Left on auto, the next enforce tick sees the version
// just stepped away from as an available update and deploys it straight
// back — undoing the rollback hours later, with nobody at the keyboard. It
// is written as a revision, so History shows it beside the rollback, and the
// operator turns auto back on when the newer version is fixed.
//
// A failure here cannot undo a rollback that already worked, so it is said
// out loud instead: auto-updates are still on, and that is worth knowing.
func (c *Checker) pauseAutoUpdates(ctx context.Context, appName string, spec *apps.AppSpec, actor string) {
	if spec.UpdatePolicy != "auto" {
		return
	}
	paused := *spec
	paused.UpdatePolicy = "notify"
	if err := c.Engine.UpdateSpec(ctx, appName, &paused, nil, "updates: auto → notify (rolled back)", actor); err != nil {
		c.Log.Error("pausing auto-updates after rollback", "app", appName, "err", err)
		c.Notify("Auto-updates still on: "+appName,
			"the rollback worked, but turning auto-updates off failed, so the next check may update it again: "+err.Error(), false)
	}
}

// Loop checks all apps every interval and enforces policies. The first pass
// runs shortly after boot rather than one full interval later — a server
// that restarts often would otherwise rarely reach its first check. Two
// minutes staggers it away from selfupdate's ~1-minute boot check so startup
// isn't one burst of network probes.
func (c *Checker) Loop(ctx context.Context, interval time.Duration) {
	loop.Every(ctx, 2*time.Minute, interval, func(ctx context.Context) {
		c.CheckAll(ctx, true)
	})
}

// CheckOne refreshes a single app — what "check for updates" on an app page
// asks for. Answering the question that was asked beats re-scanning every app.
func (c *Checker) CheckOne(ctx context.Context, name string) (CheckResult, error) {
	app, err := c.Store.AppByName(name)
	if err != nil {
		return CheckResult{}, err
	}
	spec, err := apps.LatestSpec(c.Store, app.ID)
	if err != nil {
		return CheckResult{}, err
	}
	return c.Check(ctx, app, spec), nil
}

// actedKey is the per-app setting recording the last remote ref a policy
// acted on — notified for, or attempted (and failed) an auto-update against
// — and when. Same idea as selfupdate's update_last_notified: the badge may
// stay up until the operator acts, but the push fires once per version.
func actedKey(appID int64) string {
	return fmt.Sprintf("app_update_acted:%d", appID)
}

// lastActed reads the ref last acted on for an app and when that happened
// (zero time when unknown or unparseable).
func (c *Checker) lastActed(appID int64) (string, time.Time) {
	v, err := c.Store.GetSetting(actedKey(appID))
	if err != nil {
		return "", time.Time{}
	}
	ref, ts, ok := strings.Cut(v, " ")
	if !ok {
		return ref, time.Time{}
	}
	at, _ := time.Parse(time.RFC3339, ts)
	return ref, at
}

func (c *Checker) markActed(appID int64, ref string) {
	c.Store.SetSetting(actedKey(appID), ref+" "+time.Now().UTC().Format(time.RFC3339))
}

func (c *Checker) clearActed(appID int64) {
	c.Store.DeleteSetting(actedKey(appID))
}

// notifyUpdate pushes "update available" once per remote version.
// UpdateAvailable stays true until the operator updates, and the enforce
// loop runs every few hours — re-announcing the same unapplied update each
// tick is four alerts a day carrying news the operator already has.
func (c *Checker) notifyUpdate(a store.App, spec *apps.AppSpec, res CheckResult) {
	if last, _ := c.lastActed(a.ID); last == res.To {
		return
	}
	msg := spec.Source.Image + " has a new digest"
	if spec.Source.Type == "git" {
		msg = "a new commit was pushed to " + spec.Source.Repo
	}
	c.Notify("Update available: "+a.Name, msg, true)
	c.markActed(a.ID, res.To)
}

// autoUpdate applies an available update, standing down after a failure: the
// same remote ref is not retried until it changes or autoRetryCooldown
// passes, and its failure alert fires once per ref — not once per tick.
func (c *Checker) autoUpdate(ctx context.Context, a store.App, res CheckResult) {
	last, at := c.lastActed(a.ID)
	failedBefore := last == res.To
	if failedBefore && time.Since(at) < autoRetryCooldown {
		c.Log.Debug("auto-update standing down after earlier failure", "app", a.Name, "ref", res.To)
		return
	}
	c.Log.Info("auto-updating", "app", a.Name)
	if err := c.Apply(ctx, a.Name, "auto-update", ApplyOptions{}); errors.Is(err, ErrUpdateInProgress) {
		// A deploy started between the check and here — that deploy
		// is the update happening; nothing failed.
		c.Log.Info("auto-update skipped, deploy already running", "app", a.Name)
	} else if err != nil {
		// A cancelled auto-update still stands down — retrying the run an
		// operator just stopped would be the hang all over again — but it
		// didn't fail, and whoever cancelled it knows.
		var cancelled *apps.CancelledError
		if !failedBefore && !errors.As(err, &cancelled) {
			c.Notify("Auto-update failed: "+a.Name, err.Error(), false)
		}
		c.markActed(a.ID, res.To) // start (or extend) the stand-down
	} else {
		c.Notify("Auto-updated: "+a.Name, "new version deployed and healthy", true)
		c.clearActed(a.ID)
	}
}

// storedResults reports what the most recent check recorded, for a CheckAll
// that lost the overlap guard. Stored facts, not fresh verdicts — the UI
// shows current knowledge rather than the loser pretending it scanned.
func (c *Checker) storedResults() []CheckResult {
	fleet, err := FleetStatus(c.Store)
	if err != nil {
		return nil
	}
	out := make([]CheckResult, 0, len(fleet))
	for _, f := range fleet {
		out = append(out, CheckResult{
			App: f.App, Source: f.Source,
			From: f.Current, To: f.Available,
			Comparable: f.Comparable, UpdateAvailable: f.UpdateAvailable,
		})
	}
	return out
}

// CheckAll refreshes every app and returns what each check found. When
// enforce is set, notify/auto policies fire (manual "check now" calls pass
// enforce=false so the operator just sees badges).
func (c *Checker) CheckAll(ctx context.Context, enforce bool) []CheckResult {
	// One run at a time: a manual "check now" overlapping the enforce loop
	// would double-count failure streaks (crossing failThreshold after ~1.5
	// real outages), duplicate notifications, and race SetAppUpdate writes.
	// The loser answers immediately from stored state instead of blocking.
	if !c.checkMu.TryLock() {
		return c.storedResults()
	}
	defer c.checkMu.Unlock()

	list, err := apps.WithSpecs(c.Store)
	if err != nil {
		return nil
	}

	// Every check is a network round-trip — a registry manifest lookup or a
	// git ls-remote — so running them one after another made "Check for
	// updates" cost the sum of every app's latency while the operator waited.
	// Fan out, then apply policies in list order so the results and any
	// notifications stay deterministic.
	type checked struct {
		app  store.App
		spec *apps.AppSpec
		res  CheckResult
	}
	out := make([]checked, len(list))
	var g errgroup.Group
	g.SetLimit(checkConcurrency)
	for i, a := range list {
		out[i].app = a.App
		g.Go(func() error {
			if ctx.Err() != nil {
				out[i].res = CheckResult{App: a.Name, Error: "the check was cancelled"}
				return nil
			}
			spec := a.Spec
			if spec == nil {
				out[i].res = CheckResult{App: a.Name, Error: "could not read this app's configuration"}
				return nil
			}
			out[i].spec = spec
			cctx, cancel := context.WithTimeout(ctx, checkTimeout)
			out[i].res = c.Check(cctx, &a.App, spec)
			cancel()
			if cctx.Err() != nil && out[i].res.Error != "" {
				out[i].res.Error = "timed out — is the source host reachable? " + out[i].res.Error
			}
			return nil
		})
	}
	g.Wait()

	results := make([]CheckResult, 0, len(list))
	for _, ch := range out {
		a, spec, res := ch.app, ch.spec, ch.res
		results = append(results, res)
		if res.Deploying {
			continue // not a verdict either way
		}
		if res.Error != "" {
			if ctx.Err() == nil { // shutdown cancellations aren't source failures
				sourceRef := res.Source
				if spec != nil {
					if spec.Source.Type == "git" {
						sourceRef = spec.Source.Repo
					} else if spec.Source.Image != "" {
						sourceRef = spec.Source.Image
					}
				}
				c.noteFailure(a.Name, res.Error, sourceRef)
			}
			continue
		}
		c.noteSuccess(a.Name)
		if !res.UpdateAvailable || !enforce || spec == nil {
			continue
		}
		switch spec.UpdatePolicy {
		case "notify":
			c.notifyUpdate(a, spec, res)
		case "auto":
			c.autoUpdate(ctx, a, res)
		}
	}
	return results
}
