package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/build"
	"gitea.lan/matthew/fog/internal/dockerx"
	"gitea.lan/matthew/fog/internal/store"
)

// widgetsFixture is a start page with its grant, and hook, an app declaring
// the contract, answering from a local server the way a container would.
type widgetsFixture struct {
	h      http.Handler
	newtab string
	asked  []*http.Request
}

func newWidgetsFixture(t *testing.T, app http.HandlerFunc) *widgetsFixture {
	t.Helper()
	s, h := newTestServer(t)
	f := &widgetsFixture{h: h}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.asked = append(f.asked, r)
		app(w, r)
	}))
	t.Cleanup(ts.Close)

	id, newtab := appWithToken(t, s, "newtab")
	withSpec(t, s, id, apps.AppSpec{Uses: []string{apps.UsesHome}})
	f.newtab = newtab
	hookID, _ := appWithToken(t, s, "hook")
	withSpec(t, s, hookID, apps.AppSpec{Routes: []apps.Route{{Port: 80}}})
	if err := s.store.SetAppContracts(hookID, []string{build.WidgetsContract}); err != nil {
		t.Fatal(err)
	}
	plainID, _ := appWithToken(t, s, "plain")
	withSpec(t, s, plainID, apps.AppSpec{Routes: []apps.Route{{Port: 80}}})

	s.appGrantEndpointFn = func(_ context.Context, a *store.App) (string, string, error) {
		if a.Name != "hook" {
			return "", "", fmt.Errorf("%s isn't reachable here", a.Name)
		}
		return ts.URL, "hook-token", nil
	}
	return f
}

func TestWidgetsRelay(t *testing.T) {
	f := newWidgetsFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Set-Cookie", "app=private")
		fmt.Fprint(w, `{"widgets":[{"id":"unread","title":"Unread","blocks":[{"type":"stat","value":"3"}]}]}`)
	})
	w := do(t, f.h, "GET", "/app/v1/widgets/hook", "", withBearer(f.newtab))
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if got := w.Header().Get("ETag"); got != `"v1"` {
		t.Errorf("ETag = %q", got)
	}
	if got := w.Header().Get("Set-Cookie"); got != "" {
		t.Errorf("relayed Set-Cookie %q", got)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}
	var doc struct {
		Widgets []struct{ ID string }
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil || len(doc.Widgets) != 1 || doc.Widgets[0].ID != "unread" {
		t.Errorf("body = %s (%v)", w.Body, err)
	}
	r := f.asked[0]
	if r.URL.Path != "/fog/widgets" {
		t.Errorf("asked %s", r.URL)
	}
	// The app sees its own token, never the reader's.
	if got := r.Header.Get("Authorization"); got != "Bearer hook-token" {
		t.Errorf("Authorization = %q", got)
	}
}

func TestWidgetsRelayPassesTheETagBothWays(t *testing.T) {
	f := newWidgetsFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		fmt.Fprint(w, `{"widgets":[]}`)
	})
	w := do(t, f.h, "GET", "/app/v1/widgets/hook", "", withBearer(f.newtab), func(r *http.Request) { r.Header.Set("If-None-Match", `"v1"`) })
	if w.Code != http.StatusNotModified || w.Body.Len() != 0 {
		t.Fatalf("%d %q", w.Code, w.Body)
	}
	if got := w.Header().Get("ETag"); got != `"v1"` {
		t.Errorf("ETag = %q", got)
	}
}

func TestWidgetsRelayGrant(t *testing.T) {
	s, h := newTestServer(t)
	asked := false
	s.appGrantEndpointFn = func(context.Context, *store.App) (string, string, error) {
		asked = true
		return "http://127.0.0.1:1", "t", nil
	}
	hookID, hook := appWithToken(t, s, "hook")
	withSpec(t, s, hookID, apps.AppSpec{})
	s.store.SetAppContracts(hookID, []string{build.WidgetsContract})

	if w := do(t, h, "GET", "/app/v1/widgets/hook", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("no token: %d", w.Code)
	}
	if w := do(t, h, "GET", "/app/v1/search/hook?q=x", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("no token, search: %d", w.Code)
	}
	if w := do(t, h, "GET", "/app/v1/widgets/hook", "", withBearer(hook)); w.Code != http.StatusForbidden {
		t.Errorf("no grant: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "GET", "/app/v1/search/hook?q=x", "", withBearer(hook)); w.Code != http.StatusForbidden {
		t.Errorf("no grant, search: %d %s", w.Code, w.Body)
	}
	if asked {
		t.Error("an ungranted reader reached the app")
	}
}

// newtab reads a 404 as the app not serving widgets, and stops expecting any.
func TestWidgetsRelayNotFound(t *testing.T) {
	f := newWidgetsFixture(t, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	for name, path := range map[string]string{
		"no such app":        "/app/v1/widgets/nobody",
		"undeclared":         "/app/v1/widgets/plain",
		"app has no route":   "/app/v1/widgets/hook",
		"app doesn't search": "/app/v1/search/hook?q=x",
	} {
		if w := do(t, f.h, "GET", path, "", withBearer(f.newtab)); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
	}
}

// Every other failure of the app is a 502, a 403 from it above all: newtab
// reads a 403 as its own grant being withdrawn.
func TestWidgetsRelayAppFailures(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError, http.StatusFound} {
		f := newWidgetsFixture(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "https://elsewhere.example/")
			w.Header().Set("ETag", `"stale"`)
			w.WriteHeader(status)
		})
		w := do(t, f.h, "GET", "/app/v1/widgets/hook", "", withBearer(f.newtab))
		if w.Code != http.StatusBadGateway {
			t.Errorf("app answered %d, relay %d", status, w.Code)
		}
		if got := w.Header().Get("ETag"); got != "" {
			t.Errorf("app answered %d, relayed ETag %q", status, got)
		}
		if len(f.asked) != 1 {
			t.Errorf("app answered %d, asked %d times", status, len(f.asked))
		}
	}
}

func TestWidgetsRelayRefusesAnOversizedAnswer(t *testing.T) {
	f := newWidgetsFixture(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, strings.Repeat("x", maxRelayed+1))
	})
	if w := do(t, f.h, "GET", "/app/v1/widgets/hook", "", withBearer(f.newtab)); w.Code != http.StatusBadGateway {
		t.Errorf("%d", w.Code)
	}
}

func TestSearchRelay(t *testing.T) {
	f := newWidgetsFixture(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"results":[{"title":"A book","href":"/books/1"}]}`)
	})
	w := do(t, f.h, "GET", "/app/v1/search/hook?q=red+%26+blue&admin=1", "", withBearer(f.newtab))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "A book") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	r := f.asked[0]
	if r.URL.Path != "/fog/search" || r.URL.Query().Get("q") != "red & blue" {
		t.Errorf("asked %s", r.URL)
	}
	// Only q goes on: the rest of a reader's query isn't the app's business.
	if r.URL.Query().Has("admin") {
		t.Errorf("relayed %s", r.URL.RawQuery)
	}
}

// A sealed app has no address on the fog network. The relay goes through the
// real engine here, against a Docker that lists the app on fog-sealed alone.
func TestWidgetsRelayReachesASealedApp(t *testing.T) {
	s, h := newTestServer(t)
	var authorization string
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		fmt.Fprint(w, `{"widgets":[]}`)
	}))
	t.Cleanup(app.Close)
	port := app.Listener.Addr().(*net.TCPAddr).Port

	dir, err := os.MkdirTemp("", "fog")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := dockerx.ContainerSummary{ID: "fit1", State: "running", Labels: map[string]string{"fog.app": "fit"}}
		c.NetworkSettings.Networks = map[string]dockerx.NetworkEndpoint{apps.SealedNetworkName: {IPAddress: "127.0.0.1"}}
		json.NewEncoder(w).Encode([]dockerx.ContainerSummary{c})
	}))
	t.Setenv("DOCKER_SOCK", sock)
	s.engine = &apps.Engine{Store: s.store, Vault: s.vault, Docker: dockerx.New()}

	readerID, reader := appWithToken(t, s, "newtab")
	withSpec(t, s, readerID, apps.AppSpec{Uses: []string{apps.UsesHome}})
	fitID, fitToken := appWithToken(t, s, "fit")
	withSpec(t, s, fitID, apps.AppSpec{Network: apps.NetworkSealed, Health: &apps.Health{Path: "/up", Port: port}})
	if err := s.store.SetAppContracts(fitID, []string{build.WidgetsContract}); err != nil {
		t.Fatal(err)
	}

	if w := do(t, h, "GET", "/app/v1/widgets/fit", "", withBearer(reader)); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if authorization != "Bearer "+fitToken {
		t.Errorf("Authorization = %q", authorization)
	}
}
