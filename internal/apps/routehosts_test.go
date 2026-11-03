package apps

import (
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"gitea.lan/matthew/fog/internal/store"
)

func routeEngine(t *testing.T, domain string) (*Engine, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if domain != "" {
		if err := st.SetSetting(store.SettingBaseDomain, domain); err != nil {
			t.Fatal(err)
		}
	}
	return &Engine{Store: st, Mat: &Materializer{DataDir: t.TempDir()}, Log: slog.New(slog.DiscardHandler)}, st
}

func routed(hosts ...string) *AppSpec {
	s := &AppSpec{Source: Source{Type: "image", Image: "x:1"}}
	for _, h := range hosts {
		s.Routes = append(s.Routes, Route{Host: h, Port: 80})
	}
	return s
}

// The attack: an app claims a hostname another app already answers on, or
// fog's own — standing in front of the console, or (as two blocks for one
// name) making Caddy refuse the whole config, every app's routes with it.
// Each is refused at save, with the owner named.
func TestSaveRefusesTakenHostnames(t *testing.T) {
	ctx := context.Background()
	e, _ := routeEngine(t, "home.example.com")
	if err := e.Create(ctx, "photos", routed(""), nil, "t"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		spec *AppSpec
		want string
	}{
		{"thief", routed("photos"), "photos's"},
		{"thief2", routed("photos.home.example.com"), "photos's"},
		{"console", routed("fog"), "fog's own"},
		{"console2", routed("fog.home.example.com"), "fog's own"},
		{"fog-twin", routed("x", "FOG.home.example.com."), ""}, // not a lowercase hostname: Validate's
	} {
		err := e.Create(ctx, c.name, c.spec, nil, "t")
		if err == nil {
			t.Errorf("%s: created, claiming %v", c.name, c.spec.Routes)
			continue
		}
		if c.want != "" && !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want it to name %q", c.name, err, c.want)
		}
	}
	// And an existing app can't take it by update either.
	if err := e.Create(ctx, "other", routed("other"), nil, "t"); err != nil {
		t.Fatal(err)
	}
	if err := e.UpdateSpec(ctx, "other", routed("photos"), nil, "", "t"); err == nil {
		t.Error("an update took another app's hostname")
	}
	if err := e.UpdateSpec(ctx, "other", routed("other", "elsewhere"), nil, "", "t"); err != nil {
		t.Errorf("an unrelated update was refused: %v", err)
	}
}

// Without a base domain yet, labels still clash with labels: the day a
// domain is set they'd be the same name.
func TestSaveRefusesTakenLabelsBeforeADomain(t *testing.T) {
	ctx := context.Background()
	e, _ := routeEngine(t, "")
	if err := e.Create(ctx, "photos", routed(""), nil, "t"); err != nil {
		t.Fatal(err)
	}
	if err := e.Create(ctx, "thief", routed("photos"), nil, "t"); err == nil {
		t.Error("a label clashing with another app's default was accepted")
	}
	if err := e.Create(ctx, "console", routed("fog"), nil, "t"); err == nil {
		t.Error("fog's own label was accepted")
	}
}

// A clash older state left behind (stored before the check, or made when
// a domain change turned a dotted host into another app's label) doesn't
// block the owner's unrelated edits; the app that loses it can't deploy
// claiming it.
func TestLegacyClashOwnerKeepsSavingLoserCannotDeploy(t *testing.T) {
	ctx := context.Background()
	e, st := routeEngine(t, "home.example.com")
	put := func(name string, spec *AppSpec) {
		a, err := st.CreateApp(name)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(spec)
		if _, err := st.CreateRevision(a.ID, string(b), "", "t"); err != nil {
			t.Fatal(err)
		}
	}
	put("owner", routed("shared"))
	put("loser", routed("shared"))
	if err := e.UpdateSpec(ctx, "owner", routed("shared", "more"), nil, "", "t"); err != nil {
		t.Errorf("the owner's edit was refused: %v", err)
	}
	if err := e.checkRouteClaims("loser", routed("shared"), false); err == nil || !strings.Contains(err.Error(), "owner's") {
		t.Errorf("the loser may deploy claiming the name: %v", err)
	}
	if err := e.checkRouteClaims("owner", routed("shared"), false); err != nil {
		t.Errorf("the owner may not deploy: %v", err)
	}
}

func TestRouteConflictsFirstClaimantWins(t *testing.T) {
	got := RouteConflicts([]RouteClaimant{
		{ID: 3, Name: "young", Spec: routed("shared")},
		{ID: 1, Name: "old", Spec: routed("shared")},
		{ID: 2, Name: "console", Spec: routed("fog")},
		{ID: 4, Name: "apex", Spec: routed("shared.home.example.com")}, // old's label, spelled out
		{ID: 5, Name: "nospec"},
	}, "home.example.com")
	want := map[string]string{"young": "old", "console": "", "apex": "old"}
	if len(got) != len(want) {
		t.Fatalf("conflicts = %+v", got)
	}
	for _, c := range got {
		owner, ok := want[c.App]
		if !ok || c.Owner != owner || c.Fog != (owner == "") {
			t.Errorf("unexpected %+v", c)
		}
	}
}

// An app named like a surface grant would make "uses: [chat]" mean two
// things: posting into chat, and a connection to that app.
func TestCreateRefusesSurfaceGrantNames(t *testing.T) {
	e, _ := routeEngine(t, "")
	for _, n := range []string{UsesChat, UsesEvents, UsesHome} {
		if err := e.Create(context.Background(), n, routed(), nil, "t"); err == nil {
			t.Errorf("created an app named %q", n)
		}
	}
}

// The attack: an open first route and a gated second route on the same
// container port. The direct port forwards to that port, around Caddy, so
// it would serve the gated service ungated.
func TestDirectPortRefusedWhenAnyGuardedRouteSharesItsPort(t *testing.T) {
	open := Route{Port: 8080}
	for _, c := range []struct {
		routes []Route
		want   bool
	}{
		{[]Route{open}, true},
		{[]Route{open, {Host: "admin", Port: 8080, Auth: true}}, false},
		{[]Route{open, {Host: "lan", Port: 8080, LanOnly: true}}, false},
		{[]Route{open, {Host: "admin", Port: 9090, Auth: true}}, true}, // another service
		{[]Route{{Port: 8080, Auth: true}}, false},
		{nil, false},
	} {
		if got := (&AppSpec{Routes: c.routes}).DirectPort(); got != c.want {
			t.Errorf("%+v: DirectPort = %v, want %v", c.routes, got, c.want)
		}
	}
}

// The attack: app foo with a process bar is "foo-bar" on the network,
// shadowing app foo-bar. Refused either way round.
func TestServiceNamesCannotShadowApps(t *testing.T) {
	ctx := context.Background()
	e, _ := routeEngine(t, "")
	if err := e.Create(ctx, "foo-bar", routed(), nil, "t"); err != nil {
		t.Fatal(err)
	}
	shadow := routed()
	shadow.Processes = []Process{{Name: "bar", Command: []string{"x"}}}
	if err := e.Create(ctx, "foo", shadow, nil, "t"); err == nil {
		t.Error("foo's process bar may shadow app foo-bar")
	}
	if err := e.Create(ctx, "baz", routed(), nil, "t"); err != nil {
		t.Fatal(err)
	}
	withQux := routed()
	withQux.Processes = []Process{{Name: "qux", Command: []string{"x"}}}
	if err := e.Create(ctx, "baz2", withQux, nil, "t"); err != nil {
		t.Fatal(err)
	}
	if err := e.Create(ctx, "baz2-qux", routed(), nil, "t"); err == nil {
		t.Error("an app may be named like another's process")
	}
	fogs := routed()
	fogs.Processes = []Process{{Name: "postgres", Command: []string{"x"}}}
	if err := e.Create(ctx, "fog", fogs, nil, "t"); err == nil {
		t.Error("reserved name")
	}
}
