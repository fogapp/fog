package server

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Two fogs, two SDK clients: the browser SDK across households. The Go side
// pairs the fogs and drives heartbeats once both devices exist, so the
// directories carry each other's people; the script does the rest.
func TestSDKFederatedEndToEnd(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	_, here, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(here), "..", "..")
	if _, err := os.Stat(filepath.Join(root, "frontend", "node_modules", "ts-mls")); err != nil {
		t.Skip("frontend/node_modules missing ts-mls; run npm install in frontend")
	}
	p := twoChatFogs(t)
	inviteAndJoin(t, p.bH, p.bCookie, "eve", "correct horse battery")

	// Heartbeats keep flowing in the background so the people the script
	// registers show up in each other's directories within a second.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		t := time.NewTicker(300 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if peer, err := p.b.store.FedPeerByID(p.aPeerOnB.ID); err == nil {
					p.b.fed.HeartbeatPeer(ctx, peer)
				}
				if peer, err := p.a.store.FedPeerByID(p.bPeerOnA.ID); err == nil && peer.URL != "" {
					p.a.fed.HeartbeatPeer(ctx, peer)
				}
			}
		}
	}()
	// The hub's fan-out normally rides a coalescing timer; a fast flush loop
	// keeps the test snappy.
	go func() {
		t := time.NewTicker(200 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				p.a.chat.Fed.Flush(ctx)
				p.b.chat.Fed.WaitInvites()
				p.b.chat.Fed.Flush(ctx)
			}
		}
	}()

	// The script is a client of its own: its sessions come from where it
	// connects (a web session is pinned to its address, resolvePerson).
	scriptAdmin := signInOverTCP(t, p.aHTTP.URL, "admin", "correct horse battery")
	scriptEve := signInOverTCP(t, p.bHTTP.URL, "eve", "correct horse battery")
	cmd := exec.Command(node, "--experimental-strip-types", "--no-warnings", filepath.Join(root, "clients", "chat-sdk", "test", "e2e-fed.mjs"),
		p.aHTTP.URL, scriptAdmin.Name+"="+scriptAdmin.Value, p.bHTTP.URL, scriptEve.Name+"="+scriptEve.Value)
	cmd.Dir = filepath.Join(root, "clients", "chat-sdk")
	outBytes, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, outBytes)
	}
	lines := strings.Split(strings.TrimSpace(string(outBytes)), "\n")
	var out struct {
		Steps []map[string]any `json:"steps"`
		Error string           `json:"error"`
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &out); err != nil {
		t.Fatalf("parse: %v\n%s", err, outBytes)
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
	add := get("add")
	if added := add["added"].([]any); len(added) != 1 {
		t.Fatalf("addMembers across households: %v", add)
	}
	joined := get("eve-joined")
	if joined["hubHere"] != false || joined["households"].(float64) != 2 || len(joined["members"].([]any)) != 2 {
		t.Errorf("eve's view of the replica: %v", joined)
	}
	msgs := get("messages")
	if op := msgs["opSees"].([]any); len(op) != 2 || op[0] != "hi from B" {
		t.Errorf("operator sees: %v", op)
	}
	if ev := msgs["eveSees"].([]any); len(ev) != 2 || ev[1] != "hi from A" {
		t.Errorf("eve sees: %v", ev)
	}
	if names := msgs["eveNames"].([]any); len(names) != 2 || !strings.Contains(names[1].(string), " · ") {
		t.Errorf("eve's rendering of the operator's name should carry the household: %v", names)
	}
	if get("attachment")["roundTrip"] != "cross-household bytes" {
		t.Errorf("attachment: %v", get("attachment"))
	}
	get("done")
}
