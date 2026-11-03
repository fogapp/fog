package apps

import (
	"path/filepath"
	"slices"
	"testing"

	"gitea.lan/matthew/fog/internal/store"
)

func directoryEngine(t *testing.T) (*Engine, *store.FedPeer) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	peer, err := st.CreatePairedPeer("01DIRPEER", "dave", "https://x", "k")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetFedCapability(peer.ID, store.CapAppsDirectory, store.DirGrant, "{}"); err != nil {
		t.Fatal(err)
	}
	return &Engine{Store: st}, peer
}

func addApp(t *testing.T, e *Engine, name, spec string, contracts []string) {
	t.Helper()
	app, err := e.Store.CreateApp(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Store.CreateRevision(app.ID, spec, "", "test"); err != nil {
		t.Fatal(err)
	}
	if contracts != nil {
		if err := e.Store.SetAppContracts(app.ID, contracts); err != nil {
			t.Fatal(err)
		}
	}
}

// Without a base domain the directory still carries what it can: apps with
// fully-qualified route hosts keep their URL, contracted apps appear with no
// URL, and apps with neither are omitted.
func TestDirectoryWithoutBaseDomain(t *testing.T) {
	e, peer := directoryEngine(t)
	addApp(t, e, "hub", `{"routes":[{"port":3000}]}`, []string{"fog-connect/1"})
	addApp(t, e, "feed", `{"routes":[{"host":"feed.example.org","port":80}]}`, nil)
	addApp(t, e, "manor", `{"routes":[{"port":80}]}`, nil)

	got := map[string]string{}
	for _, d := range e.DirectoryFor(peer) {
		got[d.App] = d.URL
	}
	if len(got) != 2 {
		t.Fatalf("directory: %+v", got)
	}
	if url, ok := got["hub"]; !ok || url != "" {
		t.Fatalf("contracted hub should be advertised without a URL: %+v", got)
	}
	if got["feed"] != "https://feed.example.org" {
		t.Fatalf("FQDN route should resolve without a base domain: %+v", got)
	}
}

// With a base domain, implicit hosts resolve as <app>.<domain>.
func TestDirectoryWithBaseDomain(t *testing.T) {
	e, peer := directoryEngine(t)
	e.Store.SetSetting(store.SettingBaseDomain, "impeca.io")
	addApp(t, e, "hub", `{"routes":[{"port":3000}]}`, []string{"fog-connect/1"})
	dir := e.DirectoryFor(peer)
	if len(dir) != 1 || dir[0].URL != "https://hub.impeca.io" || len(dir[0].Contracts) != 1 {
		t.Fatalf("directory: %+v", dir)
	}
}

// Finding: the apps.directory grant's scope is exactly its config, in both
// shapes the peer settings write. {} publishes every app; {"apps": [...]}
// publishes those named and no others, so unticking the last app on the
// Selected… list publishes none — it used to publish every app. A config
// that can't be read publishes nothing rather than everything.
func TestDirectoryScopeIsExactlyTheGrant(t *testing.T) {
	e, peer := directoryEngine(t)
	e.Store.SetSetting(store.SettingBaseDomain, "impeca.io")
	addApp(t, e, "hub", `{"routes":[{"port":3000}]}`, nil)
	addApp(t, e, "feed", `{"routes":[{"port":3000}]}`, nil)
	names := func(config string) []string {
		t.Helper()
		if err := e.Store.SetFedCapability(peer.ID, store.CapAppsDirectory, store.DirGrant, config); err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, d := range e.DirectoryFor(peer) {
			out = append(out, d.App)
		}
		slices.Sort(out)
		return out
	}
	for config, want := range map[string][]string{
		`{}`:                      {"feed", "hub"},
		`{"apps":["feed"]}`:       {"feed"},
		`{"apps":["Feed","hub"]}`: {"feed", "hub"},
		`{"apps":[]}`:             nil,
		`{"apps":"all"}`:          nil,
		`{"visible":true}`:        nil,
		`not json`:                nil,
	} {
		if got := names(config); !slices.Equal(got, want) {
			t.Errorf("grant %s publishes %v, want %v", config, got, want)
		}
	}
	if err := e.Store.RemoveFedCapability(peer.ID, store.CapAppsDirectory, store.DirGrant); err != nil {
		t.Fatal(err)
	}
	if d := e.DirectoryFor(peer); d != nil {
		t.Fatalf("no grant still published %+v", d)
	}
}
