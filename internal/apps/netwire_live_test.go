package apps

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/dockerx"
	"gitea.lan/matthew/fog/internal/livetest"
	"gitea.lan/matthew/fog/internal/randx"
	"gitea.lan/matthew/fog/internal/store/storetest"
)

// liveNetImage is any small image with a shell, wget and an HTTP server.
const liveNetImage = "caddy:2.10-alpine"

// The attack, against real Docker: an app on the old shared bridge reaches
// a gated app's container directly — around the proxy — and sends it
// "Remote-User: <anyone>". After ReconcileNetworks, no other app can open a
// connection to the gated app on any of its addresses, and the proxy
// reaches every app by its fog-only name.
//
// Needs docker; skipped without it, under -short, or when the docker host
// already runs a fog (its shared network exists) — the test would move that
// fog's containers.
func TestLiveNetworkIsolation(t *testing.T) {
	if testing.Short() {
		t.Skip("live docker test")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not installed")
	}
	livetest.Lock(t)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	docker := func(args ...string) (string, error) {
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	if _, err := docker("info"); err != nil {
		t.Skip("docker not reachable")
	}
	// fog talks to the daemon's socket itself (DOCKER_SOCK overrides where).
	if !dockerx.New().Available(ctx) {
		t.Skip("docker socket not reachable by fog's client; set DOCKER_SOCK")
	}
	if out, _ := docker("ps", "-aq", "--filter", "label=fog.managed=true"); out != "" {
		t.Skip("this docker host runs fog containers; not moving them")
	}
	if _, err := docker("network", "inspect", NetworkName); err == nil {
		t.Skip("this docker host has fog's shared network; not touching it")
	}
	if _, err := docker("image", "inspect", liveNetImage); err != nil {
		if out, err := docker("pull", liveNetImage); err != nil {
			t.Skipf("cannot pull %s: %s", liveNetImage, out)
		}
	}

	// Today's world: one shared bridge with everything on it.
	if out, err := docker("network", "create", NetworkName); err != nil {
		t.Fatalf("network create: %s", out)
	}
	t.Cleanup(func() { exec.Command("docker", "network", "rm", NetworkName).Run() })

	sfx := strings.ToLower(randx.Hex(3))
	gated, attacker, consumer := "gated"+sfx, "attacker"+sfx, "consumer"+sfx
	// Registered first, so it runs last, after every container is gone.
	t.Cleanup(func() {
		e := &Engine{Docker: dockerx.New(), Log: slog.New(slog.DiscardHandler)}
		for _, n := range []string{gated, attacker, consumer} {
			e.RemoveAppNetworks(context.Background(), n)
		}
	})
	st, err := storetest.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	deploy := func(name string, spec AppSpec) {
		a, err := st.CreateApp(name)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(spec)
		rev, err := st.CreateRevision(a.ID, string(b), "", "t")
		if err != nil {
			t.Fatal(err)
		}
		if err := st.SetAppCurrentRev(a.ID, rev.Rev); err != nil {
			t.Fatal(err)
		}
	}
	deploy(gated, AppSpec{Source: Source{Type: "image", Image: "x"}, Routes: []Route{{Port: 80, Auth: true}}})
	deploy(attacker, AppSpec{Source: Source{Type: "image", Image: "x"}})
	deploy(consumer, AppSpec{Source: Source{Type: "image", Image: "x"}, Env: map[string]string{"GATED_URL": "http://" + gated}})

	run := func(name string, labels map[string]string, aliases ...string) string {
		args := []string{"run", "-d", "--network", NetworkName}
		// fog knows its own containers by the names it gives them.
		if labels["fog.system"] == "caddy" {
			args = append(args, "--name", "caddy")
		}
		for _, a := range aliases {
			args = append(args, "--network-alias", a)
		}
		for k, v := range labels {
			args = append(args, "--label", k+"="+v)
		}
		args = append(args, liveNetImage, "caddy", "respond", "--listen", ":80", "Remote-User: {http.request.header.Remote-User}")
		id, err := docker(args...)
		if err != nil {
			t.Fatalf("docker run %s: %s", name, id)
		}
		t.Cleanup(func() { exec.Command("docker", "rm", "-f", id).Run() })
		return id
	}
	app := func(name string) map[string]string { return map[string]string{"fog.managed": "true", "fog.app": name} }
	gatedID := run(gated, app(gated), gated)
	attackerID := run(attacker, app(attacker), attacker)
	consumerID := run(consumer, app(consumer), consumer)
	proxyID := run("caddy", map[string]string{"fog.managed": "true", "fog.system": "caddy"})

	addrs := func(id string) []string {
		out, err := docker("inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}", id)
		if err != nil {
			t.Fatalf("inspect: %s", out)
		}
		return strings.Fields(out)
	}
	reach := func(from, target string) bool {
		out, err := docker("exec", from, "wget", "-q", "-O-", "-T", "2", "--header", "Remote-User: operator", "http://"+target+"/")
		return err == nil && strings.Contains(out, "operator")
	}

	// Before: the attacker impersonates the operator to the gated app.
	before := addrs(gatedID)
	if !reach(attackerID, before[0]) {
		t.Fatal("setup: the attacker cannot reach the gated app even on the shared bridge")
	}

	e := &Engine{Store: st, Docker: dockerx.New(), Log: slog.New(slog.DiscardHandler)}
	if err := e.ReconcileNetworks(ctx, nil); err != nil {
		t.Fatal(err)
	}

	after := addrs(gatedID)
	for _, ip := range append(before, after...) {
		if reach(attackerID, ip) {
			t.Errorf("the unwired attacker still reaches the gated app at %s", ip)
		}
	}
	if reach(attackerID, gated) {
		t.Error("the unwired attacker still reaches the gated app by name")
	}
	// The consumer too reaches it only through fog-wire, never directly
	// (internal/proxy's TestLiveWires runs the wire itself).
	for _, ip := range after {
		if reach(consumerID, ip) {
			t.Errorf("the consumer reaches the gated app directly at %s", ip)
		}
	}
	if reach(consumerID, attacker) {
		t.Error("the consumer reaches an app it isn't wired to")
	}
	for _, n := range []string{gated, attacker, consumer} {
		if !reach(proxyID, AppUpstream(n)) {
			t.Errorf("the proxy lost %s", n)
		}
	}
	// And nothing of an app's is left on the shared bridge.
	for _, id := range []string{gatedID, attackerID, consumerID} {
		out, _ := docker("inspect", "-f", "{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}", id)
		if strings.Contains(" "+out+" ", " "+NetworkName+" ") {
			t.Errorf("%.12s still on %s: %s", id, NetworkName, out)
		}
	}
	// A second reconcile has nothing left to do, and does it cleanly.
	if err := e.ReconcileNetworks(ctx, nil); err != nil {
		t.Fatal(err)
	}
}

// An app with a sidecar deployed before fog made sidecar networks: its
// database runs on the network compose made. The new replica lands on fog's
// network, so prepareAuxNetwork carries the running sidecar across under
// its name, or the release and the swap would lose their database.
func TestLiveAuxNetworkCarriesRunningSidecars(t *testing.T) {
	if testing.Short() {
		t.Skip("live docker test")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not installed")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	docker := func(args ...string) (string, error) {
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	if _, err := docker("info"); err != nil || !dockerx.New().Available(ctx) {
		t.Skip("docker not reachable by fog's client; set DOCKER_SOCK")
	}
	if _, err := docker("image", "inspect", liveNetImage); err != nil {
		if out, err := docker("pull", liveNetImage); err != nil {
			t.Skipf("cannot pull %s: %s", liveNetImage, out)
		}
	}
	name := "auxapp" + strings.ToLower(randx.Hex(3))
	e := &Engine{Docker: dockerx.New(), Log: slog.New(slog.DiscardHandler)}
	t.Cleanup(func() {
		e.RemoveAppNetworks(context.Background(), name)
		exec.Command("docker", "network", "rm", name+"_private").Run()
	})
	if out, err := docker("network", "create", name+"_private"); err != nil {
		t.Fatalf("network create: %s", out)
	}
	id, err := docker("run", "-d", "--network", name+"_private", "--network-alias", "db",
		"--label", "fog.managed=true", "--label", "fog.app="+name, "--label", "fog.aux=db",
		liveNetImage, "caddy", "respond", "--listen", ":80", "db")
	if err != nil {
		t.Fatalf("run sidecar: %s", id)
	}
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", id).Run() })

	spec := &AppSpec{Aux: []AuxService{{Name: "db", Image: "x"}}}
	network := AuxNetwork(name, spec)
	if err := e.prepareAuxNetwork(ctx, name, spec, network); err != nil {
		t.Fatal(err)
	}
	// A new replica on fog's sidecar network finds it by name.
	var out string
	for range 10 { // the sidecar may still be starting
		if out, err = docker("run", "--rm", "--network", network, liveNetImage, "wget", "-q", "-O-", "-T", "3", "http://db/"); err == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil || out != "db" {
		t.Fatalf("new replica can't reach its sidecar: %v %s", err, out)
	}
	sub, _ := docker("network", "inspect", "-f", "{{range .IPAM.Config}}{{.Subnet}}{{end}}", network)
	if !strings.HasSuffix(sub, "/27") {
		t.Errorf("sidecar network subnet %q isn't one fog chose", sub)
	}
}

// The attack: an app's image says LABEL fog.system=caddy (and fog.aux, and
// another app's fog.app). Docker gives the container its image's labels,
// and fog used to take the label's word: the app was put on every app's
// network as the proxy, and took over gated apps' names. Rendered and
// started by fog's own compose, the container must carry fog's labels, not
// the image's, and the reconcile must treat it as the app it is.
//
// Needs docker and the compose CLI (in the Colima VM on a Mac).
func TestLiveImageLabelsCannotMakeAFogContainer(t *testing.T) {
	if testing.Short() || exec.Command("docker", "compose", "version").Run() != nil {
		t.Skip("needs docker compose")
	}
	livetest.Lock(t)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	docker := func(args ...string) (string, error) {
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	if !dockerx.New().Available(ctx) {
		t.Skip("docker not reachable by fog's client")
	}
	if out, _ := docker("ps", "-aq", "--filter", "label=fog.managed=true"); out != "" {
		t.Skip("this docker host runs fog containers")
	}
	sfx := strings.ToLower(randx.Hex(3))
	img := "fog-evil-label:" + sfx
	bdir := t.TempDir()
	os.WriteFile(filepath.Join(bdir, "Dockerfile"), []byte("FROM "+liveNetImage+"\nLABEL fog.system=caddy fog.aux=x fog.process=y fog.app=victim"+sfx+"\nCMD [\"sleep\",\"3600\"]\n"), 0o644)
	if out, err := docker("build", "-q", "-t", img, bdir); err != nil {
		t.Fatalf("build: %s", out)
	}
	t.Cleanup(func() { exec.Command("docker", "rmi", "-f", img).Run() })

	evil, victim := "evil"+sfx, "victim"+sfx
	st, err := storetest.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	e := &Engine{Store: st, Docker: dockerx.New(), Log: slog.New(slog.DiscardHandler), Mat: &Materializer{DataDir: t.TempDir()}}
	for _, n := range []string{evil, victim} {
		a, _ := st.CreateApp(n)
		b, _ := json.Marshal(AppSpec{Source: Source{Type: "image", Image: img}})
		rev, _ := st.CreateRevision(a.ID, string(b), "", "t")
		st.SetAppCurrentRev(a.ID, rev.Rev)
		spec := &AppSpec{Source: Source{Type: "image", Image: img}}
		nets, err := e.prepareNetworks(ctx, n, spec)
		if err != nil {
			t.Fatal(err)
		}
		if err := e.Mat.RenderWith(n, spec, nil, img, "", nets, io.Discard); err != nil {
			t.Fatal(err)
		}
		name := n
		t.Cleanup(func() {
			e.Mat.ComposeCmd(context.Background(), name, "down").Run()
			e.RemoveAppNetworks(context.Background(), name)
		})
		if out, err := e.Mat.ComposeCmd(ctx, n, "up", "-d").CombinedOutput(); err != nil {
			t.Fatalf("compose up %s: %s", n, out)
		}
	}
	out, _ := docker("ps", "-q", "--filter", "label=com.docker.compose.project="+evil)
	labels, _ := docker("inspect", "-f", `{{index .Config.Labels "fog.system"}}|{{index .Config.Labels "fog.aux"}}|{{index .Config.Labels "fog.process"}}|{{index .Config.Labels "fog.app"}}`, out)
	if labels != "|||"+evil {
		t.Errorf("the image's labels survived: %q", labels)
	}
	if err := e.ReconcileNetworks(ctx, nil); err != nil {
		t.Fatal(err)
	}
	nets, _ := docker("inspect", "-f", "{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}", out)
	if strings.Contains(nets, OwnNetwork(victim, nil)) {
		t.Fatalf("the app joined another app's network as fog's own: %s", nets)
	}
}
