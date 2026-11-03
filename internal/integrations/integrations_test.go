package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"gitea.lan/matthew/fog/internal/appcall"
	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/store"
)

// fakeCaller is the provider as the service sees it, no HTTP in between:
// each call is answered by fn, and recorded.
type fakeCaller struct {
	mu    sync.Mutex
	calls []string
	fn    func(ctx context.Context, method, path string, body any) ([]byte, error)
}

func (f *fakeCaller) Call(ctx context.Context, app *store.App, method, path string, body any) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, method+" "+app.Name+path)
	f.mu.Unlock()
	return f.fn(ctx, method, path, body)
}

// specWriter stands in for the engine: a revision, and the secrets it was
// handed.
type specWriter struct {
	st      *store.Store
	secrets map[string]string
}

func (w *specWriter) UpdateSpec(ctx context.Context, name string, spec *apps.AppSpec, secrets map[string]string, comment, actor string) error {
	app, err := w.st.AppByName(name)
	if err != nil {
		return err
	}
	raw, _ := json.Marshal(spec)
	if _, err := w.st.CreateRevision(app.ID, string(raw), comment, actor); err != nil {
		return err
	}
	for k, v := range secrets {
		w.secrets[k] = v
	}
	return nil
}

type fixture struct {
	svc      *Service
	st       *store.Store
	caller   *fakeCaller
	specs    *specWriter
	consumer *store.App
	lucidID  int64
	mk       func(string, apps.AppSpec) *store.App
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	mk := func(name string, spec apps.AppSpec) *store.App {
		t.Helper()
		app, err := st.CreateApp(name)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(spec)
		if _, err := st.CreateRevision(app.ID, string(raw), "", "test"); err != nil {
			t.Fatal(err)
		}
		return app
	}
	lucid := mk("lucid", apps.AppSpec{
		Source: apps.Source{Type: "image", Image: "lucid"},
		Routes: []apps.Route{{Port: 80}},
		Connection: &apps.Connection{Port: 80, Offers: []apps.Offer{{
			ID: "telemetry", Title: "Telemetry", Capabilities: []string{"Requests and errors in Lucid"},
			Env: map[string]string{"LUCID_URL": "{{url}}"}, Secrets: []string{"LUCID_API_KEY"},
		}, {
			ID: "read", Title: "Read access", Consent: true, Blurb: "Reads Lucid's charts.",
			Env: map[string]string{"LUCID_READ_URL": "{{url}}"}, Secrets: []string{"LUCID_READ_TOKEN"},
		}}},
	})
	st.SetAppCurrentRev(lucid.ID, 1)
	st.SetAppDesiredState(lucid.ID, "running")
	consumer := mk("newtab", apps.AppSpec{Source: apps.Source{Type: "image", Image: "newtab"},
		Wants: []apps.Want{{Offer: "lucid/telemetry"}}})

	f := &fixture{st: st, consumer: consumer, lucidID: lucid.ID, mk: mk,
		caller: &fakeCaller{}, specs: &specWriter{st: st, secrets: map[string]string{}}}
	f.caller.fn = f.provider
	f.svc = &Service{Store: st, Specs: f.specs, Apps: f.caller,
		PublicURL: func(a *store.App) string { return "https://" + a.Name + ".home.test" },
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil))}
	return f
}

// wants replaces what the consumer's spec wants.
func (f *fixture) wants(t *testing.T, w ...apps.Want) {
	t.Helper()
	spec, _ := apps.LatestSpec(f.st, f.consumer.ID)
	spec.Wants = w
	raw, _ := json.Marshal(spec)
	if _, err := f.st.CreateRevision(f.consumer.ID, string(raw), "", "test"); err != nil {
		t.Fatal(err)
	}
}

// provider answers like Lucid: mints, reports active, revokes.
func (f *fixture) provider(ctx context.Context, method, path string, body any) ([]byte, error) {
	switch {
	case method == "POST" && path == Path:
		ask := body.(map[string]string)
		if ask["offer"] == "read" {
			return json.Marshal(map[string]any{"id": "read_" + ask["consumer"], "consent": "/connect/tkt"})
		}
		return json.Marshal(map[string]any{"id": "telemetry_" + ask["consumer"], "secrets": map[string]string{"LUCID_API_KEY": "key"}})
	case method == "GET":
		return []byte(`{"active":true,"last_activity_at":"2026-10-13T09:00:00Z"}`), nil
	case method == "DELETE":
		return nil, nil
	}
	return nil, errGone
}

func (f *fixture) connect(t *testing.T, provider string) (string, error) {
	t.Helper()
	return f.svc.Connect(context.Background(), f.consumer, ConnectRequest{Provider: provider}, "https://fog.test", "op")
}

func (f *fixture) list(t *testing.T) *List {
	t.Helper()
	list, err := f.svc.List(context.Background(), f.consumer)
	if err != nil {
		t.Fatal(err)
	}
	return list
}

func TestServiceConnectListDisconnect(t *testing.T) {
	f := newFixture(t)
	list := f.list(t)
	if len(list.Connected) != 0 || len(list.Available) != 1 {
		t.Fatalf("before: %+v", list)
	}
	if a := list.Available[0]; a.Provider != "lucid" || a.State != StateAvailable || !a.Installed || a.Consent ||
		!slices.Equal(a.Capabilities, []string{"Requests and errors in Lucid"}) {
		t.Fatalf("available: %+v", a)
	}

	consent, err := f.connect(t, "lucid")
	if err != nil || consent != "" {
		t.Fatalf("connect: %q %v", consent, err)
	}
	spec, _ := apps.LatestSpec(f.st, f.consumer.ID)
	if spec.Env["LUCID_URL"] != "http://lucid" || !slices.Contains(spec.Secrets, "LUCID_API_KEY") || f.specs.secrets["LUCID_API_KEY"] != "key" {
		t.Fatalf("spec: %v %v %v", spec.Env, spec.Secrets, f.specs.secrets)
	}

	list = f.list(t)
	if len(list.Available) != 0 || len(list.Connected) != 1 {
		t.Fatalf("after: %+v", list)
	}
	if c := list.Connected[0]; c.State != StateConnected || c.Problem != "" || c.Incomplete ||
		c.LastActivityAt != "2026-10-13T09:00:00Z" || c.ConnectedBy != "op" {
		t.Fatalf("connected: %+v", c)
	}

	if _, err := f.connect(t, "lucid"); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "already connected") {
		t.Fatalf("second connect: %v", err)
	}

	changed, err := f.svc.Disconnect(context.Background(), f.consumer, "lucid", false, "op")
	if err != nil || !changed {
		t.Fatalf("disconnect: %v %v", changed, err)
	}
	if !slices.Contains(f.caller.calls, "DELETE lucid"+Path+"/telemetry_newtab") {
		t.Fatalf("calls: %v", f.caller.calls)
	}
	spec, _ = apps.LatestSpec(f.st, f.consumer.ID)
	if _, set := spec.Env["LUCID_URL"]; set || slices.Contains(spec.Secrets, "LUCID_API_KEY") {
		t.Fatalf("spec after disconnect: %v %v", spec.Env, spec.Secrets)
	}
	if _, err := f.svc.Disconnect(context.Background(), f.consumer, "lucid", false, "op"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("disconnect again: %v", err)
	}
}

// The tab lists what the consumer wants, one entry per app: never the app
// itself, and never another app's offer it doesn't want.
func TestServiceListsWhatTheAppWants(t *testing.T) {
	f := newFixture(t)
	hub := f.mk("hub", apps.AppSpec{Source: apps.Source{Type: "image", Image: "hub"},
		Connection: &apps.Connection{Port: 3000, Offers: []apps.Offer{{ID: "calendar", Title: "Calendar", Secrets: []string{"HUB_TOKEN"}}}}})
	f.st.SetAppCurrentRev(hub.ID, 1)
	f.st.SetAppDesiredState(hub.ID, "running")
	f.wants(t,
		apps.Want{Offer: "newtab/self"},
		apps.Want{Offer: "lucid/telemetry"},
		apps.Want{Offer: "lucid/read"},
		apps.Want{Offer: "stream/library"},
	)
	got := f.list(t).Available
	var order []string
	for _, e := range got {
		order = append(order, e.Provider)
	}
	if !slices.Equal(order, []string{"lucid", "stream"}) {
		t.Fatalf("entries: %v", order)
	}
	if !got[0].Consent || !slices.Equal(got[0].Capabilities, []string{"Requests and errors in Lucid", "Reads Lucid's charts."}) {
		t.Fatalf("lucid: %+v", got[0])
	}
	if got[1].Installed || got[1].Problem != "Install stream, then connect it" {
		t.Fatalf("stream: %+v", got[1])
	}

	// A want's own capabilities say what this app gets, in place of the offers'.
	f.wants(t, apps.Want{Offer: "lucid/telemetry", Capabilities: []string{"Your traffic on the start page"}})
	if got := f.list(t).Available[0].Capabilities; !slices.Equal(got, []string{"Your traffic on the start page"}) {
		t.Fatalf("want capabilities: %v", got)
	}

	// An offer the provider doesn't make yet: listed, not connectable.
	f.wants(t, apps.Want{Offer: "lucid/dashboards"})
	if got := f.list(t).Available[0]; !strings.Contains(got.Problem, "update lucid") {
		t.Fatalf("missing offer: %+v", got)
	}
	if _, err := f.connect(t, "lucid"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("connect missing offer: %v", err)
	}
}

// Connecting an app the consumer wants two offers of mints both and sets
// them in one revision; a want added later leaves the entry incomplete
// until connected again, which adds only what is missing.
func TestServiceConnectsEveryOfferOfAnApp(t *testing.T) {
	f := newFixture(t)
	f.svc.Store.SetAppIntegrations(f.lucidID, (&apps.Declared{Offers: []apps.Offer{{
		ID: "ingest", Title: "Ingest", Env: map[string]string{"LUCID_INGEST": "{{url}}/i"}, Secrets: []string{"LUCID_API_KEY"},
	}}}).JSON())
	if _, err := f.connect(t, "lucid"); err != nil {
		t.Fatal(err)
	}
	f.wants(t, apps.Want{Offer: "lucid/telemetry"}, apps.Want{Offer: "lucid/ingest"})
	if c := f.list(t).Connected[0]; !c.Incomplete {
		t.Fatalf("a new want: %+v", c)
	}
	before, _ := f.st.LatestRevision(f.consumer.ID)
	f.caller.calls = nil
	if _, err := f.connect(t, "lucid"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.caller.calls, []string{"POST lucid" + Path}) {
		t.Fatalf("only the missing offer is minted: %v", f.caller.calls)
	}
	if after, _ := f.st.LatestRevision(f.consumer.ID); after.Rev != before.Rev+1 {
		t.Fatalf("one revision: %d → %d", before.Rev, after.Rev)
	}
	if c := f.list(t).Connected[0]; c.Incomplete {
		t.Fatalf("complete: %+v", c)
	}
	rows, _ := f.st.ListIntegrations(f.consumer.ID)
	if len(rows) != 2 {
		t.Fatalf("rows: %+v", rows)
	}
	f.caller.calls = nil
	if _, err := f.svc.Disconnect(context.Background(), f.consumer, "lucid", false, "op"); err != nil {
		t.Fatal(err)
	}
	if len(f.caller.calls) != 2 {
		t.Fatalf("both revoked: %v", f.caller.calls)
	}
}

// An app want hands the consumer the provider's connection: connecting
// adds it to uses (and wires its folders), and disconnecting takes it out.
func TestServiceAppWantGrantsUses(t *testing.T) {
	f := newFixture(t)
	sab := f.mk("sabnzbd", apps.AppSpec{Source: apps.Source{Type: "image", Image: "sab"},
		Connection: &apps.Connection{Port: 8080, Secret: "SABNZBD_API_KEY"}})
	f.st.SetAppCurrentRev(sab.ID, 1)
	f.st.SetAppDesiredState(sab.ID, "running")
	plain := f.mk("plain", apps.AppSpec{Source: apps.Source{Type: "image", Image: "plain"}})
	f.st.SetAppCurrentRev(plain.ID, 1)
	f.st.SetAppDesiredState(plain.ID, "running")
	wired := ""
	f.svc.Wire = func(name string, spec *apps.AppSpec) { wired = name }
	f.wants(t, apps.Want{App: "sabnzbd", Capabilities: []string{"Sends downloads to SABnzbd"}}, apps.Want{App: "plain"})

	if _, err := f.connect(t, "plain"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an app with no connection: %v", err)
	}
	f.caller.calls = nil
	if _, err := f.connect(t, "sabnzbd"); err != nil {
		t.Fatal(err)
	}
	spec, _ := apps.LatestSpec(f.st, f.consumer.ID)
	if !slices.Equal(spec.Uses, []string{"sabnzbd"}) || wired != "newtab" || len(f.caller.calls) != 0 {
		t.Fatalf("uses %v wired %q calls %v", spec.Uses, wired, f.caller.calls)
	}
	list := f.list(t)
	if len(list.Connected) != 1 || list.Connected[0].Provider != "sabnzbd" || list.Connected[0].Problem != "" ||
		!slices.Equal(list.Connected[0].Capabilities, []string{"Sends downloads to SABnzbd"}) {
		t.Fatalf("connected: %+v", list.Connected)
	}
	if changed, err := f.svc.Disconnect(context.Background(), f.consumer, "sabnzbd", false, "op"); err != nil || !changed {
		t.Fatalf("disconnect: %v %v", changed, err)
	}
	if spec, _ := apps.LatestSpec(f.st, f.consumer.ID); len(spec.Uses) != 0 {
		t.Fatalf("uses after: %v", spec.Uses)
	}
}

// Each refusal carries the kind its handler maps to a status, and the
// words the operator reads.
func TestServiceRefusalKinds(t *testing.T) {
	f := newFixture(t)
	if _, err := f.connect(t, "stream"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("not installed: %v", err)
	}
	if _, err := f.connect(t, "newtab"); !errors.Is(err, ErrConflict) {
		t.Fatalf("itself: %v", err)
	}
	f.wants(t)
	if _, err := f.connect(t, "lucid"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("not wanted: %v", err)
	}
	f.wants(t, apps.Want{Offer: "lucid/telemetry"})

	f.caller.fn = func(context.Context, string, string, any) ([]byte, error) {
		return nil, errors.New("Lucid has no household yet")
	}
	_, err := f.connect(t, "lucid")
	if !errors.Is(err, ErrProvider) || err.Error() != "lucid refused: Lucid has no household yet" {
		t.Fatalf("refused: %v", err)
	}
	f.caller.fn = func(context.Context, string, string, any) ([]byte, error) {
		return nil, fmt.Errorf("%w: no container", appcall.ErrUnreachable)
	}
	if _, err := f.connect(t, "lucid"); !errors.Is(err, ErrProvider) || !strings.HasPrefix(err.Error(), "Can't reach lucid") {
		t.Fatalf("unreachable: %v", err)
	}

	f.st.SetAppDesiredState(f.lucidID, "stopped")
	if _, err := f.connect(t, "lucid"); !errors.Is(err, ErrConflict) {
		t.Fatalf("stopped: %v", err)
	}

	f.svc.Specs = nil
	if _, err := f.connect(t, "lucid"); err == nil || errors.As(err, new(*Error)) {
		t.Fatalf("no engine is an internal error, not a refusal: %v", err)
	}
}

// A consent offer waits for the person; declining forgets it and revokes
// the request at the provider, approving claims and applies it.
func TestServiceConsentReturn(t *testing.T) {
	f := newFixture(t)
	f.wants(t, apps.Want{Offer: "lucid/read"})
	consent, err := f.connect(t, "lucid")
	if err != nil || consent != "https://lucid.home.test/connect/tkt" {
		t.Fatalf("ask: %q %v", consent, err)
	}
	if c := f.list(t).Connected; len(c) != 1 || c[0].State != StatePending {
		t.Fatalf("pending: %+v", c)
	}
	var state string
	f.caller.fn = func(ctx context.Context, method, path string, body any) ([]byte, error) {
		if method == "POST" && path == Path {
			u, _ := url.Parse(body.(map[string]string)["return_to"])
			state = u.Query().Get("state")
		}
		if strings.HasSuffix(path, "/claim") {
			return []byte(`{"secrets":{"LUCID_READ_TOKEN":"tok"}}`), nil
		}
		return f.provider(ctx, method, path, body)
	}
	// Cancel the first and ask again so the test holds the state.
	if changed, err := f.svc.Disconnect(context.Background(), f.consumer, "lucid", false, "op"); err != nil || changed {
		t.Fatalf("cancel: %v %v", changed, err)
	}
	if _, err := f.connect(t, "lucid"); err != nil || state == "" {
		t.Fatalf("ask again: %v %q", err, state)
	}

	if got := f.svc.Return(context.Background(), "not-a-state", true, "op"); got.Consumer != nil {
		t.Fatalf("unknown state: %+v", got)
	}
	got := f.svc.Return(context.Background(), state, true, "op")
	if got.Consumer == nil || got.Outcome != "connected=lucid" || !got.Connected {
		t.Fatalf("approved: %+v", got)
	}
	if f.specs.secrets["LUCID_READ_TOKEN"] != "tok" {
		t.Fatalf("secrets: %v", f.specs.secrets)
	}
	if again := f.svc.Return(context.Background(), state, true, "op"); again.Consumer != nil {
		t.Fatalf("a spent state lands nowhere: %+v", again)
	}
	if c := f.list(t).Connected; len(c) != 1 || c[0].State != StateConnected {
		t.Fatalf("connected: %+v", c)
	}

	if _, err := f.svc.Disconnect(context.Background(), f.consumer, "lucid", false, "op"); err != nil {
		t.Fatal(err)
	}
	f.connect(t, "lucid")
	f.caller.calls = nil
	got = f.svc.Return(context.Background(), state, false, "op")
	if got.Outcome != "declined=lucid" || got.Connected {
		t.Fatalf("declined: %+v", got)
	}
	if !slices.Contains(f.caller.calls, "DELETE lucid"+Path+"/read_newtab") {
		t.Fatalf("declined must revoke: %v", f.caller.calls)
	}
	if left, _ := f.st.ListIntegrations(f.consumer.ID); len(left) != 0 {
		t.Fatalf("declined leaves nothing: %+v", left)
	}
}

// A revoke runs under its caller's context — the request, or the server's
// background context — so neither a gone client nor Shutdown leaves it
// holding on for its full fifteen seconds.
func TestServiceRevokeUsesItsContext(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	f.caller.fn = func(c context.Context, method, path string, body any) ([]byte, error) {
		called = true
		if c.Err() == nil {
			t.Error("revoke ran under a context its caller can't cancel")
		}
		return nil, c.Err()
	}
	f.svc.revoke(ctx, &store.App{Name: "lucid"}, "remote-1")
	if !called {
		t.Fatal("the provider was never asked")
	}
}

// Refusal reads the provider's words, and a 404 to anything but a mint as
// a credential already gone.
func TestRefusal(t *testing.T) {
	resp := func(method string, status int) *http.Response {
		return &http.Response{StatusCode: status, Status: fmt.Sprintf("%d %s", status, http.StatusText(status)),
			Request: &http.Request{Method: method}}
	}
	if err := Refusal(resp("DELETE", 404), nil); !errors.Is(err, errGone) {
		t.Fatalf("delete 404: %v", err)
	}
	if err := Refusal(resp("POST", 404), nil); errors.Is(err, errGone) || err.Error() != "answered 404 Not Found" {
		t.Fatalf("post 404: %v", err)
	}
	if err := Refusal(resp("POST", 422), []byte(`{"errors":["a","b"]}`)); err.Error() != "a; b" {
		t.Fatalf("words: %v", err)
	}
}

// The attack: a provider's offer names the consumer's _fog_api_token (or a
// variable its runtime loads) as a secret it mints. Stored before names were
// checked, the declaration reaches apply; fog must refuse rather than write
// the provider's value where fog keeps the consumer's FOG_API_TOKEN — the
// provider would then be the consumer.
func TestApplyRefusesFogsOwnAndInjectableNames(t *testing.T) {
	for _, name := range []string{"_fog_api_token", "_deploy_key", "LD_PRELOAD", "NODE_OPTIONS", "http_proxy", "FOG_API_URL", "OIDC_ISSUER"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.svc.Store.SetAppIntegrations(f.lucidID, (&apps.Declared{Offers: []apps.Offer{{
				ID: "evil", Title: "Evil", Secrets: []string{name},
			}}}).JSON())
			f.caller.fn = func(ctx context.Context, method, path string, body any) ([]byte, error) {
				if method == "POST" {
					return json.Marshal(map[string]any{"id": "x", "secrets": map[string]string{name: "attacker", "LUCID_API_KEY": "key"}})
				}
				return f.provider(ctx, method, path, body)
			}
			f.wants(t, apps.Want{Offer: "lucid/evil"})
			if _, err := f.connect(t, "lucid"); err == nil {
				t.Fatal("connected an offer that sets " + name)
			}
			if v, ok := f.specs.secrets[name]; ok {
				t.Fatalf("%s was written: %q", name, v)
			}
		})
	}
}
