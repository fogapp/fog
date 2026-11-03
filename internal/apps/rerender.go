package apps

// Re-rendering: every start, restart and boot writes the app's compose file
// afresh from its current revision, under the rules and the operator's
// approvals as they stand, and brings its containers in line with it — the
// same revision and image, no pull, no build. A compose file on disk is
// what the rules were when the app was last deployed: one from before an
// upgrade kept NET_RAW and unpinned binds, and one from before an approval
// was withdrawn kept the bind. Neither is trusted.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"gitea.lan/matthew/fog/internal/dockerx"
	"gitea.lan/matthew/fog/internal/execx"
	"gitea.lan/matthew/fog/internal/fsx"
	"gitea.lan/matthew/fog/internal/loop"
	"gitea.lan/matthew/fog/internal/store"
)

// rulesError is a revision the rules refuse to bring up as it stands —
// as opposed to a step that failed (a database not yet up).
type rulesError struct{ error }

func (r rulesError) Unwrap() error { return r.error }

// notReadyError is a start that has to wait for storage: a NAS share (or
// the disk Drive was placed on) not mounted yet — after a power cut, the
// NAS is often the last thing up. The app is left stopped, not removed,
// and started when the share mounts (RestartAppsUnder, from the NAS
// manager's late-mount hook). Stopped is safe whatever rendered it: a
// container of an older render has no restart policy by then
// (disarmOldContainers), and a current one binds pins that don't exist
// until fog makes them.
type notReadyError struct{ error }

func (n notReadyError) Unwrap() error { return n.error }

// waitForStorage leaves app stopped, waiting for its storage, and says so.
func (e *Engine) waitForStorage(ctx context.Context, name string, err error) error {
	e.park(ctx, name, err, true)
	e.rulesRefused.Delete(name)
	e.Log.Info("app waits for its storage", "app", name, "err", err)
	if errors.As(err, new(notReadyError)) {
		return err
	}
	return notReadyError{err}
}

// stopContainers stops every container of app, by its project and its
// label alike.
func (e *Engine) stopContainers(ctx context.Context, name string) {
	execx.Run(e.Mat.ComposeCmd(ctx, name, "stop"))
	if e.Docker == nil {
		return
	}
	if cs, err := e.Docker.ListByLabel(ctx, "fog.app="+name); err == nil {
		for _, c := range cs {
			if dockerx.FogSystem(c) == "" && c.State == "running" {
				e.Docker.Stop(ctx, c.ID, 10)
			}
		}
	}
}

// Waiting names each app left stopped until its storage mounts, with why —
// for the issue that says so (app-waiting-for-storage).
func (e *Engine) Waiting() map[string]string {
	out := map[string]string{}
	e.waiting.Range(func(k, v any) bool {
		if w := v.(waitState); w.storage {
			out[k.(string)] = w.why
		}
		return true
	})
	return out
}

// waitState is one app left stopped to be tried again: why, whether it is
// its storage it waits for (the rest is a start that kept failing), and
// when to look again — about a minute, doubling to sixteen.
type waitState struct {
	why     string
	storage bool
	next    time.Time
	delay   time.Duration
}

// park stops app and notes it to be tried again (RetryLoop). The first
// wait is staggered, so a fleet parked at one boot doesn't come due at one
// tick.
func (e *Engine) park(ctx context.Context, name string, err error, storage bool) {
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), lifecycleTimeout)
	defer cancel()
	e.stopContainers(sctx, name)
	w := waitState{why: err.Error(), storage: storage, delay: time.Minute + time.Duration(rand.Int64N(int64(30*time.Second)))}
	if prev, ok := e.waiting.Load(name); ok {
		w.delay = min(prev.(waitState).delay*2, 16*time.Minute)
	}
	w.next = time.Now().Add(w.delay)
	e.waiting.Store(name, w)
}

// RetryWaiting starts each app waiting for its storage whose turn has
// come: the NAS manager wakes those on its shares the moment one mounts,
// and this catches the rest — a USB disk plugged in, an fstab or
// automount share, a folder made by hand. An app stopped since is
// forgotten.
func (e *Engine) RetryWaiting(ctx context.Context) {
	now := time.Now()
	tried := 0
	e.waiting.Range(func(k, v any) bool {
		name, w := k.(string), v.(waitState)
		if a, err := e.Store.AppByName(name); err != nil || a.DesiredState != "running" {
			e.waiting.Delete(name)
			return true
		}
		if now.Before(w.next) {
			return true
		}
		// A few a tick: each may wait on a NAS mount.
		if tried >= retryPerTick {
			return false
		}
		// An app busy with a deploy or a start of its own is left to it.
		unlock, ok := e.tryLock(name)
		if !ok {
			return true
		}
		tried++
		err := e.startLocked(ctx, name)
		unlock()
		switch {
		case err == nil, errors.As(err, new(notReadyError)), errors.As(err, new(rulesError)):
		default:
			e.Log.Warn("starting an app again", "app", name, "err", err)
			e.park(ctx, name, err, false)
		}
		return true
	})
}

// retryPerTick bounds the parked apps RetryWaiting tries at once.
const retryPerTick = 4

// RetryLoop is RetryWaiting every RetryInterval, on its own: a NAS mount
// that hangs holds up nothing else.
func (e *Engine) RetryLoop(ctx context.Context) {
	loop.Every(ctx, RetryInterval, RetryInterval, e.RetryWaiting)
}

// RetryInterval is how often parked apps are looked at.
const RetryInterval = time.Minute

// checkRules is what any revision must pass to run: fog's own names and
// ports, the review, the approvals, the host paths.
func (e *Engine) checkRules(app *store.App, rev int64, spec *AppSpec) error {
	if fogProjects[app.Name] {
		return rulesError{fmt.Errorf("%s is the name of fog's own %s project; this app can't run under it", app.Name, app.Name)}
	}
	for _, check := range []func() error{
		func() error { return e.checkReviewHold(app) },
		func() error { return e.checkReviewedRev(app, rev) },
		func() error { return e.checkFogPorts(spec) },
		func() error { return e.checkApprovals(app, spec) },
		func() error { return e.validateHostAccessWith(app.Name, spec, e.approvedUses(app.Name, spec)) },
	} {
		if err := check(); err != nil {
			return rulesError{err}
		}
	}
	return nil
}

// rerender writes app's compose file from its current revision under the
// rules as they stand, with the image it last deployed.
func (e *Engine) rerender(ctx context.Context, app *store.App, log io.Writer) (*AppSpec, error) {
	_, spec, err := SpecAt(e.Store, app.ID, app.CurrentRev)
	if err != nil {
		return nil, err
	}
	if err := e.checkRules(app, app.CurrentRev, spec); err != nil {
		return nil, err
	}
	image := e.deployedImage(app, spec)
	if image == "" {
		return nil, fmt.Errorf("fog doesn't know which image %s last deployed; deploy it again", app.Name)
	}
	env, missing, err := e.mergedEnvMissing(app.ID, spec, true)
	if err != nil {
		return nil, err
	}
	if len(missing) > 0 {
		e.secretsMissing.Store(app.Name, missing)
	} else {
		e.secretsMissing.Delete(app.Name)
	}
	if err := e.fogEnv(app, spec, env); err != nil {
		return nil, err
	}
	nets, err := e.prepareNetworks(ctx, app.Name, spec)
	if err != nil {
		return nil, fmt.Errorf("preparing the app's networks: %w", err)
	}
	if err := e.finishEnv(ctx, app, spec, env, app.DeployedCommit, func(string) {}, log); err != nil {
		return nil, err
	}
	// The exact images it last ran, not wherever their tags point now: a
	// failed update leaves the app's tag on the new image, and another
	// app's update moves a sidecar tag both share. Only a deploy moves an
	// app to a new image.
	image, pinned := e.pinImages(ctx, app.Name, image, spec)
	if err := e.Mat.RenderWith(app.Name, pinned, env, image, "", nets, log); err != nil {
		return nil, err
	}
	return spec, nil
}

// deployedImage is the image app last deployed: the version fog recorded,
// or else, for an image app, its reference, or else the image its compose
// file names — only ever a name, which grants nothing.
func (e *Engine) deployedImage(app *store.App, spec *AppSpec) string {
	if v, err := e.Store.CurrentAppVersion(app.ID); err == nil && v.Image != "" {
		return v.Image
	}
	if spec.Source.Type == "image" && spec.Source.Image != "" {
		return spec.Source.Image
	}
	b, err := os.ReadFile(filepath.Join(e.Mat.AppDir(app.Name), "compose.yaml"))
	if err != nil {
		return ""
	}
	var doc struct {
		Services map[string]struct {
			Image string `yaml:"image"`
		} `yaml:"services"`
	}
	if yaml.Unmarshal(b, &doc) != nil {
		return ""
	}
	return doc.Services[app.Name].Image
}

// bringUp re-renders app and brings its containers in line: recreated
// where the render changed them, started where stopped; restarted too
// when restart. A revision the rules refuse is not left running.
func (e *Engine) bringUp(ctx context.Context, app *store.App, restart bool) error {
	if e.bringUpSeam != nil {
		return e.bringUpSeam(ctx, app, restart)
	}
	var log bytes.Buffer
	spec, err := e.rerender(ctx, app, &log)
	if log.Len() > 0 {
		e.Log.Warn("re-rendering", "app", app.Name, "notes", log.String())
	}
	if errors.As(err, new(notReadyError)) {
		return e.waitForStorage(ctx, app.Name, err)
	}
	if err != nil {
		e.rulesRefused.Store(app.Name, err.Error())
		if errors.As(err, new(rulesError)) {
			e.waiting.Delete(app.Name)
			// Removed, not stopped: a stopped container keeps its old binds
			// and its restart policy, and a reboot would bring it back.
			sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), lifecycleTimeout)
			e.removeContainers(sctx, app.Name)
			cancel()
			// And nothing is left on disk for anything to run it from.
			if werr := e.Mat.disarmComposeFile(app.Name); werr != nil {
				e.Log.Warn("emptying a refused app's compose file", "app", app.Name, "err", werr)
			}
		}
		return err
	}
	lctx, cancel := context.WithTimeout(ctx, lifecycleTimeout)
	defer cancel()
	if err := execx.Step(lctx, lifecycleTimeout, "start", e.Mat.ComposeCmd(lctx, app.Name, "up", "-d", "--no-build", "--pull", "never")); err != nil {
		e.rulesRefused.Store(app.Name, err.Error())
		return err
	}
	if restart {
		if err := execx.Step(lctx, lifecycleTimeout, "restart", e.Mat.ComposeCmd(lctx, app.Name, "restart")); err != nil {
			return err
		}
	}
	e.rulesRefused.Delete(app.Name)
	e.waiting.Delete(app.Name)
	e.recordContainers(ctx, app.Name, e.deployedImage(app, spec), spec)
	if len(spec.Aux) > 0 {
		e.RemoveLegacyAuxNetwork(ctx, app.Name)
	}
	if err := e.ReconcileNetworks(ctx, map[string]*AppSpec{app.Name: spec}); err != nil {
		e.Log.Warn("network reconcile after start", "app", app.Name, "err", err)
	}
	if spec.Sealed() {
		if err := e.verifySealed(ctx, app.Name); err != nil {
			return err
		}
	}
	return nil
}

// SecretsMissing names each app started without secrets its running
// revision lists, which the vault no longer has, for the issue.
func (e *Engine) SecretsMissing() map[string][]string {
	out := map[string][]string{}
	e.secretsMissing.Range(func(k, v any) bool {
		out[k.(string)] = v.([]string)
		return true
	})
	return out
}

// reapplyRetry is how long the boot pass waits before trying again an app
// whose start failed for a reason that may pass (a database not up yet).
var reapplyRetry = 30 * time.Second

// RulesRefused names each app the rules as they stand didn't bring up,
// with why — for the issue that says so (app-not-reapplied).
func (e *Engine) RulesRefused() map[string]string {
	out := map[string]string{}
	e.rulesRefused.Range(func(k, v any) bool {
		out[k.(string)] = v.(string)
		return true
	})
	return out
}

// ReapplyRules runs at boot: every app that should be running is brought
// up afresh under the rules as they stand (bringUp) — after an upgrade,
// the new rules reach every running container, not only the next deploy.
// It also pins what a reboot's fresh tmpfs lost, which Docker refused the
// app for. A step that failed (a database not up yet) is tried again;
// what the rules refuse is stopped and filed.
func (e *Engine) ReapplyRules(ctx context.Context) {
	e.disarmOldContainers(ctx)
	e.grandfather(ctx)
	pending := e.runningApps()
	lastErr := map[string]error{}
	for attempt := 0; attempt < 3 && len(pending) > 0; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(reapplyRetry):
			}
		}
		var again []string
		for _, name := range pending {
			// Read again each time: an operator who stopped the app since
			// (or deleted it) has the last word.
			if a, err := e.Store.AppByName(name); err != nil || a.DesiredState != "running" {
				continue
			}
			err := e.startIfStillWanted(ctx, name)
			switch {
			case err == nil:
			case errors.As(err, new(rulesError)):
				e.Log.Warn("an app the rules no longer let run was stopped", "app", name, "err", err)
			case errors.As(err, new(notReadyError)):
				// Started by the NAS manager's late-mount hook once its
				// share is up, however long that takes.
			default:
				e.Log.Warn("bringing an app up under the current rules", "app", name, "err", err)
				again = append(again, name)
				lastErr[name] = err
			}
		}
		pending = again
	}
	// Still failing: not left running under an older render — stopped,
	// filed (app-not-reapplied), and tried again on its own (RetryLoop).
	for _, name := range pending {
		e.rulesRefused.Store(name, lastErr[name].Error())
		e.park(ctx, name, lastErr[name], false)
	}
}

func (e *Engine) runningApps() []string {
	list, err := e.Store.ListApps()
	if err != nil {
		e.Log.Warn("listing apps", "err", err)
		return nil
	}
	var out []string
	for _, a := range list {
		if a.CurrentRev > 0 && a.DesiredState == "running" {
			out = append(out, a.Name)
		}
	}
	return out
}

// startIfStillWanted starts name for the boot pass, under its lock, only
// if its desired state still says running when the lock is had — an
// operator's Stop between the check and the start wins.
func (e *Engine) startIfStillWanted(ctx context.Context, name string) error {
	defer e.lock(name)()
	return e.startLocked(ctx, name)
}

// startLocked is startIfStillWanted for a caller holding the app's lock.
func (e *Engine) startLocked(ctx context.Context, name string) error {
	return e.composeLifecycleLocked(ctx, name, "system", "running", "start", func(a *store.App) bool {
		return a.DesiredState == "running"
	})
}

// removeContainers removes every container of app, whatever its compose
// file says now: what the rules refuse must not be left for a restart
// policy or a reboot to bring back.
func (e *Engine) removeContainers(ctx context.Context, name string) {
	if out, err := execx.Run(e.Mat.ComposeCmd(ctx, name, "rm", "--stop", "--force")); err != nil {
		e.Log.Warn("removing an app's containers", "app", name, "err", err, "output", out)
	}
	if e.Docker == nil {
		return
	}
	cs, err := e.Docker.ListByLabel(ctx, "fog.app="+name)
	if err != nil {
		return
	}
	for _, c := range cs {
		if dockerx.FogSystem(c) == "" {
			e.Docker.Remove(ctx, c.ID, true)
		}
	}
}

// disarmOldContainers takes the restart policy from every app container
// fog didn't bring up under today's rules, first thing at boot: until the
// boot pass has rendered it again, neither Docker's restart nor a reboot
// may start it on its old binds. The boot pass then brings it up afresh,
// or removes it.
//
// Which containers are today's is fog's own record (recordContainers),
// by container ID — nothing an image can set. A label was not enough: a
// render from before fog.rules set none of its own, and an image's LABEL
// fog.rules showed through on its container.
func (e *Engine) disarmOldContainers(ctx context.Context) {
	if e.Docker == nil {
		return
	}
	cs, err := e.Docker.ListByLabel(ctx, "fog.app")
	if err != nil {
		e.Log.Warn("listing app containers", "err", err)
		return
	}
	known := map[string]map[string]bool{}
	for _, c := range cs {
		if dockerx.FogSystem(c) != "" {
			continue
		}
		app := c.Labels["fog.app"]
		ids, ok := known[app]
		if !ok {
			ids = e.recordedContainers(app)
			known[app] = ids
		}
		if ids[c.ID] {
			continue
		}
		if err := e.Docker.SetRestartPolicy(ctx, c.ID, "no"); err != nil {
			e.Log.Warn("disarming a container rendered under older rules", "container", c.ID, "err", err)
		}
	}
}

// renderedRecord is fog's note, beside an app's compose file, of the
// containers it brought up from it, under which rules, and the exact image
// each ran ("main" for the app's own, "aux:<name>" for a sidecar's), with
// the reference it was rendered from.
type renderedRecord struct {
	Rules      string                 `json:"rules"`
	Containers []string               `json:"containers"`
	Images     map[string]pinnedImage `json:"images,omitempty"`
}

// pinnedImage is a reference as the spec names it (a tag) and fog's own
// name for the image it resolved to when it last ran (pinRepo). fog's own
// name, not the image ID: the containerd image store deletes an image
// nothing names once its tag moves, even under a running container.
type pinnedImage struct {
	Tag string `json:"tag"`
	Ref string `json:"ref"`
}

// pinRepo is where fog names the images an app last ran: one tag per
// image ("main", "aux-<sidecar>"), moved only by a deploy or a start.
func pinRepo(app string) string { return "fog-pinned/" + app }

func pinTag(key string) string { return strings.ReplaceAll(key, ":", "-") }

func imageKey(c dockerx.ContainerSummary) string {
	if aux := c.Labels["fog.aux"]; aux != "" {
		return "aux:" + aux
	}
	return "main"
}

// pinImages pins the app's image and its sidecars' to the image IDs they
// last ran: from fog's record when the reference is the one recorded, or
// else (an app last brought up by an older fog) from its containers as
// they stand. A reference neither knows — a sidecar added since — renders
// as named.
func (e *Engine) pinImages(ctx context.Context, name, image string, spec *AppSpec) (string, *AppSpec) {
	rec := e.readRecord(name)
	running := map[string]string{}
	if e.Docker != nil && len(rec.Images) == 0 {
		// Last brought up by an older fog: what its containers run, while
		// the image is still there to name.
		if cs, err := e.Docker.ListByLabel(ctx, "fog.app="+name); err == nil {
			for _, c := range cs {
				if dockerx.FogSystem(c) == "" && c.ImageID != "" && c.Labels["fog.process"] == "" {
					if _, err := e.Docker.ImageID(ctx, c.ImageID); err == nil {
						running[imageKey(c)] = c.ImageID
					}
				}
			}
		}
	}
	pin := func(key, tag string) string {
		if p, ok := rec.Images[key]; ok && p.Tag == tag && p.Ref != "" {
			return p.Ref
		}
		if id := running[key]; id != "" {
			return id
		}
		return tag
	}
	out := *spec
	out.Aux = append([]AuxService(nil), spec.Aux...)
	for i := range out.Aux {
		out.Aux[i].Image = pin("aux:"+out.Aux[i].Name, out.Aux[i].Image)
	}
	// The app's own: what it last ran, whatever the version record says —
	// a deploy whose digest couldn't be read recorded no version.
	if p, ok := rec.Images["main"]; ok && p.Ref != "" {
		return p.Ref, &out
	}
	return pin("main", image), &out
}

// readRecord is fog's record for app under today's rules; empty for any
// other, or none.
func (e *Engine) readRecord(app string) renderedRecord {
	var rec renderedRecord
	if !nameRe.MatchString(app) { // a label any image could set
		return rec
	}
	b, err := os.ReadFile(filepath.Join(e.Mat.AppDir(app), renderedFile))
	if err != nil || json.Unmarshal(b, &rec) != nil || rec.Rules != RulesVersion {
		return renderedRecord{}
	}
	return rec
}

const renderedFile = "rendered.json"

// recordContainers notes app's containers as fog's own, under today's
// rules, with the exact image each runs (tags as rendered from: image for
// the app's own, spec's for its sidecars) — after every bring-up and deploy
// that succeeded — and gives each its restart policy back: it is current,
// whatever the boot pass took away before it knew. A crash between the up
// and this leaves them unrecorded: the next boot disarms them and its pass
// brings them up, recorded, again.
func (e *Engine) recordContainers(ctx context.Context, name, image string, spec *AppSpec) {
	if e.Docker == nil {
		return
	}
	cs, err := e.Docker.ListByLabel(ctx, "com.docker.compose.project="+name)
	if err != nil {
		e.Log.Warn("recording an app's containers", "app", name, "err", err)
		return
	}
	tags := map[string]string{"main": image}
	for _, a := range spec.Aux {
		tags["aux:"+a.Name] = a.Image
	}
	rec := renderedRecord{Rules: RulesVersion, Containers: []string{}, Images: map[string]pinnedImage{}}
	for _, c := range cs {
		if c.Labels["fog.app"] != name || dockerx.FogSystem(c) != "" {
			continue
		}
		rec.Containers = append(rec.Containers, c.ID)
		if k := imageKey(c); c.ImageID != "" && c.Labels["fog.process"] == "" && tags[k] != "" {
			if err := e.Docker.TagImage(ctx, c.ImageID, pinRepo(name), pinTag(k)); err != nil {
				e.Log.Warn("naming the image an app runs", "app", name, "image", c.ImageID, "err", err)
				continue
			}
			rec.Images[k] = pinnedImage{Tag: tags[k], Ref: pinRepo(name) + ":" + pinTag(k)}
		}
	}
	b, _ := json.Marshal(rec)
	if err := fsx.WriteFileAtomic(filepath.Join(e.Mat.AppDir(name), renderedFile), b, 0o600); err != nil {
		e.Log.Warn("recording an app's containers", "app", name, "err", err)
		return
	}
	for _, id := range rec.Containers {
		if err := e.Docker.SetRestartPolicy(ctx, id, "unless-stopped"); err != nil {
			e.Log.Warn("giving a container its restart policy back", "app", name, "container", id, "err", err)
		}
	}
}

// recordedContainers is the set recordContainers last noted for app under
// today's rules; empty for any other.
func (e *Engine) recordedContainers(app string) map[string]bool {
	out := map[string]bool{}
	for _, id := range e.readRecord(app).Containers {
		out[id] = true
	}
	return out
}

// BringUpLocked brings app up as a start does — rendered afresh under the
// rules, never from the compose file on disk — for a caller already holding
// the app's lock (a restore, its cancellation). Nothing else may run
// compose up on an app.
func (e *Engine) BringUpLocked(ctx context.Context, name string) error {
	app, err := e.Store.AppByName(name)
	if err != nil {
		return err
	}
	if app.CurrentRev == 0 {
		return fmt.Errorf("%s has never been deployed; deploy it", name)
	}
	if e.EnsureMounts != nil {
		_, spec, err := SpecAt(e.Store, app.ID, app.CurrentRev)
		if err != nil {
			return err
		}
		if err := e.EnsureMounts(ctx, spec.HostPaths(e.filesRoot())); err != nil {
			return e.waitForStorage(ctx, name, err)
		}
	}
	return e.bringUp(ctx, app, false)
}
