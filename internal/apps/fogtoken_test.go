package apps

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"gitea.lan/matthew/fog/internal/dockerx"
	"gitea.lan/matthew/fog/internal/store"
	"gitea.lan/matthew/fog/internal/store/storetest"
	"gitea.lan/matthew/fog/internal/vault"
)

// AppEndpoints answers every app from one container listing, each with
// its address and token, and says why for an app it can't reach.
func TestAppEndpointsListsContainersOnce(t *testing.T) {
	var lists atomic.Int32
	var labels []string
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/containers/json") {
			http.NotFound(w, r)
			return
		}
		lists.Add(1)
		labels = append(labels, r.URL.Query().Get("filters"))
		on := func(app, ip, state string) dockerx.ContainerSummary {
			return dockerx.ContainerSummary{State: state, Labels: map[string]string{"fog.app": app},
				NetworkSettings: dockerx.SummaryNetworks{Networks: map[string]dockerx.NetworkEndpoint{NetworkName: {IPAddress: ip}}}}
		}
		json.NewEncoder(w).Encode([]dockerx.ContainerSummary{
			on("feed", "10.0.0.2", "running"), on("hub", "10.0.0.3", "running"), on("stopped", "10.0.0.4", "exited"),
		})
	})
	sockDir, err := os.MkdirTemp("", "fog")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go http.Serve(ln, api)
	t.Setenv("DOCKER_SOCK", sock)

	dir := t.TempDir()
	st, err := storetest.Open(filepath.Join(dir, "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	vlt, err := vault.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	var list []store.App
	for _, name := range []string{"feed", "hub", "stopped", "norev"} {
		a, err := st.CreateApp(name)
		if err != nil {
			t.Fatal(err)
		}
		if name != "norev" {
			if _, err := st.CreateRevision(a.ID, `{"source":{"type":"image","image":"x"},"routes":[{"port":3000}]}`, "", "test"); err != nil {
				t.Fatal(err)
			}
		}
		list = append(list, *a)
	}
	e := &Engine{Store: st, Vault: vlt, Docker: dockerx.New()}

	got := e.AppEndpoints(context.Background(), list)
	if lists.Load() != 1 {
		t.Fatalf("listed containers %d times, want once", lists.Load())
	}
	if got[0].Err != nil || got[0].Base != "http://10.0.0.2:3000" || got[0].Token == "" {
		t.Fatalf("feed: %+v", got[0])
	}
	if got[1].Err != nil || got[1].Base != "http://10.0.0.3:3000" || got[1].Token == got[0].Token {
		t.Fatalf("hub: %+v", got[1])
	}
	if got[2].Err == nil || got[3].Err == nil {
		t.Fatalf("a stopped app and one with no revision are unreachable: %+v %+v", got[2], got[3])
	}
	if tok, _ := e.EnsureFogAPIToken(list[0].ID); tok != got[0].Token {
		t.Fatalf("token %q is not the app's FOG_API_TOKEN %q", got[0].Token, tok)
	}

	// One app asks about that app alone.
	e.AppEndpoints(context.Background(), list[:1])
	if !strings.Contains(labels[1], "fog.app=feed") {
		t.Fatalf("single-app filter: %s", labels[1])
	}
}
