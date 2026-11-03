package proxy

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/dockerx"
	"gitea.lan/matthew/fog/internal/livetest"
	"gitea.lan/matthew/fog/internal/store"
)

// fog-wire's own compose project, brought up by real docker compose through
// Apply's path: rendered, up, its config validated by the Caddy in it, and
// reloaded. Needs docker and the compose CLI (on a Mac with Colima, run it
// inside the VM); skipped without them or where fog already runs.
func TestLiveWireComposeUp(t *testing.T) {
	if testing.Short() || exec.Command("docker", "compose", "version").Run() != nil {
		t.Skip("needs docker compose")
	}
	livetest.Lock(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	if !dockerx.New().Available(ctx) {
		t.Skip("docker not reachable by fog's client")
	}
	if out, _ := exec.Command("docker", "ps", "-aq", "--filter", "name=^"+WireContainer+"$").Output(); len(strings.TrimSpace(string(out))) > 0 {
		t.Skip("a fog-wire already runs here")
	}
	createdNet := exec.Command("docker", "network", "inspect", apps.NetworkName).Run() != nil
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.SetSetting(SettingCaddyImage, liveCaddyImage)
	p := &Proxy{DataDir: t.TempDir(), Store: st, Docker: dockerx.New()}
	t.Cleanup(func() {
		down := exec.Command("docker", "compose", "down")
		down.Dir = p.wireDir()
		down.Run()
		// Caddy, as root in its container, wrote its state into the test's
		// folder; run as another user, the test couldn't remove it.
		exec.Command("docker", "run", "--rm", "-v", p.wireDir()+":/d", liveCaddyImage,
			"rm", "-rf", "/d/data", "/d/config").Run()
		if createdNet {
			exec.Command("docker", "network", "rm", apps.NetworkName).Run()
		}
	})
	if err := p.applyWires(ctx); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("docker", "inspect", "-f", "{{.State.Running}} {{index .Config.Labels \"fog.system\"}}", WireContainer).CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "true wire" {
		t.Fatalf("fog-wire after compose up: %q %v", out, err)
	}
	// No port published: only containers reach it.
	if out, _ := exec.Command("docker", "port", WireContainer).CombinedOutput(); strings.TrimSpace(string(out)) != "" {
		t.Errorf("fog-wire publishes %q", out)
	}
	// A second apply with nothing changed is a no-op, and doesn't fail.
	if err := p.applyWires(ctx); err != nil {
		t.Fatal(err)
	}
	// A config the Caddy in it rejects never goes live: the converge fails,
	// and the live file is the one it had.
	live := filepath.Join(confDir(p.wireDir()), "Caddyfile")
	before, _ := os.ReadFile(live)
	if err := stageConfig(p.wireDir(), []byte(":80 {\n\tnot_a_directive\n}\n")); err != nil {
		t.Fatal(err)
	}
	if err := p.composeWireConverge(ctx, p.wireDir()); err == nil {
		t.Fatal("a rejected config converged")
	}
	discardStaged(p.wireDir())
	if after, _ := os.ReadFile(live); string(after) != string(before) {
		t.Fatalf("the live config became the rejected one:\n%s", after)
	}
	if out, err := exec.Command("docker", "exec", WireContainer, "caddy", "validate", "--adapter", "caddyfile", "--config", "/etc/caddy/Caddyfile").CombinedOutput(); err != nil {
		t.Fatalf("the live config no longer validates: %s", out)
	}
}
