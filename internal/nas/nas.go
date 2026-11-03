// Package nas mounts network shares (NFS, SMB/CIFS) on the host so apps can
// use them as host-path volumes and backup targets. Fog deliberately does not
// touch /etc/fstab: the store holds desired state and fogd re-mounts enabled
// shares at every start, the same ownership model as the rest of the system.
// The mount helpers (nfs-common / cifs-utils) are installed on first use,
// like Docker and cloudflared are.
package nas

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"gitea.lan/matthew/fog/internal/hostpkg"
	"gitea.lan/matthew/fog/internal/loop"
	"gitea.lan/matthew/fog/internal/store"
	"gitea.lan/matthew/fog/internal/vault"
)

// MountRoot is where shares land unless the operator picks a path.
const MountRoot = "/mnt/nas"

// PasswordSecret is the vault key holding a CIFS share's password.
func PasswordSecret(name string) string { return "nas_" + name + "_password" }

type Manager struct {
	Store   *store.Store
	Vault   *vault.Vault
	DataDir string
	Log     *slog.Logger

	// OnLateMount, when set, is called (on its own goroutine) each time a
	// share is mounted, whatever mounted it — the reconcile loop, a deploy's
	// EnsureForPaths, the operator's Mount button: the signal that anything
	// started against the bare directory needs a restart to see the share,
	// and that apps waiting for it can start.
	OnLateMount func(sh store.NasShare)

	mu      sync.Mutex
	lastErr map[string]string // share name -> most recent mount/unmount error

	// mpLocks serializes every mount/unmount touching one mount point (see
	// lockPoint). Check-then-mount without it is a race: the reconcile loop
	// and a deploy's EnsureForPaths can each see "not mounted" and both run
	// mount(8), and Linux stacks the second mount over the first — a later
	// umount "succeeds" and reveals the hidden lower mount.
	mpMu    sync.Mutex
	mpLocks map[string]*sync.Mutex
}

// Test seams. Real mounts need root, mount(8), and a Linux mount namespace;
// these vars let tests exercise the orchestration around them — per-point
// locking, stale detection, retry flow — on a dev machine.
var (
	// linuxHost gates the paths that actually run mount(8).
	linuxHost = runtime.GOOS == "linux"
	// mountedPoints is how this package reads the mount table internally.
	mountedPoints = MountedPoints
	// hostRun executes one host-namespace command and returns its combined
	// output (the hostCmd path in production).
	hostRun = hostpkg.Runner(hostpkg.Run)
	// uidMapPath is read by MountsBlocked to tell a user namespace apart.
	uidMapPath = "/proc/self/uid_map"
)

// MountsBlocked says why this host can never mount a network share itself,
// or "" when it can. The one case is a user namespace — an unprivileged LXC
// container, where root is some unprivileged uid on the real host. The kernel
// refuses NFS and CIFS mounts there whatever the container's features say,
// and mount(8) reports only a bare "Operation not permitted". A share that is
// already mounted at its mount point (bound in from the host) still works:
// Mount accepts a live mount it finds there.
func MountsBlocked() string {
	b, err := os.ReadFile(uidMapPath)
	if err != nil {
		return "" // not Linux, or no /proc: nothing to say
	}
	// The initial namespace maps uid 0 to itself; a user namespace maps it
	// elsewhere (Proxmox's default is 0 → 100000).
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) == 3 && f[0] == "0" && f[1] == "0" {
			return ""
		}
	}
	return "fog is running in an unprivileged container, which can't mount network shares. " +
		"Mount the share on the host and bind it into the container at this share's mount point — " +
		"fog uses a share it finds already mounted there — or make the container privileged. " +
		"On Proxmox: pct set <ctid> -mp0 <host path>,mp=<mount point>."
}

// ShareStatus is a stored share plus its live state on the host.
type ShareStatus struct {
	store.NasShare
	Mounted bool `json:"mounted"`
	// Stale: listed in /proc/mounts but the filesystem no longer answers —
	// the server behind a hard mount went away (moved, renumbered, off).
	Stale      bool   `json:"stale,omitempty"`
	TotalBytes uint64 `json:"totalBytes,omitempty"`
	FreeBytes  uint64 `json:"freeBytes,omitempty"`
	LastError  string `json:"lastError,omitempty"`
}

// statfsProbe is one in-flight Statfs. The raw syscall against a hard NFS
// mount whose server vanished blocks in uninterruptible sleep — from fog's
// side, forever — so it runs on its own goroutine with a deadline, and at
// most one probe per mount point exists at a time: while one is stuck, every
// caller reads "stale" instead of stacking more D-state goroutines.
type statfsProbe struct {
	done chan struct{}
	fs   syscall.Statfs_t
	err  error
}

// The probe registry is package-wide, not per-Manager: the invariant — at
// most one in-flight Statfs per mount point, every other caller reading
// "stale" — must hold across the whole process (status pages, the backup
// target guard), or two subsystems asking about the same dead mount would
// each park a goroutine in D-state.
var (
	probeMu sync.Mutex
	probes  map[string]*statfsProbe // mount point -> in-flight statfs

	// statfsFn and statfsTimeout are seams for tests, which cannot conjure a
	// genuinely hung hard mount on demand.
	statfsFn      = syscall.Statfs
	statfsTimeout = 2 * time.Second
)

// Statfs is the timeout-safe statfs(2), for any path that may sit on a
// network share: free-space checks before a copy, status pages. stale=true
// means the call didn't return in time; the fs value is only meaningful
// when stale is false and err is nil.
func Statfs(path string) (fs syscall.Statfs_t, stale bool, err error) {
	probeMu.Lock()
	p, inflight := probes[path]
	if !inflight {
		p = &statfsProbe{done: make(chan struct{})}
		if probes == nil {
			probes = map[string]*statfsProbe{}
		}
		probes[path] = p
		go func() {
			p.err = statfsFn(path, &p.fs)
			probeMu.Lock()
			delete(probes, path)
			probeMu.Unlock()
			close(p.done)
		}()
	}
	probeMu.Unlock()
	select {
	case <-p.done:
		return p.fs, false, p.err
	case <-time.After(statfsTimeout):
		return syscall.Statfs_t{}, true, nil
	}
}

// Unresponsive reports a mounted path that no longer answers — a hard
// NFS/CIFS mount whose server went away. Guards that key on MountedPoints
// alone must also ask this: a dead hard mount passes the table check and then
// hangs the first real I/O in uninterruptible sleep. It is only the bounded
// probe; callers check the mount table themselves.
func Unresponsive(path string) bool {
	_, stale, _ := Statfs(path)
	return stale
}

// ShareState says whether path is usable as far as the stored shares go:
// name and reason are "" when it is (or sits under no share), otherwise the
// share it sits under and a short phrase for messages. A share is unusable
// both when it is absent from the mount table and when it is mounted but no
// longer answering — a hard mount whose server died, where any file I/O
// blocks in uninterruptible sleep and whatever walked in never comes back.
//
// mounted is the mount table (MountedPoints) and unresponsive the probe
// (Unresponsive); callers pass their own so their tests can simulate a
// dead share.
func ShareState(shares []store.NasShare, path string, mounted map[string]bool, unresponsive func(string) bool) (name, reason string) {
	for _, sh := range shares {
		if path != sh.MountPoint && !strings.HasPrefix(path, sh.MountPoint+"/") {
			continue
		}
		if !mounted[sh.MountPoint] {
			return sh.Name, "not mounted"
		}
		if unresponsive(sh.MountPoint) {
			return sh.Name, "mounted but not responding"
		}
	}
	return "", ""
}

func (m *Manager) setErr(name, msg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lastErr == nil {
		m.lastErr = map[string]string{}
	}
	if msg == "" {
		delete(m.lastErr, name)
	} else {
		m.lastErr[name] = msg
	}
}

func (m *Manager) getErr(name string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastErr[name]
}

// Statuses returns every stored share with its live mount state.
func (m *Manager) Statuses() ([]ShareStatus, error) {
	shares, err := m.Store.ListNasShares()
	if err != nil {
		return nil, err
	}
	mounts := mountedPoints()
	out := make([]ShareStatus, 0, len(shares))
	for _, sh := range shares {
		st := ShareStatus{NasShare: sh, Mounted: mounts[sh.MountPoint], LastError: m.getErr(sh.Name)}
		if st.Mounted {
			fs, stale, err := Statfs(sh.MountPoint)
			switch {
			case stale:
				st.Stale = true
				if st.LastError == "" {
					st.LastError = "mounted but not responding — the server may be unreachable or at a new address"
				}
			case err == nil:
				st.TotalBytes = fs.Blocks * uint64(fs.Bsize)
				st.FreeBytes = fs.Bavail * uint64(fs.Bsize)
			}
		}
		out = append(out, st)
	}
	return out, nil
}

// Validate checks a share definition before it is stored.
func Validate(sh store.NasShare) error {
	switch sh.Proto {
	case "nfs", "cifs":
	default:
		return fmt.Errorf("protocol must be nfs or cifs")
	}
	if sh.Server == "" {
		return fmt.Errorf("server is required")
	}
	if sh.Remote == "" {
		return fmt.Errorf("share path is required")
	}
	if sh.Proto == "nfs" && !strings.HasPrefix(sh.Remote, "/") {
		return fmt.Errorf("NFS exports are absolute paths (e.g. /volume1/media)")
	}
	if !filepath.IsAbs(sh.MountPoint) {
		return fmt.Errorf("mount point must be an absolute path")
	}
	if clean := filepath.Clean(sh.MountPoint); clean != sh.MountPoint || clean == "/" {
		return fmt.Errorf("mount point %q is not a clean absolute path", sh.MountPoint)
	}
	// Refuse mount points that would shadow system or fog-owned paths.
	for _, bad := range []string{"/bin", "/boot", "/dev", "/etc", "/lib", "/proc", "/root", "/run", "/sbin", "/sys", "/usr", "/var"} {
		if sh.MountPoint == bad || strings.HasPrefix(sh.MountPoint, bad+"/") {
			return fmt.Errorf("mount point under %s is not allowed", bad)
		}
	}
	if strings.ContainsAny(sh.Options, " \t\n") {
		return fmt.Errorf("options must be a single comma-separated list")
	}
	return nil
}

// lockPoint returns an unlock func after taking the mutex that serializes
// all mount/unmount work on one mount point. The lock must cover the whole
// check-then-act sequence — the invariant is that at most one goroutine sits
// between "is it mounted?" and the mount(8)/umount(8) acting on the answer.
func (m *Manager) lockPoint(mp string) func() {
	m.mpMu.Lock()
	if m.mpLocks == nil {
		m.mpLocks = map[string]*sync.Mutex{}
	}
	mu := m.mpLocks[mp]
	if mu == nil {
		mu = &sync.Mutex{}
		m.mpLocks[mp] = mu
	}
	m.mpMu.Unlock()
	mu.Lock()
	return mu.Unlock
}

// Mount mounts the share, installing the required helper package first if
// it's missing. Blocking; callers get a definitive result. Serialized per
// mount point: concurrent callers wait, then see the fresh mount table and
// return without stacking a second mount over the first.
func (m *Manager) Mount(ctx context.Context, sh store.NasShare) error {
	defer m.lockPoint(sh.MountPoint)()
	return m.mount(ctx, sh)
}

// mount is Mount without the per-point lock; the caller holds it.
func (m *Manager) mount(ctx context.Context, sh store.NasShare) error {
	if !linuxHost {
		return fmt.Errorf("NAS mounts are only supported on Linux hosts")
	}
	if err := m.ensureHelper(ctx, sh.Proto); err != nil {
		m.setErr(sh.Name, err.Error())
		return err
	}
	if err := os.MkdirAll(sh.MountPoint, 0o755); err != nil {
		m.setErr(sh.Name, err.Error())
		return err
	}
	if mountedPoints()[sh.MountPoint] {
		if _, stale, _ := Statfs(sh.MountPoint); !stale {
			m.setErr(sh.Name, "")
			return nil
		}
		// Mounted on paper, dead in practice. Reporting success here would
		// make the Mount button lie; detach the corpse and mount fresh.
		if err := m.forceUnmount(ctx, sh); err != nil {
			return fmt.Errorf("share is mounted but not responding, and detaching it failed: %w", err)
		}
	}
	if msg := MountsBlocked(); msg != "" {
		m.setErr(sh.Name, msg)
		return errors.New(msg)
	}

	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	var mountArgs []string
	var cleanup func()
	switch sh.Proto {
	case "nfs":
		mountArgs = []string{"-t", "nfs"}
		if sh.Options != "" {
			mountArgs = append(mountArgs, "-o", sh.Options)
		}
		mountArgs = append(mountArgs, sh.Server+":"+sh.Remote, sh.MountPoint)
	case "cifs":
		opts := []string{}
		if sh.Username != "" {
			// Credentials go through a 0600 file, never argv — argv is
			// world-readable in /proc while mount runs.
			pw, err := m.password(sh.Name)
			if err != nil {
				m.setErr(sh.Name, err.Error())
				return err
			}
			credFile, err := m.writeCredFile(sh.Username, pw)
			if err != nil {
				m.setErr(sh.Name, err.Error())
				return err
			}
			cleanup = func() { os.Remove(credFile) }
			opts = append(opts, "credentials="+credFile)
		} else {
			opts = append(opts, "guest")
		}
		// uid/gid 1000: fog's app containers run as 1000, and CIFS has no
		// server-side ownership to preserve.
		opts = append(opts, "uid=1000", "gid=1000")
		if sh.Options != "" {
			opts = append(opts, sh.Options)
		}
		share := "//" + sh.Server + "/" + strings.TrimPrefix(sh.Remote, "/")
		mountArgs = []string{"-t", "cifs", "-o", strings.Join(opts, ","), share, sh.MountPoint}
	default:
		return fmt.Errorf("unknown protocol %q", sh.Proto)
	}
	if cleanup != nil {
		defer cleanup()
	}
	if out, err := hostRun(ctx, nil, "mount", mountArgs...); err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		if ctx.Err() != nil {
			msg = "timed out — is the server reachable? " + msg
		}
		m.setErr(sh.Name, msg)
		return fmt.Errorf("mount failed: %s", msg)
	}
	m.setErr(sh.Name, "")
	m.Log.Info("nas share mounted", "name", sh.Name, "at", sh.MountPoint)
	if m.OnLateMount != nil {
		go m.OnLateMount(sh)
	}
	return nil
}

// Unmount unmounts the share's mount point. The directory is left in place.
// Serialized per mount point with Mount, so an unmount can't interleave with
// a reconcile that is mid-way through mounting the same path.
func (m *Manager) Unmount(ctx context.Context, sh store.NasShare) error {
	defer m.lockPoint(sh.MountPoint)()
	if !mountedPoints()[sh.MountPoint] {
		m.setErr(sh.Name, "")
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if out, err := hostRun(ctx, nil, "umount", sh.MountPoint); err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		if strings.Contains(msg, "target is busy") {
			msg += " — an app is probably using this share; stop it first"
		}
		m.setErr(sh.Name, msg)
		return fmt.Errorf("unmount failed: %s", msg)
	}
	m.setErr(sh.Name, "")
	m.Log.Info("nas share unmounted", "name", sh.Name, "at", sh.MountPoint)
	return nil
}

// ForceUnmount detaches a mount that a normal unmount can't shift: first
// umount -f (tells NFS to abandon the dead server), then umount -l (lazy
// detach — the path leaves the namespace now, the kernel finishes when the
// last user lets go). This is the escape hatch for a share whose server
// vanished mid-mount — the state a normal umount hangs on.
func (m *Manager) ForceUnmount(ctx context.Context, sh store.NasShare) error {
	defer m.lockPoint(sh.MountPoint)()
	return m.forceUnmount(ctx, sh)
}

// forceUnmount is ForceUnmount without the per-point lock; the caller holds
// it (mount takes it once and detaches a dead mount under the same hold).
func (m *Manager) forceUnmount(ctx context.Context, sh store.NasShare) error {
	if !mountedPoints()[sh.MountPoint] {
		m.setErr(sh.Name, "")
		return nil
	}
	fctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if _, err := hostRun(fctx, nil, "umount", "-f", sh.MountPoint); err == nil {
		m.setErr(sh.Name, "")
		m.Log.Info("nas share force-unmounted", "name", sh.Name, "at", sh.MountPoint)
		return nil
	}
	lctx, cancelLazy := context.WithTimeout(ctx, 20*time.Second)
	defer cancelLazy()
	if out, err := hostRun(lctx, nil, "umount", "-l", sh.MountPoint); err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		m.setErr(sh.Name, msg)
		return fmt.Errorf("force unmount failed: %s", msg)
	}
	m.setErr(sh.Name, "")
	m.Log.Info("nas share lazily detached", "name", sh.Name, "at", sh.MountPoint)
	return nil
}

// MountLoop is the mount reconcile: a pass at boot, then retries that back
// off while shares stay down and settle to a slow patrol once everything is
// up. A NAS that answers minutes after fogd — the normal order when a whole
// house powers on at once — gets mounted without anyone clicking anything.
// Failures never block fogd; they surface per-share and as issues.
func (m *Manager) MountLoop(ctx context.Context) {
	const minDelay = 15 * time.Second
	const maxDelay = 5 * time.Minute
	delay := minDelay
	loop.Run(ctx, 0, nil, func(ctx context.Context) time.Duration {
		if m.reconcile(ctx) {
			delay = maxDelay // all enabled shares up; patrol slowly
		} else {
			delay = min(delay*2, maxDelay)
		}
		return delay
	})
}

// reconcile mounts every enabled share that isn't mounted. Returns true when
// nothing is left unmounted. A share that mounts here arrived later than any
// container Docker's restart policy already started, so OnLateMount fires
// for each success (Mount fires it).
func (m *Manager) reconcile(ctx context.Context) bool {
	shares, err := m.Store.ListNasShares()
	if err != nil {
		m.Log.Error("nas: listing shares", "err", err)
		return true
	}
	// The table read here is only a fast-path filter; Mount re-checks under
	// the per-point lock, which is what makes a concurrent EnsureForPaths
	// safe rather than a second stacked mount.
	mounts := mountedPoints()
	ok := true
	for _, sh := range shares {
		if !sh.Enabled || mounts[sh.MountPoint] {
			continue
		}
		// Mount fires OnLateMount itself, as every mount does.
		if err := m.Mount(ctx, sh); err != nil {
			ok = false
			m.Log.Warn("nas: mount reconcile failed; will retry", "name", sh.Name, "err", err)
			continue
		}
	}
	return ok
}

// EnsureForPaths mounts any stored share whose mount point covers one of the
// given host paths — the deploy engine calls this so an app whose volumes
// live on a NAS never starts against an empty directory. A share mounted this
// way is re-enabled: an app depending on it is the strongest statement that
// it should come back at boot.
func (m *Manager) EnsureForPaths(ctx context.Context, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	shares, err := m.Store.ListNasShares()
	if err != nil {
		return err
	}
	// Fast-path filter only; Mount holds the per-point lock and re-checks, so
	// a deploy racing the reconcile loop cannot stack a second mount.
	mounts := mountedPoints()
	for _, sh := range shares {
		needed := false
		for _, p := range paths {
			if p == sh.MountPoint || strings.HasPrefix(p, sh.MountPoint+"/") {
				needed = true
				break
			}
		}
		if !needed || mounts[sh.MountPoint] {
			continue
		}
		if err := m.Mount(ctx, sh); err != nil {
			return fmt.Errorf("NAS share %s (%s) is needed by this app but wouldn't mount: %w", sh.Name, sh.MountPoint, err)
		}
		if !sh.Enabled {
			m.Store.SetNasShareEnabled(sh.Name, true)
			m.Log.Info("nas: share re-enabled — an app deploy depends on it", "name", sh.Name)
		}
	}
	return nil
}

func (m *Manager) password(name string) (string, error) {
	pw, err := vault.Secrets{Store: m.Store, Vault: m.Vault}.Get(0, PasswordSecret(name))
	if errors.Is(err, store.ErrNotFound) {
		return "", fmt.Errorf("no password saved for share %s", name)
	}
	return pw, err
}

func (m *Manager) writeCredFile(user, pass string) (string, error) {
	f, err := os.CreateTemp(m.DataDir, "cifs-cred-*")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if err := f.Chmod(0o600); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	if _, err := fmt.Fprintf(f, "username=%s\npassword=%s\n", user, pass); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// helperPkg maps a protocol to the Debian/Ubuntu package providing its mount
// helper and the binary that proves it's installed.
func helperPkg(proto string) (pkg, bin string) {
	if proto == "cifs" {
		return "cifs-utils", "mount.cifs"
	}
	return "nfs-common", "mount.nfs"
}

// helperInstalled is a var so tests can pretend the helper exists on hosts
// where it doesn't.
var helperInstalled = func(bin string) bool {
	if _, err := exec.LookPath(bin); err == nil {
		return true
	}
	// Mount helpers live in sbin dirs that may be off PATH under systemd.
	for _, dir := range []string{"/sbin", "/usr/sbin", "/usr/local/sbin"} {
		if fi, err := os.Stat(filepath.Join(dir, bin)); err == nil && fi.Mode()&0o111 != 0 {
			return true
		}
	}
	return false
}

// HelpersInstalled reports which protocols are ready to mount without an
// install step — surfaced in the UI so the first mount's extra wait is
// explicable.
func HelpersInstalled() map[string]bool {
	return map[string]bool{
		"nfs":  helperInstalled("mount.nfs"),
		"cifs": helperInstalled("mount.cifs"),
	}
}

// ensureHelper installs the protocol's mount helper package if missing.
// Same philosophy as the Docker and cloudflared installers: Fog does the
// boring host setup itself.
func (m *Manager) ensureHelper(ctx context.Context, proto string) error {
	pkg, bin := helperPkg(proto)
	if helperInstalled(bin) {
		return nil
	}
	m.Log.Info("nas: installing mount helper", "pkg", pkg)
	err := hostpkg.Install(ctx, hostRun, pkg)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, hostpkg.ErrNoApt):
		return fmt.Errorf("%s is missing and this host has no apt-get — install %s manually", bin, pkg)
	case errors.Is(err, hostpkg.ErrOffline):
		// A NAS mount attempted before the uplink is live must read as "no
		// internet yet", not as a package-manager mystery.
		return fmt.Errorf("mounting needs the %s package, and installing it failed because this server has no internet access yet — retry once the network is up", pkg)
	}
	return err
}

// MountedPoints parses /proc/mounts into the set of active mount points.
func MountedPoints() map[string]bool {
	out := map[string]bool{}
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			out[decodeMountEscapes(fields[1])] = true
		}
	}
	return out
}

// decodeMountEscapes decodes the \NNN octal escapes /proc/mounts uses for
// characters that would break its space-separated format — \040 space,
// \011 tab, \012 newline, \134 backslash. All must decode, not just space:
// a mount point containing any of them would otherwise mis-key every map
// lookup, silently defeating the share guards that key on these paths.
// A backslash not followed by three octal digits passes through verbatim.
func decodeMountEscapes(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) && isOctal(s[i+1]) && isOctal(s[i+2]) && isOctal(s[i+3]) {
			b.WriteByte((s[i+1]-'0')<<6 | (s[i+2]-'0')<<3 | (s[i+3] - '0'))
			i += 3
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isOctal(c byte) bool { return c >= '0' && c <= '7' }
