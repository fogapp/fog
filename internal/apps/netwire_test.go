package apps

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"gopkg.in/yaml.v3"

	"gitea.lan/matthew/fog/internal/dockerx"
)

func TestReachesFromUsesAndEnvURLs(t *testing.T) {
	installed := func(n string) bool {
		return slices.Contains([]string{"glass", "ntfy", "gotenberg", "immich", "victim", "chat"}, n)
	}
	s := &AppSpec{
		Uses:  []string{"immich", "events", "chat", "notinstalled"},
		Reach: []string{"glass", "ntfy", "gotenberg"},
		Env: map[string]string{
			"GLASS_URL":     "http://glass",
			"NTFY_URL":      "http://ntfy/topic",
			"GOTENBERG_URL": "http://gotenberg:3000",
			"DB":            "postgres://u:p@fog-postgres:5432/db",
			"PUBLIC":        "https://victim.example.com", // a dotted host is not an app
			"SELF":          "http://me:80",
			"PLAIN":         "victim", // not a URL
		},
	}
	got := s.Reaches("me", installed)
	want := []string{"glass", "gotenberg", "immich", "ntfy"}
	if !slices.Equal(got, want) {
		t.Fatalf("Reaches = %v, want %v", got, want)
	}
}

func TestPlanNetworksWiresOnlyWhatSpecsSay(t *testing.T) {
	specs := map[string]*AppSpec{
		"gated":    {Routes: []Route{{Port: 8080, Auth: true}}},
		"attacker": {},
		"consumer": {Reach: []string{"gated"}, Env: map[string]string{"U": "http://gated:8080", "DB": "postgres://u@glass:5432/x", "S": "https://glass"}},
		"glass":    {Connection: &Connection{Port: 3000}},
		"hook":     {Uses: []string{"glass"}},
		"sealed":   {Network: NetworkSealed, Reach: []string{"glass", "peer"}, Env: map[string]string{"A": "http://glass", "B": "http://peer"}},
		"peer":     {SealedPeer: true},
		"db":       {Needs: &Needs{Postgres: &PostgresNeed{Databases: []string{"d"}}}},
	}
	p := planNetworks(specs, nil)
	if p.own["sealed"] != "fog-sealed-sealed" || p.own["gated"] != "fog-app-gated" {
		t.Fatalf("own = %v", p.own)
	}
	// Only http URLs are wires fog can carry: postgres:// and https:// to an
	// app aren't. uses reaches the connection port.
	want := []Wire{
		{Provider: "gated", Consumer: "consumer", Ports: []int{8080}},
		{Provider: "glass", Consumer: "hook", Ports: []int{3000}},
		{Provider: "peer", Consumer: "sealed", Ports: []int{80}},
	}
	if fmt.Sprint(p.wires) != fmt.Sprint(want) {
		t.Fatalf("wires = %v\nwant %v", p.wires, want)
	}
	// A sealed app asking for an ordinary app is refused, not wired: the
	// wire would be its way to something with a route out.
	if !slices.Equal(p.refused, []string{"sealed → glass"}) {
		t.Fatalf("refused = %v", p.refused)
	}
	if !p.pg["db"] || p.pg["gated"] {
		t.Fatalf("pg = %v", p.pg)
	}
}

func container(id, app string, labels map[string]string, nets ...string) dockerx.ContainerSummary {
	l := map[string]string{"fog.managed": "true"}
	if app != "" {
		l["fog.app"] = app
	}
	for k, v := range labels {
		l[k] = v
	}
	n := map[string]dockerx.NetworkEndpoint{}
	for _, x := range nets {
		n[x] = dockerx.NetworkEndpoint{IPAddress: "10.0.0.1"}
	}
	c := dockerx.ContainerSummary{ID: id, State: "running", Labels: l, NetworkSettings: dockerx.SummaryNetworks{Networks: n}}
	// fog's own containers are known by the names fog gives them.
	if name, ok := map[string]string{"caddy": "/caddy", "wire": "/fog-wire", "pg": "/fog-postgres"}[id]; ok {
		c.Names = []string{name}
	} else {
		c.Names = []string{"/" + id}
	}
	return c
}

// Every container of every app starts on the old shared bridge (and an old
// per-wire network), where the attacker could reach the gated app directly.
// After the diff: each app on its own network alone, answering to its name
// and its fog-only name; the proxy on every app's network; the wire proxy on
// the consumer's network under the provider's name, and on the provider's;
// Postgres only where needed; and nothing of an app's on a shared network.
func TestDiffNetworksMovesAppsOffTheSharedBridge(t *testing.T) {
	specs := map[string]*AppSpec{
		"gated":    {},
		"attacker": {},
		"consumer": {Reach: []string{"gated"}, Env: map[string]string{"U": "http://gated"}, Needs: &Needs{Postgres: &PostgresNeed{Databases: []string{"d"}}}},
	}
	p := planNetworks(specs, nil)
	cs := []dockerx.ContainerSummary{
		container("g1", "gated", nil, NetworkName),
		container("a1", "attacker", nil, NetworkName),
		container("c1", "consumer", nil, NetworkName, "fog-link-gated.consumer"),
		container("c2", "consumer", map[string]string{"fog.process": "worker"}, NetworkName),
		container("x1", "consumer", map[string]string{"fog.aux": "redis"}, "private"),
		container("r1", "gated", map[string]string{"com.docker.compose.oneoff": "True"}, "fog-app-gated"),
		container("caddy", "", map[string]string{"fog.system": "caddy"}, NetworkName, SealedNetworkName),
		container("pg", "", map[string]string{"fog.system": "postgres"}, NetworkName, SealedNetworkName),
		container("wire", "", map[string]string{"fog.system": "wire"}, NetworkName),
		// The attack: an app whose image says LABEL fog.system=caddy.
		container("evil1", "attacker", map[string]string{"fog.system": "caddy"}, "fog-app-attacker"),
	}
	ops := diffNetworks(p, cs)
	type key struct {
		connect            bool
		container, network string
	}
	got := map[key][]string{}
	firstDisconnect := -1
	for i, op := range ops {
		got[key{op.connect, op.container, op.network}] = op.aliases
		if !op.connect && firstDisconnect < 0 {
			firstDisconnect = i
		}
		if op.connect && firstDisconnect >= 0 {
			t.Fatal("a connect came after a disconnect: a container could be left with nowhere to answer")
		}
	}
	wantOps := map[key][]string{
		{true, "g1", "fog-app-gated"}:            {"gated", "gated.fog-app"},
		{true, "a1", "fog-app-attacker"}:         {"attacker", "attacker.fog-app"},
		{true, "c1", "fog-app-consumer"}:         {"consumer", "consumer.fog-app"},
		{true, "c2", "fog-app-consumer"}:         nil,
		{true, "caddy", "fog-app-attacker"}:      nil,
		{true, "caddy", "fog-app-consumer"}:      nil,
		{true, "caddy", "fog-app-gated"}:         nil,
		{true, "pg", "fog-app-consumer"}:         {"fog-postgres"},
		{true, "wire", "fog-app-consumer"}:       {"gated"},
		{true, "wire", "fog-app-gated"}:          nil,
		{false, "g1", NetworkName}:               nil,
		{false, "a1", NetworkName}:               nil,
		{false, "c1", NetworkName}:               nil,
		{false, "c1", "fog-link-gated.consumer"}: nil,
		{false, "c2", NetworkName}:               nil,
	}
	for k, aliases := range wantOps {
		a, ok := got[k]
		if !ok {
			t.Errorf("missing %+v", k)
			continue
		}
		if !slices.Equal(a, aliases) {
			t.Errorf("%+v aliases = %v, want %v", k, a, aliases)
		}
	}
	for k := range got {
		if _, ok := wantOps[k]; !ok {
			t.Errorf("unexpected %+v", k)
		}
	}
}

// A container on the right network without the right names (an app from
// before fog-only names; the wire proxy before a new consumer) is taken off
// and put back with them, and one whose names fog didn't read is left be.
func TestDiffNetworksRepairsNames(t *testing.T) {
	p := planNetworks(map[string]*AppSpec{"a": {}, "b": {Reach: []string{"a"}, Env: map[string]string{"U": "http://a"}}}, nil)
	app := container("a1", "a", nil, "fog-app-a")
	app.NetworkSettings.Networks["fog-app-a"] = dockerx.NetworkEndpoint{DNSNames: []string{"a"}}
	wire := container("wire", "", map[string]string{"fog.system": "wire"}, "fog-app-a", "fog-app-b")
	wire.NetworkSettings.Networks["fog-app-b"] = dockerx.NetworkEndpoint{DNSNames: []string{"w"}}
	unread := container("b1", "b", nil, "fog-app-b")
	ops := diffNetworks(p, []dockerx.ContainerSummary{app, wire, unread})
	var got []string
	for _, op := range ops {
		got = append(got, fmt.Sprintf("%v %s %s %v", op.connect, op.container, op.network, op.aliases))
	}
	want := []string{
		"false a1 fog-app-a []", "true a1 fog-app-a [a a.fog-app]",
		"false wire fog-app-b []", "true wire fog-app-b [a]",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("ops = %q\nwant %q", got, want)
	}
}

// A container whose app's deploy is in flight is not the background
// reconcile's to move.
func TestNetOpTouches(t *testing.T) {
	for _, c := range []struct {
		op   netOp
		want bool
	}{
		{netOp{app: "a", network: "fog-app-b"}, true},
		{netOp{network: "fog-app-a"}, true},
		{netOp{network: "fog-sealed-a"}, true},
		{netOp{network: "fog-link-a.b"}, true},
		{netOp{network: "fog-link-b.a"}, true},
		{netOp{network: "fog-link-ab.c"}, false},
		{netOp{network: "fog-app-ab"}, false},
	} {
		if got := c.op.touches("a"); got != c.want {
			t.Errorf("%+v touches a = %v", c.op, got)
		}
	}
}

// Compose puts the main service on its own network under its name and the
// fog-only name the proxies use; a worker gets neither.
func TestRenderMainAnswersToItsFogOnlyName(t *testing.T) {
	m := &Materializer{DataDir: t.TempDir()}
	spec := &AppSpec{Processes: []Process{{Name: "worker", Command: []string{"w"}}}}
	if err := m.Render("app", spec, nil, "x:1", "", io.Discard); err != nil {
		t.Fatal(err)
	}
	services, networks := readCompose(t, m, "app")
	if len(networks) != 1 {
		t.Errorf("networks = %v", networks)
	}
	main := services["app"]["networks"].(map[string]any)["fog-app-app"].(map[string]any)
	if fmt.Sprint(main["aliases"]) != "[app app.fog-app]" {
		t.Errorf("main aliases = %v", main["aliases"])
	}
	if w := services["app-worker"]["networks"].(map[string]any)["fog-app-app"].(map[string]any); w["aliases"] != nil {
		t.Errorf("worker aliases = %v", w["aliases"])
	}
}

// A start page granted "home" fetches widget pictures from each app that
// serves widgets, on port 80, so it reaches those — and only those, and a
// sealed one only when it may be.
func TestPlanNetworksHomeGrantWiresWidgetApps(t *testing.T) {
	specs := map[string]*AppSpec{
		"newtab": {Uses: []string{UsesHome}},
		"stream": {},
		"plain":  {},
		"fit":    {Network: NetworkSealed},
	}
	p := planNetworks(specs, map[string]bool{"stream": true, "fit": true})
	want := []Wire{{Provider: "stream", Consumer: "newtab", Ports: []int{80}}}
	if fmt.Sprint(p.wires) != fmt.Sprint(want) {
		t.Fatalf("wires = %v, want %v", p.wires, want)
	}
	if len(p.refused) != 0 {
		t.Fatalf("a home grant's implicit wire filed a refusal: %v", p.refused)
	}
}

func readCompose(t *testing.T, m *Materializer, app string) (map[string]map[string]any, map[string]any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(m.AppDir(app), "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Services map[string]map[string]any `yaml:"services"`
		Networks map[string]any            `yaml:"networks"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Services, doc.Networks
}
