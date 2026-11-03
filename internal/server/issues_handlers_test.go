package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"gitea.lan/matthew/fog/internal/store"
)

func seedIssue(t *testing.T, s *Server, severity string) store.Issue {
	t.Helper()
	res, err := s.store.SyncIssues("host-env", []int64{0}, []store.IssueFinding{{
		Kind: "host-env", Subject: "datadir", Severity: severity, Title: "data dir not writable",
	}})
	if err != nil || len(res.New) != 1 {
		t.Fatalf("seed: %v %+v", err, res)
	}
	return res.New[0]
}

func TestIssuesEndpoints(t *testing.T) {
	srv, h := newTestServer(t)
	c := setup(t, h)
	issue := seedIssue(t, srv, "critical")

	// Unauthenticated list is refused.
	if w := do(t, h, "GET", "/api/issues", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated list: %d", w.Code)
	}

	w := do(t, h, "GET", "/api/issues", "", withCookie(c))
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}
	var got struct {
		Issues []issueView `json:"issues"`
	}
	json.Unmarshal(w.Body.Bytes(), &got)
	if len(got.Issues) != 1 || got.Issues[0].Category != "host" {
		t.Fatalf("want one categorized issue, got %+v", got.Issues)
	}

	w = do(t, h, "GET", "/api/issues/summary", "", withCookie(c))
	var sum map[string]any
	json.Unmarshal(w.Body.Bytes(), &sum)
	if sum["critical"] != float64(1) {
		t.Fatalf("summary: %v", sum)
	}

	// Ack, then it leaves the open filter and the summary.
	if w := do(t, h, "POST", "/api/issues/1/ack", "", withCookie(c)); w.Code != http.StatusOK {
		t.Fatalf("ack: %d %s", w.Code, w.Body)
	}
	w = do(t, h, "GET", "/api/issues?status=open", "", withCookie(c))
	json.Unmarshal(w.Body.Bytes(), &got)
	if len(got.Issues) != 0 {
		t.Fatalf("acked issue still listed as open: %+v", got.Issues)
	}
	if w := do(t, h, "POST", "/api/issues/1/ack", "", withCookie(c)); w.Code != http.StatusBadRequest {
		t.Fatalf("double ack should 400, got %d", w.Code)
	}
	if w := do(t, h, "POST", "/api/issues/1/unack", "", withCookie(c)); w.Code != http.StatusOK {
		t.Fatalf("unack: %d %s", w.Code, w.Body)
	}

	// No scanner wired (tests): manual scan refuses rather than pretending.
	if w := do(t, h, "POST", "/api/issues/scan", "", withCookie(c)); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("scan without scanner should 503, got %d", w.Code)
	}

	_ = issue
}
