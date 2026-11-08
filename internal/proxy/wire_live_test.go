package proxy

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/dockerx"
	"gitea.lan/matthew/fog/internal/livetest"
	"gitea.lan/matthew/fog/internal/randx"
	"gitea.lan/matthew/fog/internal/store/storetest"
)

// The attacks, against real Docker and a real fog-wire:
//
//   - A provider reaching back into its consumer: a wire must run one way,
//     so a gated consumer can't be sent forged identity headers by an app it
//     merely uses.
//   - A consumer sending its provider "Remote-User: operator" through the
//     wire: fog-wire deletes it.
//   - An unwired app reaching a provider at all.
//   - DNS shadowing: app foo's worker "bar" is "foo-bar" on the network;
//     the proxy must reach app foo-bar by a name the worker can't take.
//
// Needs docker; skipped without it, under -short, or on a docker host that
// already runs fog.
func TestLiveWires(t *testing.T) {
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
	if _, err := docker("info"); err != nil || !dockerx.New().Available(ctx) {
		t.Skip("docker not reachable by fog's client; set DOCKER_SOCK")
	}
	if out, _ := docker("ps", "-aq", "--filter", "label=fog.managed=true"); out != "" {
		t.Skip("this docker host runs fog containers; not moving them")
	}
	if _, err := docker("network", "inspect", apps.NetworkName); err == nil {
		t.Skip("this docker host has fog's shared network; not touching it")
	}
	if _, err := docker("image", "inspect", liveCaddyImage); err != nil {
		if out, err := docker("pull", liveCaddyImage); err != nil {
			t.Skipf("cannot pull %s: %s", liveCaddyImage, out)
		}
	}
	if out, err := docker("network", "create", apps.NetworkName); err != nil {
		t.Fatalf("network create: %s", out)
	}
	t.Cleanup(func() { exec.Command("docker", "network", "rm", apps.NetworkName).Run() })

	sfx := strings.ToLower(randx.Hex(3))
	gated, consumer, attacker, foo := "gated"+sfx, "consumer"+sfx, "attacker"+sfx, "foo"+sfx
	fooBar := foo + "-bar"
	st, err := storetest.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	deploy := func(name string, spec apps.AppSpec) {
		a, err := st.CreateApp(name)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(spec)
		rev, err := st.CreateRevision(a.ID, string(b), "", "t")
		if err != nil {
			t.Fatal(err)
		}
		st.SetAppCurrentRev(a.ID, rev.Rev)
	}
	img := apps.Source{Type: "image", Image: "x"}
	deploy(gated, apps.AppSpec{Source: img, Routes: []apps.Route{{Port: 80, Auth: true}}})
	deploy(consumer, apps.AppSpec{Source: img, Routes: []apps.Route{{Port: 80, Auth: true}}, Reach: []string{gated}, Env: map[string]string{"GATED_URL": "http://" + gated}})
	deploy(attacker, apps.AppSpec{Source: img})
	deploy(foo, apps.AppSpec{Source: img, Processes: []apps.Process{{Name: "bar", Command: []string{"x"}}}})
	deploy(fooBar, apps.AppSpec{Source: img, Routes: []apps.Route{{Port: 80}}})

	e := &apps.Engine{Store: st, Docker: dockerx.New(), Log: slog.New(slog.DiscardHandler)}
	t.Cleanup(func() {
		for _, n := range []string{gated, consumer, attacker, foo, fooBar} {
			e.RemoveAppNetworks(context.Background(), n)
		}
	})
	echo := `<% range $k, $v := .Req.Header %><% $k %>=<% index $v 0 %>
<% end %>`
	run := func(name string, labels map[string]string, body string) string {
		t.Helper()
		args := []string{"create", "--network", apps.NetworkName}
		// fog knows its own containers by the names it gives them.
		switch labels["fog.system"] {
		case "caddy":
			args = append(args, "--name", "caddy")
		case "wire":
			args = append(args, "--name", WireContainer)
		}
		for k, v := range labels {
			args = append(args, "--label", k+"="+v)
		}
		args = append(args, liveCaddyImage)
		id, err := docker(args...)
		if err != nil {
			t.Fatalf("create %s: %s", name, id)
		}
		t.Cleanup(func() { exec.Command("docker", "rm", "-f", id).Run() })
		cfg := "{\n\tadmin unix//config/admin.sock\n}\n:80 {\n\ttemplates {\n\t\tbetween <% %>\n\t}\n\trespond \"" + name + "\n" + body + "\"\n}\n"
		f := filepath.Join(t.TempDir(), "Caddyfile")
		os.WriteFile(f, []byte(cfg), 0o644)
		if out, err := docker("cp", f, id+":/etc/caddy/Caddyfile"); err != nil {
			t.Fatalf("cp: %s", out)
		}
		if out, err := docker("start", id); err != nil {
			t.Fatalf("start %s: %s", name, out)
		}
		return id
	}
	app := func(n string) map[string]string { return map[string]string{"fog.managed": "true", "fog.app": n} }
	gatedID := run(gated, app(gated), echo)
	consumerID := run(consumer, app(consumer), echo)
	attackerID := run(attacker, app(attacker), "")
	workerLabels := app(foo)
	workerLabels["fog.process"] = "bar"
	workerID := run("worker", workerLabels, "")
	fooBarID := run(fooBar, app(fooBar), "")
	proxyID := run("proxy", map[string]string{"fog.managed": "true", "fog.system": "caddy"}, "")
	wireID := run("wire", map[string]string{"fog.managed": "true", "fog.system": "wire"}, "")

	if err := e.ReconcileNetworks(ctx, nil); err != nil {
		t.Fatal(err)
	}
	// The worker answers to its compose service name on foo's network, as
	// compose makes it — the shadow of app foo-bar.
	docker("network", "disconnect", apps.OwnNetwork(foo, nil), workerID)
	if out, err := docker("network", "connect", "--alias", fooBar, apps.OwnNetwork(foo, nil), workerID); err != nil {
		t.Fatalf("worker alias: %s", out)
	}

	// fog-wire's config, from the plan and the subnets fog made.
	p := &Proxy{Docker: dockerx.New(), Store: st}
	wires, err := apps.PlanWires(st)
	if err != nil {
		t.Fatal(err)
	}
	subnets, err := p.appSubnets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(t.TempDir(), "Wirefile")
	os.WriteFile(f, []byte(wirefile(wires, subnets, nil)), 0o644)
	if out, err := docker("cp", f, wireID+":/etc/caddy/Caddyfile"); err != nil {
		t.Fatalf("cp: %s", out)
	}
	if out, err := docker("exec", wireID, "caddy", "reload", "--config", "/etc/caddy/Caddyfile"); err != nil {
		t.Fatalf("wire reload: %s", out)
	}

	get := func(from, url string) (string, bool) {
		out, err := docker("exec", from, "wget", "-q", "-O-", "-T", "3", "--header", "Remote-User: operator", "--header", "Remote_Email: op@x", url)
		return out, err == nil
	}
	addrs := func(id string) []string {
		out, _ := docker("inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}", id)
		return strings.Fields(out)
	}

	// The consumer reaches its provider through the wire, with no identity
	// it could forge.
	var body string
	var ok bool
	for range 20 {
		if body, ok = get(consumerID, "http://"+gated+"/"); ok {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if !ok || !strings.HasPrefix(body, gated) {
		logs, _ := docker("logs", wireID)
		t.Fatalf("consumer → provider over the wire: %q\n%s", body, logs)
	}
	low := strings.ToLower(body)
	if strings.Contains(low, "remote-user=") || strings.Contains(low, "remote_email=") {
		t.Errorf("the wire carried forged identity headers:\n%s", body)
	}
	// The provider can't reach back: not by name, not at any address.
	if out, ok := get(gatedID, "http://"+consumer+"/"); ok {
		t.Errorf("the provider reached its consumer by name: %q", out)
	}
	for _, ip := range addrs(consumerID) {
		if out, ok := get(gatedID, "http://"+ip+"/"); ok {
			t.Errorf("the provider reached its consumer at %s: %q", ip, out)
		}
	}
	// An unwired app reaches neither, nor the wire.
	if _, ok := get(attackerID, "http://"+gated+"/"); ok {
		t.Error("an unwired app reached the provider")
	}
	for _, ip := range append(addrs(gatedID), addrs(wireID)...) {
		if _, ok := get(attackerID, "http://"+ip+"/"); ok {
			t.Errorf("an unwired app reached %s", ip)
		}
	}
	// The proxy reaches app foo-bar by its fog-only name, never foo's worker.
	want := addrs(fooBarID)
	for range 10 {
		out, _ := docker("exec", proxyID, "getent", "hosts", apps.AppUpstream(fooBar))
		f := strings.Fields(out)
		if len(f) == 0 {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		if !containsStr(want, f[0]) {
			t.Fatalf("%s resolved to %s, not app %s (%v)", apps.AppUpstream(fooBar), f[0], fooBar, want)
		}
		break
	}
	if body, ok := get(proxyID, "http://"+apps.AppUpstream(fooBar)+"/"); !ok || !strings.HasPrefix(body, fooBar) {
		t.Errorf("the proxy reached %q for app %s", body, fooBar)
	}
}
