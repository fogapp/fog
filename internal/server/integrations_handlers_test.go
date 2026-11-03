package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/integrations"
	"gitea.lan/matthew/fog/internal/store"
)

// fakeProvider is Lucid's half of fog-integration/1, as fog sees it.
type fakeProvider struct {
	mu       sync.Mutex
	minted   map[string]bool // id → active
	revoked  []string
	auth     string
	consumer string
	refuse   string // POST answers 422 with this
	omit     bool   // POST answers without the secret
	returnTo string // where a consent offer sends the person back
	claimed  int
}

func (p *fakeProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.auth = r.Header.Get("Authorization")
	id, _ := strings.CutPrefix(r.URL.Path, integrations.Path+"/")
	switch {
	case r.Method == "POST" && r.URL.Path == integrations.Path:
		if p.refuse != "" {
			w.WriteHeader(http.StatusUnprocessableEntity)
			json.NewEncoder(w).Encode(map[string]any{"errors": []string{p.refuse}})
			return
		}
		var body struct {
			Consumer, Offer string
			ReturnTo        string `json:"return_to"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		p.consumer = body.Consumer
		if body.Offer == "read" {
			p.returnTo = body.ReturnTo
			p.minted["read_"+body.Consumer] = true
			w.WriteHeader(http.StatusAccepted)
			json.NewEncoder(w).Encode(map[string]any{"id": "read_" + body.Consumer, "consent": "/connect/tkt"})
			return
		}
		id := "telemetry_" + body.Consumer
		p.minted[id] = true
		secrets := map[string]string{"LUCID_API_KEY": "lucid_ing_" + body.Consumer}
		if p.omit {
			secrets = map[string]string{}
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"id": id, "secrets": secrets})
	case r.Method == "POST" && strings.HasSuffix(id, "/claim") && p.minted[strings.TrimSuffix(id, "/claim")]:
		p.claimed++
		json.NewEncoder(w).Encode(map[string]any{"secrets": map[string]string{"LUCID_READ_TOKEN": "lucid_read"}})
	case r.Method == "GET" && p.minted[id] || r.Method == "GET" && slices.Contains(p.revoked, id):
		json.NewEncoder(w).Encode(map[string]any{"id": id, "active": p.minted[id], "last_activity_at": "2026-10-13T09:00:00Z"})
	case r.Method == "DELETE" && p.minted[id]:
		p.minted[id] = false
		p.revoked = append(p.revoked, id)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{"errors": []string{"no such integration"}})
	}
}

type integrationFixture struct {
	s        *Server
	h        http.Handler
	cookie   *http.Cookie
	provider *fakeProvider
	newtabID int64
}

func newIntegrationFixture(t *testing.T) *integrationFixture {
	t.Helper()
	s, h := newTestServer(t)
	s.engine = &apps.Engine{Store: s.store, Vault: s.vault}
	f := &integrationFixture{s: s, h: h, cookie: setup(t, h), provider: &fakeProvider{minted: map[string]bool{}}}

	lucidID, _ := appWithToken(t, s, "lucid")
	withSpec(t, s, lucidID, apps.AppSpec{
		Source: apps.Source{Type: "image", Image: "lucid"},
		Routes: []apps.Route{{Port: 80}},
		Connection: &apps.Connection{Port: 80, Offers: []apps.Offer{{
			ID: "telemetry", Title: "Telemetry", Blurb: "Requests show up in Lucid.",
			Env: map[string]string{"LUCID_URL": "{{url}}"}, Secrets: []string{"LUCID_API_KEY"},
		}, {
			ID: "read", Title: "Read access", Consent: true,
			Env: map[string]string{"LUCID_READ_URL": "{{url}}"}, Secrets: []string{"LUCID_READ_TOKEN"},
		}}},
	})
	s.store.SetAppCurrentRev(lucidID, 1)
	s.store.SetAppDesiredState(lucidID, "running")

	f.newtabID, _ = appWithToken(t, s, "newtab")
	withSpec(t, s, f.newtabID, apps.AppSpec{
		Source: apps.Source{Type: "image", Image: "newtab"},
		Env:    map[string]string{"NEWTAB_HOST": "newtab.home", "LUCID_URL": "http://lucid"},
		Wants:  []apps.Want{{Offer: "lucid/telemetry"}},
	})

	srv := httptest.NewServer(f.provider)
	t.Cleanup(srv.Close)
	s.appGrantEndpointFn = func(ctx context.Context, app *store.App) (string, string, error) {
		return srv.URL, "fog-token-" + app.Name, nil
	}
	return f
}

func (f *integrationFixture) list(t *testing.T) integrations.List {
	t.Helper()
	w := do(t, f.h, "GET", "/api/apps/newtab/integrations", "", withCookie(f.cookie))
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}
	var out integrations.List
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func (f *integrationFixture) connect(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, f.h, "POST", "/api/apps/newtab/integrations", `{"provider":"lucid"}`, withCookie(f.cookie))
}

func (f *integrationFixture) disconnect(t *testing.T, query string) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, f.h, "DELETE", "/api/apps/newtab/integrations/lucid"+query, "", withCookie(f.cookie))
}

// Connecting lists the app, mints through the provider with its own
// FOG_API_TOKEN, and lands the env and the secret in a new revision.
func TestIntegrationConnectAndDisconnect(t *testing.T) {
	f := newIntegrationFixture(t)

	got := f.list(t)
	if len(got.Connected) != 0 || len(got.Available) != 1 {
		t.Fatalf("before: %+v", got)
	}
	a := got.Available[0]
	if a.Provider != "lucid" || a.Problem != "" || !slices.Equal(a.Capabilities, []string{"Requests show up in Lucid."}) {
		t.Fatalf("available: %+v", a)
	}
	if a.Replaces {
		t.Fatalf("LUCID_URL is already what the offer sets, so nothing is replaced: %+v", a)
	}

	if w := f.connect(t); w.Code != http.StatusCreated {
		t.Fatalf("connect: %d %s", w.Code, w.Body)
	}
	if f.provider.auth != "Bearer fog-token-lucid" || f.provider.consumer != "newtab" {
		t.Fatalf("mint call: auth %q consumer %q", f.provider.auth, f.provider.consumer)
	}
	spec := latestSpec(t, f.s, f.newtabID)
	if spec.Env["LUCID_URL"] != "http://lucid" || spec.Env["NEWTAB_HOST"] != "newtab.home" || !slices.Contains(spec.Secrets, "LUCID_API_KEY") {
		t.Fatalf("spec after connect: env %v secrets %v", spec.Env, spec.Secrets)
	}
	if v, err := f.s.secrets().Get(f.newtabID, "LUCID_API_KEY"); err != nil || v != "lucid_ing_newtab" {
		t.Fatalf("vault: %q %v", v, err)
	}

	got = f.list(t)
	if len(got.Available) != 0 || len(got.Connected) != 1 {
		t.Fatalf("after: %+v", got)
	}
	if c := got.Connected[0]; c.State != integrations.StateConnected || c.Problem != "" || c.LastActivityAt == "" {
		t.Fatalf("connected: %+v", c)
	}

	if w := f.connect(t); w.Code != http.StatusConflict {
		t.Fatalf("second connect: %d %s", w.Code, w.Body)
	}

	if w := f.disconnect(t, ""); w.Code != http.StatusOK {
		t.Fatalf("disconnect: %d %s", w.Code, w.Body)
	}
	if !slices.Equal(f.provider.revoked, []string{"telemetry_newtab"}) {
		t.Fatalf("revoked: %v", f.provider.revoked)
	}
	spec = latestSpec(t, f.s, f.newtabID)
	if _, set := spec.Env["LUCID_URL"]; set || slices.Contains(spec.Secrets, "LUCID_API_KEY") || spec.Env["NEWTAB_HOST"] == "" {
		t.Fatalf("spec after disconnect: env %v secrets %v", spec.Env, spec.Secrets)
	}
	if v, _ := f.s.secrets().Get(f.newtabID, "LUCID_API_KEY"); v != "" {
		t.Fatalf("vault still holds %q", v)
	}
	if got := f.list(t); len(got.Connected) != 0 || len(got.Available) != 1 {
		t.Fatalf("after disconnect: %+v", got)
	}
	if w := f.disconnect(t, ""); w.Code != http.StatusNotFound {
		t.Fatalf("disconnect again: %d %s", w.Code, w.Body)
	}
}

// The provider's refusal reaches the operator in its own words, and a mint
// that leaves out a declared secret is revoked rather than half-applied.
func TestIntegrationConnectRefusals(t *testing.T) {
	f := newIntegrationFixture(t)

	f.provider.refuse = "Lucid has no household yet — open it and finish setup first"
	w := f.connect(t)
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "finish setup") {
		t.Fatalf("refused: %d %s", w.Code, w.Body)
	}

	f.provider.refuse, f.provider.omit = "", true
	w = f.connect(t)
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "LUCID_API_KEY") {
		t.Fatalf("omitted: %d %s", w.Code, w.Body)
	}
	if !slices.Equal(f.provider.revoked, []string{"telemetry_newtab"}) {
		t.Fatalf("a half mint must be revoked: %v", f.provider.revoked)
	}
	if spec := latestSpec(t, f.s, f.newtabID); slices.Contains(spec.Secrets, "LUCID_API_KEY") {
		t.Fatal("nothing should land in the spec")
	}

	lucid, _ := f.s.store.AppByName("lucid")
	f.s.store.SetAppDesiredState(lucid.ID, "stopped")
	if got := f.list(t); got.Available[0].Problem != "lucid is not running" {
		t.Fatalf("stopped provider: %+v", got.Available)
	}
	if w := f.connect(t); w.Code != http.StatusConflict {
		t.Fatalf("connect to stopped: %d %s", w.Code, w.Body)
	}
	w = do(t, f.h, "POST", "/api/apps/newtab/integrations", `{"provider":"hub"}`, withCookie(f.cookie))
	if w.Code != http.StatusNotFound {
		t.Fatalf("not installed: %d %s", w.Code, w.Body)
	}
}

// A provider that can't revoke holds the disconnect until the operator
// says to go on; a credential the provider revoked itself is flagged.
func TestIntegrationDisconnectNeedsProviderOrForce(t *testing.T) {
	f := newIntegrationFixture(t)
	if w := f.connect(t); w.Code != http.StatusCreated {
		t.Fatalf("connect: %d %s", w.Code, w.Body)
	}

	// Revoked in Lucid's own Settings: the tab says so.
	f.provider.minted["telemetry_newtab"] = false
	f.provider.revoked = append(f.provider.revoked, "telemetry_newtab")
	if c := f.list(t).Connected[0]; !strings.Contains(c.Problem, "revoked") {
		t.Fatalf("revoked upstream: %+v", c)
	}

	lucid, _ := f.s.store.AppByName("lucid")
	f.s.store.SetAppDesiredState(lucid.ID, "stopped")
	if w := f.disconnect(t, ""); w.Code != http.StatusConflict {
		t.Fatalf("disconnect with provider stopped: %d %s", w.Code, w.Body)
	}
	if w := f.disconnect(t, "?force=1"); w.Code != http.StatusOK {
		t.Fatalf("forced: %d %s", w.Code, w.Body)
	}
	if spec := latestSpec(t, f.s, f.newtabID); slices.Contains(spec.Secrets, "LUCID_API_KEY") {
		t.Fatal("forced disconnect must still clean the spec")
	}
}

func TestOfferValidation(t *testing.T) {
	base := func(o apps.Offer) apps.AppSpec {
		return apps.AppSpec{Source: apps.Source{Type: "image", Image: "x"}, Connection: &apps.Connection{Port: 80, Offers: []apps.Offer{o}}}
	}
	ok := apps.Offer{ID: "telemetry", Title: "Telemetry", Env: map[string]string{"A_URL": "{{url}}"}, Secrets: []string{"A_KEY"}}
	if sp := base(ok); sp.Validate() != nil {
		t.Fatalf("valid offer refused: %v", sp.Validate())
	}
	for name, o := range map[string]apps.Offer{
		"bad id":    {ID: "Bad Id", Title: "x", Secrets: []string{"K"}},
		"no title":  {ID: "x", Secrets: []string{"K"}},
		"empty":     {ID: "x", Title: "x"},
		"bad name":  {ID: "x", Title: "x", Secrets: []string{"1K"}},
		"collision": {ID: "x", Title: "x", Env: map[string]string{"K": "v"}, Secrets: []string{"K"}},
	} {
		if sp := base(o); sp.Validate() == nil {
			t.Errorf("%s: accepted %+v", name, o)
		}
	}
	for _, w := range [][]apps.Want{
		{{Offer: "lucid"}},
		{{Offer: "lucid/telemetry", App: "lucid"}},
		{{App: "events"}},
		{{App: "sabnzbd", As: map[string]string{"A": "B"}}},
		{{App: "sabnzbd"}, {App: "sabnzbd"}},
		{{Offer: "lucid/telemetry"}, {Offer: "lucid/telemetry"}},
		{{Offer: "lucid/telemetry", As: map[string]string{"LUCID_API_KEY": "not a name"}}},
	} {
		if sp := (apps.AppSpec{Source: apps.Source{Type: "image", Image: "x"}, Wants: w}); sp.Validate() == nil {
			t.Errorf("accepted wants %+v", w)
		}
	}
	if (&apps.Connection{Port: 3000, Path: "/v1"}).ConnectionURL("lucid") != "http://lucid:3000/v1" {
		t.Error("connection URL with a port")
	}
}

// A value set by hand that connecting would change is flagged before the
// operator connects; a secret of the same name always is.
func TestIntegrationFlagsWhatItReplaces(t *testing.T) {
	f := newIntegrationFixture(t)
	withSpec(t, f.s, f.newtabID, apps.AppSpec{
		Source: apps.Source{Type: "image", Image: "newtab"},
		Env:    map[string]string{"LUCID_URL": "http://192.168.1.200:3004"},
		Wants:  []apps.Want{{Offer: "lucid/telemetry"}},
	})
	if got := f.list(t).Available[0]; !got.Replaces {
		t.Fatalf("replaces: %+v", got)
	}
}

// Wants rename what an offer sets to the names the app reads, and
// connecting sets them under those names.
func TestIntegrationWantsRename(t *testing.T) {
	f := newIntegrationFixture(t)
	withSpec(t, f.s, f.newtabID, apps.AppSpec{
		Source: apps.Source{Type: "image", Image: "newtab"},
		Wants:  []apps.Want{{Offer: "lucid/telemetry", As: map[string]string{"LUCID_API_KEY": "NEWTAB_INGEST_KEY"}}},
	})
	if w := f.connect(t); w.Code != http.StatusCreated {
		t.Fatalf("connect: %d %s", w.Code, w.Body)
	}
	spec := latestSpec(t, f.s, f.newtabID)
	if !slices.Contains(spec.Secrets, "NEWTAB_INGEST_KEY") || slices.Contains(spec.Secrets, "LUCID_API_KEY") {
		t.Fatalf("secrets: %v", spec.Secrets)
	}
	if v, _ := f.s.secrets().Get(f.newtabID, "NEWTAB_INGEST_KEY"); v != "lucid_ing_newtab" {
		t.Fatalf("renamed secret holds %q", v)
	}
	if w := f.disconnect(t, ""); w.Code != http.StatusOK {
		t.Fatalf("disconnect: %d %s", w.Code, w.Body)
	}
	if spec := latestSpec(t, f.s, f.newtabID); slices.Contains(spec.Secrets, "NEWTAB_INGEST_KEY") {
		t.Fatal("disconnect must remove the renamed secret")
	}
}

// A consent offer sends the operator to the provider's page; coming back
// approved, fog claims the credential once and sets it, under the names the
// app reads. Declined, nothing is set.
func TestIntegrationConsent(t *testing.T) {
	f := newIntegrationFixture(t)
	f.s.store.SetSetting(store.SettingBaseDomain, "home.test")
	withSpec(t, f.s, f.newtabID, apps.AppSpec{
		Source: apps.Source{Type: "image", Image: "newtab"},
		Wants: []apps.Want{{Offer: "lucid/read", As: map[string]string{
			"LUCID_READ_URL": "NEWTAB_LUCID_URL", "LUCID_READ_TOKEN": "NEWTAB_LUCID_TOKEN"}}},
	})
	if a := f.list(t).Available[0]; !a.Consent {
		t.Fatalf("consent: %+v", a)
	}
	ask := func() string {
		t.Helper()
		w := do(t, f.h, "POST", "/api/apps/newtab/integrations", `{"provider":"lucid","deploy":true}`,
			withCookie(f.cookie), withOrigin("http://example.com"))
		if w.Code != http.StatusAccepted {
			t.Fatalf("ask: %d %s", w.Code, w.Body)
		}
		var resp struct{ ConsentURL string }
		json.Unmarshal(w.Body.Bytes(), &resp)
		if resp.ConsentURL != "https://lucid.home.test/connect/tkt" {
			t.Fatalf("consent url %q", resp.ConsentURL)
		}
		if !strings.HasPrefix(f.provider.returnTo, "http://example.com/api/integrations/return?state=") {
			t.Fatalf("return_to %q", f.provider.returnTo)
		}
		return strings.TrimPrefix(f.provider.returnTo, "http://example.com")
	}
	back := func(path string) string {
		t.Helper()
		w := do(t, f.h, "GET", path, "", withCookie(f.cookie))
		if w.Code != http.StatusSeeOther {
			t.Fatalf("return: %d %s", w.Code, w.Body)
		}
		return w.Header().Get("Location")
	}

	ret := ask()
	c := f.list(t).Connected
	if len(c) != 1 || c[0].State != integrations.StatePending || c[0].Problem != "Waiting for someone to approve this in lucid" {
		t.Fatalf("pending: %+v", c)
	}
	if w := f.connect(t); w.Code != http.StatusConflict {
		t.Fatalf("second ask while pending: %d", w.Code)
	}
	if spec := latestSpec(t, f.s, f.newtabID); len(spec.Secrets) != 0 {
		t.Fatal("nothing is set before approval")
	}

	if loc := back(ret + "&result=approved"); loc != "/apps/newtab/integrations?connected=lucid" {
		t.Fatalf("approved lands on %q", loc)
	}
	spec := latestSpec(t, f.s, f.newtabID)
	if spec.Env["NEWTAB_LUCID_URL"] != "http://lucid" || !slices.Equal(spec.Secrets, []string{"NEWTAB_LUCID_TOKEN"}) {
		t.Fatalf("spec: env %v secrets %v", spec.Env, spec.Secrets)
	}
	if v, _ := f.s.secrets().Get(f.newtabID, "NEWTAB_LUCID_TOKEN"); v != "lucid_read" {
		t.Fatalf("vault: %q", v)
	}
	if c := f.list(t).Connected; len(c) != 1 || c[0].Problem != "" || c[0].State != integrations.StateConnected {
		t.Fatalf("connected: %+v", c)
	}
	// The state is spent: coming back again does nothing.
	if loc := back(ret + "&result=approved"); loc != "/apps" || f.provider.claimed != 1 {
		t.Fatalf("replay: %q claimed %d", loc, f.provider.claimed)
	}

	// Disconnect, ask again, decline.
	if w := f.disconnect(t, ""); w.Code != http.StatusOK {
		t.Fatalf("disconnect: %d %s", w.Code, w.Body)
	}
	ret = ask()
	if loc := back(ret + "&result=denied"); loc != "/apps/newtab/integrations?declined=lucid" {
		t.Fatalf("declined lands on %q", loc)
	}
	if got := f.list(t); len(got.Connected) != 0 {
		t.Fatalf("declined leaves nothing: %+v", got.Connected)
	}
	if spec := latestSpec(t, f.s, f.newtabID); len(spec.Secrets) != 0 {
		t.Fatalf("declined sets nothing: %v", spec.Secrets)
	}

	// Pending, then cancelled: revoked at the provider, nothing set.
	ask()
	if w := f.disconnect(t, ""); w.Code != http.StatusOK {
		t.Fatalf("cancel: %d %s", w.Code, w.Body)
	}
	if got := f.list(t); len(got.Connected) != 0 {
		t.Fatalf("cancelled: %+v", got.Connected)
	}
}

// Without a domain the provider has no page anyone can open: refused, and
// the request revoked.
func TestIntegrationConsentNeedsDomain(t *testing.T) {
	f := newIntegrationFixture(t)
	withSpec(t, f.s, f.newtabID, apps.AppSpec{
		Source: apps.Source{Type: "image", Image: "newtab"},
		Wants:  []apps.Want{{Offer: "lucid/read"}},
	})
	w := f.connect(t)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "Settings › Network") {
		t.Fatalf("no domain: %d %s", w.Code, w.Body)
	}
	if !slices.Equal(f.provider.revoked, []string{"read_newtab"}) {
		t.Fatalf("revoked: %v", f.provider.revoked)
	}
}

// An installed app whose spec predates offers still offers what its repo's
// fog.yaml declared at its last deploy; the consumer's repo wants work the
// same way. A push reaches them, not only a fresh install.
func TestIntegrationDeclaredInRepo(t *testing.T) {
	f := newIntegrationFixture(t)
	lucid, _ := f.s.store.AppByName("lucid")
	withSpec(t, f.s, lucid.ID, apps.AppSpec{Source: apps.Source{Type: "git", Repo: "x"}, Routes: []apps.Route{{Port: 80}}})
	withSpec(t, f.s, f.newtabID, apps.AppSpec{Source: apps.Source{Type: "image", Image: "newtab"}})
	if got := f.list(t); len(got.Available)+len(got.Connected) != 0 {
		t.Fatalf("nothing declared yet: %+v", got)
	}

	decl := &apps.Declared{Offers: []apps.Offer{{ID: "telemetry", Title: "Telemetry", Capabilities: []string{"Errors in Lucid"},
		Env: map[string]string{"LUCID_URL": apps.URLTemplate}, Secrets: []string{"LUCID_API_KEY"}}}}
	f.s.store.SetAppIntegrations(lucid.ID, decl.JSON())
	if got := f.list(t); len(got.Available) != 0 {
		t.Fatalf("an offer newtab doesn't want isn't listed: %+v", got)
	}
	want := &apps.Declared{Wants: []apps.Want{{Offer: "lucid/telemetry", As: map[string]string{"LUCID_API_KEY": "NEWTAB_INGEST_KEY"}}}}
	f.s.store.SetAppIntegrations(f.newtabID, want.JSON())

	got := f.list(t).Available
	if len(got) != 1 || !slices.Equal(got[0].Capabilities, []string{"Errors in Lucid"}) {
		t.Fatalf("declared offer, wanted: %+v", got)
	}
	if w := f.connect(t); w.Code != http.StatusCreated {
		t.Fatalf("connect: %d %s", w.Code, w.Body)
	}
	spec := latestSpec(t, f.s, f.newtabID)
	if spec.Env["LUCID_URL"] != "http://lucid" || !slices.Contains(spec.Secrets, "NEWTAB_INGEST_KEY") {
		t.Fatalf("spec: env %v secrets %v", spec.Env, spec.Secrets)
	}
	if c := f.list(t).Connected; len(c) != 1 || c[0].Problem != "" {
		t.Fatalf("connected: %+v", c)
	}
}

// An app want connects through uses, and the folders the provider shares
// are mounted into the consumer as installing it would.
func TestIntegrationAppWantWires(t *testing.T) {
	f := newIntegrationFixture(t)
	streamID, _ := appWithToken(t, f.s, "stream")
	withSpec(t, f.s, streamID, apps.AppSpec{
		Source:     apps.Source{Type: "image", Image: "stream"},
		Volumes:    []apps.Volume{{Name: "media", Mount: "/media", Files: "Content/Media"}},
		Connection: &apps.Connection{Port: 3000, Shares: []string{"media"}},
	})
	f.s.store.SetAppCurrentRev(streamID, 1)
	f.s.store.SetAppDesiredState(streamID, "running")
	withSpec(t, f.s, f.newtabID, apps.AppSpec{
		Source: apps.Source{Type: "image", Image: "newtab"},
		Wants:  []apps.Want{{App: "stream"}},
	})
	w := do(t, f.h, "POST", "/api/apps/newtab/integrations", `{"provider":"stream"}`, withCookie(f.cookie))
	if w.Code != http.StatusCreated {
		t.Fatalf("connect: %d %s", w.Code, w.Body)
	}
	spec := latestSpec(t, f.s, f.newtabID)
	if !slices.Equal(spec.Uses, []string{"stream"}) || len(spec.Volumes) == 0 {
		t.Fatalf("uses %v volumes %+v", spec.Uses, spec.Volumes)
	}
	if c := f.list(t).Connected; len(c) != 1 || c[0].Provider != "stream" {
		t.Fatalf("connected: %+v", c)
	}
	w = do(t, f.h, "DELETE", "/api/apps/newtab/integrations/stream", "", withCookie(f.cookie))
	if w.Code != http.StatusOK || len(latestSpec(t, f.s, f.newtabID).Uses) != 0 {
		t.Fatalf("disconnect: %d %s", w.Code, w.Body)
	}
}
