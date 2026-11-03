// Deploy pipeline: create/update a spec, then reconcile one revision into
// running containers — fetch, build, provision datastores, render, release.
package apps

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"gitea.lan/matthew/fog/internal/build"
	"gitea.lan/matthew/fog/internal/dockerx"
	"gitea.lan/matthew/fog/internal/execx"
	"gitea.lan/matthew/fog/internal/lucid"
	"gitea.lan/matthew/fog/internal/store"
)

// ---- CRUD ----

func (e *Engine) Create(ctx context.Context, name string, spec *AppSpec, secrets map[string]string, actor string) error {
	return e.create(ctx, name, spec, secrets, actor, false)
}

// CreateForReview creates an app that no deploy will touch until an
// operator has read its review and confirmed it (ConfirmReview) — how an
// imported manifest lands (hostaccess.go).
func (e *Engine) CreateForReview(ctx context.Context, name string, spec *AppSpec, secrets map[string]string, actor string) error {
	return e.create(ctx, name, spec, secrets, actor, true)
}

func (e *Engine) create(ctx context.Context, name string, spec *AppSpec, secrets map[string]string, actor string, hold bool) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	// "uses: [chat]" grants posting into chat, not a connection to an app
	// named chat: an app may not take a name a uses entry means otherwise.
	if SurfaceGrant(name) {
		return fmt.Errorf("name %q is reserved", name)
	}
	// fog's own compose projects and containers are named fog-…; an app may
	// not be one.
	if strings.HasPrefix(name, "fog-") || fogProjects[name] {
		return fmt.Errorf("names starting fog- are fog's own")
	}
	if err := spec.Validate(); err != nil {
		return err
	}
	for k := range secrets {
		if err := checkFogSecretName(k); err != nil {
			return err
		}
		if err := checkToolName(k); err != nil {
			return err
		}
	}
	if err := e.validateHostAccess(name, spec); err != nil {
		return err
	}
	if err := checkAuxNames(name, spec); err != nil {
		return err
	}
	if err := e.checkRouteClaims(name, spec, true); err != nil {
		return err
	}
	if err := e.checkServiceNames(name, spec); err != nil {
		return err
	}
	changed := false
	defer e.specChanged(ctx, name, &changed)
	defer e.lockThenChange(ctx, name, &changed)()

	app, err := e.Store.CreateApp(name)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return fmt.Errorf("an app named %q already exists", name)
		}
		return err
	}
	// A failure past this point must not leave a half-created app burning
	// the name; the row delete cascades away any secrets already stored.
	fail := func(err error) error {
		e.Store.DeleteApp(app.ID)
		return err
	}
	if hold {
		// Before the first revision exists, so there is no moment at which
		// the app is deployable.
		if err := e.Store.SetAppReviewHold(app.ID, true); err != nil {
			return fail(err)
		}
	}
	for k, v := range secrets {
		if err := e.secrets().Put(app.ID, k, v); err != nil {
			return fail(err)
		}
	}
	if spec.Source.Type == "git" {
		if err := e.ensureGitCredentials(app.ID); err != nil {
			return fail(err)
		}
	}
	specJSON, _ := json.Marshal(spec)
	if _, err := e.Store.CreateRevision(app.ID, string(specJSON), "initial", actor); err != nil {
		return fail(err)
	}
	e.Store.Audit(actor, "app.create", name, "")
	changed = true
	return nil
}

// UpdateSpec stores a new revision. Deploying it is a separate (usually
// async) call so API requests return immediately.
func (e *Engine) UpdateSpec(ctx context.Context, name string, spec *AppSpec, secrets map[string]string, comment, actor string) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	for k := range secrets {
		if err := checkFogSecretName(k); err != nil {
			return err
		}
		if err := checkToolName(k); err != nil {
			return err
		}
	}
	if err := e.validateHostAccess(name, spec); err != nil {
		return err
	}
	if err := checkAuxNames(name, spec); err != nil {
		return err
	}
	if err := e.checkRouteClaims(name, spec, true); err != nil {
		return err
	}
	if err := e.checkServiceNames(name, spec); err != nil {
		return err
	}
	changed := false
	defer e.specChanged(ctx, name, &changed)
	defer e.lockThenChange(ctx, name, &changed)()
	app, err := e.Store.AppByName(name)
	if err != nil {
		return err
	}
	// Encrypt before anything is written, so a vault failure surfaces while
	// the store is still untouched.
	ciphertexts := map[string]string{}
	for k, v := range secrets {
		if v == "" {
			continue
		}
		ct, err := e.Vault.Encrypt(v)
		if err != nil {
			return err
		}
		ciphertexts[k] = ct
	}
	specJSON, _ := json.Marshal(spec)
	rev, err := e.Store.CreateRevision(app.ID, string(specJSON), comment, actor)
	if err != nil {
		return err
	}
	// Secret mutations only after the revision insert succeeds: mutating first
	// meant a failed insert left the stored secrets matching a spec that never
	// existed — including deleting a secret the running revision still lists.
	for k, v := range secrets {
		if v == "" { // empty value = delete secret
			if err := e.Store.DeleteSecret(app.ID, k); err != nil {
				return err
			}
			continue
		}
		if err := e.Store.SetSecret(app.ID, k, ciphertexts[k]); err != nil {
			return err
		}
	}
	e.Store.Audit(actor, "app.update", name, fmt.Sprintf(`{"rev":%d}`, rev.Rev))
	changed = true
	return nil
}

// setDeployedCommitUpdate records what a git deploy left running. A deploy
// of the branch head is, by definition, up to date. A rollback is not: it
// deliberately runs something older, and must keep saying so — clearing the
// badge here would hide the very update the operator has just stepped back
// from, until the next scheduled check hours later.
func (e *Engine) setDeployedCommitUpdate(appID int64, commit string, pinned bool) error {
	if !pinned {
		return e.Store.SetAppUpdate(appID, commit, commit, false)
	}
	remote := ""
	if u, err := e.Store.AppUpdateFor(appID); err == nil {
		remote = u.RemoteDigest
	}
	if remote == "" {
		remote = commit
	}
	return e.Store.SetAppUpdate(appID, commit, remote, !strings.HasPrefix(remote, commit))
}

// Deploy applies a specific revision (0 = latest) at its source's current
// version — the branch head for git apps, whatever the tag resolves to for
// image apps.
func (e *Engine) Deploy(ctx context.Context, name string, revNum int64, actor string) error {
	return e.deploy(ctx, name, revNum, "", actor)
}

// DeployVersion applies the latest revision pinned to one version the app
// has run before: an exact image digest, or an exact commit rebuilt from
// source. The rollback verb.
//
// The config revision is deliberately the current one — going back a
// version is not going back a configuration. An operator who wants the old
// settings too rolls the revision back from History, which is its own
// question with its own answer.
func (e *Engine) DeployVersion(ctx context.Context, name, ref, actor string) error {
	if ref == "" {
		return fmt.Errorf("no version given")
	}
	return e.deploy(ctx, name, 0, ref, actor)
}

func (e *Engine) deploy(ctx context.Context, name string, revNum int64, pin, actor string) error {
	changed := false
	defer e.lockThenChange(ctx, name, &changed)()
	app, err := e.Store.AppByName(name)
	if err != nil {
		return err
	}
	if err := e.checkReviewHold(app); err != nil {
		return err
	}
	var rev *store.AppRevision
	if revNum == 0 {
		rev, err = e.Store.LatestRevision(app.ID)
	} else {
		rev, err = e.Store.RevisionByRev(app.ID, revNum)
	}
	if err != nil {
		return err
	}
	if err := e.checkReviewedRev(app, rev.Rev); err != nil {
		return err
	}
	// Even a failed deploy may have changed what serves, so the proxy
	// re-reads routes either way.
	changed = true
	ctx, txn := lucid.Start(ctx, "job", "deploy")
	txn.Attr("app", app.Name)
	txn.Attr("actor", actor)
	txn.Attr("rev", rev.Rev)
	err = e.deployLocked(ctx, app, rev, pin, actor)
	txn.End(err)
	return err
}

// deployLocked runs the full pipeline for one revision:
// fetch/build (git) → provision datastores → render → release → swap/up.
// Caller holds the app lock.
//
// pin, when set, names the exact version to land on instead of the source's
// current one — a manifest digest for an image app, a commit sha for a git
// app. Everything after resolving the image is identical: a rollback is a
// deploy of a known version, not a separate, less-tested path.
func (e *Engine) deployLocked(ctx context.Context, app *store.App, rev *store.AppRevision, pin, actor string) error {
	deployID, err := e.Store.CreateDeploy(app.ID, rev.ID, actor)
	if err != nil {
		return err
	}
	lv := e.liveFor(app.Name)
	ctx = lv.startCtx(ctx)
	defer lv.finish()
	fail := func(err error) error {
		if by := e.CancelledBy(app.Name); by != "" {
			e.Store.FinishDeploy(deployID, "cancelled", lv.String()+"\nstopped at: "+err.Error())
			return &CancelledError{By: by}
		}
		e.Store.FinishDeploy(deployID, "failed", lv.String()+"\nERROR: "+err.Error())
		e.notify("Deploy failed: "+app.Name, err.Error(), false)
		return fmt.Errorf("deploy %s rev %d: %w", app.Name, rev.Rev, err)
	}

	spec, err := ParseSpec(rev.Spec)
	if err != nil {
		return fail(err)
	}
	// An app named as one of fog's own compose projects (saved before such
	// names were refused) would remove fog's proxy as an orphan.
	if fogProjects[app.Name] {
		return fail(fmt.Errorf("%s is the name of fog's own %s project; this app can't be deployed under it", app.Name, app.Name))
	}
	if err := e.checkFogPorts(spec); err != nil {
		return fail(err)
	}
	// Whatever this revision says of itself, it deploys only with what the
	// operator approved for the app as things stand (approvals.go).
	if err := e.checkApprovals(app, spec); err != nil {
		return fail(err)
	}
	// The domain may have changed since the save, making a dotted host and
	// another app's label the same name: the app that would lose it does not
	// go live claiming it.
	if err := e.checkRouteClaims(app.Name, spec, false); err != nil {
		return fail(err)
	}
	env, err := e.baseEnv(app, spec)
	if err != nil {
		return fail(err)
	}
	// The app's own network and its wires (netwire.go), with fog's proxy and
	// Postgres already on its own network before the release command runs.
	nets, err := e.prepareNetworks(ctx, app.Name, spec)
	if err != nil {
		return fail(fmt.Errorf("preparing the app's networks: %w", err))
	}
	// Storage first: cheap, and failing here beats failing after a build.
	if e.EnsureMounts != nil {
		if paths := spec.HostPaths(e.filesRoot()); len(paths) > 0 {
			lv.setStage("checking storage mounts")
			if err := e.EnsureMounts(ctx, paths); err != nil {
				fmt.Fprintln(lv, err.Error())
				return fail(err)
			}
		}
	}

	// Resolve the image: prebuilt reference, or build from git.
	image := spec.Source.Image
	commit := ""
	if pin != "" && spec.Source.Type == "image" {
		image = dockerx.PinDigest(image, pin)
		fmt.Fprintf(lv, "pinned to %s\n", image)
	}
	if spec.Source.Type == "git" {
		// A missing or undecryptable key must fail here, as itself — swallowed,
		// it resurfaced minutes later as a baffling SSH auth error.
		key, err := e.appSecret(app.ID, SecretDeployKey)
		if err != nil {
			return fail(fmt.Errorf("could not read deploy key: %w", err))
		}
		branch := spec.Source.BranchOrDefault()
		// Preflight: fail in seconds with a precise reason, not minutes in.
		lv.setStage("checking repository access")
		if err := e.Builder.LsRemote(ctx, app.Name, spec.Source.Repo, branch, key); err != nil {
			fmt.Fprintln(lv, err.Error())
			return fail(err)
		}
		lv.setStage("fetching source")
		if pin != "" {
			commit, err = e.Builder.FetchCommit(ctx, app.Name, spec.Source.Repo, branch, pin, key, lv)
		} else {
			commit, err = e.Builder.Fetch(ctx, app.Name, spec.Source.Repo, branch, key, lv)
		}
		if err != nil {
			return fail(err)
		}
		if err := e.Builder.CheckDockerfile(app.Name, spec.Source.Dockerfile); err != nil {
			fmt.Fprintln(lv, err.Error())
			return fail(err)
		}
		lv.setStage("building image (commit " + commit[:12] + ")")
		image, err = e.Builder.Build(ctx, app.Name, commit, spec.Source.Dockerfile, spec.Source.BuildArgs, lv)
		if err != nil {
			return fail(err)
		}
	}

	// Routed apps get a stable host port (served by fogd's TCP forwarder —
	// tunnel routes and no-domain setups reach the app at <server>:<port>).
	if app.HostPort == 0 && spec.DirectPort() {
		if p, aerr := e.Store.AllocateHostPort(app.ID); aerr == nil {
			app.HostPort = p
			fmt.Fprintf(lv, "allocated direct port %d\n", p)
		} else {
			e.Log.Warn("host port allocation", "app", app.Name, "err", aerr)
		}
	}

	if err := e.finishEnv(ctx, app, spec, env, commit, lv.setStage, lv); err != nil {
		return fail(err)
	}

	if err := e.Mat.RenderWith(app.Name, spec, env, image, "", nets, lv); err != nil {
		return fail(err)
	}

	// From the release command until the old replica is gone, this app holds
	// up to twice its usual share of the shared cluster's connections. Taken
	// after the build, so only the overlaps queue, and before the release,
	// whose migrations open a pool of their own.
	switchedOver := func() {}
	if spec.Needs != nil && spec.Needs.Postgres != nil {
		release, err := e.pgSwitch.acquire(ctx, app.Name, func(holder string) {
			lv.setStage("waiting for " + holder + " to finish switching over")
			fmt.Fprintf(lv, "waiting for %s's switchover: apps on the shared Postgres switch over one at a time\n", holder)
		})
		if err != nil {
			return fail(fmt.Errorf("waiting to switch over: %w", err))
		}
		var once sync.Once
		switchedOver = func() { once.Do(release) }
		defer switchedOver()
	}

	// Release phase: one-off container from the NEW image, exactly once,
	// before any process switches over (the migrations slot).
	if len(spec.Release) > 0 {
		lv.setStage("running release command")
		if err := e.runRelease(ctx, app.Name, spec.Release, releaseTimeout, lv); err != nil {
			return fail(err)
		}
	}

	// Converge. Main service uses a blue-green overlap when possible.
	lv.setStage("starting containers")
	cctx, cancelConverge := context.WithTimeout(ctx, convergeTimeout)
	err = e.converge(cctx, app.Name, spec, lv)
	cancelConverge()
	switchedOver()
	if err != nil {
		return fail(err)
	}
	// Bring every running container in line with the new wiring: the
	// providers this app now consumes, the proxy, the app leaving the old
	// shared networks. A failure here is the periodic reconcile's to retry;
	// it is said, not swallowed.
	if len(spec.Aux) > 0 {
		e.RemoveLegacyAuxNetwork(ctx, app.Name)
	}
	if err := e.ReconcileNetworks(ctx, map[string]*AppSpec{app.Name: spec}); err != nil {
		fmt.Fprintf(lv, "WARNING: %v\n", err)
		e.Log.Warn("network reconcile after deploy", "app", app.Name, "err", err)
	}
	if spec.Sealed() {
		if err := e.verifySealed(ctx, app.Name); err != nil {
			return fail(err)
		}
	}

	// Post-success bookkeeping. The deploy itself succeeded — a failed write
	// here must not fail it retroactively — but every write is checked and
	// surfaced loudly in both the deploy log and the engine log: a silently
	// lost SetAppCurrentRev leaves a deployed app reading as "new" with no
	// trail to explain why.
	record := func(what string, err error) {
		if err != nil {
			fmt.Fprintf(lv, "WARNING: deploy succeeded but recording %s failed: %v\n", what, err)
			e.Log.Error("deploy bookkeeping write failed", "app", app.Name, "write", what, "err", err)
		}
	}
	if spec.Source.Type == "git" {
		// The image just deployed and the one before it, for a rollback;
		// a git app's image runs to gigabytes, and five of each was most
		// of a small system disk.
		e.pruneBuiltImages(ctx, app.Name, 2)
		// The repo describes its own getting-started info (fog.yaml).
		manifest := e.Builder.ReadRepoManifest(app.Name)
		record("repo notes", e.Store.SetAppRepoNotes(app.ID, manifest.GettingStarted))
		record("app contracts", e.Store.SetAppContracts(app.ID, manifest.Contracts()))
		// What it offers other apps and wants of theirs: recorded each
		// deploy, so a push reaches the installed app.
		declared, derr := DeclaredFrom(manifest.Integrations)
		if derr != nil {
			fmt.Fprintf(lv, "WARNING: fog.yaml's integrations block was ignored: %v\n", derr)
			e.Log.Warn("app declares unreadable integrations", "app", app.Name, "err", derr)
		}
		record("integrations", e.Store.SetAppIntegrations(app.ID, declared.JSON()))
		// What work it hands fog's transcoders (fog-compute/1).
		record("compute", e.Store.SetAppCompute(app.ID, manifest.Compute.JSON()))
		if l := manifest.Compute.UnknownLocality(); l != "" {
			fmt.Fprintf(lv, "WARNING: fog.yaml's compute.inference.locality %q isn't one fog knows; holding the app to %s\n", l, build.LocalityHousehold)
		}
		// Identity-permanence facts + contract validation (Project J): what a
		// host change costs, and any contract fog doesn't recognise. A declared
		// contract fog can't honour is logged loudly — the app asked for
		// behaviour this fogd won't give it.
		unknown := manifest.UnknownContracts()
		if len(unknown) > 0 {
			e.Log.Warn("app declares unknown fog contracts", "app", app.Name, "contracts", unknown)
		}
		record("federation facts", e.Store.SetAppFederationFacts(app.ID, store.FederationFacts{
			Contracts:        manifest.Contracts(),
			HostChange:       manifest.Connect.HostChange,
			UnknownContracts: unknown,
		}))
		// Record what's actually running, and settle the update badge: a
		// deploy of the branch head clears it, a rollback deliberately
		// leaves it up.
		record("deployed commit", e.Store.SetAppDeployedCommit(app.ID, commit))
		record("update state", e.setDeployedCommitUpdate(app.ID, commit, pin != ""))
	}
	// The version that just went live, for the Updates tab's history and the
	// rollbacks it offers. A git app is its commit; an image app is the
	// digest the tag resolved to — read now, while the tag still points at
	// what this deploy ran, because tomorrow it points somewhere else.
	versionRef := commit
	if spec.Source.Type == "image" {
		versionRef = pin
		if versionRef == "" {
			digest, derr := e.Docker.ImageDigest(ctx, image)
			if derr != nil {
				fmt.Fprintf(lv, "note: could not read the digest of %s, so this version won't be listed as a rollback point: %v\n", image, derr)
			}
			versionRef = digest
		}
	}
	record("version history", e.Store.RecordAppVersion(app.ID, spec.Source.Type, versionRef, image, rev.Rev, actor))
	record("current revision", e.Store.SetAppCurrentRev(app.ID, rev.Rev))
	e.rulesRefused.Delete(app.Name)
	e.waiting.Delete(app.Name)
	e.recordContainers(ctx, app.Name, image, spec)
	record("deploy result", e.Store.FinishDeploy(deployID, "succeeded", lv.String()))
	detail := fmt.Sprintf("revision %d is live", rev.Rev)
	if commit != "" {
		detail = fmt.Sprintf("revision %d (commit %.12s) is live", rev.Rev, commit)
	}
	e.notify("Deployed: "+app.Name, detail, true)
	return nil
}

// runRelease executes the one-off release container (migrations slot). The
// container gets a deterministic name: on timeout execx kills the docker CLI's
// process group, but the container itself lives in dockerd and keeps running —
// and with the CLI dead its --rm never fires. The name is the handle that lets
// this run remove a timed-out predecessor's container before starting, and its
// own after a timeout, so a retried deploy can never run two migrations at once.
func (e *Engine) runRelease(ctx context.Context, name string, release []string, timeout time.Duration, lv io.Writer) error {
	cname := "fog-release-" + name
	e.Docker.Remove(ctx, cname, true)
	args := append([]string{"run", "--rm", "--no-deps", "--name", cname, name}, release...)
	fmt.Fprintf(lv, "$ docker compose %s\n", strings.Join(args, " "))
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// Output streams straight into the live log (same as build.go): a
	// 10-minute migration shows its progress line by line, not a frozen
	// screen and then everything at once. The live buffer is also what the
	// error paths hand the operator, so nothing extra is captured here.
	cmd := e.Mat.ComposeCmd(rctx, name, args...)
	cmd.Stdout = lv
	cmd.Stderr = lv
	err := execx.Step(rctx, timeout, "release command", cmd)
	if execx.TimedOut(err) {
		// The cleanup must survive the cancelled deploy it is cleaning up.
		rmCtx, rmCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		e.Docker.Remove(rmCtx, cname, true)
		rmCancel()
		return fmt.Errorf("%w; removed its leftover container %s", err, cname)
	}
	return err
}

// renderEnvTemplates expands {{commit}} and {{fqdn}} in env values, in place.
//
// A variable whose template needs a hostname is DROPPED when there is no
// domain yet, rather than expanded to nothing. An empty expansion is worse
// than an absent variable: "https://{{fqdn}}" became the origin "https://",
// which the app read as configuration and honoured, refusing every request
// it was meant to allow — and it stayed baked in the container after the
// domain was finally set, because templates expand only at deploy time.
func renderEnvTemplates(env map[string]string, commit, fqdn string, lv io.Writer) {
	for k, v := range env {
		v = strings.ReplaceAll(v, "{{commit}}", commit)
		if fqdn == "" && strings.Contains(v, "{{fqdn}}") {
			delete(env, k)
			if lv != nil {
				fmt.Fprintf(lv, "skipping %s: no domain is set yet, so {{fqdn}} has nothing to expand to\n", k)
			}
			continue
		}
		env[k] = strings.ReplaceAll(v, "{{fqdn}}", fqdn)
	}
}

// checkFogPorts refuses a host port fog holds (fogPortErr) — fogd's own is
// known only to the running fogd (FogHostAddr), so checked here as well as
// in Validate.
func (e *Engine) checkFogPorts(spec *AppSpec) error {
	for _, p := range spec.Ports {
		if err := fogPortErr(p.EffectiveHostPort(), e.FogdPort()); err != nil {
			return err
		}
	}
	return nil
}

// baseEnv is the app's environment as its spec and fog give it: its plain
// and secret variables, then fog's own — the API token and URL, the
// inference pool when the repo asks for it, Flare's DSN.
func (e *Engine) baseEnv(app *store.App, spec *AppSpec) (map[string]string, error) {
	env, err := e.mergedEnv(app.ID, spec)
	if err != nil {
		return nil, err
	}
	return env, e.fogEnv(app, spec, env)
}

// fogEnv adds fog's own variables to env: the API token and URL, the
// inference pool when the repo asks for it, Flare's DSN.
func (e *Engine) fogEnv(app *store.App, spec *AppSpec, env map[string]string) error {
	// The app-facing fogd surface: every app gets the bearer token
	// (fog-backup/1 and /app/v1 authenticate with it) and, when fogd's
	// docker-gateway address is known, the URL to call it on. Apps that
	// never call fogd simply never use either — fog stays a convenience,
	// never a dependency.
	tok, err := e.EnsureFogAPIToken(app.ID)
	if err != nil {
		return err
	}
	env["FOG_API_TOKEN"] = tok
	if e.FogHostAddr != "" {
		env["FOG_API_URL"] = "http://" + e.FogHostAddr
		// fog's inference pool (docs/compute-contract.md), for an app whose
		// fog.yaml asks for it: an OpenAI-compatible base and key. The key
		// is the app's own token, so switching the pool off for it in
		// Settings › Compute is the revocation.
		if spec.Source.Type == "git" && e.Builder.ReadRepoManifest(app.Name).Compute.Inference != nil {
			env["FOG_INFERENCE_URL"] = "http://" + e.FogHostAddr + "/app/v1/inference/v1"
			env["FOG_INFERENCE_KEY"] = tok
		}
	}
	// Flare (clients/flare/README.md): every app gets an error-reporting DSN,
	// unconditionally — an app that never reports simply never POSTs. The DSN
	// points at host.docker.internal (materialize pins it to the docker
	// gateway), and its path prefix keeps the derived endpoint outside /api/.
	if e.FogHostAddr != "" {
		key, err := e.ensureErrorsKey(app.ID)
		if err != nil {
			return err
		}
		env["FLARE_DSN"] = fmt.Sprintf("http://%s@%s/ingest/%d", key, e.FogHostAddr, app.ID)
	}
	return nil
}

// finishEnv adds what is known only once the image is (a git app's
// fog.yaml is read from the checkout): the database connection, the
// template variables, household sign-in. Every step is idempotent — the
// role's password and the sign-in client's secret are minted once — so a
// start renders the same environment the deploy did.
func (e *Engine) finishEnv(ctx context.Context, app *store.App, spec *AppSpec, env map[string]string, commit string, stage func(string), log io.Writer) error {
	// Managed Postgres: provision role/dbs/extensions, inject connection env.
	if spec.Needs != nil && spec.Needs.Postgres != nil {
		stage("preparing database")
		conn, err := e.PG.Provision(ctx, app.Name, app.ID, spec.Needs.Postgres.Databases, spec.Needs.Postgres.Extensions)
		if err != nil {
			return fmt.Errorf("postgres provisioning: %w", err)
		}
		fields := map[string]string{"host": conn.Host, "port": conn.Port, "user": conn.User, "password": conn.Password, "dbname": conn.DBName}
		for envVar, field := range spec.Needs.Postgres.Inject {
			env[envVar] = fields[field]
		}
		log.Write([]byte("postgres provisioned\n"))
	}

	// Template vars in env values.
	domain, _ := e.Store.GetSetting(store.SettingBaseDomain)
	fqdn := ""
	if len(spec.Routes) > 0 {
		fqdn = RouteHost(app.Name, spec.Routes[0], domain)
	}
	renderEnvTemplates(env, commit, fqdn, log)

	// Household sign-in (docs/signin-contract.md): an app whose fog.yaml
	// declares fog-signin/1 is registered as a client of fog's provider and
	// handed its configuration. A manifest that declares it wrongly fails
	// the deploy, naming the field: the app asked for something it would
	// not get.
	if e.Signin != nil {
		stage("registering sign-in")
		if err := e.registerSignin(app, spec, fqdn, domain, env, log); err != nil {
			return err
		}
	}

	return nil
}
