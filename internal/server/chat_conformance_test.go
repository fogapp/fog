package server

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/chat"
)

// The conformance suite a standalone client runs against a scratch fogd,
// here against this build: every check must pass, including "the server
// holds ciphertext only".
func TestChatConformanceSuite(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	_, here, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(here), "..", "..")
	if _, err := os.Stat(filepath.Join(root, "frontend", "node_modules", "ts-mls")); err != nil {
		t.Skip("frontend/node_modules missing ts-mls; run npm install in frontend")
	}
	srv, h := newTestServer(t)
	admin := setup(t, h)
	bot, err := chat.NewBot(srv.chat, srv.vault)
	if err != nil {
		t.Fatal(err)
	}
	go bot.Run(t.Context())

	ts := httptest.NewServer(h)
	defer ts.Close()
	// The script is a client of its own: its session comes from where it
	// connects (a web session is pinned to its address, resolvePerson).
	scriptAdmin := signInOverTCP(t, ts.URL, "admin", "correct horse battery")
	logFile := filepath.Join(t.TempDir(), "conformance.log")
	cmd := exec.Command(node, "--experimental-strip-types", "--no-warnings", filepath.Join(root, "clients", "chat-sdk", "test", "conformance.mjs"))
	cmd.Dir = filepath.Join(root, "clients", "chat-sdk")
	cmd.Env = append(os.Environ(), "FOG_E2E_LOG="+logFile, "FOG_URL="+ts.URL, "FOG_OPERATOR_COOKIE="+scriptAdmin.Name+"="+scriptAdmin.Value, "FOG_CONFORMANCE_RATELIMIT=1")
	timer := time.AfterFunc(150*time.Second, func() {
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
	})
	outBytes, runErr := cmd.CombinedOutput()
	timer.Stop()
	lines := strings.Split(strings.TrimSpace(string(outBytes)), "\n")
	var report struct {
		OK     bool `json:"ok"`
		Checks []struct {
			Name   string `json:"name"`
			OK     bool   `json:"ok"`
			Detail any    `json:"detail"`
		} `json:"checks"`
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &report); err != nil {
		trail, _ := os.ReadFile(logFile)
		t.Fatalf("no report (%v): %s\n--- progress ---\n%s", runErr, outBytes, trail)
	}
	for _, c := range report.Checks {
		if !c.OK {
			t.Errorf("check %s failed: %v", c.Name, c.Detail)
		}
	}
	want := []string{"people-join", "device-bootstrap", "keypackage-single-use", "deviceless-refused", "sessions-listed", "dm-decrypts", "server-holds-ciphertext", "blob-is-ciphertext", "operator-cannot-read", "operator-does-not-see-room", "stale-commit-refused", "cursors", "stream-replay", "rate-limit"}
	seen := map[string]bool{}
	for _, c := range report.Checks {
		seen[c.Name] = true
	}
	for _, w := range want {
		if !seen[w] {
			t.Errorf("check %s never ran", w)
		}
	}
	if !report.OK && runErr == nil {
		t.Errorf("report says not ok but node exited 0")
	}
	// The throwaway people are gone.
	people := decode(t, do(t, h, "GET", "/api/people", "", withCookie(admin)))["people"].([]any)
	for _, p := range people {
		if strings.HasPrefix(p.(map[string]any)["handle"].(string), "conf-") {
			t.Errorf("conformance person left behind: %v", p)
		}
	}
}
