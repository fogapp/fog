// Package vpn gives fog an away-from-home path that needs no open port:
// tailscaled, installed and supervised as a child process of fogd, joined
// to the operator's own tailnet and advertising one route — fog's own LAN
// address.
//
// The point of advertising a route rather than handing out fog's tailnet
// address is that nothing else has to change. `*.<domain>` keeps holding
// the LAN address (the `lan` target in internal/dnssync), the wildcard
// certificate keeps matching, and a phone on cellular resolves the same
// name to the same address and reaches it through the tunnel. No DNS split,
// no second hostname, no port forward, and it works behind CGNAT.
//
// Fog's job stops at the connector: which devices may join, and whether the
// advertised route is approved, are decisions made in the operator's
// Tailscale admin console. What fogd owes them is a truthful account of
// which of those steps is still outstanding, which is what Status reports.
package vpn

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gitea.lan/matthew/fog/internal/execx"
	"gitea.lan/matthew/fog/internal/store"
	"gitea.lan/matthew/fog/internal/supervise"
	"gitea.lan/matthew/fog/internal/vault"
	"golang.org/x/sync/singleflight"
)

const (
	// SecretAuthKey is the vault key for a Tailscale auth key. Never
	// returned by any API, and never passed on a command line.
	SecretAuthKey = "tailscale_auth_key"

	// SettingEnabled drives autostart at boot: set when the operator starts
	// away access, cleared when they stop it.
	SettingEnabled = "vpn_enabled"

	// SettingRoutes overrides the advertised route. Empty — the normal
	// case — means the host route to fog's own LAN address.
	SettingRoutes = "vpn_routes"
)

const defaultBinDir = "/usr/local/bin"

// snapshotTTL bounds how often the UI's polling turns into a subprocess.
// Short enough that a state change shows up on the next poll; long enough
// that several open tabs don't each spawn a CLI every few seconds.
const snapshotTTL = 3 * time.Second

type Manager struct {
	Store   *store.Store
	Vault   *vault.Vault
	Log     *slog.Logger
	DataDir string
	BinDir  string        // tests/dev override; default /usr/local/bin
	LANIP   func() string // fog's current LAN address

	once sync.Once
	sup  *supervise.Process

	startMu sync.Mutex // one Start at a time

	// life is what every join runs under. Stop and Shutdown cancel it, and
	// wait out the join in flight, before the route is withdrawn: a join
	// that finished afterwards would advertise the route again on its way
	// out. Start makes a fresh one.
	lifeMu     sync.Mutex
	life       context.Context
	lifeCancel context.CancelFunc

	// upMu serialises joins: two `tailscale up`s racing each other leave
	// whichever finished last in charge, not whichever was asked last.
	upMu sync.Mutex
	// refreshQueued is set while a Refresh's re-advertise waits its turn
	// for upMu; a second Refresh meanwhile has nothing to add, since the
	// queued one reads the route only once it runs.
	refreshQueued atomic.Bool

	statusRead singleflight.Group // concurrent Status calls share one daemon read

	mu         sync.Mutex
	upErr      string
	reauthNext bool // the next join logs out first; consumed by whichever join runs next
	version    string
	route      string // what tailscaled was last asked to advertise
	loopRoute  string // the route the current run was started for; the loop's goroutine only
	snap       *Snapshot
	snapAt     time.Time
}

type Status struct {
	supervise.Status
	Version string `json:"version,omitempty"`
	UpError string `json:"upError,omitempty"`

	KeyConfigured bool     `json:"keyConfigured"`
	LoggedIn      bool     `json:"loggedIn"`
	Route         string   `json:"route,omitempty"`
	RouteError    string   `json:"routeError,omitempty"`
	Notes         []string `json:"notes,omitempty"`

	// StatusKnown says whether Snapshot below is a reading fogd actually
	// took. Without it the UI would have to treat "we haven't asked the
	// daemon yet" and "the daemon says the route isn't approved" as the
	// same thing, and tell the operator to go fix something that may
	// already be fine.
	StatusKnown bool      `json:"statusKnown"`
	Snapshot    *Snapshot `json:"snapshot,omitempty"`
}

func (m *Manager) binDir() string {
	if m.BinDir != "" {
		return m.BinDir
	}
	return defaultBinDir
}

func (m *Manager) cliBin() string    { return filepath.Join(m.binDir(), "tailscale") }
func (m *Manager) daemonBin() string { return filepath.Join(m.binDir(), "tailscaled") }
func (m *Manager) stateDir() string  { return filepath.Join(m.DataDir, "tailscale") }
func (m *Manager) sockPath() string  { return filepath.Join(m.stateDir(), "tailscaled.sock") }
func (m *Manager) statePath() string { return filepath.Join(m.stateDir(), "tailscaled.state") }

func (m *Manager) installed() bool {
	return supervise.Executable(m.cliBin()) && supervise.Executable(m.daemonBin())
}

// loggedIn reports whether this node already holds a node key, so a restart
// needs no auth key. A node key lives in the state file, so a non-empty one
// is the fact to read.
func (m *Manager) loggedIn() bool {
	fi, err := os.Stat(m.statePath())
	return err == nil && fi.Size() > 0
}

func (m *Manager) secrets() vault.Secrets { return vault.Secrets{Store: m.Store, Vault: m.Vault} }

func (m *Manager) authKey() (string, bool) {
	k, err := m.secrets().Get(0, SecretAuthKey)
	if err != nil || strings.TrimSpace(k) == "" {
		return "", false
	}
	return strings.TrimSpace(k), true
}

func (m *Manager) lanIP() string {
	if m.LANIP == nil {
		return ""
	}
	return m.LANIP()
}

// DesiredRoute is the route this node should advertise: the operator's
// override if they set one, else the host route to fog's LAN address.
func (m *Manager) DesiredRoute() (string, error) {
	if v, _ := m.Store.GetSetting(SettingRoutes); strings.TrimSpace(v) != "" {
		return ParseRoutes(v)
	}
	return AutoRoute(m.lanIP())
}

func (m *Manager) hostname() string {
	name, _ := m.Store.GetSetting(store.SettingServerName)
	return Hostname(name)
}

// proc is the supervisor, built on first use so a Manager stays a plain
// struct literal.
func (m *Manager) proc() *supervise.Process {
	m.once.Do(func() {
		m.sup = &supervise.Process{
			Name:      "tailscaled",
			Log:       m.Log,
			Installed: m.installed,
			Install:   m.install,
			Command:   m.command,
			Started:   m.started,
			Exited:    m.clearSnapshot,
			Stopping:  m.withdrawRoutes,
		}
	})
	return m.sup
}

func (m *Manager) Status() Status {
	ps := m.proc().Status(m.installed())
	m.mu.Lock()
	st := Status{
		Status:  ps,
		Version: m.version,
		UpError: m.upErr,
		Route:   m.route,
	}
	running := ps.Running
	route := m.route
	snap, snapAt := m.snap, m.snapAt
	m.mu.Unlock()

	_, st.KeyConfigured = m.authKey()
	st.LoggedIn = m.loggedIn()
	if st.Route == "" {
		// Not running yet: show what it *would* advertise, and say plainly
		// when that can't be worked out rather than showing nothing.
		if r, err := m.DesiredRoute(); err == nil {
			st.Route = r
			route = r
		} else {
			st.RouteError = err.Error()
		}
	}
	st.Notes = m.notes(st.Route)

	if running {
		if snap == nil || time.Since(snapAt) > snapshotTTL {
			// Every open tab polls this; when the snapshot lapses they
			// share one `tailscale status` rather than each spawning one.
			v, err, _ := m.statusRead.Do(route, func() (any, error) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				return m.readStatus(ctx, route)
			})
			fresh, _ := v.(Snapshot)
			if err == nil {
				snap = &fresh
				m.mu.Lock()
				m.snap, m.snapAt = snap, time.Now()
				m.mu.Unlock()
			} else {
				// A failed read must not resurrect a stale answer as if it
				// were current.
				snap = nil
			}
		}
		if snap != nil {
			st.StatusKnown = true
			st.Snapshot = snap
		}
	}
	return st
}

// notes are the things fogd can see about the host that would make away
// access not work, said before the operator has to discover them.
func (m *Manager) notes(route string) []string {
	var out []string
	if needsForwarding(route) && !ipForwarding() {
		out = append(out, "IP forwarding is off (net.ipv4.ip_forward=0), which a route wider than a single host needs. Enable it with: sysctl -w net.ipv4.ip_forward=1")
	}
	return out
}

// needsForwarding: a host route to fog's own address is delivered locally,
// so it works with forwarding off. Anything wider is genuinely routed
// through this box and needs the kernel's permission to do that.
func needsForwarding(route string) bool {
	for _, r := range strings.Split(route, ",") {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		if !strings.HasSuffix(r, "/32") && !strings.HasSuffix(r, "/128") {
			return true
		}
	}
	return false
}

var forwardingPath = "/proc/sys/net/ipv4/ip_forward"

func ipForwarding() bool {
	b, err := os.ReadFile(forwardingPath)
	if err != nil {
		// Not Linux, or an unreadable proc: don't claim a fault we can't see.
		return true
	}
	return strings.TrimSpace(string(b)) == "1"
}

// Start begins supervision, installing tailscale first when needed.
// Returns immediately; progress is visible via Status. Pass reauth to log
// out first and join again with the saved key — the way to move fog to a
// different tailnet, which `tailscale up` will not do on its own.
func (m *Manager) Start(reauth bool) error {
	route, err := m.DesiredRoute()
	if err != nil {
		return err
	}
	_, hasKey := m.authKey()
	if !hasKey && !m.loggedIn() {
		return fmt.Errorf("no auth key saved — generate one in the Tailscale admin console and paste it first")
	}
	if reauth && !hasKey {
		return fmt.Errorf("switching tailnets needs a new auth key")
	}
	if err := os.MkdirAll(m.stateDir(), 0o700); err != nil {
		return err
	}
	m.startMu.Lock()
	defer m.startMu.Unlock()
	if m.proc().State().Desired && !reauth {
		return nil
	}
	m.mu.Lock()
	m.upErr = ""
	m.reauthNext = reauth
	m.mu.Unlock()
	m.renewLife()
	res := m.proc().Start()
	m.Store.SetSetting(SettingEnabled, "1")
	if res != supervise.Launched {
		// A live loop picks the daemon back up on its own (or already has
		// it); all this call still owes is the (re)join.
		go m.up(route, m.takeReauth())
	}
	return nil
}

// takeReauth consumes a pending reauth request, so the logout-and-rejoin
// it asks for happens once, on whichever join gets there first.
func (m *Manager) takeReauth() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.reauthNext
	m.reauthNext = false
	return r
}

// Stop terminates tailscaled and disables restarts. The node stays logged
// in, so starting again needs no key — and stopping does not surrender the
// machine's place in the tailnet, which is a decision for the admin console.
//
// The route is withdrawn before the daemon is signalled, and that ordering
// is the whole point: see withdrawRoutes.
func (m *Manager) Stop() {
	m.clearSnapshot()
	m.Store.SetSetting(SettingEnabled, "")
	m.endJoins()
	m.proc().Stop()
}

// Shutdown is Stop for fogd exiting: the route is withdrawn and the daemon
// stopped the same way, but autostart stays on so the next boot brings away
// access back. Without it the daemon dies with fogd's cgroup and the route
// is never withdrawn — the dead-tunnel case withdrawRoutes exists to avoid,
// on every restart and self-update.
func (m *Manager) Shutdown() {
	m.clearSnapshot()
	m.endJoins()
	m.proc().Stop()
}

// lifeCtx is the context joins run under; cancelled once Stop or Shutdown
// has run, until the next Start.
func (m *Manager) lifeCtx() context.Context {
	m.lifeMu.Lock()
	defer m.lifeMu.Unlock()
	if m.life == nil {
		m.life, m.lifeCancel = context.WithCancel(context.Background())
	}
	return m.life
}

// renewLife gives a Start after a Stop a context its joins can run under.
func (m *Manager) renewLife() {
	m.lifeMu.Lock()
	defer m.lifeMu.Unlock()
	if m.life == nil || m.life.Err() != nil {
		m.life, m.lifeCancel = context.WithCancel(context.Background())
	}
}

// endJoins cancels every join and waits for the one in flight to return —
// its `tailscale up` is killed with the context — so that nothing
// advertises the route after withdrawRoutes has taken it back. Joins asked
// for afterwards find the context already ended and do nothing.
func (m *Manager) endJoins() {
	m.lifeMu.Lock()
	if m.life == nil {
		m.life, m.lifeCancel = context.WithCancel(context.Background())
	}
	m.lifeCancel()
	m.lifeMu.Unlock()
	m.upMu.Lock()
	m.upMu.Unlock()
}

func (m *Manager) clearSnapshot() {
	m.mu.Lock()
	m.snap, m.snapAt = nil, time.Time{}
	m.mu.Unlock()
}

// withdrawRoutes stops advertising before the daemon goes away.
//
// This is not tidiness. A device that has accepted fog's route reaches
// 192.168.1.200 *only* through the tailnet — the host route beats its own
// on-link LAN route — so a daemon that simply dies leaves every such device
// pointing into a dead tunnel, unable to reach fog even sitting on the same
// switch, until the coordination server gets round to withdrawing the route
// for an offline node. Withdrawing while still online pushes that
// withdrawal to peers immediately, and they fall back to the LAN path they
// had all along.
//
// Best effort and firmly bounded: a stop that hangs on an unreachable
// control plane would be worse than the problem.
func (m *Manager) withdrawRoutes() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := m.cli(ctx, "set", "--advertise-routes="); err != nil {
		m.Log.Warn("vpn: could not withdraw the route before stopping", "err", err)
		return
	}
	m.Log.Info("vpn: route withdrawn; peers fall back to the LAN")
	// Give the netmap update a moment to reach peers before the tunnel
	// they'd learn it over goes away.
	select {
	case <-ctx.Done():
	case <-time.After(routeWithdrawSettle):
	}
}

// routeWithdrawSettle is how long Stop waits after withdrawing before
// killing the daemon. Long enough for the control plane to push the change,
// short enough that turning a switch off still feels like turning it off.
var routeWithdrawSettle = 1500 * time.Millisecond

// Refresh re-advertises when the route fog should be announcing has changed
// — a new DHCP lease, a cable moved to another router. internal/netwatch
// calls it for the same reason internal/dnssync gets kicked: the address
// this was all set up around is state the network can invalidate underfoot.
func (m *Manager) Refresh() {
	ps := m.proc().State()
	m.mu.Lock()
	cur := m.route
	m.mu.Unlock()
	running, desired := ps.Running, ps.Desired
	if !desired {
		// Autostart at boot can lose a race with DHCP: with no LAN address
		// yet there is no route to advertise, and Start refuses. The first
		// address the box gets is the moment to try again, which is exactly
		// when this is called.
		if m.Enabled() {
			if err := m.Start(false); err != nil {
				m.Log.Debug("vpn: still not startable", "err", err)
			}
		}
		return
	}
	if !running {
		return
	}
	if want, err := m.DesiredRoute(); err != nil || want == cur {
		return
	}
	// The re-join can take minutes (the socket wait, then `tailscale up`
	// against a control plane that may be slow), and the caller is the
	// network watcher, whose next change — the one dnssync needs to hear —
	// must not wait behind it. So it runs aside, after any join already in
	// flight, and reads the route only once its turn comes: the address may
	// have moved again by then.
	if !m.refreshQueued.CompareAndSwap(false, true) {
		return
	}
	go func() {
		m.upMu.Lock()
		defer m.upMu.Unlock()
		m.refreshQueued.Store(false)
		m.mu.Lock()
		cur := m.route
		m.mu.Unlock()
		want, err := m.DesiredRoute()
		if err != nil || want == cur {
			return
		}
		m.Log.Info("vpn: LAN address changed, re-advertising", "was", cur, "now", want)
		m.upLocked(want, false)
	}()
}

// Enabled reports whether the operator left away access on, for autostart.
func (m *Manager) Enabled() bool {
	v, _ := m.Store.GetSetting(SettingEnabled)
	return v == "1"
}

// command builds one tailscaled run, for the route fog should advertise
// right now.
func (m *Manager) command() (*exec.Cmd, error) {
	route, err := m.DesiredRoute()
	if err != nil {
		return nil, err
	}
	m.loopRoute = route
	return exec.Command(m.daemonBin(),
		"--state="+m.statePath(),
		"--socket="+m.sockPath(),
		"--statedir="+m.stateDir(),
	), nil
}

// started joins the tailnet alongside the daemon, not before it: `tailscale
// up` talks to the socket tailscaled is still opening.
func (m *Manager) started(pid int) {
	go m.up(m.loopRoute, m.takeReauth())
}

// up brings the node into the tailnet with the route fog wants advertised,
// once any join already in flight has finished.
func (m *Manager) up(route string, reauth bool) {
	m.upMu.Lock()
	defer m.upMu.Unlock()
	m.upLocked(route, reauth)
}

// upLocked is up for a caller already holding upMu. A join asked for after
// Stop or Shutdown does nothing: the route is being, or has been, withdrawn.
func (m *Manager) upLocked(route string, reauth bool) {
	life := m.lifeCtx()
	if life.Err() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(life, 4*time.Minute)
	defer cancel()
	err := m.bringUp(ctx, route, reauth)
	if life.Err() != nil {
		// Cut short by a stop: not a failure worth showing, and whatever it
		// half-did is about to be withdrawn.
		return
	}
	m.mu.Lock()
	if err != nil {
		m.upErr = err.Error()
	} else {
		m.upErr = ""
		m.route = route
	}
	// Whatever just changed, the cached reading predates it.
	m.snap, m.snapAt = nil, time.Time{}
	m.mu.Unlock()
	if err != nil {
		m.Log.Warn("tailscale up failed", "err", err, "routes", route)
	} else {
		m.Log.Info("tailscale up", "routes", route)
	}
}

func (m *Manager) bringUp(ctx context.Context, route string, reauth bool) error {
	if err := m.waitForSocket(ctx); err != nil {
		return err
	}
	key, hasKey := m.authKey()
	if reauth && hasKey {
		c, cancel := context.WithTimeout(ctx, time.Minute)
		if _, err := m.cli(c, "logout"); err != nil {
			m.Log.Warn("tailscale logout before rejoin", "err", err)
		}
		cancel()
	}
	// A node that already holds a key rejoins from its own state; spending
	// a single-use auth key on every fogd restart would burn it.
	needKey := reauth
	if !needKey {
		c, cancel := context.WithTimeout(ctx, 10*time.Second)
		snap, err := m.readStatus(c, route)
		cancel()
		needKey = err != nil || !snap.HaveNodeKey
	}

	args := []string{
		"up",
		"--advertise-routes=" + route,
		// fog is a door into this house, not the way out of wherever the
		// laptop is: taking the tailnet's routes or DNS would change how
		// the server itself reaches the internet, which nobody asked for.
		"--accept-routes=false",
		"--accept-dns=false",
		"--hostname=" + m.hostname(),
		// Every preference fog cares about is named above, so --reset makes
		// this idempotent: without it, `up` refuses to run whenever a flag
		// differs from the stored prefs.
		"--reset",
	}
	if needKey && hasKey {
		// file: rather than the key itself — an auth key on the command
		// line is readable by every process on the box via ps.
		path, cleanup, err := m.keyFile(key)
		if err != nil {
			return err
		}
		defer cleanup()
		args = append(args, "--authkey=file:"+path)
	}
	c, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	_, err := m.cli(c, args...)
	return err
}

// keyFile stages the auth key where only fogd can read it, for the file:
// form of --authkey.
func (m *Manager) keyFile(key string) (string, func(), error) {
	f, err := os.CreateTemp(m.stateDir(), ".authkey-*")
	if err != nil {
		return "", nil, err
	}
	name := f.Name()
	cleanup := func() { os.Remove(name) }
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		cleanup()
		return "", nil, err
	}
	if _, err := f.WriteString(key + "\n"); err != nil {
		f.Close()
		cleanup()
		return "", nil, err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", nil, err
	}
	return name, cleanup, nil
}

// waitForSocket blocks until tailscaled is listening, so the first `up`
// doesn't race the daemon's startup.
func (m *Manager) waitForSocket(ctx context.Context) error {
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(m.sockPath()); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("tailscaled did not open its socket within 30s")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (m *Manager) readStatus(ctx context.Context, route string) (Snapshot, error) {
	out, err := m.cli(ctx, "status", "--json")
	if err != nil {
		return Snapshot{}, err
	}
	return parseStatus(out, route)
}

func (m *Manager) cli(ctx context.Context, args ...string) ([]byte, error) {
	full := append([]string{"--socket=" + m.sockPath()}, args...)
	cmd := execx.Command(ctx, m.cliBin(), full...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		if msg == "" {
			return out.Bytes(), err
		}
		return out.Bytes(), fmt.Errorf("%s", msg)
	}
	return out.Bytes(), nil
}
