package vpn

import (
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/store"
	"gitea.lan/matthew/fog/internal/vault"
)

// stubs stands in for the two binaries: a "daemon" that opens the socket
// and then sits there, and a "CLI" that answers `status --json` with a
// joined, route-approved node and succeeds at everything else. Enough to
// exercise supervision, the join, and Status end to end without a tailnet.
func stubs(t *testing.T, m *Manager) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell stubs are POSIX")
	}
	daemon := "#!/bin/sh\n" +
		"sock=$(echo \"$@\" | tr ' ' '\\n' | sed -n 's/^--socket=//p')\n" +
		": > \"$sock\"\n" +
		"trap 'exit 0' TERM\n" +
		"while true; do sleep 0.05; done\n"
	// The daemon's node key is modelled by a marker the join creates, so
	// the first join sees a node with no key and later ones don't — which
	// is the difference the auth-key handling turns on.
	cli := "#!/bin/sh\n" +
		"d=$(dirname \"$0\")\n" +
		"key=false; [ -f \"$d/joined\" ] && key=true\n" +
		"for a in \"$@\"; do\n" +
		"  if [ \"$a\" = status ]; then\n" +
		"    echo '{\"BackendState\":\"Running\",\"HaveNodeKey\":'$key',\"CurrentTailnet\":{\"Name\":\"example.com\"},\"Self\":{\"DNSName\":\"fog.example-tail.ts.net.\",\"Online\":true,\"TailscaleIPs\":[\"100.1.2.3\"],\"PrimaryRoutes\":[\"192.168.1.200/32\"]}}'\n" +
		"    exit 0\n" +
		"  fi\n" +
		"  if [ \"$a\" = logout ]; then rm -f \"$d/joined\"; exit 0; fi\n" +
		"  if [ \"$a\" = set ]; then echo \"$@\" >> \"$d/set.log\"; exit 0; fi\n" +
		"done\n" +
		// A join against a slow control plane: the seconds in the slow
		// marker pass before the join lands.
		"[ -f \"$d/slow\" ] && sleep \"$(cat \"$d/slow\")\"\n" +
		"echo \"$@\" >> \"$d/up.log\"\n" +
		": > \"$d/joined\"\n"
	for name, body := range map[string]string{"tailscaled": daemon, "tailscale": cli} {
		if err := os.WriteFile(filepath.Join(m.binDir(), name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func lifecycleManager(t *testing.T) *Manager {
	t.Helper()
	// The real settle wait is for a control plane that isn't here.
	old := routeWithdrawSettle
	routeWithdrawSettle = 10 * time.Millisecond
	t.Cleanup(func() { routeWithdrawSettle = old })
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	vlt, err := vault.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{
		Store:   st,
		Vault:   vlt,
		Log:     slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		DataDir: dir,
		BinDir:  t.TempDir(),
		LANIP:   func() string { return "192.168.1.200" },
	}
	stubs(t, m)
	t.Cleanup(m.Stop)
	return m
}

func saveKey(t *testing.T, m *Manager, key string) {
	t.Helper()
	ct, err := m.Vault.Encrypt(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Store.SetSecret(0, SecretAuthKey, ct); err != nil {
		t.Fatal(err)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestStartRefusesWithoutAKey(t *testing.T) {
	m := lifecycleManager(t)
	if err := m.Start(false); err == nil {
		t.Fatal("started with no auth key and no prior login")
	}
	if m.Enabled() {
		t.Error("a refused start still turned autostart on")
	}
}

func TestStartRefusesWithoutALANAddress(t *testing.T) {
	m := lifecycleManager(t)
	saveKey(t, m, "tskey-auth-test")
	m.LANIP = func() string { return "" }
	if err := m.Start(false); err == nil {
		t.Fatal("started with no route to advertise")
	}
}

func TestStartJoinsAndReportsTheDaemonsOwnAnswer(t *testing.T) {
	m := lifecycleManager(t)
	saveKey(t, m, "tskey-auth-test")

	// Before anything runs, the page must not claim knowledge it lacks —
	// but it should still say what the route would be.
	pre := m.Status()
	if pre.StatusKnown || pre.Snapshot != nil {
		t.Error("reported daemon state before the daemon existed")
	}
	if pre.Route != "192.168.1.200/32" {
		t.Errorf("Route = %q, want the derived host route", pre.Route)
	}

	if err := m.Start(false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the route to be reported approved", func() bool {
		s := m.Status()
		return s.Running && s.StatusKnown && s.Snapshot != nil && s.Snapshot.RouteApproved
	})
	s := m.Status()
	if s.Snapshot.TailnetIP != "100.1.2.3" || s.Snapshot.Tailnet != "example.com" {
		t.Errorf("snapshot not read from the daemon: %+v", s.Snapshot)
	}
	if !m.Enabled() {
		t.Error("autostart not recorded")
	}

	// The join must carry the route, refuse the tailnet's DNS and routes,
	// and never put the auth key on a command line.
	waitFor(t, "the join to be recorded", func() bool { return readUpLog(t, m) != "" })
	log := readUpLog(t, m)
	for _, want := range []string{"--advertise-routes=192.168.1.200/32", "--accept-dns=false", "--accept-routes=false", "--reset"} {
		if !contains(log, want) {
			t.Errorf("`tailscale up` missing %s; got %q", want, log)
		}
	}
	if contains(log, "tskey-auth-test") {
		t.Errorf("auth key passed on the command line: %q", log)
	}
	if !contains(log, "--authkey=file:") {
		t.Errorf("auth key not passed at all: %q", log)
	}

	m.Stop()
	waitFor(t, "the daemon to stop", func() bool { return !m.Status().Running })
	// A device that accepted the route reaches fog only through the tailnet,
	// so the route has to be withdrawn while this node can still tell
	// anyone — otherwise stopping away access takes fog off the LAN too, for
	// every device holding it.
	setLog := readLog(t, m, "set.log")
	if !contains(setLog, "--advertise-routes=") {
		t.Errorf("stop did not withdraw the route before killing the daemon; set.log = %q", setLog)
	}
	if m.Enabled() {
		t.Error("stop left autostart on — it would come back at the next boot")
	}
	// A stopped daemon has no state to report, and last time's reading is
	// not this time's fact.
	if s := m.Status(); s.StatusKnown || s.Snapshot != nil {
		t.Error("stale snapshot survived the stop")
	}
}

// fogd exiting must withdraw the route like an operator's stop does, but
// leave autostart on: a restart or self-update is not the operator turning
// away access off.
func TestShutdownWithdrawsButKeepsAutostart(t *testing.T) {
	m := lifecycleManager(t)
	saveKey(t, m, "tskey-auth-test")
	if err := m.Start(false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the daemon to run", func() bool { return m.Status().Running })

	m.Shutdown()
	waitFor(t, "the daemon to stop", func() bool { return !m.Status().Running })
	if setLog := readLog(t, m, "set.log"); !contains(setLog, "--advertise-routes=") {
		t.Errorf("shutdown did not withdraw the route; set.log = %q", setLog)
	}
	if !m.Enabled() {
		t.Error("shutdown turned autostart off — away access would not come back after the restart")
	}
}

// The key is spent on the first join; a restart rejoins from the node's own
// state, because burning a single-use key on every fogd restart would take
// away access down until someone noticed.
func TestRejoinDoesNotSpendTheKeyAgain(t *testing.T) {
	m := lifecycleManager(t)
	saveKey(t, m, "tskey-auth-test")
	if err := m.Start(false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the first join", func() bool { return contains(readUpLog(t, m), "--authkey=file:") })
	os.Remove(filepath.Join(m.binDir(), "up.log"))

	// Second join, with the daemon already reporting a node key.
	m.up("192.168.1.200/32", false)
	log := readUpLog(t, m)
	if log == "" {
		t.Fatal("no second join recorded")
	}
	if contains(log, "--authkey") {
		t.Errorf("key spent again on rejoin: %q", log)
	}
}

func TestRefreshReadvertisesAfterTheLANAddressChanges(t *testing.T) {
	m := lifecycleManager(t)
	saveKey(t, m, "tskey-auth-test")
	if err := m.Start(false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the first join", func() bool { return contains(readUpLog(t, m), "192.168.1.200/32") })
	os.Remove(filepath.Join(m.binDir(), "up.log"))

	m.LANIP = func() string { return "10.0.0.5" }
	m.Refresh()
	waitFor(t, "the new route to be advertised", func() bool {
		return contains(readUpLog(t, m), "--advertise-routes=10.0.0.5/32")
	})
}

// Refresh is called from the network watcher, whose next change must reach
// dnssync without waiting out a re-join: the join runs aside, and Refresh
// returns at once.
func TestRefreshDoesNotWaitForTheJoin(t *testing.T) {
	m := lifecycleManager(t)
	saveKey(t, m, "tskey-auth-test")
	if err := m.Start(false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the first join", func() bool { return contains(readUpLog(t, m), "192.168.1.200/32") })
	os.WriteFile(filepath.Join(m.binDir(), "slow"), []byte("2"), 0o644)

	m.LANIP = func() string { return "10.0.0.5" }
	start := time.Now()
	m.Refresh()
	m.Refresh() // a second change while the first waits adds nothing
	if d := time.Since(start); d > time.Second {
		t.Errorf("Refresh held its caller for %v while the join ran", d)
	}
	waitFor(t, "the new route to be advertised", func() bool {
		return contains(readUpLog(t, m), "--advertise-routes=10.0.0.5/32")
	})
}

// A join still in flight when away access stops is ended before the route
// is withdrawn; finishing afterwards it would advertise the route again,
// pointing every device that accepts it into a tunnel about to close.
func TestStopEndsAJoinInFlight(t *testing.T) {
	m := lifecycleManager(t)
	saveKey(t, m, "tskey-auth-test")
	if err := m.Start(false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the first join", func() bool { return contains(readUpLog(t, m), "192.168.1.200/32") })
	os.WriteFile(filepath.Join(m.binDir(), "slow"), []byte("1"), 0o644)

	joined := make(chan struct{})
	go func() { m.up("10.0.0.5/32", false); close(joined) }()
	time.Sleep(300 * time.Millisecond) // the join is now sleeping in `tailscale up`
	m.Shutdown()
	select {
	case <-joined:
	default:
		t.Error("Shutdown returned with the join still running")
	}
	time.Sleep(1500 * time.Millisecond) // past when the slow join would have landed
	if contains(readUpLog(t, m), "10.0.0.5/32") {
		t.Error("a join finished after Shutdown and re-advertised the route")
	}
	// A join asked for after the stop does nothing until the next Start.
	os.Remove(filepath.Join(m.binDir(), "slow"))
	m.up("10.0.0.6/32", false)
	if contains(readUpLog(t, m), "10.0.0.6/32") {
		t.Error("a join ran after Shutdown")
	}
}

// An override is what the route becomes, and it's normalized on the way in.
func TestRouteOverride(t *testing.T) {
	m := lifecycleManager(t)
	if err := m.Store.SetSetting(SettingRoutes, "192.168.1.0/24"); err != nil {
		t.Fatal(err)
	}
	got, err := m.DesiredRoute()
	if err != nil || got != "192.168.1.0/24" {
		t.Fatalf("DesiredRoute = %q, %v", got, err)
	}
	// A subnet wider than one host is genuinely routed through this box, so
	// the operator is told when the kernel won't do that.
	forwardingPath = filepath.Join(t.TempDir(), "ip_forward")
	t.Cleanup(func() { forwardingPath = "/proc/sys/net/ipv4/ip_forward" })
	os.WriteFile(forwardingPath, []byte("0\n"), 0o644)
	if len(m.Status().Notes) == 0 {
		t.Error("no note about IP forwarding being off")
	}
	os.WriteFile(forwardingPath, []byte("1\n"), 0o644)
	if n := m.Status().Notes; len(n) != 0 {
		t.Errorf("note raised with forwarding on: %v", n)
	}
}

func readUpLog(t *testing.T, m *Manager) string { return readLog(t, m, "up.log") }

func readLog(t *testing.T, m *Manager, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(m.binDir(), name))
	if err != nil {
		return ""
	}
	return string(b)
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
