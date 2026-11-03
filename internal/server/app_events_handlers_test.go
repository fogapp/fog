package server

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"gitea.lan/matthew/fog/internal/apps"
)

// withSpec records spec as the app's latest revision.
func withSpec(t *testing.T, s *Server, appID int64, spec apps.AppSpec) {
	t.Helper()
	raw, _ := json.Marshal(spec)
	if _, err := s.store.CreateRevision(appID, string(raw), "test", "test"); err != nil {
		t.Fatal(err)
	}
}

func TestAppEventsPublishAndRead(t *testing.T) {
	s, h := newTestServer(t)
	lucidID, lucid := appWithToken(t, s, "lucid")
	withSpec(t, s, lucidID, apps.AppSpec{})
	hudID, hud := appWithToken(t, s, "hud")
	withSpec(t, s, hudID, apps.AppSpec{Uses: []string{"events"}})

	// Starting from now: the cursor to resume from, no history.
	w := do(t, h, "GET", "/app/v1/events", "", withBearer(hud))
	var start struct{ Cursor int64 }
	json.Unmarshal(w.Body.Bytes(), &start)

	w = do(t, h, "POST", "/app/v1/events",
		`[{"id":"inc-1","source":"forged","type":"lucid.incident.opened","subject":"api p95","data":{"title":"API slow"}},
		  {"type":"lucid.incident.closed"}]`, withBearer(lucid))
	if w.Code != http.StatusAccepted {
		t.Fatalf("publish: %d %s", w.Code, w.Body)
	}
	// A single structured event is accepted as well as a batch.
	if w := do(t, h, "POST", "/app/v1/events", `{"id":"inc-1","type":"lucid.incident.opened"}`, withBearer(lucid)); w.Code != http.StatusAccepted || !strings.Contains(w.Body.String(), `"new":0`) {
		t.Fatalf("retry: %d %s", w.Code, w.Body)
	}

	w = do(t, h, "GET", "/app/v1/events?after="+strconv.FormatInt(start.Cursor, 10), "", withBearer(hud))
	if w.Code != http.StatusOK {
		t.Fatalf("read: %d %s", w.Code, w.Body)
	}
	var page struct {
		Events []struct {
			Source, Type, Subject string
			Data                  map[string]any
		}
	}
	json.Unmarshal(w.Body.Bytes(), &page)
	if len(page.Events) != 2 || page.Events[0].Source != "lucid" || page.Events[0].Data["title"] != "API slow" {
		t.Fatalf("page = %s", w.Body)
	}

	// Without the grant, an app can publish but not read.
	if w := do(t, h, "GET", "/app/v1/events?after=0", "", withBearer(lucid)); w.Code != http.StatusForbidden {
		t.Fatalf("ungranted read: %d", w.Code)
	}
	if w := do(t, h, "POST", "/app/v1/events", `{"type":"Bad Type"}`, withBearer(lucid)); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid publish: %d", w.Code)
	}
	if w := do(t, h, "GET", "/app/v1/events", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", w.Code)
	}
}

func TestAppConnections(t *testing.T) {
	s, h := newTestServer(t)
	sabID, _ := appWithToken(t, s, "sabnzbd")
	withSpec(t, s, sabID, apps.AppSpec{Connection: &apps.Connection{Port: 8080, Secret: "SABNZBD_API_KEY"}})
	if err := s.secrets().Put(sabID, "SABNZBD_API_KEY", "k3y"); err != nil {
		t.Fatal(err)
	}
	haID, _ := appWithToken(t, s, "homeassistant")
	withSpec(t, s, haID, apps.AppSpec{Connection: &apps.Connection{Port: 8123, Secret: "HASS_TOKEN"}})
	plainID, _ := appWithToken(t, s, "whisper")
	withSpec(t, s, plainID, apps.AppSpec{})
	hudID, hud := appWithToken(t, s, "hud")
	withSpec(t, s, hudID, apps.AppSpec{Uses: []string{"events", "sabnzbd", "homeassistant", "whisper", "lidarr"}})

	w := do(t, h, "GET", "/app/v1/connections", "", withBearer(hud))
	if w.Code != http.StatusOK {
		t.Fatalf("connections: %d %s", w.Code, w.Body)
	}
	var resp struct{ Connections []appConnection }
	json.Unmarshal(w.Body.Bytes(), &resp)
	want := []appConnection{
		{App: "sabnzbd", URL: "http://sabnzbd:8080", Credential: "k3y"},
		{App: "homeassistant", URL: "http://homeassistant:8123", Problem: "set the HASS_TOKEN secret on homeassistant (Config → Secrets)"},
		{App: "whisper", Problem: "offers no connection"},
		{App: "lidarr", Problem: "not installed"},
	}
	if len(resp.Connections) != len(want) {
		t.Fatalf("got %+v", resp.Connections)
	}
	for i := range want {
		got := resp.Connections[i]
		got.Paths, got.Unseen = nil, nil
		if !reflect.DeepEqual(got, want[i]) {
			t.Errorf("connection %d = %+v, want %+v", i, got, want[i])
		}
	}

	// An app that names nothing is handed nothing — not every connection.
	sabToken := "apptoken-sabnzbd"
	w = do(t, h, "GET", "/app/v1/connections", "", withBearer(sabToken))
	if !strings.Contains(w.Body.String(), `"connections":[]`) {
		t.Fatalf("ungranted: %s", w.Body)
	}
}

// A connection carries the path translations between the two apps' views
// of the folders they share, and names the shared folders the consumer
// cannot see — the rows a person used to type into Hook.
func TestAppConnectionPaths(t *testing.T) {
	s, h := newTestServer(t)
	sabID, _ := appWithToken(t, s, "sabnzbd")
	withSpec(t, s, sabID, apps.AppSpec{
		Volumes: []apps.Volume{
			{Name: "media", Mount: "/data", Host: "/srv/content/Downloads"},
			{Name: "extra", Mount: "/extra", Host: "/mnt/elsewhere"},
		},
		Connection: &apps.Connection{Port: 8080, Shares: []string{"media", "extra"}},
	})
	hookID, hook := appWithToken(t, s, "hook")
	withSpec(t, s, hookID, apps.AppSpec{
		Uses:    []string{"sabnzbd"},
		Volumes: []apps.Volume{{Name: "content", Mount: "/data", Host: "/srv/content"}},
	})
	w := do(t, h, "GET", "/app/v1/connections", "", withBearer(hook))
	var resp struct{ Connections []appConnection }
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Connections) != 1 {
		t.Fatalf("got %s", w.Body)
	}
	c := resp.Connections[0]
	if !reflect.DeepEqual(c.Paths, []apps.PathRow{{Remote: "/data", Local: "/data/Downloads"}}) {
		t.Errorf("paths = %+v", c.Paths)
	}
	if !reflect.DeepEqual(c.Unseen, []string{"extra"}) {
		t.Errorf("unseen = %+v", c.Unseen)
	}
}
