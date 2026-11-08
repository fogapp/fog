package apps

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitea.lan/matthew/fog/internal/store/storetest"
)

// Fog's own storage binds freely; the short list is refused whatever is
// acknowledged; everything else on the host is the operator's to
// acknowledge — a denylist can't be complete, so it is the other way round.
func TestHostPathVerdict(t *testing.T) {
	h := HostStorage{App: "app", DataDirs: []string{"/data/fog"}, Managed: []string{"/mnt/nas"}}
	refused := []string{
		"/", "//", "/./", "/../",
		"/var/run/docker.sock", "/var/run/docker.sock/", "//var//run//docker.sock",
		"/var/run/../run/docker.sock", "/run/docker.sock", "/srv/runtime/docker.sock",
		"/run", "/var/run", "/var", "/var/lib", "/var/lib/docker/volumes",
		"/run/containerd/containerd.sock", "/proc", "/proc/1/root", "/sys/fs/cgroup", "/dev", "/dev/sda",
		"/var/lib/fog", "/var/lib/fog/", "/var/lib/fog/apps/other/.env", "/var/lib/fog/data/other",
		"/data", "/data/fog", "/data/fog/vault.key", "/data/./fog/../fog/apps",
		"/var/lib/fog/files/.fog", "/var/lib/fog/files/../vault.key", "/var/lib/fog/filesystem",
	}
	for _, p := range refused {
		if r, _ := h.HostPathVerdict(p, false); r == "" {
			t.Errorf("%q not refused", p)
		}
	}
	// The reviewer's: each was root on the host, or someone's keys, and none
	// was on any list.
	needAck := []string{
		"/run/systemd", "/run/systemd/private", "/run/dbus", "/run/user/1000", "/home/kim/.ssh",
		"/var/spool/cron", "/var/lib/systemd", "/etc", "/etc/shadow", "/root/.ssh", "/boot", "/usr/bin",
		"/mnt/media", "/srv/photos", "/opt/sonarr/config", "/var/lib/fogbound",
		"/etc/localtime",                        // writable: a write into /etc
		"/var/lib/fog/files", "/data/fog/files", // Drive's whole root
		// Other apps' data, every app's at once, other households', fog's own.
		"/data/fog/files/Apps/Paperless", "/data/fog/files/Apps", "/data/fog/files/apps/paperless/config",
		"/data/fog/files/Peers/kim",
	}
	for _, p := range needAck {
		r, ack := h.HostPathVerdict(p, false)
		if r != "" || ack == "" {
			t.Errorf("%q: refused %q, ack %q — want an acknowledgement needed", p, r, ack)
		}
	}
	free := []string{
		"/var/lib/fog/files/Content/Movies", "/data/fog/files/Documents", // Drive folders
		"/data/fog/files/Apps/App/config", // the app's own Drive folder
		"/mnt/nas", "/mnt/nas/media/tv",   // a NAS share
		"/data/fog/data/app/cache", // the app's own data
	}
	for _, p := range free {
		if r, ack := h.HostPathVerdict(p, false); r != "" || ack != "" {
			t.Errorf("%q: refused %q, ack %q — want fog's own storage", p, r, ack)
		}
	}
	if _, ack := h.HostPathVerdict("/etc/localtime", true); ack != "" {
		t.Errorf("read-only /etc/localtime needs %q", ack)
	}
}

func dangerousSpec(host string) *AppSpec {
	return &AppSpec{
		Source:  Source{Type: "image", Image: "evil/portainer:latest"},
		Volumes: []Volume{{Name: "sock", Mount: "/var/run/docker.sock", Host: host}},
	}
}

// The attack: a spec that binds systemd's private socket (root on the host
// through D-Bus), someone's ssh keys, cron's spool — paths no denylist had
// — is refused at save until the spec acknowledges each; and the Docker
// socket, fog's data and / are refused even then.
func TestCheckHostAccessNeedsAnAcknowledgement(t *testing.T) {
	h := HostStorage{App: "evil", Managed: []string{"/mnt/nas"}}
	for _, host := range []string{"/run/systemd/private", "/run/dbus/system_bus_socket", "/home/kim/.ssh",
		"/var/spool/cron", "/var/lib/systemd", "/run/user/0", "/etc", "/mnt/media"} {
		s := dangerousSpec(host)
		if err := s.Validate(); err != nil {
			t.Errorf("%s: Validate = %v (the spec alone can't tell; CheckHostAccess does)", host, err)
		}
		if err := s.CheckHostAccess(h); err == nil || !strings.Contains(err.Error(), "acknowledgeHostAccess") {
			t.Errorf("%s: CheckHostAccess = %v, want a refusal naming acknowledgeHostAccess", host, err)
		}
		s.AcknowledgeHostAccess = []string{host}
		if err := s.CheckHostAccess(h); err != nil {
			t.Errorf("%s acknowledged: %v", host, err)
		}
	}
	for _, host := range []string{"/var/run/docker.sock", "/var/run/../run/docker.sock", "//", "/", "/var/lib/fog",
		"/var/lib/fog/", "/var/lib/fog/apps/manor", "/run/", "/proc", "/dev/sda"} {
		s := dangerousSpec(host)
		s.AcknowledgeHostAccess = []string{host}
		if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "never") {
			t.Errorf("host %q acknowledged: Validate = %v, want refused outright", host, err)
		}
	}
	// A NAS share needs nothing.
	if err := dangerousSpec("/mnt/nas/media").CheckHostAccess(h); err != nil {
		t.Errorf("a NAS share refused: %v", err)
	}
	// A sidecar is no way around it.
	aux := &AppSpec{Source: Source{Type: "image", Image: "x"}, Aux: []AuxService{{
		Name: "agent", Image: "evil/agent",
		Volumes: []Volume{{Name: "root", Mount: "/host", Host: "/run/systemd"}},
	}}}
	if err := aux.CheckHostAccess(h); err == nil {
		t.Error("a sidecar mounting /run/systemd passed")
	}
	// An acknowledgement of one path is not one of another, and can't wait
	// for a later edit to slip a path in under it.
	s := dangerousSpec("/etc")
	s.AcknowledgeHostAccess = []string{"/mnt/media"}
	if err := s.Validate(); err == nil {
		t.Error("an acknowledgement of a path no volume mounts passed")
	}
	// An acknowledgement covers a bind, never a chown of a system folder.
	s = dangerousSpec("/etc")
	s.Volumes[0].Owner = "1000:1000"
	s.AcknowledgeHostAccess = []string{"/etc"}
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "chown") {
		t.Errorf("chown of /etc: Validate = %v, want a refusal", err)
	}
}

// Render is the backstop: a revision saved before these rules, a data dir
// that isn't the default, or a harmless-looking path that is a symlink,
// all fail the deploy.
func TestRenderRefusesHostAccessAtDeploy(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "fogdata")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	m := &Materializer{DataDir: dataDir}
	render := func(host string, ack ...string) error {
		spec := dangerousSpec(host)
		spec.AcknowledgeHostAccess = ack
		return m.Render("evil", spec, nil, "evil:latest", "", io.Discard)
	}
	for _, p := range []string{dataDir, filepath.Join(dataDir, "apps", "manor"), root} {
		if err := render(p, p); err == nil {
			t.Errorf("binding %s rendered, acknowledged", p)
		}
	}
	link := filepath.Join(root, "innocent")
	if err := os.Symlink(dataDir, link); err != nil {
		t.Fatal(err)
	}
	if err := render(link, link); err == nil {
		t.Error("a symlink to the data dir rendered")
	}
	if err := render(filepath.Join(link, "apps"), filepath.Join(link, "apps")); err == nil {
		t.Error("a path under a symlink to the data dir rendered")
	}
	rootLink := filepath.Join(root, "everything")
	if err := os.Symlink("/", rootLink); err != nil {
		t.Fatal(err)
	}
	if err := render(rootLink, rootLink); err == nil {
		t.Error("a symlink to / rendered, acknowledged")
	}
	// An ordinary host folder renders only acknowledged.
	share := filepath.Join(root, "share")
	os.Mkdir(share, 0o755)
	if err := render(share); err == nil {
		t.Error("an unacknowledged host folder rendered")
	}
	if err := render(share, share); err != nil {
		t.Errorf("an acknowledged host folder refused: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(dataDir, "apps", "evil", "compose.yaml")); err == nil && strings.Contains(string(b), dataDir+":") {
		t.Errorf("a refused bind reached compose.yaml:\n%s", b)
	}
}

func reviewEngine(t *testing.T) *Engine {
	t.Helper()
	st, err := storetest.Open(filepath.Join(t.TempDir(), "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &Engine{Store: st, Mat: &Materializer{DataDir: t.TempDir()}, Log: slog.New(slog.DiscardHandler)}
}

// The import attack, end to end at the engine: an imported app is created
// held, every deploy path refuses it, and only a confirmation of the
// revision that was read releases it.
func TestCreateForReviewHoldsEveryDeploy(t *testing.T) {
	e := reviewEngine(t)
	ctx := context.Background()
	spec := dangerousSpec("/run/systemd/private")
	spec.AcknowledgeHostAccess = []string{"/run/systemd/private"} // the manifest acknowledges itself
	spec.Ports = []Port{{Port: 22, HostPort: 22}}
	spec.DeployStrategy = "restart"
	if err := e.CreateForReview(ctx, "evil", spec, nil, "admin"); err != nil {
		t.Fatal(err)
	}
	held, err := e.ReviewPending("evil")
	if err != nil || !held {
		t.Fatalf("imported app not held: %v %v", held, err)
	}
	var pending *ErrReviewPending
	if err := e.Deploy(ctx, "evil", 0, "mcp:assistant"); !errors.As(err, &pending) {
		t.Fatalf("Deploy of a held app = %v, want ErrReviewPending", err)
	}
	if err := e.DeployVersion(ctx, "evil", "sha256:abc", "webhook"); !errors.As(err, &pending) {
		t.Fatalf("DeployVersion of a held app = %v, want ErrReviewPending", err)
	}

	// The review shows what the manifest asks for, acknowledgement and all.
	review, rev, err := e.AppReview("evil")
	if err != nil {
		t.Fatal(err)
	}
	if len(review.Mounts) != 1 || review.Mounts[0].Dangerous == "" || !review.Mounts[0].Acknowledged {
		t.Errorf("review mounts = %+v", review.Mounts)
	}
	if len(review.Ports) != 1 || !review.Ports[0].LowPort || len(review.Warnings) == 0 {
		t.Errorf("review = %+v", review)
	}

	// A confirmation of a revision that is no longer the latest is refused:
	// an edit between reading and confirming can't ride along unread.
	spec2 := *spec
	spec2.Notes = "changed"
	if err := e.UpdateSpec(ctx, "evil", &spec2, nil, "edit", "admin"); err != nil {
		t.Fatal(err)
	}
	if err := e.ConfirmReview("evil", rev, false, "admin"); !errors.Is(err, ErrReviewStale) {
		t.Fatalf("stale confirmation = %v, want ErrReviewStale", err)
	}
	if held, _ := e.ReviewPending("evil"); !held {
		t.Fatal("a stale confirmation released the hold")
	}
	_, rev, _ = e.AppReview("evil")
	if err := e.ConfirmReview("evil", rev, false, "admin"); err != nil {
		t.Fatal(err)
	}
	if held, _ := e.ReviewPending("evil"); held {
		t.Fatal("confirmation left the hold")
	}

	// An app created the ordinary way is never held.
	if err := e.Create(ctx, "plain", &AppSpec{Source: Source{Type: "image", Image: "x"}}, nil, "admin"); err != nil {
		t.Fatal(err)
	}
	if held, _ := e.ReviewPending("plain"); held {
		t.Error("an ordinary create was held")
	}
}

// The live data dir is protected at save, not only at render.
func TestEngineSaveRefusesLiveDataDir(t *testing.T) {
	e := reviewEngine(t)
	spec := dangerousSpec(e.Mat.DataDir + "/apps")
	if err := e.Create(context.Background(), "evil", spec, nil, "admin"); err == nil {
		t.Fatal("a spec binding the live data dir was saved")
	}
}

// A compose file's dangerous binds and privilege settings come through as
// warnings, never as an acknowledgement — and the spec they produce is
// refused until the operator acknowledges.
func TestImportComposeFlagsHostAccess(t *testing.T) {
	services, err := ImportCompose(`
services:
  portainer:
    image: portainer/portainer-ce
    privileged: true
    network_mode: host
    volumes:
      - /var/run/../run/docker.sock:/var/run/docker.sock
      - /:/host:ro
      - /etc/localtime:/etc/localtime:ro
`)
	if err != nil {
		t.Fatal(err)
	}
	s := services[0]
	joined := strings.Join(s.Warnings, "\n")
	for _, want := range []string{"/run/docker.sock", "mounts / —", "privileged dropped", "network_mode dropped"} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings miss %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "/etc/localtime") {
		t.Errorf("read-only /etc/localtime flagged:\n%s", joined)
	}
	if len(s.Spec.AcknowledgeHostAccess) != 0 {
		t.Errorf("import acknowledged on the operator's behalf: %v", s.Spec.AcknowledgeHostAccess)
	}
	if err := s.Spec.Validate(); err == nil {
		t.Error("an imported spec binding docker.sock validated without an acknowledgement")
	}
}

// An import needs no dangerous host path to reach another app's private
// data: a Drive folder in that app's own Apps/<Name>, or a follows. The
// review names both, and the apps it is wired to on the network.
func TestReviewShowsOtherAppsDataAndNetworkReach(t *testing.T) {
	e := reviewEngine(t)
	ctx := context.Background()
	if err := e.Create(ctx, "glass", &AppSpec{Source: Source{Type: "image", Image: "glass"}}, nil, "admin"); err != nil {
		t.Fatal(err)
	}
	spec := &AppSpec{
		Source: Source{Type: "image", Image: "evil"},
		Uses:   []string{"glass"},
		Env:    map[string]string{"TARGET": "http://glass:3000/api"},
		Volumes: []Volume{
			{Name: "loot", Mount: "/loot", Files: "Apps/Glass/config"},
			{Name: "mine", Mount: "/data", Files: "Apps/Evil"},
			{Name: "tag", Mount: "/tag", Files: "Content/Tag", Follows: "glass/config"},
		}, // Glass's folder needs the manifest's acknowledgement to be saved
		// at all, and the operator's confirmation to bind.
		AcknowledgeHostAccess: []string{"Apps/Glass/config"},
	}
	if err := e.CreateForReview(ctx, "evil", spec, nil, "admin"); err != nil {
		t.Fatal(err)
	}
	r, _, err := e.AppReview("evil")
	if err != nil {
		t.Fatal(err)
	}
	var loot *ReviewMount
	for i := range r.Mounts {
		if r.Mounts[i].Volume == "loot" {
			loot = &r.Mounts[i]
		}
		if r.Mounts[i].Volume == "mine" {
			t.Error("the app's own data folder listed as reaching out")
		}
	}
	if loot == nil || loot.OtherApp != "Glass" || loot.Dangerous == "" || !loot.Acknowledged {
		t.Errorf("another app's data folder not named: %+v", r.Mounts)
	}
	joined := strings.Join(r.Warnings, "\n")
	if !strings.Contains(joined, "Glass's own data folder Apps/Glass/config") || !strings.Contains(joined, "follows glass/config") {
		t.Errorf("warnings miss the other app's folder or the follows:\n%s", joined)
	}
	if len(r.Reaches) != 1 || r.Reaches[0] != "glass" {
		t.Errorf("reaches = %v, want [glass]", r.Reaches)
	}
}
