package proxy

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/randx"
	"gitea.lan/matthew/fog/internal/store/storetest"
)

// liveCurlImage stands in for cloudflared: a client of the tunnel socket.
const liveCurlImage = "curlimages/curl:8.11.1"

// The tunnel listener, against a real Caddy in real Docker networks (fifth
// review, 1). It used to be a TCP port answering only the host's bridge
// address — and an app container that forged that address (ARP-spoofing
// the gateway, with raw sockets) reached it and named any visitor it liked.
// Now it is a unix socket in a directory only fog's user opens, mounted
// into Caddy alone:
//
//   - through the socket (cloudflared's way in), fogd is told the
//     Cf-Connecting-Ip, that it came through the tunnel, and https;
//   - an app container, raw sockets and all, has no path to it: every TCP
//     port of Caddy's it can reach names it as itself and the ordinary
//     listener, whatever X-Fog-* or Cf-Connecting-Ip it sends — and the
//     gate's sub-request too;
//   - a LAN-only route refuses everyone through the tunnel.
//
// Needs docker; skipped without it, or under -short.
func TestLiveTunnelSocketIsCloudflaredsAlone(t *testing.T) {
	if testing.Short() {
		t.Skip("live docker test")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not installed")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	docker := func(args ...string) (string, error) {
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	if _, err := docker("info"); err != nil {
		t.Skip("docker not reachable")
	}
	for _, img := range []string{liveCaddyImage, liveCurlImage} {
		if _, err := docker("image", "inspect", img); err != nil {
			if out, err := docker("pull", img); err != nil {
				t.Skipf("cannot pull %s: %s", img, out)
			}
		}
	}
	suffix := randx.Hex(4)
	edge, appNet, vol := "fogtest-edge-"+suffix, "fogtest-app-"+suffix, "fogtest-tunnel-"+suffix
	for _, n := range []string{edge, appNet} {
		if out, err := docker("network", "create", n); err != nil {
			t.Fatalf("network create: %s", out)
		}
		t.Cleanup(func() { exec.Command("docker", "network", "rm", n).Run() })
	}
	if out, err := docker("volume", "create", vol); err != nil {
		t.Fatalf("volume create: %s", out)
	}
	t.Cleanup(func() { exec.Command("docker", "volume", "rm", "-f", vol).Run() })

	// fogd stood in for by :9001, which echoes what it receives — and, as
	// the gate, passes everyone with the client address it was told as
	// Remote-User; apps by an echo on :9000.
	p := &Proxy{FogUpstream: "localhost:9001", tunnelOn: true}
	routes := []route{
		{Host: "fog.home.test", Upstream: p.FogUpstream, Fog: true},
		{Host: "gated.home.test", Upstream: "localhost:9000", Auth: true},
		{Host: "lan.home.test", Upstream: "localhost:9000", LanOnly: true},
	}
	echo := `
http://:9000 {
	templates {
		between <% %>
	}
	respond "<% range $k, $v := .Req.Header %><% $k %>=<% index $v 0 %>
<% end %>" 200
}
http://:9001 {
	header Remote-User "told:{http.request.header.X-Fog-Client-Ip}"
	templates {
		between <% %>
	}
	respond "<% range $k, $v := .Req.Header %><% $k %>=<% index $v 0 %>
<% end %>" 200
}
`
	start := func(cfg string) string {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "Caddyfile"), []byte(cfg), 0o644); err != nil {
			t.Fatal(err)
		}
		id, err := docker("create", "--network", edge, "-v", vol+":"+tunnelInContainer, liveCaddyImage)
		if err != nil {
			t.Fatalf("docker create: %s", id)
		}
		t.Cleanup(func() { exec.Command("docker", "rm", "-f", id).Run() })
		if out, err := docker("cp", filepath.Join(dir, "Caddyfile"), id+":/etc/caddy/Caddyfile"); err != nil {
			t.Fatalf("docker cp: %s", out)
		}
		if out, err := docker("network", "connect", appNet, id); err != nil {
			t.Fatalf("network connect: %s", out)
		}
		if out, err := docker("start", id); err != nil {
			t.Fatalf("docker start: %s", out)
		}
		return id
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
	// viaSocket asks as cloudflared does: over the socket.
	viaSocket := func(id, host string) (string, map[string]string) {
		t.Helper()
		var out string
		var err error
		for range 50 {
			out, err = docker("run", "--rm", "-v", vol+":"+tunnelInContainer, liveCurlImage,
				"-s", "-w", "\n%{http_code}", "--unix-socket", tunnelInContainer+"/"+tunnelSocketName,
				"-H", "Host: "+host, "-H", "Cf-Connecting-Ip: 198.51.100.7", "-H", "X-Fog-Client-Ip: 10.0.0.1", "http://x/")
			if err == nil && !strings.HasSuffix(out, "\n000") {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		if err != nil {
			logs, _ := docker("logs", id)
			t.Fatalf("%s through the socket: %v %s\n%s", host, err, out, logs)
		}
		i := strings.LastIndex(out, "\n")
		return out[i+1:], headers(out[:i])
	}
	// fromApp asks from a container on an app's network, raw sockets and
	// all, with every header that would claim to be the tunnel: "" when
	// nothing answers.
	fromApp := func(url, host string) string {
		t.Helper()
		out, err := docker("run", "--rm", "--network", appNet, "--cap-add", "NET_RAW", "--cap-add", "NET_ADMIN", liveCaddyImage,
			"wget", "-q", "-T", "5", "-O", "-", "--header", "Host: "+host,
			"--header", "Cf-Connecting-Ip: 198.51.100.7", "--header", "X-Fog-Client-Ip: 198.51.100.7",
			"--header", "X-Fog-Via: tunnel", "--header", "X-Fog-Proxy-Secret: guess", url)
		if err != nil {
			return ""
		}
		return out
	}

	// An http setup's config.
	id := start("{\n\tauto_https off\n\tadmin localhost:2019\n}\n" + p.caddyfile("home.test", routes, false, "") + echo)
	code, h := viaSocket(id, "fog.home.test")
	if code != "200" || h["x-fog-client-ip"] != "198.51.100.7" || h["x-fog-via"] != ViaTunnel || h["x-forwarded-proto"] != "https" {
		t.Fatalf("cloudflared through the socket: %s, fogd told client %q via %q proto %q", code, h["x-fog-client-ip"], h["x-fog-via"], h["x-forwarded-proto"])
	}
	if code, _ := viaSocket(id, "lan.home.test"); code != "403" {
		t.Fatalf("a LAN-only route through the tunnel: %s", code)
	}
	if code, h := viaSocket(id, "gated.home.test"); code != "200" || h["remote-user"] != "told:198.51.100.7" {
		t.Fatalf("the gate through the tunnel was told %q (%s)", h["remote-user"], code)
	}
	// A restart finds its old socket file in place, and serves again.
	if out, err := docker("restart", id); err != nil {
		t.Fatalf("restart: %s", out)
	}
	if code, _ := viaSocket(id, "fog.home.test"); code != "200" {
		logs, _ := docker("logs", id)
		t.Fatalf("after a restart the socket answers %s\n%s", code, logs)
	}

	caddyIP, err := docker("inspect", "-f", "{{(index .NetworkSettings.Networks \""+appNet+"\").IPAddress}}", id)
	if err != nil || caddyIP == "" {
		t.Fatalf("caddy's address on the app network: %s", caddyIP)
	}
	h = headers(fromApp("http://"+caddyIP+"/", "fog.home.test"))
	if ip := h["x-fog-client-ip"]; ip == "" || ip == "198.51.100.7" || h["x-fog-via"] != ViaEdge || h["x-fog-proxy-secret"] == "guess" {
		t.Fatalf("an app container reached fogd as %q via %q", ip, h["x-fog-via"])
	}
	h = headers(fromApp("http://"+caddyIP+"/", "gated.home.test"))
	if u := h["remote-user"]; u == "" || u == "told:198.51.100.7" {
		t.Fatalf("the gate's sub-request took the client's X-Fog-Client-Ip: %q", u)
	}
	for _, port := range []string{"18443", "443", "8443", "2019"} {
		if out := fromApp("http://"+caddyIP+":"+port+"/", "fog.home.test"); strings.Contains(out, "X-Fog-Via=tunnel") {
			t.Fatalf("an app container reached the tunnel on :%s", port)
		}
	}

	// An https setup's config, its certificate from Caddy's own CA here.
	cfg := strings.ReplaceAll(p.caddyfile("home.test", routes, true, ""), "dns cloudflare {env.DNS_API_TOKEN}", "issuer internal")
	exec.Command("docker", "rm", "-f", id).Run()
	id = start("{\n\tadmin localhost:2019\n\tlocal_certs\n}\n" + cfg + echo)
	if code, h := viaSocket(id, "fog.home.test"); code != "200" || h["x-fog-via"] != ViaTunnel || h["x-forwarded-proto"] != "https" {
		t.Fatalf("an https setup through the socket: %s %v", code, h)
	}
}

// The tunnel listener's shape, without docker: a socket in a directory of
// its own, mounted into Caddy, and no TCP port.
func TestTunnelListenerShape(t *testing.T) {
	routes := []route{
		{Host: "fog.example.com", Upstream: "host.docker.internal:8080", Fog: true},
		{Host: "lan.example.com", Upstream: "lan.fog-app:80", LanOnly: true},
	}
	p := &Proxy{FogUpstream: "host.docker.internal:8080", DataDir: "/var/lib/fog", tunnelOn: true}
	out := p.caddyfile("example.com", routes, true, "")
	main, tunnel, ok := strings.Cut(out, "http://*.example.com, http://example.com {")
	if !ok {
		t.Fatalf("no tunnel site:\n%s", out)
	}
	if !strings.Contains(tunnel, "bind unix//run/fog-tunnel/caddy.sock|0666") {
		t.Fatalf("the tunnel site is not on the socket:\n%s", tunnel)
	}
	if !strings.Contains(tunnel, HeaderClientIP+" {http.request.header.Cf-Connecting-Ip}") || strings.Contains(tunnel, "{remote_host}") {
		t.Fatalf("on the tunnel listener fogd is not told Cloudflare's visitor:\n%s", tunnel)
	}
	if strings.Contains(main, "Cf-Connecting-Ip}") {
		t.Fatal("the ordinary listener names a client from Cf-Connecting-Ip")
	}
	if !strings.Contains(tunnel, fmt.Sprintf("respond %q 403", lanRefusal)) {
		t.Fatal("a LAN-only route answers through the tunnel")
	}
	p.tunnelOn = false
	if strings.Contains(p.caddyfile("example.com", routes, true, ""), "bind unix") {
		t.Fatal("a tunnel socket with no tunnel set up")
	}
	if got := p.TunnelService(); got != "unix:/var/lib/fog/caddy/tunnel/caddy.sock" {
		t.Fatalf("service %q", got)
	}
	st, err := storetest.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	raw, err := (&Proxy{Store: st}).composeFile()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "./tunnel:/run/fog-tunnel") || strings.Contains(string(raw), "18443") {
		t.Fatalf("compose:\n%s", raw)
	}
}
