package proxy

import (
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// liveCaddyImage is the stock Caddy the live test runs fog's Caddyfile in.
// Its header handling is the same code as the DNS-module build fog ships.
const liveCaddyImage = "caddy:2.10-alpine"

// The attack, against a real Caddy: an internet client sends identity
// headers in spellings Caddy's delete-by-name used to leave alone —
// "Remote_User", "REMOTE_EMAIL", "Fog_Assertion", "Remote.User" — which a
// CGI-style server folds into HTTP_REMOTE_USER and believes. On an open
// route nothing may reach the app; on a gated route only what the gate set.
//
// Needs docker; skipped without it, or under -short.
func TestLiveCaddyStripsForgedIdentityHeaders(t *testing.T) {
	if testing.Short() {
		t.Skip("live docker test")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not installed")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	docker := func(args ...string) (string, error) {
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	if _, err := docker("info"); err != nil {
		t.Skip("docker not reachable")
	}
	if _, err := docker("image", "inspect", liveCaddyImage); err != nil {
		if out, err := docker("pull", liveCaddyImage); err != nil {
			t.Skipf("cannot pull %s: %s", liveCaddyImage, out)
		}
	}

	// fog's own config for two routes, with fogd stood in for by a stub on
	// :9001 that passes everyone as alice (and, like a person with no email
	// on file, sets no Remote-Email), and the apps by an echo on :9000.
	p := &Proxy{FogUpstream: "localhost:9001"}
	cfg := p.caddyfile("home.test", []route{
		{Host: "fog.home.test", Upstream: p.FogUpstream, Fog: true},
		{Host: "open.home.test", Upstream: "localhost:9000"},
		{Host: "gated.home.test", Upstream: "localhost:9000", Auth: true},
	}, false, "")
	cfg = "{\n\tauto_https off\n\tadmin localhost:2019\n}\n" + cfg + `
http://:9000 {
	templates {
		between <% %>
	}
	respond "<% range $k, $v := .Req.Header %><% $k %>=<% index $v 0 %>
<% end %>"
}
http://:9001 {
	header Remote-User alice
	header Remote-Name Alice
	header Remote-Groups household
	header Fog-Assertion signed-by-fog
	templates {
		between <% %>
	}
	respond "<% range $k, $v := .Req.Header %><% $k %>=<% index $v 0 %>
<% end %>" 200
}
`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Caddyfile"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	// Copied in, not bind-mounted: a VM-backed docker (Colima) shares only
	// some host paths.
	id, err := docker("create", "-p", "127.0.0.1::80", liveCaddyImage)
	if err != nil {
		t.Fatalf("docker create: %s", id)
	}
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", id).Run() })
	if out, err := docker("cp", filepath.Join(dir, "Caddyfile"), id+":/etc/caddy/Caddyfile"); err != nil {
		t.Fatalf("docker cp: %s", out)
	}
	if out, err := docker("start", id); err != nil {
		t.Fatalf("docker start: %s", out)
	}
	hostPort, err := docker("port", id, "80/tcp")
	if err != nil {
		t.Fatalf("docker port: %s", hostPort)
	}
	hostPort = strings.Split(hostPort, "\n")[0]

	get := func(host string) string {
		t.Helper()
		var last error
		for range 50 {
			req, _ := http.NewRequestWithContext(ctx, "GET", "http://"+hostPort+"/", nil)
			req.Host = host
			for k, v := range map[string]string{
				"Remote_User":   "operator",
				"REMOTE_EMAIL":  "operator@home.test",
				"remote_name":   "Operator",
				"Remote_Groups": "admins",
				"Fog_Assertion": "forged",
				"Remote.User":   "operator",
				"REMOTE-USER":   "operator",
				"Remote-Email":  "operator@home.test",
				// Names fog never sets, which an app may still read.
				"Remote-Roles":    "admin",
				"Remote_Anything": "x",
				"Fog-Assertion-X": "forged",
				"fog.whatever":    "forged",
				// Federation's own, which fogd needs on its route.
				"Fog-Instance":  "peer-instance",
				"Fog-Signature": "peer-signature",
			} {
				req.Header[k] = []string{v} // as sent, not canonicalised
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				last = err
				time.Sleep(100 * time.Millisecond)
				continue
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 {
				logs, _ := docker("logs", id)
				t.Fatalf("%s: status %d: %s\n%s", host, resp.StatusCode, body, logs)
			}
			return string(body)
		}
		logs, _ := docker("logs", id)
		t.Fatalf("%s: no answer: %v\n%s", host, last, logs)
		return ""
	}
	headers := func(body string) map[string]string {
		out := map[string]string{}
		for _, l := range strings.Split(body, "\n") {
			if k, v, ok := strings.Cut(l, "="); ok {
				out[strings.ToLower(k)] = v
			}
		}
		return out
	}
	// Any header named Remote… or Fog…, in any spelling, is forged unless
	// allowed names it, exactly, with its value.
	forged := func(t *testing.T, h map[string]string, allowed map[string]string) {
		t.Helper()
		for k, v := range h {
			if !strings.HasPrefix(k, "remote") && !strings.HasPrefix(k, "fog") {
				continue
			}
			if want, ok := allowed[k]; !ok || v != want {
				t.Errorf("received %s: %s", k, v)
			}
		}
	}

	open := headers(get("open.home.test"))
	forged(t, open, nil)

	gated := headers(get("gated.home.test"))
	forged(t, gated, map[string]string{
		"remote-user": "alice", "remote-name": "Alice", "remote-groups": "household", "fog-assertion": "signed-by-fog",
	})
	if gated["remote-user"] != "alice" {
		t.Errorf("gated route lost the gate's Remote-User: %v", gated)
	}

	// fogd's own route: the federation's Fog-* headers arrive; nothing that
	// claims a person does.
	fog := headers(get("fog.home.test"))
	for k, v := range fog {
		norm := strings.NewReplacer("_", "-", ".", "-").Replace(k)
		if strings.HasPrefix(k, "remote") || norm == "fog-assertion" {
			t.Errorf("fogd received %s: %s", k, v)
		}
	}
	if fog["fog-instance"] != "peer-instance" || fog["fog-signature"] != "peer-signature" {
		t.Errorf("fog's route lost the federation's headers: %v", fog)
	}
}
