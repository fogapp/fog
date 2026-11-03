// Operations over several apps at once, run when something they share
// changes underneath them: the Files root, a NAS share, the Postgres
// cluster, or a recovery from the offsite locker.
package apps

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// PostgresUpgradeActor is who the audit log says stopped and started the
// database-backed apps around a Postgres major upgrade.
const PostgresUpgradeActor = "postgres-upgrade"

// filesApps names the running apps that mount a Files folder.
func (e *Engine) filesApps() ([]string, error) {
	list, err := WithSpecs(e.Store)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, a := range list {
		if a.DesiredState != "running" || a.Spec == nil || len(a.Spec.FilesVolumes()) == 0 {
			continue
		}
		out = append(out, a.Name)
	}
	return out, nil
}

// RedeployFilesApps redeploys, each on its own goroutine, every running app
// that mounts a Files folder, so its compose file binds the folder where it
// is now. Called after the Files root moves. It returns once every deploy
// has, so a caller that counts it (fogd's shutdown) counts them too.
func (e *Engine) RedeployFilesApps(ctx context.Context) {
	names, err := e.filesApps()
	if err != nil {
		return
	}
	var wg sync.WaitGroup
	for _, name := range names {
		e.Log.Info("redeploying app: its Files folders moved with the root", "app", name)
		wg.Go(func() {
			if err := e.Deploy(ctx, name, 0, "system"); err != nil {
				e.Log.Warn("redeploy after files root move", "app", name, "err", err)
			}
		})
	}
	wg.Wait()
}

// appsUnder names the running apps with a volume on or below mountPoint.
func (e *Engine) appsUnder(mountPoint string) ([]string, error) {
	list, err := WithSpecs(e.Store)
	if err != nil {
		return nil, err
	}
	root := e.filesRoot()
	var out []string
	for _, a := range list {
		if a.DesiredState != "running" || a.Spec == nil {
			continue
		}
		for _, p := range a.Spec.HostPaths(root) {
			if mountPoint == "" || p == mountPoint || strings.HasPrefix(p, mountPoint+"/") {
				out = append(out, a.Name)
				break
			}
		}
	}
	return out, nil
}

// RestartAppsUnder restarts, one after another, every running app with a
// volume on the NAS share mounted at mountPoint. Docker's restart policy
// brings app containers up before the NAS mount loop runs, so such a
// container is bound to the bare directory underneath the share; restarting
// it once the share mounts is the same guarantee EnsureMounts gives a
// deploy, applied late.
func (e *Engine) RestartAppsUnder(ctx context.Context, mountPoint, shareName string) {
	names, err := e.appsUnder(mountPoint)
	if err != nil {
		return
	}
	for _, name := range names {
		e.Log.Info("restarting app: its NAS share mounted after the app started", "app", name, "share", shareName)
		if err := e.Restart(ctx, name, "system"); err != nil {
			e.Log.Warn("restart after late NAS mount", "app", name, "err", err)
		}
	}
}

// DeployingAny names an app with a deploy-shaped operation in flight, the
// first in app order, if there is one.
func (e *Engine) DeployingAny() (string, bool) {
	list, _ := e.Store.ListApps()
	for _, a := range list {
		if e.Deploying(a.Name) {
			return a.Name, true
		}
	}
	return "", false
}

// StopAppsUnder stops every running app with a volume at, under or above
// path and returns the names it stopped, so exactly those come back through
// StartApps. "Above" counts: an app that binds the whole Drive root sees
// whatever is mounted inside it as of when its container was made. It
// stops at the first failure, returning the ones already stopped.
func (e *Engine) StopAppsUnder(ctx context.Context, path, actor string) ([]string, error) {
	list, err := WithSpecs(e.Store)
	if err != nil {
		return nil, err
	}
	root := e.filesRoot()
	var stopped []string
	for _, a := range list {
		if a.DesiredState != "running" || a.Spec == nil {
			continue
		}
		touches := false
		for _, p := range a.Spec.HostPaths(root) {
			if p == path || strings.HasPrefix(p, path+"/") || strings.HasPrefix(path, p+"/") {
				touches = true
				break
			}
		}
		if !touches {
			continue
		}
		if err := e.Stop(ctx, a.Name, actor); err != nil {
			return stopped, fmt.Errorf("stopping %s: %w", a.Name, err)
		}
		stopped = append(stopped, a.Name)
	}
	return stopped, nil
}

// StartApps starts the named apps. Every app is tried even when one fails:
// an app left down because another wouldn't start is worse than the error,
// which names each failure.
func (e *Engine) StartApps(ctx context.Context, names []string, actor string) error {
	var errs []string
	for _, name := range names {
		if err := e.Start(ctx, name, actor); err != nil {
			errs = append(errs, name+": "+err.Error())
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// StopPostgresApps stops every running app that uses the shared Postgres
// cluster and returns the names it stopped, so exactly those come back
// through StartPostgresApps. It stops at the first failure, returning the
// apps already stopped with the error.
func (e *Engine) StopPostgresApps(ctx context.Context) ([]string, error) {
	list, err := WithSpecs(e.Store)
	if err != nil {
		return nil, err
	}
	var stopped []string
	for _, a := range list {
		spec := a.Spec
		if spec == nil || spec.Needs == nil || spec.Needs.Postgres == nil || a.DesiredState != "running" {
			continue
		}
		if err := e.Stop(ctx, a.Name, PostgresUpgradeActor); err != nil {
			return stopped, fmt.Errorf("stopping %s: %w", a.Name, err)
		}
		stopped = append(stopped, a.Name)
	}
	return stopped, nil
}

// StartPostgresApps starts the named apps — the ones StopPostgresApps
// stopped. Every app is tried even when one fails: an app left down because
// another wouldn't start is worse than the error, which names each failure.
func (e *Engine) StartPostgresApps(ctx context.Context, names []string) error {
	return e.StartApps(ctx, names, PostgresUpgradeActor)
}

// DeployRecovered deploys an app brought back from the offsite locker with
// the artifact at artifactPath staged for its first boot (StageArtifact),
// so a delegated app restores its own data as it starts.
func (e *Engine) DeployRecovered(ctx context.Context, name, artifactPath string) error {
	if e.StageArtifact == nil {
		return errors.New("restoring from an artifact needs the backup manager")
	}
	app, err := e.Store.AppByName(name)
	if err != nil {
		return err
	}
	spec, err := LatestSpec(e.Store, app.ID)
	if err != nil {
		return err
	}
	if err := e.StageArtifact(name, spec, artifactPath, true, false, "offsite-recover"); err != nil {
		return err
	}
	// The operator brought this app back: what it was approved to do comes
	// back with it.
	if err := e.ApproveSpec(name, spec, "offsite"); err != nil {
		return err
	}
	return e.Deploy(ctx, name, 0, "offsite")
}
