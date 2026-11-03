package apps

import (
	"context"
	"log/slog"
	"net"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/dockerx"
	"gitea.lan/matthew/fog/internal/livetest"
	"gitea.lan/matthew/fog/internal/randx"
	"gitea.lan/matthew/fog/internal/store"
)

// The attack, live: an app container holds a port of fog's when fog's own
// container comes up — as at boot, when Docker starts the apps before fogd
// — and fog's container can't publish it. ReclaimFogPorts stops the app,
// and fog's container then comes up on the port.
func TestLiveReclaimFogPorts(t *testing.T) {
	if testing.Short() {
		t.Skip("live")
	}
	livetest.Lock(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	if !dockerx.New().Available(ctx) {
		t.Skip("docker not reachable by fog's client")
	}
	docker := func(args ...string) (string, error) {
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	// A port free here; the test runs on the docker host (the VM).
	ln, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	pub := "0.0.0.0:" + strconv.Itoa(port) + ":80"

	sfx := strings.ToLower(randx.Hex(3))
	evil, fog := "fogports-evil-"+sfx, "fogports-fog-"+sfx
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", evil, fog).Run() })
	if out, err := docker("run", "-d", "--name", evil, "--restart", "unless-stopped",
		"--label", "fog.app=evil"+sfx, "-p", pub, liveNetImage, "sleep", "3600"); err != nil {
		t.Fatalf("app: %s", out)
	}
	// fog's own container, wanting the port: refused while the app holds it.
	if out, err := docker("run", "-d", "--name", fog, "-p", pub, liveNetImage, "sleep", "3600"); err == nil {
		t.Fatalf("fog's container published the app's port: %s", out)
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a, _ := st.CreateApp("evil" + sfx)
	st.SetAppDesiredState(a.ID, "running")
	e := &Engine{Store: st, Docker: dockerx.New(), Log: slog.New(slog.DiscardHandler),
		FogHostAddr: "host.docker.internal:" + strconv.Itoa(port)}
	if !e.ReclaimFogPorts(ctx) {
		t.Fatal("nothing reclaimed")
	}
	if out, _ := docker("inspect", "-f", "{{.State.Running}}", evil); out != "false" {
		t.Fatalf("the app still runs: %s", out)
	}
	if a, _ := st.AppByName("evil" + sfx); a.DesiredState != "stopped" {
		t.Errorf("desired state %s", a.DesiredState)
	}
	if out, err := docker("start", fog); err != nil {
		t.Fatalf("fog's container still can't have its port: %s", out)
	}
	// Nothing left to reclaim, and fog's own container is never the holder.
	if e.ReclaimFogPorts(ctx) {
		t.Error("reclaimed again")
	}
}
