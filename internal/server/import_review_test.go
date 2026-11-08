package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"gitea.lan/matthew/fog/internal/apps"
)

// A manifest that binds the Docker socket is the whole server for whoever
// wrote it. Import must never deploy it: it is refused outright without an
// acknowledgement, and with one it is created held — no deploy, the review
// lists the socket, every deploy path refuses, and only a confirmation of
// the revision that was read releases it.
func TestImportManifestNeverDeploysUnreviewed(t *testing.T) {
	t.Parallel()

	s, h := newTestServer(t)
	s.engine = &apps.Engine{Store: s.store, Vault: s.vault, Mat: &apps.Materializer{DataDir: s.cfg.DataDir}}
	admin := setup(t, h)

	importYAML := func(y string) *httpResult {
		body, _ := json.Marshal(map[string]string{"yaml": y})
		w := do(t, h, "POST", "/api/apps/import", string(body), withCookie(admin))
		return &httpResult{w.Code, w.Body.String()}
	}

	mount := func(host string, ack bool) string {
		y := "name: evil\nspec:\n  source: {type: image, image: evil:latest}\n  volumes:\n    - {name: sock, mount: /var/run/docker.sock, host: \"" + host + "\"}\n"
		if ack {
			y += "  acknowledgeHostAccess: [\"" + host + "\"]\n"
		}
		return y
	}
	refusedNothingCreated := func(host, want string, ack bool) {
		t.Helper()
		r := importYAML(mount(host, ack))
		if r.code != http.StatusBadRequest || !strings.Contains(r.body, want) {
			t.Fatalf("import mounting %s (acknowledged %v): %d %s", host, ack, r.code, r.body)
		}
		if _, err := s.store.AppByName("evil"); err == nil {
			t.Fatalf("a refused import of %s created the app", host)
		}
	}
	// The Docker socket, /, fog's data: refused by any spelling — even when
	// the manifest acknowledges them itself.
	for _, host := range []string{"/var/run/docker.sock", "/var/run/../run/docker.sock", "//", s.cfg.DataDir + "/"} {
		refusedNothingCreated(host, "never", false)
		refusedNothingCreated(host, "never", true)
	}
	// Any other host folder outside fog's storage: refused unacknowledged.
	refusedNothingCreated("/run/systemd/private", "acknowledgeHostAccess", false)

	// Acknowledged by the manifest itself: created, held, reviewed.
	r := importYAML(`name: evil
spec:
  source: {type: image, image: evil:latest}
  deployStrategy: restart
  acknowledgeHostAccess: [/run/systemd/private]
  volumes:
    - {name: sock, mount: /run/systemd/private, host: /run/systemd/private}
  ports:
    - {port: 22, hostPort: 2222}
  secrets: [ADMIN_TOKEN]
  uses: [events]
`)
	if r.code != http.StatusCreated {
		t.Fatalf("import: %d %s", r.code, r.body)
	}
	var got struct {
		Rev           int64       `json:"rev"`
		ReviewPending bool        `json:"reviewPending"`
		Review        apps.Review `json:"review"`
	}
	if err := json.Unmarshal([]byte(r.body), &got); err != nil {
		t.Fatal(err)
	}
	if !got.ReviewPending || got.Rev == 0 {
		t.Fatalf("import answered %s", r.body)
	}
	rv := got.Review
	if len(rv.Mounts) != 1 || rv.Mounts[0].Host != "/run/systemd/private" || rv.Mounts[0].Dangerous == "" || !rv.Mounts[0].Acknowledged {
		t.Errorf("review mounts = %+v", rv.Mounts)
	}
	if len(rv.Ports) != 1 || rv.Ports[0].HostPort != 2222 || len(rv.Secrets) != 1 || len(rv.Uses) != 1 || len(rv.Acknowledge) != 1 {
		t.Errorf("review = %+v", rv)
	}

	app, err := s.store.AppByName("evil")
	if err != nil {
		t.Fatal(err)
	}
	if s.engine.Deploying("evil") {
		t.Fatal("import started a deploy")
	}
	if ds, _ := s.store.ListDeploySummaries(app.ID, 10, 0); len(ds) != 0 {
		t.Fatalf("import recorded deploys: %+v", ds)
	}

	// The ordinary deploy button refuses, saying why.
	w := do(t, h, "POST", "/api/apps/evil/actions", `{"action":"deploy"}`, withCookie(admin))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "review") {
		t.Fatalf("deploy of a held app: %d %s", w.Code, w.Body)
	}
	// The app page says it is held.
	w = do(t, h, "GET", "/api/apps/evil", "", withCookie(admin))
	if !strings.Contains(w.Body.String(), `"reviewPending":true`) {
		t.Errorf("app detail doesn't say the app is held: %s", w.Body)
	}
	// A confirmation must name a revision, and the latest one.
	w = do(t, h, "POST", "/api/apps/evil/review/confirm", `{}`, withCookie(admin))
	if w.Code != http.StatusBadRequest {
		t.Errorf("confirmation without a revision: %d %s", w.Code, w.Body)
	}
	w = do(t, h, "POST", "/api/apps/evil/review/confirm", `{"rev":`+strconv.FormatInt(got.Rev+1, 10)+`}`, withCookie(admin))
	if w.Code != http.StatusConflict {
		t.Errorf("confirmation of a revision that isn't the latest: %d %s", w.Code, w.Body)
	}
	if held, _ := s.engine.ReviewPending("evil"); !held {
		t.Fatal("a refused confirmation released the hold")
	}
	// Unauthenticated: no.
	w = do(t, h, "POST", "/api/apps/evil/review/confirm", `{"rev":`+strconv.FormatInt(got.Rev, 10)+`}`)
	if w.Code != http.StatusUnauthorized && w.Code != http.StatusForbidden {
		t.Errorf("unauthenticated confirmation: %d", w.Code)
	}
	// The review reads back for the page an import lands on.
	w = do(t, h, "GET", "/api/apps/evil/review", "", withCookie(admin))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"reviewPending":true`) {
		t.Errorf("review: %d %s", w.Code, w.Body)
	}
}

type httpResult struct {
	code int
	body string
}
