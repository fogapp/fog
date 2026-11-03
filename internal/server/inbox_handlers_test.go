package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestInboxSummary(t *testing.T) {
	srv, h := newTestServer(t)
	c := setup(t, h)
	seedIssue(t, srv, "warning")
	srv.store.CreateNotice("backup-stale", "hub", "hub has no recent backup", "")

	if w := do(t, h, "GET", "/api/inbox/summary", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: %d", w.Code)
	}
	w := do(t, h, "GET", "/api/inbox/summary", "", withCookie(c))
	if w.Code != http.StatusOK {
		t.Fatalf("summary: %d %s", w.Code, w.Body)
	}
	var got struct {
		Errors  int            `json:"errors"`
		Issues  map[string]int `json:"issues"`
		Notices int            `json:"notices"`
		Fog     struct {
			Available bool `json:"available"`
		} `json:"fogUpdate"`
	}
	json.Unmarshal(w.Body.Bytes(), &got)
	if got.Errors != 0 || got.Issues["warning"] != 1 || got.Issues["critical"] != 0 || got.Notices != 1 || got.Fog.Available {
		t.Fatalf("summary: %+v", got)
	}
	// The summary was once a map, which encodes keys sorted; the struct
	// keeps that byte for byte.
	want := `{"errors":0,"feedback":{"fromPeers":0,"open":0},"fogUpdate":{"available":false},"issues":{"critical":0,"info":0,"warning":1},"notices":1}`
	if body := strings.TrimSpace(w.Body.String()); body != want {
		t.Errorf("summary body\n got %s\nwant %s", body, want)
	}
}
