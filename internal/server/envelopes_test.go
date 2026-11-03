package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/dockerx"
)

// The envelopes below were built as maps until they became named structs
// for tsgen. A map encodes its keys sorted, so each struct keeps its
// fields in alphabetical order: re-encoding the top level through a map
// must give back the very bytes the handler wrote.
func TestEnvelopesEncodeAsTheirMapsDid(t *testing.T) {
	srv, h := newTestServer(t)
	srv.engine = &apps.Engine{Store: srv.store}
	// /api/system asks Docker; a socket nobody listens on answers "not
	// available" at once.
	t.Setenv("DOCKER_SOCK", filepath.Join(t.TempDir(), "none.sock"))
	srv.docker = dockerx.New()
	c := setup(t, h)
	if _, err := srv.store.CreateApp("web"); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{
		"/api/setup/status",
		"/api/auth/me",
		"/api/people/me",
		"/api/people",
		"/api/system",
		"/api/federation",
		"/api/offsite/targets",
		"/api/offsite/presets",
		"/api/apps/web/deploy-status",
		"/api/inbox/summary",
	} {
		w := do(t, h, "GET", path, "", withCookie(c))
		if w.Code != http.StatusOK {
			t.Errorf("%s: %d %s", path, w.Code, w.Body)
			continue
		}
		body := bytes.TrimSpace(w.Body.Bytes())
		if path == "/api/system" && !bytes.Contains(body, []byte(`"docker":{"available":false},`)) {
			t.Errorf("/api/system: docker without version and info should be bare: %s", body)
		}
		var top map[string]json.RawMessage
		if err := json.Unmarshal(body, &top); err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		again, _ := json.Marshal(top)
		if !bytes.Equal(again, body) {
			t.Errorf("%s: keys out of map order\n got %s\nwant %s", path, body, again)
		}
	}
}

// Exact bodies where nothing in them varies from run to run.
func TestEnvelopeBodies(t *testing.T) {
	srv, h := newTestServer(t)
	srv.engine = &apps.Engine{Store: srv.store}
	c := setup(t, h)
	if _, err := srv.store.CreateApp("web"); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"/api/auth/me":         `{"displayName":"admin","handle":"admin","operator":true,"role":"operator"}`,
		"/api/offsite/targets": `{"passphraseSet":false,"targets":[]}`,
		// No deploy yet: last is absent, not null.
		"/api/apps/web/deploy-status": `{"current":{"running":false}}`,
	} {
		w := do(t, h, "GET", path, "", withCookie(c))
		if got := strings.TrimSpace(w.Body.String()); got != want {
			t.Errorf("%s\n got %s\nwant %s", path, got, want)
		}
	}
}

// With an update checker, the fog release row sends every field even when
// it is empty or false; only the no-checker row is the bare "available".
func TestFogUpdateViewKeepsEmptyFields(t *testing.T) {
	cur, latest, notes, ignored := "v1.0.0", "", "", false
	got, _ := json.Marshal(fogUpdateView{CurrentVersion: &cur, Ignored: &ignored, LatestVersion: &latest, Notes: &notes})
	want := `{"available":false,"currentVersion":"v1.0.0","ignored":false,"latestVersion":"","notes":""}`
	if string(got) != want {
		t.Errorf("with a checker\n got %s\nwant %s", got, want)
	}
	if got, _ := json.Marshal(fogUpdateView{}); string(got) != `{"available":false}` {
		t.Errorf("without a checker: %s", got)
	}
}
