package apps

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gitea.lan/matthew/fog/internal/build"
	"gitea.lan/matthew/fog/internal/dockerx"
	"gitea.lan/matthew/fog/internal/fogports"
	"gitea.lan/matthew/fog/internal/store"
)

// The attack: an import's first revision binds /root/.ssh and acknowledges
// it itself. The operator edits the bind away and confirms the clean
// revision — and rolling back to the first gave the app root's keys. Now a
// revision older than the confirmed one doesn't deploy, nor does any
// revision needing what the operator hasn't approved, whatever it says of
// itself; and an assistant can't deploy a revision with access the latest
// lacks.
func TestRollbackToAnUnapprovedRevisionIsRefused(t *testing.T) {
	e := reviewEngine(t)
	ctx := context.Background()
	evil := &AppSpec{
		Source:                Source{Type: "image", Image: "evil/app:1"},
		Volumes:               []Volume{{Name: "keys", Mount: "/keys", Host: "/root/.ssh"}},
		AcknowledgeHostAccess: []string{"/root/.ssh"},
	}
	if err := e.CreateForReview(ctx, "evil", evil, nil, "import"); err != nil {
		t.Fatal(err)
	}
	clean := &AppSpec{Source: evil.Source}
	if err := e.UpdateSpec(ctx, "evil", clean, nil, "drop the bind", "admin"); err != nil {
		t.Fatal(err)
	}
	_, rev, err := e.AppReview("evil")
	if err != nil || rev != 2 {
		t.Fatalf("review at rev %d: %v", rev, err)
	}
	if err := e.ConfirmReview("evil", rev, false, "admin"); err != nil {
		t.Fatal(err)
	}

	// The rollback, from History: refused, it was never read.
	if err := e.Deploy(ctx, "evil", 1, "admin"); err == nil || !strings.Contains(err.Error(), "older than the one its review was confirmed at") {
		t.Fatalf("rollback to the unread revision = %v", err)
	}
	// From an assistant: refused before it starts.
	if err := e.CheckRollbackSameAccess("evil", 1); err == nil {
		t.Fatal("an assistant may deploy the revision that binds /root/.ssh")
	}
	if err := e.CheckRollbackSameAccess("evil", 2); err != nil {
		t.Fatalf("an assistant may not redeploy the latest's equal: %v", err)
	}

	// A later revision that brings the bind back — written by an
	// integration, a webhook, an assistant's edit, not the operator — is
	// past the confirmed one, and still doesn't deploy: its own
	// acknowledgement is no approval.
	if err := e.UpdateSpec(ctx, "evil", evil, nil, "integration", "integration:x"); err != nil {
		t.Fatal(err)
	}
	if err := e.Deploy(ctx, "evil", 0, "webhook"); err == nil || !strings.Contains(err.Error(), "no operator has approved: host folder /root/.ssh") {
		t.Fatalf("deploy of a self-acknowledged bind = %v", err)
	}

	// The operator's own save approves it; their withdrawal takes it back,
	// and the old revision is refused again.
	app, _ := e.Store.AppByName("evil")
	if err := e.ApproveSpec("evil", evil, "admin"); err != nil {
		t.Fatal(err)
	}
	if err := e.checkApprovals(app, evil); err != nil {
		t.Fatalf("approved and still refused: %v", err)
	}
	if err := e.ApproveSpec("evil", clean, "admin"); err != nil {
		t.Fatal(err)
	}
	if err := e.checkApprovals(app, evil); err == nil {
		t.Fatal("a withdrawn approval still holds")
	}
}

// The attack: an app publishing 443 — or fogd's port — wins it from fog
// at boot, when Docker starts the apps before fogd brings Caddy up, and
// answers in fog's place. Every port fog holds (fogports, the one list
// fog's own compose files publish from) is refused outright: no
// acknowledgement, a stored one included, lets an app have it.
func TestFogsHostPortsAreNeverAnApps(t *testing.T) {
	ports := []int{fogports.DirectMin, fogports.DirectMax}
	for _, p := range fogports.Published() {
		ports = append(ports, p.Host)
	}
	if !slices.Contains(ports, 443) || !slices.Contains(ports, 80) {
		t.Fatalf("fog's own ports miss Caddy's: %v", ports)
	}
	for _, hp := range ports {
		for _, proto := range []string{"tcp", "udp"} {
			// As an old revision would carry it, acknowledgement and all.
			raw := `{"source":{"type":"image","image":"x"},"deployStrategy":"restart",` +
				`"ports":[{"port":8080,"hostPort":` + strconv.Itoa(hp) + `,"protocol":"` + proto + `"}],` +
				`"acknowledgeHostPorts":[` + strconv.Itoa(hp) + `]}`
			s, err := ParseSpec(raw)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "publish another") {
				t.Errorf("host port %d/%s: %v", hp, proto, err)
			}
		}
	}
	e := &Engine{FogHostAddr: "host.docker.internal:8080"}
	if err := e.checkFogPorts(&AppSpec{Ports: []Port{{Port: 1, HostPort: 8080}}}); err == nil {
		t.Error("fogd's own port accepted")
	}
	if err := e.checkFogPorts(&AppSpec{Ports: []Port{{Port: 53, HostPort: 53, Protocol: "udp"}}}); err != nil {
		t.Errorf("an ordinary port refused: %v", err)
	}

	// One stored before fog refused it deploys no more, and doesn't start.
	e, st := routeEngine(t, "")
	a, _ := st.CreateApp("squat")
	r, _ := st.CreateRevision(a.ID, `{"source":{"type":"image","image":"x"},"deployStrategy":"restart","ports":[{"port":443,"hostPort":443}],"acknowledgeHostPorts":[443]}`, "", "t")
	st.SetAppCurrentRev(a.ID, r.Rev)
	if err := e.Deploy(context.Background(), "squat", 0, "t"); err == nil || !strings.Contains(err.Error(), "fog's caddy") {
		t.Fatalf("deploy of a stored squatter = %v", err)
	}
	if err := e.Start(context.Background(), "squat", "t"); err == nil || !strings.Contains(err.Error(), "fog's caddy") {
		t.Fatalf("start of a stored squatter = %v", err)
	}
}

// The attack, at boot: the app holds fog's port already. fog stops it —
// every container of it — before bringing its own services up, keeps it
// stopped, and says why; fog's own Caddy and an app on an ordinary port
// are left alone.
func TestReclaimFogPortsStopsTheHolder(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "calls")
	held := func(id, app string, pub int, names ...string) dockerx.ContainerSummary {
		c := dockerx.ContainerSummary{ID: id, State: "running", Names: names, Labels: map[string]string{"fog.app": app},
			Ports: []dockerx.ContainerPort{{IP: "0.0.0.0", PrivatePort: 80, PublicPort: pub, Type: "tcp"}}}
		return c
	}
	api := &fakeDockerAPI{cliLog: logPath, list: func(int) []dockerx.ContainerSummary {
		return []dockerx.ContainerSummary{
			held("caddy1", "caddy", 443, "/caddy"),
			held("evil1", "evil", 443, "/evil-app-1"),
			held("evilside", "evil", 0, "/evil-db-1"),
			held("fogd1", "sly", 8080, "/sly-app-1"),
			held("good1", "good", 8443, "/good-app-1"),
		}
	}}
	e := swapEngine(t, api, "evil")
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e.Store, e.FogHostAddr = st, "host.docker.internal:8080"
	for _, n := range []string{"evil", "sly", "good"} {
		a, _ := st.CreateApp(n)
		st.SetAppDesiredState(a.ID, "running")
	}

	if !e.ReclaimFogPorts(context.Background()) {
		t.Fatal("nothing reclaimed")
	}
	b, _ := os.ReadFile(logPath)
	calls := string(b)
	for _, id := range []string{"evil1", "evilside", "fogd1"} {
		if !strings.Contains(calls, "/containers/"+id+"/stop") {
			t.Errorf("%s not stopped:\n%s", id, calls)
		}
	}
	for _, id := range []string{"caddy1", "good1"} {
		if strings.Contains(calls, id) {
			t.Errorf("%s touched:\n%s", id, calls)
		}
	}
	for n, want := range map[string]string{"evil": "stopped", "sly": "stopped", "good": "running"} {
		a, _ := st.AppByName(n)
		if a.DesiredState != want {
			t.Errorf("%s desired %s, want %s", n, a.DesiredState, want)
		}
	}
	notices, _ := st.ActiveNotices()
	found := false
	for _, n := range notices {
		if n.Kind == "fog-port-taken" && n.App == "evil" && strings.Contains(n.Body, "443/tcp") {
			found = true
		}
	}
	if !found {
		t.Errorf("no notice for evil: %+v", notices)
	}
}

// The attack: a provider is deleted, and an import takes its name. The
// apps that followed its folders, used or reached it, were wired to the
// newcomer at once — held for review, unread, its folder at /root. Now a
// deleted app is forgotten by every other app, and a held app is nobody's
// provider until it is confirmed.
func TestDeletedProviderIsForgottenAndHeldAppsWireNothing(t *testing.T) {
	t.Setenv("DOCKER_SOCK", filepath.Join(t.TempDir(), "none.sock"))
	e := reviewEngine(t)
	e.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	e.Docker = dockerx.New()
	e.Builder = &build.Builder{DataDir: t.TempDir()}
	ctx := context.Background()
	provider := &AppSpec{
		Source:     Source{Type: "image", Image: "sab:1"},
		Volumes:    []Volume{{Name: "downloads", Mount: "/downloads", Files: "Downloads"}},
		Connection: &Connection{Port: 8080, Shares: []string{"downloads"}},
	}
	if err := e.Create(ctx, "sab", provider, nil, "admin"); err != nil {
		t.Fatal(err)
	}
	consumer := &AppSpec{
		Source:  Source{Type: "image", Image: "hook:1"},
		Uses:    []string{"sab"},
		Reach:   []string{"sab"},
		Volumes: []Volume{{Name: "dl", Mount: "/dl", Files: "Downloads", Follows: "sab/downloads"}},
	}
	if err := e.Create(ctx, "hook", consumer, nil, "admin"); err != nil {
		t.Fatal(err)
	}

	if err := e.Delete(ctx, "sab", false, "admin"); err != nil {
		t.Fatal(err)
	}
	app, _ := e.Store.AppByName("hook")
	got, err := LatestSpec(e.Store, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(got.Uses, "sab") || slices.Contains(got.Reach, "sab") || got.Volumes[0].Follows != "" {
		t.Fatalf("hook still names the deleted sab: uses %v reach %v follows %q", got.Uses, got.Reach, got.Volumes[0].Follows)
	}
	if got.Volumes[0].Files != "Downloads" {
		t.Errorf("hook lost the folder it had: %+v", got.Volumes[0])
	}

	// The newcomer, held: no follower resolves to it, no wire reaches it,
	// and it inherits no approval of the old one's.
	imp := &AppSpec{
		Source:                Source{Type: "image", Image: "evil:1"},
		Volumes:               []Volume{{Name: "downloads", Mount: "/downloads", Host: "/root"}},
		AcknowledgeHostAccess: []string{"/root"},
		Connection:            &Connection{Port: 8080, Shares: []string{"downloads"}},
	}
	if err := e.CreateForReview(ctx, "sab", imp, nil, "import"); err != nil {
		t.Fatal(err)
	}
	sab, _ := e.Store.AppByName("sab")
	e.Store.SetAppCurrentRev(sab.ID, 1) // as if it ran: held is what keeps it out
	specs, _, err := deployedSpecsFrom(e.Store, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := specs["sab"]; ok {
		t.Error("the held sab is among the deployed apps wires are planned from")
	}
	follower := &AppSpec{Uses: []string{"sab"}, Volumes: []Volume{{Name: "dl", Mount: "/dl", Files: "Downloads", Follows: "sab/downloads"}}}
	if ResolveFollows(follower, func(string) *AppSpec { return nil }) || follower.Volumes[0].Host != "" {
		t.Error("a follower resolved with no provider")
	}
	if have, _ := e.Store.AppApprovals(sab.ID); len(have) != 0 {
		t.Errorf("the newcomer inherited approvals: %v", have)
	}
	if reviewed, _ := e.Store.AppReviewedRev(sab.ID); reviewed != 0 {
		t.Errorf("the newcomer inherited a reviewed revision: %d", reviewed)
	}
}
