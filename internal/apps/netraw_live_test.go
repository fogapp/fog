package apps

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/dockerx"
	"gitea.lan/matthew/fog/internal/randx"
)

// The attack: an app with Docker's default CAP_NET_RAW ARP-spoofs its
// network's gateway to fog's proxy, then speaks to the proxy's tunnel
// listener as the gateway — and names any client. Without NET_RAW there is
// no raw socket: no forged ARP, no forged IP source. Ping still works
// (ICMP datagram sockets). Proven with scapy, against a container fog
// renders, beside a control with Docker's defaults.
//
// Needs docker, the compose CLI and network to build a scapy image (the
// Colima VM on a Mac); skipped without them.
func TestLiveAppsCannotSpoofTheGateway(t *testing.T) {
	if testing.Short() || exec.Command("docker", "compose", "version").Run() != nil {
		t.Skip("needs docker compose")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()
	docker := func(args ...string) (string, error) {
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	if !dockerx.New().Available(ctx) {
		t.Skip("docker not reachable by fog's client")
	}
	img := "fog-scapy-test:1"
	if _, err := docker("image", "inspect", img); err != nil {
		bdir := t.TempDir()
		os.WriteFile(filepath.Join(bdir, "Dockerfile"), []byte("FROM python:3.12-alpine\nRUN pip install --no-cache-dir scapy==2.6.1\n"), 0o644)
		if out, err := docker("build", "-q", "-t", img, bdir); err != nil {
			t.Skipf("can't build a scapy image: %s", out)
		}
	}
	sfx := strings.ToLower(randx.Hex(3))
	app := "spoofer" + sfx
	e := &Engine{Docker: dockerx.New(), Log: slog.New(slog.DiscardHandler), Mat: &Materializer{DataDir: t.TempDir()}}
	spec := &AppSpec{Source: Source{Type: "image", Image: img}, Command: []string{"sleep", "3600"}}
	net := OwnNetwork(app, spec)
	gw, err := e.Docker.EnsureIsolatedNetwork(ctx, net, false, map[string]string{netLabel: "app", "fog.app": app})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		e.Mat.ComposeCmd(context.Background(), app, "down").Run()
		e.RemoveAppNetworks(context.Background(), app)
	})

	// The proxy's tunnel listener stands in: it answers on 18443 with who
	// it thinks is asking.
	listener, err := docker("run", "-d", "--network", net, liveNetImage, "caddy", "respond", "--listen", ":18443", "{remote_host}")
	if err != nil {
		t.Fatalf("listener: %s", listener)
	}
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", listener).Run() })
	ip := func(id string) string {
		out, _ := docker("inspect", "-f", "{{(index .NetworkSettings.Networks \""+net+"\").IPAddress}}", id)
		return out
	}
	mac := func(id string) string {
		out, _ := docker("inspect", "-f", "{{(index .NetworkSettings.Networks \""+net+"\").MacAddress}}", id)
		return out
	}
	lip, lmac := ip(listener), mac(listener)
	// The listener knows the gateway's real address.
	docker("exec", listener, "ping", "-c", "1", "-W", "2", gw)
	gwMAC := func() string {
		out, _ := docker("exec", listener, "ip", "neigh", "show", gw)
		return out
	}
	real := gwMAC()
	spoof := `from scapy.all import *
conf.verb = 0
sendp(Ether(dst="` + lmac + `")/ARP(op=2, psrc="` + gw + `", pdst="` + lip + `", hwdst="` + lmac + `"), iface="eth0", count=6, inter=0.3)
print("SENT")`
	forge := `from scapy.all import *
conf.verb = 0
send(IP(src="` + gw + `", dst="` + lip + `")/TCP(dport=18443, flags="S"))
print("SENT")`

	// Past the neighbour entry's lock time, so a reply may override it.
	time.Sleep(2 * time.Second)
	// Control: Docker's defaults, and the spoof lands.
	ctl, err := docker("run", "-d", "--network", net, img, "sleep", "3600")
	if err != nil {
		t.Fatalf("control: %s", ctl)
	}
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", ctl).Run() })
	if out, _ := docker("exec", ctl, "python3", "-c", spoof); !strings.Contains(out, "SENT") {
		t.Skipf("control couldn't send ARP (%q); the test proves nothing here", out)
	}
	if got := gwMAC(); !strings.Contains(got, mac(ctl)) {
		t.Skipf("control's spoof didn't land (%q); the test proves nothing here", got)
	}
	docker("rm", "-f", ctl)
	docker("exec", listener, "ip", "neigh", "flush", "dev", "eth0")
	docker("exec", listener, "ping", "-c", "1", "-W", "2", gw)
	real = gwMAC()
	time.Sleep(2 * time.Second)

	// fog's own render: no raw socket at all.
	if err := e.Mat.Render(app, spec, nil, img, "", io.Discard); err != nil {
		t.Fatal(err)
	}
	if out, err := e.Mat.ComposeCmd(ctx, app, "up", "-d").CombinedOutput(); err != nil {
		t.Fatalf("compose up: %s", out)
	}
	cid, _ := docker("ps", "-q", "--filter", "label=com.docker.compose.project="+app)
	for name, script := range map[string]string{"ARP spoof": spoof, "forged-source SYN to 18443": forge} {
		out, _ := docker("exec", cid, "python3", "-c", script)
		if strings.Contains(out, "SENT") || !strings.Contains(out, "Operation not permitted") {
			t.Errorf("%s from a fog app: %q", name, out)
		}
	}
	if got := gwMAC(); got != real {
		t.Errorf("the listener's gateway entry changed: %q, was %q", got, real)
	}
	// And an ordinary app loses nothing it needs: ping works.
	if out, err := docker("exec", cid, "ping", "-c", "1", "-W", "2", lip); err != nil {
		t.Errorf("ping without NET_RAW: %s", out)
	}
}
