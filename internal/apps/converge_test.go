// Regression coverage for the blue-green swap and the release phase, run
// against a fake docker CLI (a PATH shim that records its argv) and a fake
// Docker Engine API (an HTTP server behind DOCKER_SOCK).
package apps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/dockerx"
)

// fakeDockerCLI puts a `docker` shim first on PATH that appends each argv to
// a log file and then runs the given extra script (empty = exit 0).
func fakeDockerCLI(t *testing.T, extra string) (logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "docker.log")
	// fog names compose's file, project and settings file on every call
	// (ComposeCmd); the shim records and matches what follows them.
	script := "#!/bin/sh\nif [ \"$1\" = compose ] && [ \"$2\" = --project-name ]; then shift 9; set -- compose \"$@\"; fi\n" +
		"echo \"$@\" >> \"" + logPath + "\"\n" + extra + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

func cliLog(t *testing.T, logPath string) string {
	t.Helper()
	b, _ := os.ReadFile(logPath)
	return string(b)
}

// fakeDockerAPI serves the Engine API calls converge uses. list is called
// once per /containers/json request with the request ordinal (1-based);
// inspected collects the container IDs handed to /containers/{id}/json, each
// of which is reported running with a fog-network IP of 127.0.0.1. Every
// lifecycle request (a POST or DELETE) succeeds and is appended to cliLog
// as "api METHOD path", so the one log shows API calls and CLI calls in the
// order they happened.
type fakeDockerAPI struct {
	mu        sync.Mutex
	listCalls int
	inspected []string
	list      func(call int) []dockerx.ContainerSummary
	cliLog    string
	// network is the one network an inspected container reports; "" is fog.
	network string
}

func (f *fakeDockerAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method != http.MethodGet:
		call := "api " + r.Method + " " + strings.TrimPrefix(r.URL.Path, "/v1.44")
		if r.URL.RawQuery != "" {
			call += "?" + r.URL.RawQuery
		}
		if f.cliLog != "" {
			lf, err := os.OpenFile(f.cliLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
			if err == nil {
				fmt.Fprintln(lf, call)
				lf.Close()
			}
		}
		w.WriteHeader(http.StatusNoContent)
	case strings.HasSuffix(r.URL.Path, "/containers/json"):
		f.mu.Lock()
		f.listCalls++
		cs := f.list(f.listCalls)
		f.mu.Unlock()
		json.NewEncoder(w).Encode(cs)
	case strings.HasSuffix(r.URL.Path, "/json"):
		parts := strings.Split(r.URL.Path, "/")
		id := parts[len(parts)-2]
		f.mu.Lock()
		f.inspected = append(f.inspected, id)
		f.mu.Unlock()
		network := f.network
		if network == "" {
			network = NetworkName
		}
		fmt.Fprintf(w, `{"State":{"Running":true},"NetworkSettings":{"Networks":{%q:{"IPAddress":"127.0.0.1"}}}}`, network)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeDockerAPI) sawInspect(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.inspected {
		if s == id {
			return true
		}
	}
	return false
}

// swapEngine wires an Engine whose Docker client talks to the fake API and
// whose compose/exec calls hit the PATH shim.
func swapEngine(t *testing.T, api *fakeDockerAPI, appName string) *Engine {
	t.Helper()
	// os.MkdirTemp keeps the socket path under the ~104-byte sun_path limit,
	// which t.TempDir's nested name can exceed.
	sockDir, err := os.MkdirTemp("", "fog")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go http.Serve(ln, api)
	t.Setenv("DOCKER_SOCK", sock)

	dataDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dataDir, "apps", appName), 0o755); err != nil {
		t.Fatal(err)
	}
	return &Engine{
		Docker: dockerx.New(),
		Mat:    &Materializer{DataDir: dataDir},
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// healthSpec returns a swap-capable spec whose health probe hits a local HTTP
// server, so waitHealthy succeeds immediately.
func healthSpec(t *testing.T) *AppSpec {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(ts.Close)
	port := ts.Listener.Addr().(*net.TCPAddr).Port
	return &AppSpec{Health: &Health{Path: "/ping", Port: port}}
}

func mainC(id, state string) dockerx.ContainerSummary {
	return onNetwork(id, state, NetworkName)
}

// onNetwork is a main container of "app" attached to networks, addressed as
// Docker lists it.
func onNetwork(id, state string, networks ...string) dockerx.ContainerSummary {
	c := dockerx.ContainerSummary{ID: id, State: state, Labels: map[string]string{"fog.app": "app"}}
	c.NetworkSettings.Networks = map[string]dockerx.NetworkEndpoint{}
	for _, n := range networks {
		c.NetworkSettings.Networks[n] = dockerx.NetworkEndpoint{IPAddress: "127.0.0.1"}
	}
	return c
}

// A stale exited main container (a crashed replica, or an old one whose rm
// failed) must never be mistaken for the new replica: the swap identifies the
// new one by absence from the pre-scale-up set, health-checks it, and drains
// the genuinely old container.
func TestSwapIgnoresStaleExitedContainer(t *testing.T) {
	logPath := fakeDockerCLI(t, "")
	api := &fakeDockerAPI{list: func(call int) []dockerx.ContainerSummary {
		if call == 1 {
			return []dockerx.ContainerSummary{mainC("old1", "running"), mainC("stale1", "exited")}
		}
		// Post-scale-up: docker lists the stale corpse after the new replica.
		return []dockerx.ContainerSummary{mainC("old1", "running"), mainC("new1", "running"), mainC("stale1", "exited")}
	}}
	api.cliLog = logPath
	e := swapEngine(t, api, "app")

	if err := e.converge(context.Background(), "app", healthSpec(t), io.Discard); err != nil {
		t.Fatalf("converge: %v", err)
	}
	if api.sawInspect("stale1") {
		t.Error("health-checked the stale exited container")
	}
	if !api.sawInspect("new1") {
		t.Error("never health-checked the new replica")
	}
	log := cliLog(t, logPath)
	for _, want := range []string{
		"api POST /networks/fog/disconnect",
		"api POST /containers/old1/stop?t=30",
		"api DELETE /containers/old1",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("old replica not drained (no %s):\n%s", want, log)
		}
	}
	if strings.Contains(log, "/containers/new1") {
		t.Errorf("new replica touched:\n%s", log)
	}
	if !strings.Contains(log, "--no-deps") || !strings.Contains(log, "--scale app=2") {
		t.Errorf("scale-up missing --no-deps:\n%s", log)
	}
}

// A new replica that exits before it can be identified (or a scale-up that
// produces anything other than exactly one new running container) fails the
// deploy loudly, naming what appeared — never health-checking a guess.
func TestSwapFailsLoudlyWhenNewReplicaExited(t *testing.T) {
	logPath := fakeDockerCLI(t, "")
	api := &fakeDockerAPI{list: func(call int) []dockerx.ContainerSummary {
		if call == 1 {
			return []dockerx.ContainerSummary{mainC("old1", "running")}
		}
		return []dockerx.ContainerSummary{mainC("old1", "running"), mainC("new1", "exited")}
	}}
	api.cliLog = logPath
	e := swapEngine(t, api, "app")

	err := e.converge(context.Background(), "app", healthSpec(t), io.Discard)
	if err == nil {
		t.Fatal("converge succeeded with no running new replica")
	}
	if !strings.Contains(err.Error(), "new1 (exited)") {
		t.Errorf("error does not name the exited container: %v", err)
	}
	if len(api.inspected) != 0 {
		t.Errorf("health-checked containers despite ambiguity: %v", api.inspected)
	}
	if log := cliLog(t, logPath); strings.Contains(log, "/containers/old1") {
		t.Errorf("old replica touched on the failure path:\n%s", log)
	}
}

// A timed-out release run leaves its container alive inside dockerd (execx
// kills only the CLI's process group, so --rm never fires). The run must
// remove the named container afterwards, or a retried deploy would start a
// second migration alongside the first.
func TestReleaseTimeoutRemovesNamedContainer(t *testing.T) {
	logPath := fakeDockerCLI(t, `case "$*" in compose\ run*) sleep 5;; esac`)
	e := swapEngine(t, &fakeDockerAPI{cliLog: logPath}, "app")

	err := e.runRelease(context.Background(), "app", []string{"migrate"}, 2*time.Second, io.Discard)
	if err == nil {
		t.Fatal("runRelease succeeded despite timeout")
	}
	if !strings.Contains(err.Error(), "timed out") || !strings.Contains(err.Error(), "fog-release-app") {
		t.Errorf("error does not report the cleanup: %v", err)
	}
	log := cliLog(t, logPath)
	runIdx := strings.Index(log, "compose run")
	if runIdx < 0 {
		t.Fatalf("release run never issued:\n%s", log)
	}
	const rm = "api DELETE /containers/fog-release-app?force=true"
	if !strings.Contains(log[:runIdx], rm) {
		t.Errorf("no pre-run cleanup of a leftover container:\n%s", log)
	}
	if !strings.Contains(log[runIdx:], rm) {
		t.Errorf("timed-out container not removed:\n%s", log)
	}
	if !strings.Contains(log, "--name fog-release-app app migrate") {
		t.Errorf("release container not deterministically named:\n%s", log)
	}
}

// waitForLog polls the live log until it contains marker, then releases the
// shim by creating gate. Deterministic streaming proof: the shim prints the
// marker and then blocks until the gate file exists, so the marker can only
// be observed mid-command if output streams — CombinedOutput would deadlock
// the old way's buffer until the command exited.
func waitForLog(t *testing.T, lv *liveDeploy, marker, gate string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(lv.String(), marker) {
		if time.Now().After(deadline) {
			t.Fatalf("%q never appeared in the live log while the command ran; log:\n%s", marker, lv.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := os.WriteFile(gate, nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

// A long release phase must feed the live-log UI line by line, not hand over
// one buffered blob when the phase ends.
func TestReleaseStreamsOutputLive(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "gate")
	fakeDockerCLI(t, `case "$*" in compose\ run*) echo release-marker
while [ ! -f "`+gate+`" ]; do sleep 0.02; done;; esac`)
	e := swapEngine(t, &fakeDockerAPI{}, "app")
	lv := &liveDeploy{}
	lv.start()

	done := make(chan error, 1)
	go func() { done <- e.runRelease(context.Background(), "app", []string{"migrate"}, 30*time.Second, lv) }()
	waitForLog(t, lv, "release-marker", gate)
	if err := <-done; err != nil {
		t.Fatalf("runRelease: %v", err)
	}
}

// Same for the converge/up call — a slow image pull shows progress live.
func TestConvergeStreamsOutputLive(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "gate")
	fakeDockerCLI(t, `case "$*" in compose\ up*) echo pull-marker
while [ ! -f "`+gate+`" ]; do sleep 0.02; done;; esac`)
	dataDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dataDir, "apps", "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	e := &Engine{Mat: &Materializer{DataDir: dataDir}}
	lv := &liveDeploy{}
	lv.start()

	done := make(chan error, 1)
	// No health spec → the plain composeUp path.
	go func() { done <- e.converge(context.Background(), "app", &AppSpec{}, lv) }()
	waitForLog(t, lv, "pull-marker", gate)
	if err := <-done; err != nil {
		t.Fatalf("converge: %v", err)
	}
}

// A sealed app swaps like any other, on the network it lives on: the new
// replica is probed at its sealed address and the old one drained from
// fog-sealed. There is no fog network to disconnect it from.
func TestSwapOnTheSealedNetwork(t *testing.T) {
	logPath := fakeDockerCLI(t, "")
	api := &fakeDockerAPI{network: SealedNetworkName, list: func(call int) []dockerx.ContainerSummary {
		if call == 1 {
			return []dockerx.ContainerSummary{onNetwork("old1", "running", SealedNetworkName)}
		}
		return []dockerx.ContainerSummary{onNetwork("old1", "running", SealedNetworkName), onNetwork("new1", "running", SealedNetworkName)}
	}}
	api.cliLog = logPath
	e := swapEngine(t, api, "app")
	spec := healthSpec(t)
	spec.Network = NetworkSealed

	if err := e.converge(context.Background(), "app", spec, io.Discard); err != nil {
		t.Fatalf("converge: %v", err)
	}
	log := cliLog(t, logPath)
	if !strings.Contains(log, "api POST /networks/fog-sealed/disconnect") {
		t.Errorf("old replica not drained from fog-sealed:\n%s", log)
	}
	if strings.Contains(log, "/networks/fog/disconnect") {
		t.Errorf("disconnected from a network the replica isn't on:\n%s", log)
	}
}

// The drain follows the replica, not the spec. Sealing an app swaps out a
// replica on the fog network, and a sealed peer's old replica has to leave
// both networks' DNS.
func TestSwapDrainsEveryNetworkTheOldReplicaIsOn(t *testing.T) {
	for name, tc := range map[string]struct {
		old  []string
		spec func(*AppSpec)
		want []string
	}{
		"the deploy that seals": {[]string{NetworkName}, func(s *AppSpec) { s.Network = NetworkSealed }, []string{"fog"}},
		"a sealed peer":         {[]string{NetworkName, SealedNetworkName}, func(s *AppSpec) { s.SealedPeer = true }, []string{"fog", "fog-sealed"}},
	} {
		t.Run(name, func(t *testing.T) {
			logPath := fakeDockerCLI(t, "")
			api := &fakeDockerAPI{list: func(call int) []dockerx.ContainerSummary {
				if call == 1 {
					return []dockerx.ContainerSummary{onNetwork("old1", "running", tc.old...)}
				}
				return []dockerx.ContainerSummary{onNetwork("old1", "running", tc.old...), onNetwork("new1", "running", NetworkName, SealedNetworkName)}
			}}
			api.cliLog = logPath
			api.network = SealedNetworkName
			e := swapEngine(t, api, "app")
			spec := healthSpec(t)
			tc.spec(spec)
			if err := e.converge(context.Background(), "app", spec, io.Discard); err != nil {
				t.Fatalf("converge: %v", err)
			}
			log := cliLog(t, logPath)
			for _, n := range tc.want {
				if !strings.Contains(log, "api POST /networks/"+n+"/disconnect") {
					t.Errorf("not drained from %s:\n%s", n, log)
				}
			}
		})
	}
}

// fog finds a sealed app on the only network it is on, and a sealed peer on
// the fog network it keeps.
func TestMainIPOnTheSealedNetwork(t *testing.T) {
	at := func(networks map[string]dockerx.NetworkEndpoint) []dockerx.ContainerSummary {
		c := dockerx.ContainerSummary{State: "running", Labels: map[string]string{"fog.app": "fit"}}
		c.NetworkSettings.Networks = networks
		return []dockerx.ContainerSummary{c}
	}
	sealed := at(map[string]dockerx.NetworkEndpoint{SealedNetworkName: {IPAddress: "10.9.0.2"}, "fit_private": {IPAddress: "10.8.0.2"}})
	if got := MainIP(sealed, "fit"); got != "10.9.0.2" {
		t.Errorf("sealed app = %q", got)
	}
	peer := at(map[string]dockerx.NetworkEndpoint{SealedNetworkName: {IPAddress: "10.9.0.3"}, NetworkName: {IPAddress: "10.0.0.3"}})
	if got := MainIP(peer, "fit"); got != "10.0.0.3" {
		t.Errorf("sealed peer = %q", got)
	}
}

// A sealed deploy is checked against what Docker says, so a stale network
// (a sidecar network from before the app was sealed) is a failure, not a
// quiet route out. Stopped containers have no route to use.
func TestSealedBreaches(t *testing.T) {
	internal := func(n string) (bool, error) {
		if n == "lost" {
			return false, errors.New("no such network")
		}
		return n == SealedNetworkName || n == "app_private", nil
	}
	if got := sealedBreaches([]dockerx.ContainerSummary{
		onNetwork("web", "running", SealedNetworkName, "app_private"),
		onNetwork("gone", "exited", NetworkName),
	}, internal); len(got) != 0 {
		t.Errorf("a sealed app breached: %v", got)
	}
	got := sealedBreaches([]dockerx.ContainerSummary{
		onNetwork("a", "running", SealedNetworkName, NetworkName),
		onNetwork("b", "running", "stale_private"),
		onNetwork("c", "running", "lost"),
	}, internal)
	if len(got) != 3 || !strings.Contains(got[0], "a on fog") || !strings.Contains(got[1], "b on stale_private") || !strings.Contains(got[2], "no such network") {
		t.Errorf("breaches = %v", got)
	}
}

// An app's address comes off the container listing: its running main
// container's place on the fog network, never a sidecar's or another app's.
func TestMainIP(t *testing.T) {
	at := func(app, state, ip string, labels ...string) dockerx.ContainerSummary {
		c := dockerx.ContainerSummary{State: state, Labels: map[string]string{"fog.app": app}}
		for i := 0; i+1 < len(labels); i += 2 {
			c.Labels[labels[i]] = labels[i+1]
		}
		c.NetworkSettings.Networks = map[string]dockerx.NetworkEndpoint{NetworkName: {IPAddress: ip}}
		return c
	}
	cs := []dockerx.ContainerSummary{
		at("hub", "running", "10.0.0.9", "fog.aux", "postgres"),
		at("hub", "exited", "10.0.0.8"),
		at("paperless", "running", "10.0.0.7"),
		at("hub", "running", "10.0.0.2"),
	}
	if got := MainIP(cs, "hub"); got != "10.0.0.2" {
		t.Errorf("MainIP(hub) = %q", got)
	}
	if got := MainIP(cs, "immich"); got != "" {
		t.Errorf("MainIP(immich) = %q", got)
	}
}
