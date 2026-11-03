package server

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/auth"
)

// flareApp creates an app with a minted _errors_key, returning (appID, key).
// Mirrors what the apps engine's ensureErrorsKey does at deploy time, without needing an
// engine in the test harness.
func flareApp(t *testing.T, s *Server, name string) (int64, string) {
	t.Helper()
	app, err := s.store.CreateApp(name)
	if err != nil {
		t.Fatal(err)
	}
	key := "testkey-" + name
	ct, err := s.vault.Encrypt(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.SetSecret(app.ID, apps.SecretErrorsKey, ct); err != nil {
		t.Fatal(err)
	}
	return app.ID, key
}

// envelope builds a minimal newline-delimited envelope around one event.
func envelope(eventJSON string) string {
	return "{}\n" + `{"type":"event","length":` + strconv.Itoa(len(eventJSON)) + "}\n" + eventJSON + "\n"
}

const rubyEvent = `{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc","platform":"ruby","level":"error",` +
	`"transaction":"PostsController#show","release":"abc123","environment":"production",` +
	`"exception":{"values":[{"type":"PG::UndefinedColumn","value":"column posts.slug does not exist",` +
	`"mechanism":{"type":"rails","handled":false},` +
	`"stacktrace":{"frames":[{"filename":"app/models/post.rb","function":"find!","lineno":42,"in_app":true}]}}]}}`

func withSentryAuth(key string) func(*http.Request) {
	return func(r *http.Request) {
		r.Header.Set("X-Sentry-Auth", "Sentry sentry_version=7, sentry_client=test/1.0, sentry_key="+key)
	}
}

func TestFlareIngestStoresEvent(t *testing.T) {
	srv, h := newTestServer(t)
	appID, key := flareApp(t, srv, "manor")

	w := do(t, h, "POST", fmt.Sprintf("/ingest/api/%d/envelope/", appID), envelope(rubyEvent), withSentryAuth(key))
	if w.Code != http.StatusOK {
		t.Fatalf("ingest: %d %s", w.Code, w.Body)
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("9ec79c33ec9942ab8353589fcb2e04dc")) {
		t.Fatalf("response should echo the event id: %s", w.Body)
	}

	groups, err := srv.store.ListErrorGroups(appID, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 {
		t.Fatalf("groups = %d, want 1", len(groups))
	}
	g := groups[0]
	if g.ErrorClass != "PG::UndefinedColumn" || !g.Unhandled || g.Culprit != "PostsController#show" || g.LastRelease != "abc123" {
		t.Fatalf("group: %+v", g)
	}

	// The group is the durable record, and the only Inbox row for it: no
	// notice restates the Errors tab's own count beside it.
	notices, _ := srv.store.ActiveNotices()
	if len(notices) != 0 {
		t.Fatalf("ingest should file no notice, got %+v", notices)
	}
}

func TestFlareIngestNoTrailingSlash(t *testing.T) {
	srv, h := newTestServer(t)
	appID, key := flareApp(t, srv, "manor")
	w := do(t, h, "POST", fmt.Sprintf("/ingest/api/%d/envelope", appID), envelope(rubyEvent), withSentryAuth(key))
	if w.Code != http.StatusOK {
		t.Fatalf("no-slash ingest: %d %s", w.Code, w.Body)
	}
}

func TestFlareIngestGzip(t *testing.T) {
	srv, h := newTestServer(t)
	appID, key := flareApp(t, srv, "manor")

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	gz.Write([]byte(envelope(rubyEvent)))
	gz.Close()

	r := httptest.NewRequest("POST", fmt.Sprintf("/ingest/api/%d/envelope/", appID), bytes.NewReader(buf.Bytes()))
	r.Header.Set("Content-Encoding", "gzip")
	withSentryAuth(key)(r)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("gzip ingest: %d %s", w.Code, w.Body)
	}
	if groups, _ := srv.store.ListErrorGroups(appID, "", 10); len(groups) != 1 {
		t.Fatalf("groups = %d, want 1", len(groups))
	}
}

func TestFlareIngestAuth(t *testing.T) {
	srv, h := newTestServer(t)
	appID, _ := flareApp(t, srv, "manor")

	// Wrong key → 403 (the SDK-visible "stop sending" signal).
	w := do(t, h, "POST", fmt.Sprintf("/ingest/api/%d/envelope/", appID), envelope(rubyEvent), withSentryAuth("wrong"))
	if w.Code != http.StatusForbidden {
		t.Fatalf("wrong key: %d, want 403", w.Code)
	}
	// No key at all → 403.
	w = do(t, h, "POST", fmt.Sprintf("/ingest/api/%d/envelope/", appID), envelope(rubyEvent))
	if w.Code != http.StatusForbidden {
		t.Fatalf("missing key: %d, want 403", w.Code)
	}
	// Unknown app → 404, indistinguishable from a bad path.
	w = do(t, h, "POST", "/ingest/api/9999/envelope/", envelope(rubyEvent), withSentryAuth("x"))
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown app: %d, want 404", w.Code)
	}
	// Nothing stored by any of it.
	if groups, _ := srv.store.ListErrorGroups(appID, "", 10); len(groups) != 0 {
		t.Fatalf("rejected requests must store nothing, got %d groups", len(groups))
	}
}

func TestFlareIngestKeyInEnvelopeDSN(t *testing.T) {
	// sentry-ruby can carry credentials in the envelope header's dsn field
	// instead of the auth header.
	srv, h := newTestServer(t)
	appID, key := flareApp(t, srv, "manor")
	env := fmt.Sprintf(`{"dsn":"http://%s@fog:8080/ingest/%d"}`, key, appID) + "\n" +
		`{"type":"event","length":` + strconv.Itoa(len(rubyEvent)) + "}\n" + rubyEvent + "\n"
	w := do(t, h, "POST", fmt.Sprintf("/ingest/api/%d/envelope/", appID), env)
	if w.Code != http.StatusOK {
		t.Fatalf("dsn-header auth: %d %s", w.Code, w.Body)
	}
}

func TestFlareIngestCrossOriginAllowed(t *testing.T) {
	// The Origin guard protects /api/*; ingest lives outside it by design
	// (a browser SDK later must be able to POST cross-origin).
	srv, h := newTestServer(t)
	appID, key := flareApp(t, srv, "manor")
	w := do(t, h, "POST", fmt.Sprintf("/ingest/api/%d/envelope/", appID), envelope(rubyEvent),
		withSentryAuth(key), withOrigin("https://manor.example.com"))
	if w.Code != http.StatusOK {
		t.Fatalf("cross-origin ingest: %d %s", w.Code, w.Body)
	}
	if groups, _ := srv.store.ListErrorGroups(appID, "", 10); len(groups) != 1 {
		t.Fatalf("groups = %d, want 1", len(groups))
	}
}

func TestFlareIngestRateLimit(t *testing.T) {
	srv, h := newTestServer(t)
	appID, key := flareApp(t, srv, "manor")
	srv.flareLimiter = auth.NewRateLimiter(1, time.Minute)

	if w := do(t, h, "POST", fmt.Sprintf("/ingest/api/%d/envelope/", appID), envelope(rubyEvent), withSentryAuth(key)); w.Code != http.StatusOK {
		t.Fatalf("first: %d", w.Code)
	}
	w := do(t, h, "POST", fmt.Sprintf("/ingest/api/%d/envelope/", appID), envelope(rubyEvent), withSentryAuth(key))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("second: %d, want 429", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("429 must carry Retry-After — it's what makes SDKs shed load")
	}
}

func TestFlareIngestDedupesRetries(t *testing.T) {
	srv, h := newTestServer(t)
	appID, key := flareApp(t, srv, "manor")
	path := fmt.Sprintf("/ingest/api/%d/envelope/", appID)
	do(t, h, "POST", path, envelope(rubyEvent), withSentryAuth(key))
	do(t, h, "POST", path, envelope(rubyEvent), withSentryAuth(key)) // transport retry

	groups, _ := srv.store.ListErrorGroups(appID, "", 10)
	if len(groups) != 1 || groups[0].EventsCount != 1 {
		t.Fatalf("retry must not double-count: %+v", groups)
	}
}

func TestFlareIngestIgnoresNonEventItems(t *testing.T) {
	srv, h := newTestServer(t)
	appID, key := flareApp(t, srv, "manor")
	report := `{"discarded_events":[]}`
	env := "{}\n" +
		`{"type":"client_report","length":` + strconv.Itoa(len(report)) + "}\n" + report + "\n" +
		`{"type":"event","length":` + strconv.Itoa(len(rubyEvent)) + "}\n" + rubyEvent + "\n"
	w := do(t, h, "POST", fmt.Sprintf("/ingest/api/%d/envelope/", appID), env, withSentryAuth(key))
	if w.Code != http.StatusOK {
		t.Fatalf("mixed envelope: %d %s", w.Code, w.Body)
	}
	if groups, _ := srv.store.ListErrorGroups(appID, "", 10); len(groups) != 1 {
		t.Fatalf("only the event item should store: %d groups", len(groups))
	}
}
