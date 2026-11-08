package server

import (
	"net/http"
	"strings"
	"testing"
)

// corruptApp seeds an app whose only revision holds unparseable spec JSON —
// the shape a partially-written or damaged DB row takes.
func corruptApp(t *testing.T, srv *Server, name string) {
	t.Helper()
	app, err := srv.store.CreateApp(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.store.CreateRevision(app.ID, `{"source": tru`, "", "test"); err != nil {
		t.Fatal(err)
	}
}

// A corrupt revision row must surface as a 500 naming the app, never as a
// clean 200 with an empty spec the UI would render — or worse, export a blank
// manifest an operator archives as if it were the app.
func TestCorruptSpecIsA500NotAnEmptySpec(t *testing.T) {
	t.Parallel()

	srv, h := newTestServer(t)
	c := setup(t, h)
	corruptApp(t, srv, "blog")

	for _, path := range []string{"/api/apps/blog", "/api/apps/blog/export", "/api/system/export"} {
		w := do(t, h, "GET", path, "", withCookie(c))
		if w.Code != http.StatusInternalServerError {
			t.Errorf("GET %s = %d %s, want 500", path, w.Code, w.Body)
		}
	}
	// The export-all body must not have started streaming YAML before the
	// error was known.
	w := do(t, h, "GET", "/api/system/export", "", withCookie(c))
	if strings.Contains(w.Body.String(), "Fog server export") {
		t.Errorf("export-all wrote the attachment preamble before failing: %s", w.Body)
	}
}

// A manifest is how an app moves between servers, so the fields that draw its
// boundary have to survive the trip in both directions. Losing one of them
// quietly would hand back an unsealed app.
func TestManifestCarriesTheNetworkBoundary(t *testing.T) {
	t.Parallel()

	m, err := yamlManifest(`
name: fit
spec:
  source: {type: image, image: "x:1"}
  network: sealed
  routes:
    - port: 3000
      lanOnly: true
`)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Spec.Validate(); err != nil {
		t.Fatal(err)
	}
	if !m.Spec.Sealed() || !m.Spec.Routes[0].LanOnly {
		t.Fatalf("read %+v", m.Spec)
	}
	out, err := specYAML(*m)
	if err != nil {
		t.Fatal(err)
	}
	back, err := yamlManifest(string(out))
	if err != nil {
		t.Fatal(err)
	}
	if !back.Spec.Sealed() || !back.Spec.Routes[0].LanOnly {
		t.Fatalf("export dropped a field:\n%s", out)
	}

	peer, err := yamlManifest("name: ntfy\nspec:\n  source: {type: image, image: \"x:1\"}\n  sealedPeer: true\n")
	if err != nil {
		t.Fatal(err)
	}
	if !peer.Spec.SealedPeer || peer.Spec.Network != "" {
		t.Fatalf("read %+v", peer.Spec)
	}
}
