package vpn

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/store"
	"gitea.lan/matthew/fog/internal/vault"
	"gitea.lan/matthew/fog/internal/wan"
)

// Live tests actually download tailscale and run the daemon. They need
// network, Linux and root (tailscaled creates a TUN device), so they are
// opt-in the same way internal/wan's router probes are:
//
//	sudo FOG_VPN_LIVE=1 ./vpn.test -test.run TestLive -test.v
//
// Nothing here touches /usr/local/bin or the running fogd: the manager gets
// a temp BinDir and a temp DataDir, and takes them with it.
func requireLive(t *testing.T) {
	t.Helper()
	if os.Getenv("FOG_VPN_LIVE") != "1" {
		t.Skip("set FOG_VPN_LIVE=1 to run live tailscale tests")
	}
}

func TestLiveInstall(t *testing.T) {
	requireLive(t)
	m := lifecycleManager(t)
	// Drop the shell stubs: this is the real download.
	os.Remove(m.cliBin())
	os.Remove(m.daemonBin())

	if err := m.install(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !m.installed() {
		t.Fatal("install reported success but the binaries aren't there")
	}
	out, err := exec.Command(m.cliBin(), "version").CombinedOutput()
	if err != nil {
		t.Fatalf("installed tailscale won't run: %v: %s", err, out)
	}
	t.Logf("installed version %s\n%s", m.Status().Version, strings.TrimSpace(string(out)))
}

// The daemon comes up, opens its socket, and answers `status --json` in the
// shape parseStatus expects — the assumption the whole status line rests on.
func TestLiveDaemonStatus(t *testing.T) {
	requireLive(t)
	m := lifecycleManager(t)
	os.Remove(m.cliBin())
	os.Remove(m.daemonBin())
	if err := m.install(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A deliberately invalid key: joining a real tailnet isn't this test's
	// business, but the daemon running and the error surfacing both are.
	saveKey(t, m, "tskey-auth-notarealkey")
	if err := m.Start(false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the daemon to report its own state", func() bool {
		s := m.Status()
		return s.Running && s.StatusKnown
	})
	// `tailscale up` blocks; the operator-facing result of the join isn't a
	// fact until it returns, so don't judge it before then.
	waitFor(t, "the join attempt to finish", func() bool {
		s := m.Status()
		return s.UpError != "" || (s.Snapshot != nil && s.Snapshot.HaveNodeKey)
	})
	s := m.Status()
	t.Logf("backendState=%q haveNodeKey=%v route=%q upErr=%q health=%v",
		s.Snapshot.BackendState, s.Snapshot.HaveNodeKey, s.Route, s.UpError, s.Snapshot.Health)

	if s.Route != "192.168.1.200/32" && !strings.HasSuffix(s.Route, "/32") {
		t.Errorf("Route = %q, want a host route", s.Route)
	}
	// Nothing is joined, so nothing may read as approved.
	if s.Snapshot.RouteApproved || s.Snapshot.Online {
		t.Errorf("unjoined node reported as working: %+v", s.Snapshot)
	}
	if s.Snapshot.BackendState == "" {
		t.Error("BackendState empty — the field name parseStatus reads has moved")
	}
	// A bad key must land in front of the operator, not only in a log.
	if s.UpError == "" {
		t.Error("an invalid auth key produced no error for the page to show")
	}

	// And the CLI's own view agrees the daemon is really there.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := m.cli(ctx, "status", "--json"); err != nil {
		t.Errorf("status through the manager's CLI path failed: %v", err)
	}

	m.Stop()
	waitFor(t, "the daemon to stop", func() bool { return !m.Status().Running })
}

// liveManager builds a manager against real paths when the environment asks
// for them, so a join lands the node key where fogd will later look for it
// rather than in a temp dir that leaves a dead machine in the tailnet.
//
//	FOG_VPN_LIVE_BINDIR   where the binaries go   (default: a temp dir)
//	FOG_VPN_LIVE_DATADIR  <dir>/tailscale is the state dir (default: temp)
//	FOG_VPN_LIVE_KEY_FILE a file holding an auth key, so the key never
//	                      appears in an environment or a process listing
//
// The store and vault stay temporary either way: fogd's live database is
// open in another process and is not this test's to touch.
func liveManager(t *testing.T) *Manager {
	t.Helper()
	tmp := t.TempDir()
	st, err := store.Open(filepath.Join(tmp, "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	vlt, err := vault.Open(tmp)
	if err != nil {
		t.Fatal(err)
	}
	binDir, dataDir := os.Getenv("FOG_VPN_LIVE_BINDIR"), os.Getenv("FOG_VPN_LIVE_DATADIR")
	if binDir == "" {
		binDir = t.TempDir()
	}
	if dataDir == "" {
		dataDir = t.TempDir()
	}
	m := &Manager{
		Store:   st,
		Vault:   vlt,
		Log:     slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})),
		DataDir: dataDir,
		BinDir:  binDir,
		LANIP:   func() string { return liveLANIP(t) },
	}
	t.Cleanup(m.Stop)
	return m
}

// liveLANIP uses the same discovery fogd does in production.
func liveLANIP(t *testing.T) string {
	t.Helper()
	if ip := os.Getenv("FOG_VPN_LIVE_LANIP"); ip != "" {
		return ip
	}
	ip := wan.LANIP()
	if ip == "" {
		t.Fatal("could not read the LAN address")
	}
	return ip
}

// TestLiveJoin actually joins the tailnet. It is the one test that changes
// something outside this machine — a node appears in the operator's
// Tailscale account — so it needs a key handed to it deliberately.
func TestLiveJoin(t *testing.T) {
	requireLive(t)
	keyFile := os.Getenv("FOG_VPN_LIVE_KEY_FILE")
	if keyFile == "" {
		t.Skip("set FOG_VPN_LIVE_KEY_FILE to a file holding an auth key")
	}
	raw, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	m := liveManager(t)
	if !m.installed() {
		if err := m.install(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	saveKey(t, m, strings.TrimSpace(string(raw)))

	if err := m.Start(false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the daemon to report its own state", func() bool {
		s := m.Status()
		return s.Running && s.StatusKnown
	})
	waitFor(t, "the join to finish", func() bool {
		s := m.Status()
		return s.UpError != "" || (s.Snapshot != nil && s.Snapshot.HaveNodeKey)
	})
	s := m.Status()
	if s.UpError != "" {
		t.Fatalf("join failed: %s", s.UpError)
	}
	t.Logf("joined: state=%q tailnet=%q name=%q ip=%q route=%q approved=%v primaryRoutes=%v",
		s.Snapshot.BackendState, s.Snapshot.Tailnet, s.Snapshot.DNSName,
		s.Snapshot.TailnetIP, s.Route, s.Snapshot.RouteApproved, s.Snapshot.PrimaryRoutes)

	waitFor(t, "the node to come online", func() bool {
		s := m.Status()
		return s.StatusKnown && s.Snapshot != nil && s.Snapshot.Online
	})
	s = m.Status()
	if s.Snapshot.TailnetIP == "" {
		t.Error("joined but no tailnet address")
	}
	// Whether the route is approved is the operator's call in the admin
	// console; what must be true here is that fog asked for the right one.
	t.Logf("advertised %q; approved=%v", s.Route, s.Snapshot.RouteApproved)
	if !strings.HasSuffix(s.Route, "/32") {
		t.Errorf("advertised %q, want a host route", s.Route)
	}
}

// TestLiveRoute re-reads an already-joined node: no key needed, which is
// itself the thing being checked — a restart must not want one.
func TestLiveRoute(t *testing.T) {
	requireLive(t)
	m := liveManager(t)
	if !m.loggedIn() {
		t.Skip("no existing tailscale state; run TestLiveJoin first")
	}
	if err := m.Start(false); err != nil {
		t.Fatalf("a joined node could not start without a key: %v", err)
	}
	waitFor(t, "the node to come online", func() bool {
		s := m.Status()
		return s.StatusKnown && s.Snapshot != nil && s.Snapshot.Online
	})
	s := m.Status()
	t.Logf("name=%q ip=%q route=%q approved=%v primaryRoutes=%v health=%v",
		s.Snapshot.DNSName, s.Snapshot.TailnetIP, s.Route,
		s.Snapshot.RouteApproved, s.Snapshot.PrimaryRoutes, s.Snapshot.Health)
	if !s.Snapshot.RouteApproved {
		t.Logf("route %s is not approved yet — approve it in the admin console", s.Route)
	}
}
