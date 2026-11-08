package server

import (
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"slices"
	"testing"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/build"
	"gitea.lan/matthew/fog/internal/dockerx"
)

// The attack: a provider is deleted and an import takes its name. Its
// consumers still held their connection to "glass" — the env pointing at
// it, the reach, the rows naming it — and were the newcomer's at once,
// held for review and unread. Deleting a provider disconnects every app
// connected to it; a held app is nobody's provider until it is confirmed.
func TestDeletedProviderLeavesItsConsumersNothing(t *testing.T) {
	f := newIntegrationFixture(t)
	t.Setenv("DOCKER_SOCK", filepath.Join(t.TempDir(), "none.sock"))
	f.s.engine.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	f.s.engine.Mat = &apps.Materializer{DataDir: f.s.cfg.DataDir}
	f.s.engine.Builder = &build.Builder{DataDir: f.s.cfg.DataDir}
	f.s.engine.Docker = dockerx.New()
	if w := f.connect(t); w.Code != http.StatusCreated {
		t.Fatalf("connect: %d %s", w.Code, w.Body)
	}
	if spec := latestSpec(t, f.s, f.newtabID); !slices.Contains(spec.Reach, "glass") {
		t.Fatalf("connected without reach: %v", spec.Reach)
	}

	if w := do(t, f.h, "DELETE", "/api/apps/glass", "", withCookie(f.cookie)); w.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	spec := latestSpec(t, f.s, f.newtabID)
	if slices.Contains(spec.Reach, "glass") || slices.Contains(spec.Uses, "glass") || slices.Contains(spec.Secrets, "GLASS_API_KEY") {
		t.Fatalf("newtab still names the deleted glass: reach %v uses %v secrets %v", spec.Reach, spec.Uses, spec.Secrets)
	}
	if _, set := spec.Env["GLASS_URL"]; set {
		t.Fatalf("newtab keeps glass's env: %v", spec.Env)
	}
	if rows, _ := f.s.store.IntegrationsWithProvider("glass"); len(rows) != 0 {
		t.Fatalf("connections to the deleted glass remain: %+v", rows)
	}

	// The newcomer, held: not a provider anyone can connect to.
	imp := &apps.AppSpec{Source: apps.Source{Type: "image", Image: "evil"}, Routes: []apps.Route{{Port: 80}},
		Connection: &apps.Connection{Port: 80, Offers: []apps.Offer{{
			ID: "telemetry", Title: "Telemetry", Env: map[string]string{"GLASS_URL": "{{url}}"}, Secrets: []string{"GLASS_API_KEY"},
		}}}}
	if err := f.s.engine.CreateForReview(t.Context(), "glass", imp, nil, "import"); err != nil {
		t.Fatal(err)
	}
	got := f.list(t)
	if len(got.Connected) != 0 || len(got.Available) != 1 || got.Available[0].Installed {
		t.Fatalf("a held import is offered as a provider: %+v", got)
	}
	if w := f.connect(t); w.Code == http.StatusCreated {
		t.Fatal("connected to a held import")
	}
}
