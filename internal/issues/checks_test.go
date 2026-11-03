package issues

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/backup"
	"gitea.lan/matthew/fog/internal/dockerx"
	"gitea.lan/matthew/fog/internal/nas"
	"gitea.lan/matthew/fog/internal/pg"
	"gitea.lan/matthew/fog/internal/store"
)

func appWith(id int64, name string, spec *apps.AppSpec) appInfo {
	return appInfo{app: store.App{ID: id, Name: name, DesiredState: "running"}, spec: spec}
}

func TestRouteHostCollision(t *testing.T) {
	sn := &snapshot{baseDomain: "home.example", apps: []appInfo{
		appWith(1, "feed", &apps.AppSpec{Routes: []apps.Route{{Port: 80}}}),
		appWith(2, "manor", &apps.AppSpec{Routes: []apps.Route{{Port: 80, Host: "feed.home.example"}}}),
		appWith(3, "hub", &apps.AppSpec{Routes: []apps.Route{{Port: 80}}}),
	}}
	rep := checkRouteHostCollision(context.Background(), sn)
	if len(rep.findings) != 1 || rep.findings[0].Subject != "feed.home.example" {
		t.Fatalf("want one collision on feed.home.example, got %+v", rep.findings)
	}
}

func TestHostPortCollision(t *testing.T) {
	sn := &snapshot{apps: []appInfo{
		appWith(1, "a", &apps.AppSpec{Ports: []apps.Port{{Port: 8080, HostPort: 9000}}}),
		appWith(2, "b", &apps.AppSpec{Ports: []apps.Port{{Port: 3000, HostPort: 9000}}}),
		appWith(3, "c", &apps.AppSpec{Ports: []apps.Port{{Port: 3000, HostPort: 9001}}}),
	}}
	rep := checkHostPortCollision(context.Background(), sn)
	if len(rep.findings) != 1 || rep.findings[0].Subject != "9000/tcp" {
		t.Fatalf("want one collision on 9000/tcp, got %+v", rep.findings)
	}
}

func TestBackupUnscheduled(t *testing.T) {
	seven := 7
	specs := []*apps.AppSpec{
		{Backup: &apps.BackupSpec{Enabled: true}},                  // unscheduled → finding
		{Backup: &apps.BackupSpec{Enabled: true, HourUTC: &seven}}, // per-app hour → fine
		{Backup: &apps.BackupSpec{Enabled: false}},                 // disabled → fine
	}
	sn := &snapshot{}
	for i, s := range specs {
		sn.apps = append(sn.apps, appWith(int64(i+1), string(rune('a'+i)), s))
	}
	rep := checkBackupUnscheduled(context.Background(), sn)
	if len(rep.findings) != 1 || rep.findings[0].AppID != 1 {
		t.Fatalf("want one unscheduled finding for app 1, got %+v", rep.findings)
	}
	// A valid global hour clears it.
	sn.backupHourGlobal = "3"
	if rep := checkBackupUnscheduled(context.Background(), sn); len(rep.findings) != 0 {
		t.Fatalf("global hour should clear the finding, got %+v", rep.findings)
	}
}

func TestVolumeOffShare(t *testing.T) {
	share := func(mp string, mounted bool) nas.ShareStatus {
		s := nas.ShareStatus{Mounted: mounted}
		s.Name = "media"
		s.MountPoint = mp
		return s
	}
	spec := &apps.AppSpec{Volumes: []apps.Volume{{Name: "media", Mount: "/data/media", Host: "/mnt/media/feed"}}}
	sn := &snapshot{
		apps:   []appInfo{appWith(1, "feed", spec)},
		shares: []nas.ShareStatus{share("/mnt/media", false)},
		nasOK:  true,
	}
	rep := checkVolumeOffShare(context.Background(), sn)
	if len(rep.findings) != 1 || rep.findings[0].Severity != "critical" {
		t.Fatalf("unmounted share under a host volume should be critical, got %+v", rep.findings)
	}
	sn.shares = []nas.ShareStatus{share("/mnt/media", true)}
	if rep := checkVolumeOffShare(context.Background(), sn); len(rep.findings) != 0 {
		t.Fatalf("mounted share should be clean, got %+v", rep.findings)
	}
	// Sibling path outside the share must not match by prefix accident.
	sn.shares = []nas.ShareStatus{share("/mnt/med", false)}
	if rep := checkVolumeOffShare(context.Background(), sn); len(rep.findings) != 0 {
		t.Fatalf("/mnt/media is not under /mnt/med, got %+v", rep.findings)
	}
}

// A snapshot whose docker listing failed carries no container evidence:
// every check that reads appInfo.main must return an empty report — zero
// findings AND zero evaluated scopes, so SyncIssues never runs and open
// issues survive the sweep (the rule TestSweepFailedCheckResolvesNothing
// pins down at the sync layer).
func TestContainerChecksSkipWhenDockerListingFailed(t *testing.T) {
	delegated := &apps.AppSpec{
		Needs:  &apps.Needs{Postgres: &apps.PostgresNeed{}},
		Backup: &apps.BackupSpec{Mode: "delegated"},
	}
	a := appWith(1, "feed", delegated)
	// Manifest evidence that would file a skew finding if the check ran.
	a.lastBackup = &store.Backup{Manifest: `{"pg": {"dump_tool": "pg_dump (PostgreSQL) 9.6"}}`}
	sn := &snapshot{dockerOK: false, apps: []appInfo{a}}

	sc := &Scanner{Log: slog.Default(), Docker: &dockerx.Client{},
		FetchDescriptor: func(context.Context, *store.App, *apps.AppSpec) (*backup.Descriptor, error) {
			t.Fatal("descriptor must not be fetched when the listing failed")
			return nil, nil
		}}
	for name, run := range map[string]func(context.Context, *snapshot) report{
		"container-drift":    sc.checkContainerDrift,
		"pg-dump-skew":       sc.checkPGDumpSkew,
		"pg-server-mismatch": sc.checkPGServerMismatch,
	} {
		rep := run(context.Background(), sn)
		if len(rep.findings) != 0 || len(rep.evaluated) != 0 {
			t.Errorf("%s must report nothing on a failed docker listing, got %+v", name, rep)
		}
	}

	// Control: the same snapshot with a healthy listing files the drift —
	// the guard is the listing flag, not the nil main itself.
	sn.dockerOK = true
	rep := sc.checkContainerDrift(context.Background(), sn)
	if len(rep.findings) != 1 || rep.findings[0].Kind != "container-drift" {
		t.Fatalf("healthy listing with a missing main should file drift, got %+v", rep.findings)
	}
}

// Same rule for the NAS side: unreadable share statuses are missing
// evidence, not "all mounted" — the storage checks must not evaluate.
func TestStorageChecksSkipWhenNasStatusFailed(t *testing.T) {
	spec := &apps.AppSpec{Volumes: []apps.Volume{{Name: "media", Mount: "/data/media", Host: "/mnt/media/feed"}}}
	sn := &snapshot{
		nasOK:     false,
		apps:      []appInfo{appWith(1, "feed", spec)},
		backupDir: "/mnt/media/backups",
	}
	for name, run := range map[string]func(context.Context, *snapshot) report{
		"volume-off-share":  checkVolumeOffShare,
		"share-unavailable": checkShareUnavailable,
	} {
		rep := run(context.Background(), sn)
		if len(rep.findings) != 0 || len(rep.evaluated) != 0 {
			t.Errorf("%s must report nothing on a failed NAS status read, got %+v", name, rep)
		}
	}
}

func TestHealthUnmonitored(t *testing.T) {
	sn := &snapshot{apps: []appInfo{
		// A finding: routed traffic, no health check.
		appWith(1, "a", &apps.AppSpec{Routes: []apps.Route{{Port: 80}}}),
		// Fine: it has a health check.
		appWith(2, "b", &apps.AppSpec{Routes: []apps.Route{{Port: 80}}, Health: &apps.Health{Path: "/up"}}),
		// Fine: no traffic to monitor.
		appWith(3, "c", &apps.AppSpec{}),
	}}
	rep := checkHealthUnmonitored(context.Background(), sn)
	if len(rep.findings) != 1 || rep.findings[0].AppID != 1 {
		t.Fatalf("want one unmonitored finding, got %+v", rep.findings)
	}
}

func TestSecretsDrift(t *testing.T) {
	manifest := `{"secrets_fingerprint": {"SECRET_KEY_BASE": "sha256:9f2c11ab04d2"}}`
	a := appWith(1, "feed", &apps.AppSpec{})
	a.lastBackup = &store.Backup{Manifest: manifest}
	sn := &snapshot{apps: []appInfo{a}}

	sc := &Scanner{SecretValue: func(int64, string) (string, error) { return "changed-value", nil }}
	rep := sc.checkBackupSecretsDrift(context.Background(), sn)
	if len(rep.findings) != 1 {
		t.Fatalf("changed secret should drift, got %+v", rep.findings)
	}

	// The recipe is sha256: + first 12 hex of the value — verify a match
	// stays quiet by fingerprinting a known value.
	val := "the-actual-secret"
	sc = &Scanner{SecretValue: func(int64, string) (string, error) { return val, nil }}
	a.lastBackup = &store.Backup{Manifest: `{"secrets_fingerprint": {"SECRET_KEY_BASE": "` + secretFingerprint(val) + `"}}`}
	sn = &snapshot{apps: []appInfo{a}}
	if rep := sc.checkBackupSecretsDrift(context.Background(), sn); len(rep.findings) != 0 {
		t.Fatalf("matching fingerprint should be clean, got %+v", rep.findings)
	}
}

// The skew rule has a direction. Older client than server: the outage
// (critical). Newer client: works, but wrong (warning, pointing at the
// upgrade). Equal: clean. And with no known server major there is no
// evidence, so nothing files and nothing resolves.
func TestPGDumpSkewDirection(t *testing.T) {
	delegated := &apps.AppSpec{
		Needs:  &apps.Needs{Postgres: &apps.PostgresNeed{}},
		Backup: &apps.BackupSpec{Mode: "delegated"},
	}
	probe := func(version string) *Scanner {
		return &Scanner{Log: slog.Default(), ExecInApp: func(context.Context, string, time.Duration, ...string) (string, error) {
			return "pg_dump (PostgreSQL) " + version, nil
		}}
	}
	running := func() appInfo {
		a := appWith(1, "hub", delegated)
		a.main = &dockerx.ContainerSummary{State: "running"}
		return a
	}
	cases := []struct {
		name     string
		client   string
		server   int
		severity string // "" = no finding
	}{
		{"older client is critical", "15.19", 16, "critical"},
		{"newer client is a warning", "18.4", 16, "warning"},
		{"equal is clean", "18.6", 18, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sn := &snapshot{dockerOK: true, pgMajor: c.server, apps: []appInfo{running()}}
			rep := probe(c.client).checkPGDumpSkew(context.Background(), sn)
			if len(rep.evaluated) != 1 {
				t.Fatalf("the app must count as evaluated, got %+v", rep)
			}
			if c.severity == "" {
				if len(rep.findings) != 0 {
					t.Fatalf("expected no finding, got %+v", rep.findings)
				}
				return
			}
			if len(rep.findings) != 1 || rep.findings[0].Severity != c.severity || rep.findings[0].Kind != "pg-dump-skew" {
				t.Fatalf("expected one %s pg-dump-skew, got %+v", c.severity, rep.findings)
			}
		})
	}
	t.Run("unknown server major is no evidence", func(t *testing.T) {
		sn := &snapshot{dockerOK: true, pgMajor: 0, apps: []appInfo{running()}}
		rep := probe("15.19").checkPGDumpSkew(context.Background(), sn)
		if len(rep.findings) != 0 || len(rep.evaluated) != 0 {
			t.Fatalf("expected an empty report, got %+v", rep)
		}
	})
}

// The upgrade prompt is host-scoped and keyed on the data directory's
// major: behind the target files a warning that points at the button,
// at the target it evaluates clean (so the finding resolves), and an
// unknown major is silence.
func TestPGUpgradeAvailable(t *testing.T) {
	sc := &Scanner{Log: slog.Default()}
	rep := sc.checkPGUpgradeAvailable(context.Background(), &snapshot{pgMajor: pg.TargetMajor - 2})
	if len(rep.findings) != 1 || rep.findings[0].Kind != "postgres-upgrade-available" || rep.findings[0].AppID != 0 || rep.findings[0].FixURL != "/settings/storage/database" {
		t.Fatalf("behind the target must file the host-scoped prompt, got %+v", rep)
	}
	rep = sc.checkPGUpgradeAvailable(context.Background(), &snapshot{pgMajor: pg.TargetMajor})
	if len(rep.findings) != 0 || len(rep.evaluated) != 1 || rep.evaluated[0] != 0 {
		t.Fatalf("at the target must evaluate the host scope clean, got %+v", rep)
	}
	rep = sc.checkPGUpgradeAvailable(context.Background(), &snapshot{})
	if len(rep.findings) != 0 || len(rep.evaluated) != 0 {
		t.Fatalf("unknown major must be silence, got %+v", rep)
	}
}

// Legacy volume backups are worth an issue only where the operator can do
// something about them: a git-source app can adopt the contract; an image
// app cannot, so it is evaluated (an old issue resolves) but never filed.
func TestBackupUnverifiedModeOnlyForGitApps(t *testing.T) {
	volumes := func(source string) *apps.AppSpec {
		return &apps.AppSpec{Source: apps.Source{Type: source}, Backup: &apps.BackupSpec{Enabled: true}}
	}
	sn := &snapshot{apps: []appInfo{
		appWith(1, "hub", volumes("git")),
		appWith(2, "paperless", volumes("image")),
		appWith(3, "feed", &apps.AppSpec{Source: apps.Source{Type: "git"}, Backup: &apps.BackupSpec{Enabled: true, Mode: "delegated"}}),
	}}
	rep := checkBackupUnverifiedMode(context.Background(), sn)
	if len(rep.evaluated) != 3 {
		t.Fatalf("every app with a spec is evaluated, got %v", rep.evaluated)
	}
	if len(rep.findings) != 1 || rep.findings[0].AppName != "hub" || rep.findings[0].FixURL != "/apps/hub/drive?tab=schedule" {
		t.Fatalf("only the git-source volume app files, got %+v", rep.findings)
	}
}

// A stopped delegated app cannot back up, so its database probe issues
// have nothing to be about: it evaluates clean (an old issue resolves)
// rather than being skipped for lack of evidence, which would leave a
// pre-upgrade finding open forever. An app wanted running but without a
// container is still no evidence.
func TestPGProbesResolveForStoppedApps(t *testing.T) {
	delegated := &apps.AppSpec{
		Needs:  &apps.Needs{Postgres: &apps.PostgresNeed{}},
		Backup: &apps.BackupSpec{Mode: "delegated"},
	}
	stopped := appWith(1, "hub", delegated)
	stopped.app.DesiredState = "stopped"
	stopped.lastBackup = &store.Backup{Manifest: `{"pg": {"dump_tool": "pg_dump (PostgreSQL) 15.1"}}`}
	wantedButDown := appWith(2, "manor", delegated)
	sn := &snapshot{dockerOK: true, pgMajor: 18, apps: []appInfo{stopped, wantedButDown}}
	sc := &Scanner{Log: slog.Default(),
		ExecInApp: func(context.Context, string, time.Duration, ...string) (string, error) {
			return "", context.DeadlineExceeded
		},
		FetchDescriptor: func(context.Context, *store.App, *apps.AppSpec) (*backup.Descriptor, error) {
			t.Fatal("no descriptor fetch for apps that are not running")
			return nil, nil
		}}
	for name, run := range map[string]func(context.Context, *snapshot) report{
		"pg-dump-skew":       sc.checkPGDumpSkew,
		"pg-server-mismatch": sc.checkPGServerMismatch,
	} {
		rep := run(context.Background(), sn)
		if len(rep.findings) != 0 {
			t.Errorf("%s: no findings for apps that are not running, got %+v", name, rep.findings)
		}
		if len(rep.evaluated) != 1 || rep.evaluated[0] != 1 {
			t.Errorf("%s: the stopped app evaluates clean and the wanted-but-down one is no evidence, got %v", name, rep.evaluated)
		}
	}
}

func TestDockerMissing(t *testing.T) {
	// Not wired: no claim either way.
	if rep := (&Scanner{}).checkDockerMissing(context.Background(), &snapshot{}); len(rep.evaluated) != 0 {
		t.Fatalf("unwired probe must not evaluate the host: %+v", rep)
	}
	sc := &Scanner{DockerAvailable: func(context.Context) bool { return false }}
	rep := sc.checkDockerMissing(context.Background(), &snapshot{})
	if len(rep.findings) != 1 || rep.findings[0].Severity != "critical" || rep.findings[0].FixURL != "/settings/server/docker" {
		t.Fatalf("missing daemon files a critical host issue with the fix link, got %+v", rep)
	}
	sc.DockerAvailable = func(context.Context) bool { return true }
	rep = sc.checkDockerMissing(context.Background(), &snapshot{})
	if len(rep.findings) != 0 || len(rep.evaluated) != 1 {
		t.Fatalf("available daemon evaluates the host and files nothing, got %+v", rep)
	}
}

// The probe answers for the identity apps write as, and asks about the
// folder fog actually hands them — never the Files root, which on a plain
// install is fog's own and no promise to anyone.
func TestFilesOwnerCannotWrite(t *testing.T) {
	onFiles := &apps.AppSpec{Volumes: []apps.Volume{{Name: "media", Mount: "/data", Files: "Apps/Media"}}}
	managedOnly := &apps.AppSpec{Volumes: []apps.Volume{{Name: "config", Mount: "/config"}}}
	sc := &Scanner{Log: slog.Default(), FilesOwner: func() (int, int, bool) { return 1000, 1000, true }}
	base := func(specs ...*apps.AppSpec) *snapshot {
		sn := &snapshot{filesRoot: "/srv/files", nasOK: true}
		for i, sp := range specs {
			sn.apps = append(sn.apps, appWith(int64(i+1), fmt.Sprintf("app%d", i+1), sp))
		}
		return sn
	}
	var asked []string
	denied := func(_ context.Context, dir string, _, _ int) (bool, string, error) {
		asked = append(asked, dir)
		return false, "Permission denied", nil
	}

	swap := canWriteAs
	t.Cleanup(func() { canWriteAs = swap })

	// Two apps sharing one library: one probe, a finding each, both scopes
	// evaluated so a fixed share resolves both.
	canWriteAs = denied
	rep := sc.checkFilesOwnerCannotWrite(context.Background(), base(onFiles, onFiles))
	if len(asked) != 1 || asked[0] != "/srv/files/Apps/Media" {
		t.Fatalf("probed %v, want one ask about the app's own folder", asked)
	}
	if len(rep.findings) != 2 || len(rep.evaluated) != 2 {
		t.Fatalf("findings=%d evaluated=%d, want one each per app", len(rep.findings), len(rep.evaluated))
	}
	f := rep.findings[0]
	if f.Severity != "critical" || f.AppName != "app1" || f.Subject != "/srv/files/Apps/Media" {
		t.Errorf("finding = %+v", f)
	}
	if !strings.Contains(f.Detail, "Permission denied") {
		t.Errorf("the finding should carry the reason: %q", f.Detail)
	}

	// An app with only managed volumes is never handed a folder in Files,
	// so it is not evaluated at all — its scope belongs to no verdict here.
	asked = nil
	if rep := sc.checkFilesOwnerCannotWrite(context.Background(), base(managedOnly)); len(rep.findings) != 0 || len(rep.evaluated) != 0 || asked != nil {
		t.Errorf("nothing to probe: %+v %v", rep, asked)
	}

	canWriteAs = func(context.Context, string, int, int) (bool, string, error) { return true, "", nil }
	if rep := sc.checkFilesOwnerCannotWrite(context.Background(), base(onFiles)); len(rep.findings) != 0 || len(rep.evaluated) != 1 {
		t.Errorf("a writable folder is clean but still evaluated: %+v", rep)
	}

	// No verdict without evidence: a probe that could not run, an owner fog
	// does not enforce, and a folder under an unmounted share (which is
	// checkShareUnavailable's to report) must all stay quiet.
	canWriteAs = func(context.Context, string, int, int) (bool, string, error) {
		return false, "", errors.New("fork/exec: operation not permitted")
	}
	if rep := sc.checkFilesOwnerCannotWrite(context.Background(), base(onFiles)); len(rep.findings) != 0 || len(rep.evaluated) != 0 {
		t.Errorf("a broken instrument is not a verdict: %+v", rep)
	}
	canWriteAs = denied
	unowned := &Scanner{Log: slog.Default(), FilesOwner: func() (int, int, bool) { return 0, 0, false }}
	if rep := unowned.checkFilesOwnerCannotWrite(context.Background(), base(onFiles)); len(rep.findings) != 0 || len(rep.evaluated) != 0 {
		t.Errorf("fog promises nothing when it does not own what it creates: %+v", rep)
	}
	sn := base(onFiles)
	sn.shares = []nas.ShareStatus{{Mounted: false}}
	sn.shares[0].MountPoint = "/srv/files"
	if rep := sc.checkFilesOwnerCannotWrite(context.Background(), sn); len(rep.findings) != 0 {
		t.Errorf("an unmounted share is already its own issue: %+v", rep.findings)
	}
}
