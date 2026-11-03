package apps

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"testing"

	"gitea.lan/matthew/fog/internal/execx"
	"gitea.lan/matthew/fog/internal/store"
)

// fleetEngine is an Engine over a real store whose compose calls hit the
// PATH shim. The shim also logs the directory it ran in — the app's — so a
// test can tell which app each call was for. fail names apps whose compose
// call exits non-zero.
func fleetEngine(t *testing.T, fail ...string) (*Engine, string) {
	t.Helper()
	extra := `echo "in $(basename "$PWD")" >> "$FOG_SHIM_LOG"` + "\n"
	for _, name := range fail {
		extra += `[ "$(basename "$PWD")" = "` + name + `" ] && exit 1` + "\n"
	}
	logPath := fakeDockerCLI(t, extra)
	t.Setenv("FOG_SHIM_LOG", logPath)
	e := versionEngine(t)
	e.Mat = &Materializer{DataDir: t.TempDir()}
	e.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	// Which apps are started, not how: a start's compose call, through the
	// shim, stands in for the render and bring-up (rerender.go).
	e.bringUpSeam = func(ctx context.Context, app *store.App, restart bool) error {
		verb := "start"
		if restart {
			verb = "restart"
		}
		_, err := execx.Run(e.Mat.ComposeCmd(ctx, app.Name, verb))
		return err
	}
	return e, logPath
}

// mkFleetApp creates an app with one revision of spec (none when empty)
// and the given desired state, and the app directory compose runs in.
func mkFleetApp(t *testing.T, e *Engine, name, desired, spec string) {
	t.Helper()
	a, err := e.Store.CreateApp(name)
	if err != nil {
		t.Fatal(err)
	}
	if spec != "" {
		r, err := e.Store.CreateRevision(a.ID, spec, "", "test")
		if err != nil {
			t.Fatal(err)
		}
		e.Store.SetAppCurrentRev(a.ID, r.Rev) // deployed: only that starts
	}
	if err := e.Store.SetAppDesiredState(a.ID, desired); err != nil {
		t.Fatal(err)
	}
	if e.Mat != nil {
		if err := os.MkdirAll(e.Mat.AppDir(name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func desiredState(t *testing.T, e *Engine, name string) string {
	t.Helper()
	a, err := e.Store.AppByName(name)
	if err != nil {
		t.Fatal(err)
	}
	return a.DesiredState
}

// After the Files root moves, only running apps that mount a Files folder
// are redeployed: a host-path volume doesn't move with the root, and a
// stopped app is left stopped.
func TestFilesAppsAreTheRunningFilesMounters(t *testing.T) {
	e := versionEngine(t)
	mkFleetApp(t, e, "paperless", "running", `{"volumes":[{"files":"Apps/Paperless/config","mount":"/config"}]}`)
	mkFleetApp(t, e, "sidecar", "running", `{"aux":[{"name":"s","volumes":[{"files":"Content/Movies","mount":"/m"}]}]}`)
	mkFleetApp(t, e, "parked", "stopped", `{"volumes":[{"files":"Apps/Parked","mount":"/data"}]}`)
	mkFleetApp(t, e, "pool", "running", `{"volumes":[{"host":"/srv/pool","mount":"/pool"}]}`)
	mkFleetApp(t, e, "fresh", "running", "")
	mkFleetApp(t, e, "corrupt", "running", `{not-json`)

	got, err := e.filesApps()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"paperless", "sidecar"}; !reflect.DeepEqual(got, want) {
		t.Errorf("filesApps = %v, want %v", got, want)
	}
}

// A NAS share that mounts late restarts exactly the running apps with a
// volume on it — not one on a sibling path that merely shares a prefix,
// not a stopped one, and not one on the local disk.
func TestRestartAppsUnderRestartsOnlyTheShareDependents(t *testing.T) {
	e, logPath := fleetEngine(t)
	mkFleetApp(t, e, "jellyfin", "running", `{"volumes":[{"host":"/mnt/nas/media","mount":"/media"}]}`)
	mkFleetApp(t, e, "root", "running", `{"volumes":[{"host":"/mnt/nas","mount":"/all"}]}`)
	mkFleetApp(t, e, "parked", "stopped", `{"volumes":[{"host":"/mnt/nas/media","mount":"/media"}]}`)
	mkFleetApp(t, e, "sibling", "running", `{"volumes":[{"host":"/mnt/nas2/x","mount":"/x"}]}`)
	mkFleetApp(t, e, "local", "running", `{"volumes":[{"host":"/srv/pool","mount":"/pool"}]}`)

	e.RestartAppsUnder(context.Background(), "/mnt/nas", "nas")

	log := cliLog(t, logPath)
	if got := strings.Count(log, "compose restart"); got != 2 {
		t.Errorf("%d restarts, want 2:\n%s", got, log)
	}
	for _, name := range []string{"jellyfin", "root"} {
		if !strings.Contains(log, "in "+name+"\n") {
			t.Errorf("%s was not restarted:\n%s", name, log)
		}
	}
	for _, name := range []string{"parked", "sibling", "local"} {
		if strings.Contains(log, "in "+name+"\n") {
			t.Errorf("%s was restarted:\n%s", name, log)
		}
	}
}

// Files volumes resolve against the live root when deciding what sits on
// the share — the root itself may live on the NAS.
func TestRestartAppsUnderResolvesFilesVolumes(t *testing.T) {
	e := versionEngine(t)
	e.Mat = &Materializer{FilesRoot: func() string { return "/mnt/nas/fog" }}
	mkFleetApp(t, e, "paperless", "running", `{"volumes":[{"files":"Apps/Paperless","mount":"/data"}]}`)
	got, err := e.appsUnder("/mnt/nas")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"paperless"}) {
		t.Errorf("appsUnder = %v, want [paperless]", got)
	}
}

// The Postgres upgrade stops the running apps that need the cluster and
// reports exactly those, so exactly those come back.
func TestStopPostgresApps(t *testing.T) {
	const needsPG = `{"needs":{"postgres":{"databases":["main"]}}}`
	e, logPath := fleetEngine(t)
	mkFleetApp(t, e, "db", "running", needsPG)
	mkFleetApp(t, e, "dbparked", "stopped", needsPG)
	mkFleetApp(t, e, "web", "running", `{}`)

	stopped, err := e.StopPostgresApps(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stopped, []string{"db"}) {
		t.Errorf("stopped = %v, want [db]", stopped)
	}
	if log := cliLog(t, logPath); strings.Count(log, "compose stop") != 1 || !strings.Contains(log, "in db\n") {
		t.Errorf("compose calls:\n%s", log)
	}
	if got := desiredState(t, e, "db"); got != "stopped" {
		t.Errorf("db desired state = %q, want stopped", got)
	}
	if got := desiredState(t, e, "web"); got != "running" {
		t.Errorf("web desired state = %q, want running", got)
	}
}

// A stop that fails ends the sweep and hands back what was already
// stopped, so the upgrade can bring those back before giving up.
func TestStopPostgresAppsStopsAtTheFirstFailure(t *testing.T) {
	const needsPG = `{"needs":{"postgres":{"databases":["main"]}}}`
	e, _ := fleetEngine(t, "b")
	mkFleetApp(t, e, "a", "running", needsPG)
	mkFleetApp(t, e, "b", "running", needsPG)
	mkFleetApp(t, e, "c", "running", needsPG)

	stopped, err := e.StopPostgresApps(context.Background())
	if err == nil || !strings.Contains(err.Error(), "stopping b") {
		t.Errorf("err = %v, want one naming b", err)
	}
	if !reflect.DeepEqual(stopped, []string{"a"}) {
		t.Errorf("stopped = %v, want [a]", stopped)
	}
	if got := desiredState(t, e, "c"); got != "running" {
		t.Errorf("c was touched after the failure: %q", got)
	}
}

// Bringing the apps back tries every one, and the error names each that
// failed.
func TestStartPostgresAppsTriesEveryApp(t *testing.T) {
	e, _ := fleetEngine(t, "a")
	mkFleetApp(t, e, "a", "stopped", `{}`)
	mkFleetApp(t, e, "b", "stopped", `{}`)

	err := e.StartPostgresApps(context.Background(), []string{"a", "b"})
	if err == nil || !strings.HasPrefix(err.Error(), "a: ") {
		t.Errorf("err = %v, want one naming a", err)
	}
	if got := desiredState(t, e, "b"); got != "running" {
		t.Errorf("b after a failed to start: %q, want running", got)
	}
	if err := e.StartPostgresApps(context.Background(), []string{"b"}); err != nil {
		t.Errorf("all started: %v", err)
	}
}

// The upgrade refuses under an app mid-deploy, and names it.
func TestDeployingAny(t *testing.T) {
	e := versionEngine(t)
	mkFleetApp(t, e, "a", "running", `{}`)
	mkFleetApp(t, e, "b", "running", `{}`)
	if name, busy := e.DeployingAny(); busy {
		t.Errorf("idle fleet reads as busy with %q", name)
	}
	if !e.BeginOp("b", "deploying") {
		t.Fatal("BeginOp refused")
	}
	if name, busy := e.DeployingAny(); !busy || name != "b" {
		t.Errorf("DeployingAny = %q, %v; want b, true", name, busy)
	}
	e.EndOp("b")
	if name, busy := e.DeployingAny(); busy {
		t.Errorf("after EndOp, still busy with %q", name)
	}
}

// An offsite recovery stages the artifact against the app's newest spec,
// confirmed and attributed to the recovery, before deploying; a staging
// failure stops it there.
func TestDeployRecoveredStagesBeforeDeploying(t *testing.T) {
	e := versionEngine(t)
	if err := e.DeployRecovered(context.Background(), "x", "/a.tar"); err == nil {
		t.Error("no StageArtifact wired, yet no error")
	}

	mkFleetApp(t, e, "manor", "running", `{"source":{"type":"image","image":"manor:2"}}`)
	stageErr := errors.New("artifact belongs to another app")
	var gotName, gotPath, gotBy, gotImage string
	var gotConfirm bool
	e.StageArtifact = func(name string, spec *AppSpec, path string, confirm, _ bool, by string) error {
		gotName, gotPath, gotConfirm, gotBy, gotImage = name, path, confirm, by, spec.Source.Image
		return stageErr
	}
	if err := e.DeployRecovered(context.Background(), "manor", "/lockers/1/artifact.tar"); !errors.Is(err, stageErr) {
		t.Errorf("err = %v, want the staging error", err)
	}
	if gotName != "manor" || gotPath != "/lockers/1/artifact.tar" || !gotConfirm || gotBy != "offsite-recover" || gotImage != "manor:2" {
		t.Errorf("staged %q %q confirm=%v by %q image %q", gotName, gotPath, gotConfirm, gotBy, gotImage)
	}
	if deploys, _ := e.Store.ListDeploySummaries(mustAppID(t, e, "manor"), 10, 0); len(deploys) != 0 {
		t.Errorf("deployed after staging failed: %+v", deploys)
	}

	if err := e.DeployRecovered(context.Background(), "ghost", "/a.tar"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown app: %v", err)
	}
}

func mustAppID(t *testing.T, e *Engine, name string) int64 {
	t.Helper()
	a, err := e.Store.AppByName(name)
	if err != nil {
		t.Fatal(err)
	}
	return a.ID
}
