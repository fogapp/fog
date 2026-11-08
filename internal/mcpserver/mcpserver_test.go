// Tool behavior over a real store, driven through the same streamable-HTTP
// handler fogd mounts (stateless mode: a bare tools/call POST per request,
// no handshake). Auth lives in package server and is tested there.
package mcpserver

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/store"
	"gitea.lan/matthew/fog/internal/store/storetest"
)

func newTestHandler(t *testing.T) (*store.Store, http.Handler) {
	t.Helper()
	st, err := storetest.Open(filepath.Join(t.TempDir(), "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st, New(Deps{Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
}

// callResult is the slice of a JSON-RPC answer the assertions care about.
type callResult struct {
	Result struct {
		IsError           bool            `json:"isError"`
		StructuredContent json.RawMessage `json:"structuredContent"`
		Content           []struct {
			Text string `json:"text"`
		} `json:"content"`
	} `json:"result"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func call(t *testing.T, h http.Handler, tool string, args any) callResult {
	t.Helper()
	argJSON, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, tool, argJSON)
	r := httptest.NewRequest("POST", "/mcp", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("call %s: HTTP %d %s", tool, w.Code, w.Body)
	}
	var res callResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("call %s: bad JSON-RPC answer %q: %v", tool, w.Body, err)
	}
	if res.Error != nil {
		t.Fatalf("call %s: protocol error: %s", tool, res.Error.Message)
	}
	return res
}

func TestIssuesListEmpty(t *testing.T) {
	_, h := newTestHandler(t)
	res := call(t, h, "fog_issues_list", struct{}{})
	if res.Result.IsError {
		t.Fatalf("unexpected tool error: %+v", res.Result.Content)
	}
	var out struct {
		Issues []any `json:"issues"`
	}
	if err := json.Unmarshal(res.Result.StructuredContent, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Issues) != 0 {
		t.Errorf("fresh store: got %d issues, want 0", len(out.Issues))
	}
}

func TestIssueAckWritesAudit(t *testing.T) {
	st, h := newTestHandler(t)
	if _, err := st.SyncIssues("host-env", nil, []store.IssueFinding{{
		Kind: "host-env", Severity: "warning", Title: "swap is off", Subject: "swap",
	}}); err != nil {
		t.Fatal(err)
	}
	issues, err := st.ListIssues([]string{"open"}, 10)
	if err != nil || len(issues) != 1 {
		t.Fatalf("seed issue: %v (%d issues)", err, len(issues))
	}

	res := call(t, h, "fog_issue_ack", map[string]int64{"id": issues[0].ID})
	if res.Result.IsError {
		t.Fatalf("ack failed: %+v", res.Result.Content)
	}
	got, err := st.IssueByID(issues[0].ID)
	if err != nil || got.Status != "acked" {
		t.Fatalf("after ack: status=%q err=%v, want acked", got.Status, err)
	}
	entries, err := st.RecentAudit(5)
	if err != nil || len(entries) == 0 {
		t.Fatalf("audit: %v (%d entries)", err, len(entries))
	}
	if entries[0].Actor != "mcp" || entries[0].Action != "issue.ack" {
		t.Errorf("audit entry: actor=%q action=%q, want mcp/issue.ack", entries[0].Actor, entries[0].Action)
	}
}

func TestAppBackupSetPartialUpdate(t *testing.T) {
	st, err := storetest.Open(filepath.Join(t.TempDir(), "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := New(Deps{Store: st, Engine: &apps.Engine{Store: st, Log: log}, Log: log})

	app, err := st.CreateApp("feed")
	if err != nil {
		t.Fatal(err)
	}
	spec := `{"source":{"type":"git","repo":"ssh://gitea@host/m/feed.git"},"routes":[{"port":80}],"backup":{"enabled":true,"beforeUpdate":true}}`
	if _, err := st.CreateRevision(app.ID, spec, "", "test"); err != nil {
		t.Fatal(err)
	}

	res := call(t, h, "fog_app_backup_set", map[string]any{"name": "feed", "mode": "delegated", "timeout": "45m"})
	if res.Result.IsError {
		t.Fatalf("backup set failed: %+v", res.Result.Content)
	}
	got, err := apps.LatestSpec(st, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	b := got.Backup
	if b == nil || b.Mode != "delegated" || b.Timeout != "45m" {
		t.Fatalf("backup after set: %+v, want delegated/45m", b)
	}
	// Untouched fields survive the partial update.
	if !b.Enabled || b.BeforeUpdate == nil || !*b.BeforeUpdate {
		t.Errorf("enabled/beforeUpdate were clobbered: %+v", b)
	}
	// The rest of the spec rides along unchanged.
	if got.Source.Repo != "ssh://gitea@host/m/feed.git" || len(got.Routes) != 1 {
		t.Errorf("spec outside backup changed: %+v", got)
	}

	// An invalid mode is refused by spec validation, as a tool error.
	res = call(t, h, "fog_app_backup_set", map[string]any{"name": "feed", "mode": "junk"})
	if !res.Result.IsError {
		t.Fatal("invalid mode should be a tool error")
	}
}

func TestNilDepsFailCleanly(t *testing.T) {
	_, h := newTestHandler(t)
	// No engine wired: the tool must answer with a tool error, not a panic
	// or a protocol failure.
	res := call(t, h, "fog_apps_list", struct{}{})
	if !res.Result.IsError {
		t.Fatal("fog_apps_list without an engine should be a tool error")
	}
	res = call(t, h, "fog_issues_scan", struct{}{})
	if !res.Result.IsError {
		t.Fatal("fog_issues_scan without a scanner should be a tool error")
	}
}

// fog_app_get shows the source, never a password an ssh:// URL carries or
// a build arg.
func TestSourceViewRedactsCredentials(t *testing.T) {
	got := sourceView(apps.Source{Type: "git", Repo: "ssh://gitea:hunter2@host/m/feed.git", BuildArgs: map[string]string{"TOKEN": "abc"}})
	if strings.Contains(got.Repo, "hunter2") || got.BuildArgs != nil {
		t.Fatalf("source view leaked a credential: %+v", got)
	}
	if !strings.Contains(got.Repo, "gitea") || !strings.Contains(got.Repo, "host/m/feed.git") {
		t.Fatalf("source view lost the repo: %+v", got)
	}
	for _, repo := range []string{"ssh://gitea@host/m/feed.git", "gitea@host:m/feed.git", "/srv/git/feed.git"} {
		if got := sourceView(apps.Source{Repo: repo}); got.Repo != repo {
			t.Errorf("sourceView(%q).Repo = %q, want it unchanged", repo, got.Repo)
		}
	}
}
