package apps

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gitea.lan/matthew/fog/internal/store/storetest"
)

// The attack: systemd's private socket (root on the host through D-Bus) was
// on no denylist. It renders only acknowledged — and the runtime's socket
// never, acknowledged or not.
func TestRenderRefusesSystemdSocketUnacknowledged(t *testing.T) {
	m := &Materializer{DataDir: t.TempDir()}
	spec := dangerousSpec("/run/systemd/private")
	if err := m.Render("evil", spec, nil, "x:1", "", io.Discard); err == nil || !strings.Contains(err.Error(), "acknowledgeHostAccess") {
		t.Fatalf("Render = %v, want an acknowledgement asked for", err)
	}
	spec = dangerousSpec("/run/containerd/containerd.sock")
	spec.AcknowledgeHostAccess = []string{"/run/containerd/containerd.sock"}
	if err := m.Render("evil", spec, nil, "x:1", "", io.Discard); err == nil {
		t.Fatal("an acknowledged runtime socket rendered")
	}
}

// The attack: an app named fog-caddy is fog's proxy's compose project, and
// its `up --remove-orphans` removed the proxy and took its ports. Refused
// at create (the whole fog- prefix), and at deploy for one stored before.
func TestFogsOwnProjectNamesAreRefused(t *testing.T) {
	e, st := routeEngine(t, "")
	for _, n := range []string{"fog-caddy", "fog-wire", "fog-postgres", "fog-anything"} {
		if err := e.Create(context.Background(), n, routed(), nil, "t"); err == nil {
			t.Errorf("created %s", n)
		}
	}
	a, _ := st.CreateApp("fog-caddy")
	b, _ := json.Marshal(routed())
	st.CreateRevision(a.ID, string(b), "", "t")
	if err := e.Deploy(context.Background(), "fog-caddy", 0, "t"); err == nil || !strings.Contains(err.Error(), "fog's own") {
		t.Fatalf("deploy of a stored fog-caddy = %v", err)
	}
}

// The attack: an imported app's env names "http://vault" before any vault
// is installed. Installing one later used to wire the two silently. Now
// only an approved provider (reach, uses) is wired; the mention is an
// issue asking the operator, and approving it is what wires.
func TestEnvURLsWireOnlyApprovedApps(t *testing.T) {
	importer := &AppSpec{Env: map[string]string{"U": "http://vault:8200/v1", "L": "http://glass"}, Reach: []string{"glass"}}
	specs := map[string]*AppSpec{"importer": importer, "glass": {}}
	if w := planNetworks(specs, nil).wires; len(w) != 1 || w[0].Provider != "glass" {
		t.Fatalf("wires = %v", w)
	}
	specs["vault"] = &AppSpec{} // installed later
	for _, w := range planNetworks(specs, nil).wires {
		if w.Provider == "vault" {
			t.Fatal("an app installed later was wired to one that merely names it")
		}
	}
	if refs := UnapprovedRefs(specs); !slices.Equal(refs, []string{"importer → vault"}) {
		t.Fatalf("unapproved = %v", refs)
	}
	importer.Reach = append(importer.Reach, "vault")
	var vault []int
	for _, w := range planNetworks(specs, nil).wires {
		if w.Provider == "vault" {
			vault = w.Ports
		}
	}
	if !slices.Equal(vault, []int{8200}) {
		t.Fatalf("approved, vault wired on %v", vault)
	}
	// The one-time upgrade approves what the env already named among
	// installed apps — never one installed after.
	legacy := &AppSpec{Env: map[string]string{"N": "http://ntfy", "F": "http://future"}}
	if !legacy.GrandfatherReach("legacy", func(n string) bool { return n == "ntfy" }) || !slices.Equal(legacy.Reach, []string{"ntfy"}) {
		t.Fatalf("grandfathered = %v", legacy.Reach)
	}
}

// The review shows where an imported app's settings point, passwords
// masked, and the reach it asks for.
func TestReviewShowsEnvLinksAndReach(t *testing.T) {
	r := BuildReview("app", &AppSpec{Source: Source{Type: "image", Image: "x"}, Reach: []string{"vault"},
		Env: map[string]string{"DB": "postgres://admin:hunter2@db.example.com:5432/x", "MODE": "fast", "API": "http://vault:8200"}})
	got := strings.Join(r.EnvLinks, " | ")
	if strings.Contains(got, "hunter2") || !strings.Contains(got, "DB=postgres://admin:•••@db.example.com:5432/x") ||
		!strings.Contains(got, "API=http://vault:8200") || strings.Contains(got, "MODE") {
		t.Fatalf("env links = %q", got)
	}
	if !slices.Equal(r.ReachAsked, []string{"vault"}) {
		t.Fatalf("reach asked = %v", r.ReachAsked)
	}
}

// The one-time upgrade pass: an existing app keeps the wires and host
// folders it had — as approvals of their own — and the pass runs once.
func TestGrandfatherOnceApprovesWhatExists(t *testing.T) {
	st, err := storetest.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	e := &Engine{Store: st, Mat: &Materializer{DataDir: t.TempDir()}, Log: discardLog()}
	put := func(name string, spec AppSpec) {
		a, _ := st.CreateApp(name)
		b, _ := json.Marshal(spec)
		st.CreateRevision(a.ID, string(b), "", "t")
	}
	put("ntfy", AppSpec{Source: Source{Type: "image", Image: "x"}})
	put("feed", AppSpec{Source: Source{Type: "image", Image: "x"}, Env: map[string]string{"NTFY_URL": "http://ntfy"},
		Volumes: []Volume{{Name: "media", Mount: "/media", Host: "/mnt/media/feed"}}})
	e.GrandfatherReachOnce(context.Background())
	a, _ := st.AppByName("feed")
	spec, _ := LatestSpec(st, a.ID)
	if !slices.Equal(spec.Reach, []string{"ntfy"}) || !slices.Equal(spec.AcknowledgeHostAccess, []string{"/mnt/media/feed"}) {
		t.Fatalf("grandfathered spec: reach %v, ack %v", spec.Reach, spec.AcknowledgeHostAccess)
	}
	put("later", AppSpec{Source: Source{Type: "image", Image: "x"}})
	before, _ := st.LatestRevision(a.ID)
	e.GrandfatherReachOnce(context.Background())
	if after, _ := st.LatestRevision(a.ID); after.Rev != before.Rev {
		t.Fatal("the pass ran twice")
	}
}

func discardLog() *slog.Logger { return slog.New(slog.DiscardHandler) }
