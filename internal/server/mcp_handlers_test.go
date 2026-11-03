// The MCP boundary: /mcp answers to a named MCP token and nothing else —
// no session, no cookie, and deliberately no Origin guard (a bearer header
// can't be sent cross-site by a browser, so same-origin is not the
// credential here). Tokens are per client: revoking one must not touch the
// others, and what a client does must be attributed to its own name.
package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func withMCPBearer(token string) func(*http.Request) {
	return func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+token)
		// The streamable transport requires clients to accept both answer
		// shapes.
		r.Header.Set("Accept", "application/json, text/event-stream")
	}
}

// mcpCall is a minimal JSON-RPC tools/call body; the server runs stateless,
// so no initialize handshake or session header is needed per request.
const mcpListIssues = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fog_issues_list","arguments":{}}}`
const mcpAckIssue = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fog_issue_ack","arguments":{"id":1}}}`

type mcpTokenRow struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	CreatedAt  string `json:"createdAt"`
	LastUsedAt string `json:"lastUsedAt"`
	Token      string `json:"token"`
}

// createMCPToken mints a token through the API and returns the row, which
// carries the plaintext exactly once.
func createMCPToken(t *testing.T, h http.Handler, c *http.Cookie, name string) mcpTokenRow {
	t.Helper()
	w := do(t, h, "POST", "/api/settings/mcp-tokens", `{"name":"`+name+`"}`, withCookie(c))
	if w.Code != http.StatusOK {
		t.Fatalf("create token %q: %d %s", name, w.Code, w.Body)
	}
	var row mcpTokenRow
	json.Unmarshal(w.Body.Bytes(), &row)
	if row.Token == "" || row.ID == 0 || row.Name != name || row.CreatedAt == "" {
		t.Fatalf("create token %q: incomplete response %s", name, w.Body)
	}
	return row
}

func listMCPTokens(t *testing.T, h http.Handler, c *http.Cookie) []mcpTokenRow {
	t.Helper()
	w := do(t, h, "GET", "/api/settings/mcp-tokens", "", withCookie(c))
	if w.Code != http.StatusOK {
		t.Fatalf("list tokens: %d %s", w.Code, w.Body)
	}
	var body struct {
		Tokens []mcpTokenRow `json:"tokens"`
	}
	json.Unmarshal(w.Body.Bytes(), &body)
	return body.Tokens
}

func withRemoteAddr(addr string) func(*http.Request) {
	return func(r *http.Request) { r.RemoteAddr = addr }
}

// The pre-auth rate limit is keyed by source IP: an unauthenticated
// brute-forcer must starve only itself, never the legitimate clients — and
// authenticated clients on different IPs must not share a bucket.
func TestMCPRateLimitIsPerSourceIP(t *testing.T) {
	_, h := newTestServer(t)
	c := setup(t, h)
	tok := createMCPToken(t, h, c, "claude-code")

	limited := false
	for i := 0; i < 200 && !limited; i++ {
		w := do(t, h, "POST", "/mcp", mcpListIssues, withMCPBearer("wrong-token"), withRemoteAddr("198.51.100.7:40000"))
		switch w.Code {
		case http.StatusTooManyRequests:
			limited = true
		case http.StatusUnauthorized:
			// still burning through the budget
		default:
			t.Fatalf("brute-force attempt %d: got %d %s", i, w.Code, w.Body)
		}
	}
	if !limited {
		t.Fatal("an unauthenticated brute-force loop was never rate limited")
	}

	// The attacker's exhausted budget belongs to the attacker's IP alone.
	w := do(t, h, "POST", "/mcp", mcpListIssues, withMCPBearer(tok.Token), withRemoteAddr("192.0.2.10:50000"))
	if w.Code != http.StatusOK {
		t.Errorf("legitimate client on another IP: got %d %s, want 200", w.Code, w.Body)
	}
}

func TestMCPRequiresToken(t *testing.T) {
	_, h := newTestServer(t)
	setup(t, h)

	// No Authorization header at all.
	if w := do(t, h, "POST", "/mcp", mcpListIssues); w.Code != http.StatusUnauthorized {
		t.Errorf("no bearer: got %d, want 401", w.Code)
	}
	// A token when none is configured.
	if w := do(t, h, "POST", "/mcp", mcpListIssues, withMCPBearer("nope")); w.Code != http.StatusUnauthorized {
		t.Errorf("unconfigured token: got %d, want 401", w.Code)
	}
}

// Two clients, two tokens: each opens /mcp, and revoking one leaves the
// other working — the whole point of naming them.
func TestMCPTokensAreIndependent(t *testing.T) {
	_, h := newTestServer(t)
	c := setup(t, h)

	if tokens := listMCPTokens(t, h, c); len(tokens) != 0 {
		t.Fatalf("fresh box: got %d tokens, want 0", len(tokens))
	}

	code := createMCPToken(t, h, c, "claude-code")
	claw := createMCPToken(t, h, c, "openclaw")
	if code.Token == claw.Token {
		t.Fatal("two tokens minted the same secret")
	}

	tokens := listMCPTokens(t, h, c)
	if len(tokens) != 2 {
		t.Fatalf("after two creates: got %d tokens, want 2", len(tokens))
	}
	// The plaintext exists only in the create response — the list is digests
	// and metadata, or "shown once" would be a lie.
	listed := do(t, h, "GET", "/api/settings/mcp-tokens", "", withCookie(c)).Body.String()
	if strings.Contains(listed, code.Token) || strings.Contains(listed, claw.Token) {
		t.Errorf("list leaked a plaintext token: %s", listed)
	}

	// Both open the door; a near-miss doesn't.
	for _, tok := range []mcpTokenRow{code, claw} {
		if w := do(t, h, "POST", "/mcp", mcpListIssues, withMCPBearer(tok.Token)); w.Code != http.StatusOK {
			t.Errorf("%s bearer: got %d %s, want 200", tok.Name, w.Code, w.Body)
		}
	}
	if w := do(t, h, "POST", "/mcp", mcpListIssues, withMCPBearer(code.Token+"x")); w.Code != http.StatusUnauthorized {
		t.Errorf("wrong bearer: got %d, want 401", w.Code)
	}

	// /mcp sits outside /api: a cross-origin POST with a valid bearer is
	// fine — the token, not the origin, is the credential.
	w := do(t, h, "POST", "/mcp", mcpListIssues, withMCPBearer(code.Token), withOrigin("https://evil.example"))
	if w.Code != http.StatusOK {
		t.Errorf("cross-origin with bearer: got %d, want 200", w.Code)
	}

	// Revoking one client cuts off exactly that client.
	if w := do(t, h, "DELETE", "/api/settings/mcp-tokens/"+strconv.FormatInt(code.ID, 10), "", withCookie(c)); w.Code != http.StatusOK {
		t.Fatalf("revoke: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "POST", "/mcp", mcpListIssues, withMCPBearer(code.Token)); w.Code != http.StatusUnauthorized {
		t.Errorf("after revoke: got %d, want 401", w.Code)
	}
	if w := do(t, h, "POST", "/mcp", mcpListIssues, withMCPBearer(claw.Token)); w.Code != http.StatusOK {
		t.Errorf("other token after revoke: got %d %s, want 200", w.Code, w.Body)
	}
	if tokens := listMCPTokens(t, h, c); len(tokens) != 1 || tokens[0].Name != "openclaw" {
		t.Errorf("after revoke: got %v, want just openclaw", tokens)
	}

	// A second revoke of the same id has nothing to delete.
	if w := do(t, h, "DELETE", "/api/settings/mcp-tokens/"+strconv.FormatInt(code.ID, 10), "", withCookie(c)); w.Code != http.StatusNotFound {
		t.Errorf("revoke twice: got %d, want 404", w.Code)
	}
}

// A change made through MCP is attributed to the token that made it, so the
// audit log distinguishes one client from another.
func TestMCPAuditNamesTheClient(t *testing.T) {
	srv, h := newTestServer(t)
	c := setup(t, h)
	seedIssue(t, srv, "warning")
	tok := createMCPToken(t, h, c, "openclaw")

	w := do(t, h, "POST", "/mcp", mcpAckIssue, withMCPBearer(tok.Token))
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), `"isError":true`) {
		t.Fatalf("issue ack: %d %s", w.Code, w.Body)
	}

	w = do(t, h, "GET", "/api/audit", "", withCookie(c))
	if w.Code != http.StatusOK {
		t.Fatalf("audit: %d %s", w.Code, w.Body)
	}
	var entries []struct {
		Actor  string `json:"actor"`
		Action string `json:"action"`
	}
	json.Unmarshal(w.Body.Bytes(), &entries)
	found := false
	for _, e := range entries {
		if e.Action == "issue.ack" {
			found = true
			if e.Actor != "mcp:openclaw" {
				t.Errorf("issue.ack actor = %q, want mcp:openclaw", e.Actor)
			}
		}
	}
	if !found {
		t.Errorf("no issue.ack entry in audit log: %s", w.Body)
	}
}

func TestMCPTokenNameRules(t *testing.T) {
	_, h := newTestServer(t)
	c := setup(t, h)

	if w := do(t, h, "POST", "/api/settings/mcp-tokens", `{"name":"  "}`, withCookie(c)); w.Code != http.StatusBadRequest {
		t.Errorf("blank name: got %d, want 400", w.Code)
	}
	long := strings.Repeat("x", maxMCPTokenName+1)
	if w := do(t, h, "POST", "/api/settings/mcp-tokens", `{"name":"`+long+`"}`, withCookie(c)); w.Code != http.StatusBadRequest {
		t.Errorf("overlong name: got %d, want 400", w.Code)
	}
	createMCPToken(t, h, c, "laptop")
	if w := do(t, h, "POST", "/api/settings/mcp-tokens", `{"name":"laptop"}`, withCookie(c)); w.Code != http.StatusConflict {
		t.Errorf("duplicate name: got %d, want 409", w.Code)
	}
	if tokens := listMCPTokens(t, h, c); len(tokens) != 1 {
		t.Errorf("rejected creates left %d tokens, want 1", len(tokens))
	}
}

func TestMCPTokenEndpointsNeedSession(t *testing.T) {
	_, h := newTestServer(t)
	setup(t, h)
	if w := do(t, h, "GET", "/api/settings/mcp-tokens", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("list without session: got %d, want 401", w.Code)
	}
	if w := do(t, h, "POST", "/api/settings/mcp-tokens", `{"name":"x"}`); w.Code != http.StatusUnauthorized {
		t.Errorf("create without session: got %d, want 401", w.Code)
	}
	if w := do(t, h, "DELETE", "/api/settings/mcp-tokens/1", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("revoke without session: got %d, want 401", w.Code)
	}
}

// Drive through /mcp: a token minted the default way cannot read another
// app's credentials or a dotfile; one the operator minted with "may read app
// data and hidden files" can. The right comes from the token row, through
// mcpAuthed, not from anything the client says.
func TestMCPDriveReachFollowsTheToken(t *testing.T) {
	srv, h := newTestServer(t)
	c := setup(t, h)
	root := srv.files.Root()
	for rel, body := range map[string]string{
		"Apps/Paperless/creds.txt": "DB_PASSWORD=hunter2",
		".secrets.txt":             "machine x password s3cret",
	} {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	read := func(tok, path string) string {
		body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fog_files_read","arguments":{"path":"` + path + `"}}}`
		w := do(t, h, "POST", "/mcp", body, withMCPBearer(tok))
		if w.Code != http.StatusOK {
			t.Fatalf("read %s: %d %s", path, w.Code, w.Body)
		}
		return w.Body.String()
	}

	narrow := createMCPToken(t, h, c, "assistant")
	for _, p := range []string{"Apps/Paperless/creds.txt", "apps/Paperless/creds.txt", ".secrets.txt"} {
		if got := read(narrow.Token, p); strings.Contains(got, "hunter2") || strings.Contains(got, "s3cret") || !strings.Contains(got, `"isError":true`) {
			t.Errorf("default token read %s: %s", p, got)
		}
	}

	w := do(t, h, "POST", "/api/settings/mcp-tokens", `{"name":"trusted","canReadAllFiles":true}`, withCookie(c))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"canReadAllFiles":true`) {
		t.Fatalf("create broad token: %d %s", w.Code, w.Body)
	}
	var broad mcpTokenRow
	json.Unmarshal(w.Body.Bytes(), &broad)
	if got := read(broad.Token, "Apps/Paperless/creds.txt"); !strings.Contains(got, "hunter2") {
		t.Errorf("broad token read of app data: %s", got)
	}
	if got := read(broad.Token, ".secrets.txt"); !strings.Contains(got, "s3cret") {
		t.Errorf("broad token read of a dotfile: %s", got)
	}
	// The list says which token holds the broad right.
	listed := do(t, h, "GET", "/api/settings/mcp-tokens", "", withCookie(c)).Body.String()
	if !strings.Contains(listed, `"name":"trusted","canPostChat":false,"canReadAllFiles":true`) {
		t.Errorf("token list does not show the right: %s", listed)
	}
}
