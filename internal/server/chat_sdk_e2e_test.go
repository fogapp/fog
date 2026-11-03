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
	"gitea.lan/matthew/fog/internal/store"
)

// The browser SDK against the real hub: two Node processes' worth of client
// in one script, talking HTTP and WebSocket to an httptest server with the
// fog bot running. Skips when Node or the SDK's dependencies are absent.
func TestSDKEndToEnd(t *testing.T) {
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
	inviteAndJoin(t, h, admin, "dave", "correct horse battery")

	bot, err := chat.NewBot(srv.chat, srv.vault)
	if err != nil {
		t.Fatal(err)
	}
	// The bot answers any "ping" so the script can prove a live round trip
	// through a room it joined after the bot's earlier notice.
	bot.OnMessage = func(room *store.ChatRoom, ev *store.ChatEvent, c chat.Content) {
		if c.Body != nil && strings.HasPrefix(c.Body.Text, "ping") {
			go bot.Send(room.ID, chat.TextMessage("", "pong: "+c.Body.Text))
		}
	}
	ctx, cancel := t.Context(), func() {}
	defer cancel()
	go bot.Run(ctx)
	// Give the bot a moment to create #fog and publish group info.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if rooms, _ := srv.store.ChatRoomsFor(bot.Addr()); len(rooms) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	srv.store.CreateNotice("test", "", "Hello from fog", "the bot mirrors this")
	bot.MirrorNoticesNow()

	ts := httptest.NewServer(h)
	defer ts.Close()
	// The script is a client of its own: its sessions come from where it
	// connects (a web session is pinned to its address, resolvePerson).
	scriptAdmin := signInOverTCP(t, ts.URL, "admin", "correct horse battery")
	scriptDave := signInOverTCP(t, ts.URL, "dave", "correct horse battery")

	logFile := filepath.Join(t.TempDir(), "e2e.log")
	cmd := exec.Command(node, "--experimental-strip-types", "--no-warnings", filepath.Join(root, "clients", "chat-sdk", "test", "e2e.mjs"),
		ts.URL, scriptAdmin.Name+"="+scriptAdmin.Value, scriptDave.Name+"="+scriptDave.Value)
	cmd.Dir = filepath.Join(root, "clients", "chat-sdk")
	cmd.Env = append(os.Environ(), "FOG_E2E_LOG="+logFile)
	// A hang in the script must fail the test, not the whole suite.
	timer := time.AfterFunc(120*time.Second, func() {
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
	})
	outBytes, err := cmd.CombinedOutput()
	timer.Stop()
	if err != nil {
		trail, _ := os.ReadFile(logFile)
		t.Fatalf("node: %v\n%s\n--- progress ---\n%s", err, outBytes, trail)
	}
	// The script prints one JSON line last; anything before it is noise.
	lines := strings.Split(strings.TrimSpace(string(outBytes)), "\n")
	var out struct {
		Steps []map[string]any `json:"steps"`
		Error string           `json:"error"`
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &out); err != nil {
		t.Fatalf("parse script output: %v\n%s", err, outBytes)
	}
	if out.Error != "" {
		t.Fatalf("script error: %s\nsteps: %v", out.Error, out.Steps)
	}
	get := func(name string) map[string]any {
		for _, s := range out.Steps {
			if s["name"] == name {
				return s
			}
		}
		t.Fatalf("step %q missing; got %v", name, out.Steps)
		return nil
	}
	init := get("init")
	if init["opVerified"] != true || init["daveVerified"] != true || init["opHasIdentity"] != true {
		t.Errorf("first devices should be self-attested and verified: %v", init)
	}
	joined := get("dave-joined")
	if joined["epoch"].(float64) != 1 || len(joined["members"].([]any)) != 2 {
		t.Errorf("dave joined: %v", joined)
	}
	msgs := get("messages")
	if opSees := msgs["opSees"].([]any); len(opSees) != 1 || opSees[0] != "hello dave" {
		t.Errorf("operator timeline: %v", opSees)
	}
	if thread := msgs["thread"].([]any); len(thread) != 2 || thread[1] != "hello back" {
		t.Errorf("thread: %v", thread)
	}
	if msgs["replies"].(float64) != 1 {
		t.Errorf("reply count: %v", msgs["replies"])
	}
	if re := msgs["reactions"].([]any); len(re) != 1 || re[0].(map[string]any)["emoji"] != "👍" {
		t.Errorf("reactions: %v", re)
	}
	ed := get("edit-delete")
	if ed["edited"] != "hello dave (edited)" || ed["deleted"] != true {
		t.Errorf("edit/delete: %v", ed)
	}
	if get("attachment")["roundTrip"] != "attachment bytes" {
		t.Errorf("attachment: %v", get("attachment"))
	}
	fa := get("fog-available")
	if fa["present"] != true || fa["joinable"] != true {
		t.Errorf("#fog availability: %v", fa)
	}
	fj := get("fog-joined")
	if bm := fj["botMessages"].([]any); len(bm) != 1 || bm[0] != "pong: ping from dave" {
		t.Errorf("bot messages after joining #fog: %v", fj)
	}
	if hits := get("search")["hits"].([]any); len(hits) != 1 {
		t.Errorf("search hits: %v", hits)
	}
	sd := get("second-device")
	if sd["verified"] != false || sd["hasIdentity"] != false || sd["sameAddr"] != true {
		t.Errorf("second device should start unverified under the same person: %v", sd)
	}
	sib := get("sibling")
	if sees := sib["tabletSees"].([]any); len(sees) != 1 || sees[0] != "for both of dave's devices" || sib["tabletSyncedRoom"] != "ready" {
		t.Errorf("sibling device: %v", sib)
	}
	hist := get("history")
	if ts := hist["tabletSees"].([]any); len(ts) != 1 || ts[0] != "before dave" {
		t.Errorf("history to the tablet: %v", hist)
	}
	if ds := hist["daveSees"].([]any); len(ds) != 1 || ds[0] != "before dave" {
		t.Errorf("history to dave: %v", hist)
	}
	if lines := get("membership-lines")["lines"].([]any); len(lines) < 1 {
		t.Errorf("membership lines: %v", lines)
	}
	rs := get("restore")
	if rs["phraseWords"].(float64) != 12 || rs["sameDevice"] != true || rs["verifiedOp"] != true || rs["hasHistory"] != true || rs["joinedNothing"] != true || rs["readsNew"] != true {
		t.Errorf("restore: %v", rs)
	}
	if get("revoke")["tabletLeaves"].(float64) != 1 {
		t.Errorf("after revoking the tablet, dave should hold one leaf: %v", get("revoke"))
	}
	get("done")

	// Nothing in the database is readable: the plaintexts never appear.
	rows, err := srv.store.DB().Query(`SELECT payload FROM chat_events WHERE kind = 'mls_private'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var payload []byte
		rows.Scan(&payload)
		n++
		for _, secret := range []string{"hello dave", "hello back", "see attached", "race message", "Hello from fog"} {
			if strings.Contains(string(payload), secret) {
				t.Fatalf("plaintext %q found in the hub's log", secret)
			}
		}
	}
	if n < 5 {
		t.Errorf("expected several private events in the log, got %d", n)
	}
}
