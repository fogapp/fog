// Start/stop/restart and deletion, including teardown of everything an app
// owns outside the store: containers, images, build workspace, data.
package apps

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"gitea.lan/matthew/fog/internal/build"
	"gitea.lan/matthew/fog/internal/store"

	"gitea.lan/matthew/fog/internal/execx"
)

// ---- lifecycle ----

func (e *Engine) Start(ctx context.Context, name, actor string) error {
	return e.composeLifecycle(ctx, name, actor, "running", "start")
}

func (e *Engine) Stop(ctx context.Context, name, actor string) error {
	return e.composeLifecycle(ctx, name, actor, "stopped", "stop")
}

func (e *Engine) Restart(ctx context.Context, name, actor string) error {
	return e.composeLifecycle(ctx, name, actor, "running", "restart")
}

func (e *Engine) composeLifecycle(ctx context.Context, name, actor, desired, verb string) error {
	return e.composeLifecycleIf(ctx, name, actor, desired, verb, nil)
}

// composeLifecycleIf is composeLifecycle that does nothing when still,
// asked under the app's lock, says no.
func (e *Engine) composeLifecycleIf(ctx context.Context, name, actor, desired, verb string, still func(*store.App) bool) error {
	defer e.lock(name)()
	return e.composeLifecycleLocked(ctx, name, actor, desired, verb, still)
}

// composeLifecycleLocked is composeLifecycleIf for a caller holding the
// app's lock.
func (e *Engine) composeLifecycleLocked(ctx context.Context, name, actor, desired, verb string, still func(*store.App) bool) error {
	app, err := e.Store.AppByName(name)
	if err != nil {
		return err
	}
	if still != nil && !still(app) {
		return nil
	}
	if verb != "stop" && app.CurrentRev > 0 {
		// Starting containers whose volumes live on a NAS requires the
		// share to be up — mount on demand rather than binding an empty
		// directory.
		if e.EnsureMounts != nil {
			_, spec, err := SpecAt(e.Store, app.ID, app.CurrentRev)
			if err != nil {
				return err
			}
			if err := e.EnsureMounts(ctx, spec.HostPaths(e.filesRoot())); err != nil {
				// A share not up yet: the app waits for it, stopped — and
				// is to run once it can (RetryWaiting, the mount hook).
				e.Store.SetAppDesiredState(app.ID, desired)
				return e.waitForStorage(ctx, name, err)
			}
		}
		// Rendered afresh, under the rules and approvals as they stand
		// (rerender.go): never what an older compose file says.
		if err := e.bringUp(ctx, app, verb == "restart"); err != nil {
			if errors.As(err, new(notReadyError)) {
				e.Store.SetAppDesiredState(app.ID, desired)
			}
			return err
		}
	} else if verb != "stop" {
		// Never deployed: there's nothing fog rendered to start, and a
		// compose file on disk is never run as it stands.
		return fmt.Errorf("%s has never been deployed; deploy it", name)
	} else {
		lctx, cancel := context.WithTimeout(ctx, lifecycleTimeout)
		defer cancel()
		if err := execx.Step(lctx, lifecycleTimeout, verb, e.Mat.ComposeCmd(lctx, name, verb)); err != nil {
			return err
		}
	}
	e.Store.SetAppDesiredState(app.ID, desired)
	e.Store.Audit(actor, "app."+verb, name, "")
	return nil
}

func (e *Engine) Delete(ctx context.Context, name string, deleteData bool, actor string) error {
	changed := false
	defer e.specChanged(ctx, name, &changed)
	defer e.lockThenChange(ctx, name, &changed)()
	app, err := e.Store.AppByName(name)
	if err != nil {
		return err
	}
	// The latest spec drives teardown of everything living outside the
	// store — read it before the row (and its revisions) cascade away.
	//
	// An unreadable spec still lets the delete proceed, because refusing would
	// leave the operator with an app they cannot remove. But it is worth
	// saying loudly: without it we don't know which databases to deprovision
	// or which images this app owned, so those are left behind rather than
	// silently skipped.
	spec, err := LatestSpec(e.Store, app.ID)
	if err != nil {
		e.Log.Warn("deleting an app whose stored configuration could not be read; "+
			"its databases and images are being left in place",
			"app", name, "err", err)
		spec = &AppSpec{}
	}

	// Best-effort container teardown; the app dir may already be gone.
	dctx, cancelDown := context.WithTimeout(ctx, teardownTimeout)
	if _, err := execx.Run(e.Mat.ComposeCmd(dctx, name, "down")); err != nil {
		e.Log.Warn("compose down during delete", "app", name, "err", err)
	}
	e.RemoveAppNetworks(dctx, name)
	// fog's names for the images it last ran (rerender.go).
	for _, p := range e.readRecord(name).Images {
		if e.Docker != nil {
			e.Docker.RemoveImage(dctx, p.Ref)
		}
	}
	if e.Mat.Unpin != nil {
		if err := e.Mat.Unpin(dctx, filepath.Join(e.Mat.BindsDir(), name)); err != nil {
			e.Log.Warn("removing the app's pins", "app", name, "err", err)
		}
	}
	cancelDown()

	// Images. Git-built images belong to this app alone; pulled images go
	// only when no other app's current spec references them. Docker refuses
	// to remove an image a container still uses, so this stays safe even if
	// the teardown above missed something.
	if spec.Source.Type == "git" {
		e.pruneBuiltImages(ctx, name, 0)
		// Fog can delete its copy of the deploy key but not the public half
		// authorized on the git host — that's the operator's move. Read the
		// key now, before the row cascade takes the secret with it.
		if pub, err := e.appSecret(app.ID, SecretDeployKeyPub); err == nil {
			e.Store.CreateNotice("deploy-key-cleanup", name,
				"Remove "+name+"'s deploy key from the git host",
				"Fog deleted its copy of the key, but the repository still trusts the public half. "+
					"Remove this deploy key in the repo's settings:\n\n"+
					"Repository: "+spec.Source.Repo+"\n"+pub)
		}
	}
	e.removeUnreferencedImages(ctx, app.ID, spec)
	// Build workspace: checkout plus the on-disk deploy key copy.
	e.Builder.RemoveBuilder(ctx, name)
	if err := e.Builder.Remove(name); err != nil {
		e.Log.Warn("removing build dir", "app", name, "err", err)
	}

	// Database contents and snapshots are unrecreatable user data — they
	// go only when the user explicitly asked for their data to go too.
	if deleteData {
		if spec.Needs != nil && spec.Needs.Postgres != nil {
			if err := e.PG.Deprovision(ctx, name, spec.Needs.Postgres.Databases); err != nil {
				e.Log.Warn("postgres deprovision", "app", name, "err", err)
			}
		}
		if e.RemoveBackups != nil {
			if err := e.RemoveBackups(name); err != nil {
				e.Log.Warn("removing backups", "app", name, "err", err)
			}
		}
	}

	if err := e.Mat.Remove(name, deleteData); err != nil {
		return err
	}
	if err := e.Store.DeleteApp(app.ID); err != nil {
		return err
	}
	// After the row is gone, so Drive no longer counts the folder as one
	// this app mounts — the guard that refuses to trash a mounted folder is
	// exactly right until the moment there is no app to mount it.
	if deleteData && e.TrashDriveFolder != nil {
		if err := e.TrashDriveFolder(name); err != nil {
			e.Log.Warn("trashing the app's Drive folder", "app", name, "err", err)
		}
	}
	e.Store.Audit(actor, "app.delete", name, fmt.Sprintf(`{"deleteData":%v}`, deleteData))
	e.forgetApp(ctx, name, actor)
	e.rulesRefused.Delete(name)
	e.waiting.Delete(name)
	changed = true
	return nil
}

// forgetApp takes a deleted app out of every other app's spec: its name in
// uses and reach, and the follows of a volume that tracked one of its
// folders. Left behind, they would be approvals and wiring for whatever app
// takes the name next — a held import named like the old provider
// repointed its followers at once.
//
// The volume keeps the folder it has, but a folder in the deleted app's
// own Apps/<Name> was the consumer's only because that app shared it: from
// its next start the consumer binds it only if the operator approves it,
// as any other app's folder (deliberately — a later app of that name would
// own it). A notice names each such consumer.
func (e *Engine) forgetApp(ctx context.Context, name, actor string) {
	list, err := WithSpecs(e.Store)
	if err != nil {
		return
	}
	for _, a := range list {
		if a.Spec == nil || a.Name == name {
			continue
		}
		spec := *a.Spec
		changed := false
		drop := func(xs []string) []string {
			out := xs[:0:0]
			for _, x := range xs {
				if x == name {
					changed = true
					continue
				}
				out = append(out, x)
			}
			return out
		}
		spec.Uses, spec.Reach = drop(a.Spec.Uses), drop(a.Spec.Reach)
		unfollow := func(vols []Volume) []Volume {
			out := append([]Volume(nil), vols...)
			for i := range out {
				if app, _, err := parseFollows(out[i].Follows); err == nil && app == name {
					out[i].Follows = ""
					changed = true
				}
			}
			return out
		}
		spec.Volumes = unfollow(a.Spec.Volumes)
		spec.Aux = append([]AuxService(nil), a.Spec.Aux...)
		for i := range spec.Aux {
			spec.Aux[i].Volumes = unfollow(spec.Aux[i].Volumes)
		}
		if !changed {
			continue
		}
		var orphaned []string
		for _, dv := range spec.driveVolumes() {
			if rel := cleanDriveRel(dv.Files); (Volume{Files: rel}).IsAppData(name) && !spec.acknowledgedDrive(rel) {
				orphaned = append(orphaned, rel)
			}
		}
		if len(orphaned) > 0 {
			// Asked for in the spec, so it still saves; approved only when
			// the operator says so (approvals.go).
			spec.AcknowledgeHostAccess = append(append([]string{}, spec.AcknowledgeHostAccess...), orphaned...)
			e.Store.CreateNotice("shared-folder-orphaned", a.Name,
				a.Name+" will stop at its next start: "+name+" no longer shares "+strings.Join(orphaned, ", "),
				name+" was deleted, and with it the sharing that let "+a.Name+" bind "+strings.Join(orphaned, ", ")+
					". That folder is no app's to share now, and an app installed later as "+name+" would own it. "+
					"From its next start "+a.Name+" is stopped unless you approve the folder — save its settings with it — or point the volume elsewhere.")
		}
		if err := e.UpdateSpec(ctx, a.Name, &spec, nil, name+" was deleted: forget it", actor); err != nil {
			e.Log.Warn("forgetting a deleted app", "app", a.Name, "deleted", name, "err", err)
		}
	}
}

// specImages lists the pulled image references a spec depends on: the main
// image for image-source apps plus every aux sidecar image.
func specImages(spec *AppSpec) []string {
	var refs []string
	if spec.Source.Type == "image" && spec.Source.Image != "" {
		refs = append(refs, spec.Source.Image)
	}
	for _, a := range spec.Aux {
		if a.Image != "" {
			refs = append(refs, a.Image)
		}
	}
	return refs
}

// unreferencedImages returns spec's images that none of the others use.
func unreferencedImages(spec *AppSpec, others []*AppSpec) []string {
	own := map[string]bool{}
	for _, ref := range specImages(spec) {
		own[ref] = true
	}
	for _, o := range others {
		if o.Source.Image != "" {
			delete(own, o.Source.Image)
		}
		for _, a := range o.Aux {
			delete(own, a.Image)
		}
	}
	refs := make([]string, 0, len(own))
	for ref := range own {
		refs = append(refs, ref)
	}
	return refs
}

// pruneBuiltImages keeps the newest keep fog/<app> image tags, for
// rollback, and removes the rest. Never fails a deploy: an image a
// container still uses is refused by Docker and stays.
func (e *Engine) pruneBuiltImages(ctx context.Context, app string, keep int) {
	repo := build.ImageRepo(app)
	images, err := e.Docker.ListImages(ctx, repo)
	if err != nil {
		e.Log.Warn("listing built images", "app", app, "err", err)
		return
	}
	var tags []string // newest first, as ListImages orders images
	for _, im := range images {
		for _, t := range im.RepoTags {
			if strings.HasPrefix(t, repo+":") {
				tags = append(tags, t)
			}
		}
	}
	for _, t := range tags[min(keep, len(tags)):] {
		e.Docker.RemoveImage(ctx, t)
	}
}

// removeUnreferencedImages removes the app's pulled images unless another
// app's latest revision still references them. Best-effort: a shared or
// still-in-use image simply fails the rmi and stays.
func (e *Engine) removeUnreferencedImages(ctx context.Context, appID int64, spec *AppSpec) {
	if len(specImages(spec)) == 0 {
		return
	}
	latest, err := LatestSpecs(e.Store)
	if err != nil {
		return
	}
	var others []*AppSpec
	for id, o := range latest {
		if id != appID {
			others = append(others, o)
		}
	}
	for _, ref := range unreferencedImages(spec, others) {
		e.Docker.RemoveImage(ctx, ref)
	}
}
