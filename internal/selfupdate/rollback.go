package selfupdate

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gitea.lan/matthew/fog/internal/config"
	"gitea.lan/matthew/fog/internal/fsx"
	"gitea.lan/matthew/fog/internal/store"
	"gitea.lan/matthew/fog/packaging"
)

// A self-update is on trial until the new fogd proves itself. Apply writes a
// marker beside the binary (fogd.update-pending: the two versions, both
// binaries' sha256, and a deadline) before it swaps anything. The new fogd,
// once it has answered its own health check for healthyFor, deletes the
// marker: the update is confirmed. Nothing else deletes it in time when the
// new fogd can't start, crash-loops, or hangs — and then fogd-rollback
// (packaging/fogd-rollback), a shell script that needs nothing from either
// binary, puts fogd.prev back and leaves fogd.rolled-back for the restored
// fogd to post to the Inbox.
//
// fogd-rollback runs from two places, so either alone is enough:
//   - ExecStartPre=- in fogd.service, before every start. A crash-looping
//     fogd is restarted every few seconds; the first start after the
//     deadline starts the old binary instead.
//   - a one-shot systemd timer Apply arms with systemd-run at the deadline,
//     which covers a unit installed before the ExecStartPre line existed and
//     a fogd that hangs without being restarted.
//
// It can't loop: the marker is claimed before anything is restored, so an
// update rolls back at most once; the restored fogd marks the failed
// version ignored and never re-applies anything by itself.

const (
	pendingSuffix    = ".update-pending"
	rolledBackSuffix = ".rolled-back"
	guardName        = "fogd-rollback"
	unitName         = "fogd.service"

	// defaultGuardDelay bounds how long a new fogd has to become healthy:
	// systemd's start timeout (90s) for a hang before READY, plus room for
	// a slow first start's migrations, plus healthyFor.
	defaultGuardDelay = 5 * time.Minute
	// healthyFor is how long the new fogd must keep answering before it
	// confirms itself — long enough that a crash shortly after start is
	// still caught.
	healthyFor = 30 * time.Second
)

// pending is the marker's content: key=value lines, read by the shell guard
// with sed, so no value may hold a newline (versions are checked by
// parseVersion, digests are hex).
type pending struct {
	// Kind is "binary" (a new fogd swapped in) or "units" (new unit files
	// installed under the running fogd; see syncUnits).
	Kind                   string
	ToVersion, FromVersion string
	ToSHA, FromSHA         string
	Deadline               time.Time
	Unit                   string
	// For a units trial: the socket unit, where unit files live, where the
	// new and previous ones are staged, fogd's listen address (the socket
	// takes the same port), and the hash a failed change is refused by.
	Socket, UnitDir, Stage, Listen, UnitsHash string
}

func (p pending) encode() []byte {
	var b bytes.Buffer
	kind := p.Kind
	if kind == "" {
		kind = "binary"
	}
	fmt.Fprintf(&b, "kind=%s\nto_version=%s\nfrom_version=%s\nto_sha256=%s\nfrom_sha256=%s\ndeadline=%d\nunit=%s\n",
		kind, p.ToVersion, p.FromVersion, p.ToSHA, p.FromSHA, ceilUnix(p.Deadline), p.Unit)
	if kind == "units" {
		fmt.Fprintf(&b, "socket=%s\nunit_dir=%s\nstage=%s\nlisten=%s\nunits_hash=%s\n",
			p.Socket, p.UnitDir, p.Stage, p.Listen, p.UnitsHash)
	}
	return b.Bytes()
}

// ceilUnix is t in whole seconds, rounded up. The guard compares the
// deadline with `date +%s`, which rounds down; rounding the deadline down
// too (Unix()) let the guard act up to a second before the change's time
// was up. Rounded up, it never acts early, only up to a second late.
func ceilUnix(t time.Time) int64 {
	s := t.Unix()
	if t.Nanosecond() > 0 {
		s++
	}
	return s
}

func parseKV(r io.Reader) map[string]string {
	kv := map[string]string{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		if line == "--- journal" {
			break
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			if _, seen := kv[k]; !seen {
				kv[k] = v
			}
		}
	}
	return kv
}

func readPending(path string) (pending, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return pending{}, err
	}
	kv := parseKV(bytes.NewReader(b))
	secs, err := strconv.ParseInt(kv["deadline"], 10, 64)
	if err != nil {
		return pending{}, fmt.Errorf("%s: no readable deadline", path)
	}
	return pending{
		Kind:      kv["kind"],
		ToVersion: kv["to_version"], FromVersion: kv["from_version"],
		ToSHA: kv["to_sha256"], FromSHA: kv["from_sha256"],
		Deadline: time.Unix(secs, 0), Unit: kv["unit"],
		Socket: kv["socket"], UnitDir: kv["unit_dir"], Stage: kv["stage"],
		Listen: kv["listen"], UnitsHash: kv["units_hash"],
	}, nil
}

// underSystemd reports whether this fogd is a systemd service: systemd sets
// INVOCATION_ID for every unit it starts. Outside one (a dev run) there is
// nothing to restart into and no guard to arm.
func underSystemd() bool { return os.Getenv("INVOCATION_ID") != "" }

// installGuard writes fogd-rollback beside the executable — this binary's
// copy, so the guard always matches the marker format of the fogd that
// wrote the marker.
func installGuard(dir string) (string, error) {
	p := filepath.Join(dir, guardName)
	err := fsx.WriteAtomic(p, 0o755, func(w io.Writer) error {
		_, err := w.Write(packaging.Rollback)
		return err
	})
	if err != nil {
		return "", fmt.Errorf("installing %s: %w", guardName, err)
	}
	return p, nil
}

// armTimer starts the one-shot timer that runs the guard at the deadline.
// It lives in a transient unit of its own, outside fogd's cgroup, so it
// outlives the fogd that armed it and doesn't depend on the one replacing it.
func armTimer(ctx context.Context, guard string, after time.Duration) error {
	secs := int(after.Round(time.Second) / time.Second)
	unit := fmt.Sprintf("fogd-update-guard-%d", time.Now().UnixNano())
	cmd := exec.CommandContext(ctx, "systemd-run",
		"--unit="+unit,
		"--description=fogd self-update rollback guard",
		"--collect",
		fmt.Sprintf("--on-active=%ds", secs+5),
		"--timer-property=AccuracySec=1s",
		guard, "--timer")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("systemd-run: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// armGuard prepares the way back before Apply swaps anything: the guard
// script on disk and, under systemd, its timer. An error means the update
// must not go ahead.
func (u *Updater) armGuard(ctx context.Context, exe string, after time.Duration) error {
	guard, err := installGuard(filepath.Dir(exe))
	if err != nil {
		return err
	}
	if u.arm != nil {
		return u.arm(guard, after)
	}
	if !underSystemd() {
		return nil
	}
	if err := armTimer(ctx, guard, after); err != nil {
		return fmt.Errorf("couldn't arm the rollback guard, so this update would have no way back: %w", err)
	}
	return nil
}

func (u *Updater) guardDelay() time.Duration {
	if u.delay > 0 {
		return u.delay
	}
	return defaultGuardDelay
}

// confirmUpdate runs as fogd starts. When this fogd is the version an update
// is on trial with, it waits until the process has answered its health
// check continuously for healthyFor, then deletes the marker. It returns
// once the trial is settled either way, or when ctx ends.
func (u *Updater) confirmUpdate(ctx context.Context) {
	exe, err := u.executable()
	if err != nil {
		return
	}
	marker := exe + pendingSuffix
	p, err := readPending(marker)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	log := slog.Default().With("component", "selfupdate")
	if err != nil {
		// Unreadable: nothing can act on it, and it would block every
		// later update. The guard ignores it too.
		log.Warn("removing an unreadable update marker", "err", err)
		os.Remove(marker)
		return
	}
	if p.ToVersion != u.current() {
		// The marker isn't about this binary: the guard (or a person) has
		// already put another one in place.
		os.Remove(marker)
		return
	}
	log.Info("update on trial: confirming once healthy", "version", p.ToVersion, "deadline", p.Deadline)
	var since time.Time
	lastErr := ""
	tick := time.NewTicker(u.healthEvery())
	defer tick.Stop()
	for {
		err := u.healthy(ctx)
		// New unit files are healthy only with the socket they bring
		// actually holding the port.
		if err == nil && p.Kind == "units" && !u.unitActive(p.Socket) {
			err = fmt.Errorf("%s isn't active", p.Socket)
		}
		if err != nil {
			since = time.Time{}
			if err.Error() != lastErr {
				log.Warn("update on trial: not healthy yet", "err", err)
				lastErr = err.Error()
			}
		} else if since.IsZero() {
			since = time.Now()
		}
		if !since.IsZero() && time.Since(since) >= u.healthyWindow() {
			if err := os.Remove(marker); err == nil {
				log.Info("update confirmed", "version", p.ToVersion)
				u.Store.Audit("system", "fogd.self-update.confirmed", p.ToVersion, "")
			} else if !errors.Is(err, os.ErrNotExist) {
				log.Error("update healthy, but its marker can't be removed — the rollback guard will undo it", "err", err)
			}
			return
		}
		if _, err := os.Stat(marker); errors.Is(err, os.ErrNotExist) {
			return // the guard claimed it: a rollback is under way
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (u *Updater) healthEvery() time.Duration {
	if u.healthTick > 0 {
		return u.healthTick
	}
	return 5 * time.Second
}

func (u *Updater) healthyWindow() time.Duration {
	if u.healthWindow > 0 {
		return u.healthWindow
	}
	return healthyFor
}

// healthy is fogd's own view of itself: its database answers, and its HTTP
// listener serves the setup status every client opens with.
func (u *Updater) healthy(ctx context.Context) error {
	if _, err := u.Store.GetSetting(store.SettingSetupDone); err != nil {
		return fmt.Errorf("database: %w", err)
	}
	target := u.healthURL
	if target == "" {
		target = selfURL(config.FromEnv().ListenAddr)
	}
	if target == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", target, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s: HTTP %d", target, resp.StatusCode)
	}
	return nil
}

// selfURL is where this fogd answers itself, from its listen address: a
// wildcard listener is reached on loopback. Empty when it can't be told.
func selfURL(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil || port == "" {
		return ""
	}
	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/api/setup/status"
}

// reportRollback runs as fogd starts: when the guard put this binary back,
// the Inbox says so — which version failed, and the journal from its tries —
// and the failed version is marked ignored so the update prompt stops
// offering it. Settings › Server › Updates can still apply it on purpose.
func (u *Updater) reportRollback(send func(title, msg string, ok bool)) {
	exe, err := u.executable()
	if err != nil {
		return
	}
	path := exe + rolledBackSuffix
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	kv := parseKV(bytes.NewReader(b))
	failed, restored := kv["failed_version"], kv["restored_version"]
	journal := ""
	if _, j, ok := strings.Cut(string(b), "--- journal\n"); ok {
		journal = strings.TrimSpace(j)
	}
	if kv["kind"] == "units" {
		u.reportUnitsRollback(path, kv, journal, send)
		return
	}
	at := ""
	if secs, err := strconv.ParseInt(kv["at"], 10, 64); err == nil {
		at = " at " + time.Unix(secs, 0).UTC().Format(time.RFC3339)
	}
	// A new fogd that couldn't open its port failed because something else
	// held it — another process on this machine, in the moment fogd's own
	// listener was down — not because the release is bad. That release
	// isn't marked ignored, and the notice says what happened instead.
	// (Under fogd.socket systemd holds the port throughout, and this can't
	// happen.)
	portTaken := strings.Contains(strings.ToLower(journal), "address already in use")
	title := fmt.Sprintf("Fog %s didn't start, so %s is back", orUnknown(failed), orUnknown(restored))
	body := fmt.Sprintf("Fog updated itself to %s, but the new version never became healthy in the minutes it was given. "+
		"The previous version, %s, was put back automatically%s, and everything is running on it.\n\n"+
		"%s won't be offered again in the update prompt; Settings › Server › Updates can still apply it once a fix is out. "+
		"If the update changed the database before it failed, those changes are still there.",
		orUnknown(failed), orUnknown(restored), at, orUnknown(failed))
	if portTaken {
		title = fmt.Sprintf("Fog %s couldn't open its port, so %s is back", orUnknown(failed), orUnknown(restored))
		body = fmt.Sprintf("Fog updated itself to %s, but while it restarted another process on this machine took fog's port, "+
			"so the new version couldn't listen and never became healthy. The previous version, %s, was put back automatically%s.\n\n"+
			"That isn't a fault in %s, so it is still offered: apply it again from Settings › Server › Updates. "+
			"Something on this machine bound fog's port the moment it was free — find it with `ss -ltnp` and stop it. "+
			"Once this server is on fogd.socket (its next successful self-update installs it, as does re-running install.sh), systemd holds the port across restarts and this can't recur.",
			orUnknown(failed), orUnknown(restored), at, orUnknown(failed))
	}
	if journal != "" {
		body += "\n\nThe new version's last log lines:\n\n" + journal
	}
	if err := u.Store.CreateNotice("selfupdate-rolled-back", "", title, body); err != nil {
		return // keep the record: the next start tries again
	}
	if failed != "" && !portTaken {
		u.Store.SetSetting(store.SettingIgnoredUpdateVersion, failed)
	}
	u.Store.Audit("system", "fogd.self-update.rolled-back", failed, "")
	if send != nil {
		send(title, "The previous version was restored automatically. Details are in the Inbox.", false)
	}
	os.Remove(path)
}

func orUnknown(s string) string {
	if s == "" {
		return "an unknown version"
	}
	return s
}

// unitActive asks systemd whether a unit is active.
func (u *Updater) unitActive(name string) bool {
	if name == "" {
		return false
	}
	return exec.Command("systemctl", "is-active", "--quiet", name).Run() == nil
}

// --- the unit files follow the binary ------------------------------------
//
// A self-update replaces the binary; the unit files a server was installed
// with stay as they were — so a server installed before fogd.socket, the
// hardening or the ExecStartPre guard would never get them without
// install.sh being re-run. syncUnits closes that. It runs as fogd starts,
// once this fogd has proven itself (no trial pending): when the installed
// fogd.service and fogd.socket aren't the ones this fogd shipped with, it
// installs those, as a trial of their own.
//
// The unit files it installs are the release's: the same bytes release.sh
// lists (with their sha256) in the signed manifest, built into this binary
// from packaging/ at the same commit — and this binary was itself verified
// against that manifest before it ran. Changing units only once the binary
// is healthy keeps every step a single change with a known-good state
// behind it: a binary that fails rolls back to the old binary on the old
// units (nothing else changed); units that fail roll back to the old units
// under the binary that was healthy on them.
//
// fogd's sandbox keeps /etc read-only, so the change itself is done by
// fogd-rollback --install-units in a transient unit outside it: back up the
// installed files, install the new ones (atomically, drop-ins untouched,
// a socket port drop-in added when the server's FOG_LISTEN isn't the
// socket's default), daemon-reload, enable the socket, then stop fogd,
// start the socket on the freed port, and start fogd on it. The trial
// marker, the guard and its timer are in place first; the new fogd start
// confirms the trial only once healthy with the socket active. A unit
// change that fails is refused by its hash afterwards, so it isn't tried
// again until a release ships different units.

// SettingUnitsRefused holds the hash of unit files that were rolled back.
const SettingUnitsRefused = "selfupdate_units_refused"

func (u *Updater) unitDir() string {
	if u.units != nil && u.units.dir != "" {
		return u.units.dir
	}
	return "/etc/systemd/system"
}

func (u *Updater) unitNames() (service, socket string) {
	if u.units != nil && u.units.service != "" {
		return u.units.service, u.units.socket
	}
	return unitName, "fogd.socket"
}

func (u *Updater) unitTexts() (service, socket []byte) {
	if u.units != nil && u.units.text != nil {
		return u.units.text()
	}
	return packaging.Unit, packaging.Socket
}

// unitSeams points syncUnits at other unit names, files and text: the
// real-systemd tests run a scratch fogd-test.service beside the host's own.
type unitSeams struct {
	dir, service, socket string
	text                 func() (service, socket []byte)
	launch               func(guard string) error
}

func unitsHash(service, socket []byte) string {
	h := sha256.New()
	h.Write(service)
	h.Write([]byte{0})
	h.Write(socket)
	return hex.EncodeToString(h.Sum(nil))
}

// syncUnits brings the installed unit files to the ones this fogd shipped
// with (see above). It returns once the change is handed to systemd —
// which then restarts this fogd — or when there is nothing to do.
func (u *Updater) syncUnits(ctx context.Context) error {
	if !underSystemd() {
		return nil
	}
	exe, err := u.executable()
	if err != nil {
		return err
	}
	if _, err := os.Stat(exe + pendingSuffix); err == nil {
		return nil // a trial is open; this waits for the next start
	}
	service, socket := u.unitNames()
	dir := u.unitDir()
	installed, err := os.ReadFile(filepath.Join(dir, service))
	if err != nil {
		return nil // fogd's unit isn't where fog installs it: not ours to manage
	}
	// Only a unit that runs this binary is fog's to replace; a custom
	// layout is left as its owner made it.
	if !bytes.Contains(installed, []byte("ExecStart="+exe+"\n")) {
		return nil
	}
	newService, newSocket := u.unitTexts()
	hash := unitsHash(newService, newSocket)
	if refused, _ := u.Store.GetSetting(SettingUnitsRefused); refused == hash {
		return nil
	}
	installedSocket, _ := os.ReadFile(filepath.Join(dir, socket))
	if bytes.Equal(installed, newService) && bytes.Equal(installedSocket, newSocket) && u.unitActive(socket) {
		return nil
	}

	log := slog.Default().With("component", "selfupdate")
	stage := exe + ".units"
	os.RemoveAll(stage)
	if err := os.MkdirAll(filepath.Join(stage, "new"), 0o755); err != nil {
		return err
	}
	for name, b := range map[string][]byte{service: newService, socket: newSocket} {
		if err := os.WriteFile(filepath.Join(stage, "new", name), b, 0o644); err != nil {
			return err
		}
	}
	sum, err := fsx.FileSHA256(exe)
	if err != nil {
		return err
	}
	delay := u.guardDelay()
	marker := exe + pendingSuffix
	p := pending{
		Kind: "units", ToVersion: u.current(), FromVersion: u.current(), ToSHA: sum, FromSHA: sum,
		Deadline: time.Now().Add(delay), Unit: service,
		Socket: socket, UnitDir: dir, Stage: stage, Listen: config.FromEnv().ListenAddr, UnitsHash: hash,
	}
	if err := fsx.WriteAtomic(marker, 0o644, func(w io.Writer) error {
		_, err := w.Write(p.encode())
		return err
	}); err != nil {
		return err
	}
	if err := u.armGuard(ctx, exe, delay); err != nil {
		os.Remove(marker)
		return err
	}
	guard := filepath.Join(filepath.Dir(exe), guardName)
	launch := func(guard string) error {
		cmd := exec.CommandContext(ctx, "systemd-run",
			fmt.Sprintf("--unit=fogd-install-units-%d", time.Now().UnixNano()),
			"--description=fogd unit files update", "--collect", guard, "--install-units")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("systemd-run: %v: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	if u.units != nil && u.units.launch != nil {
		launch = u.units.launch
	}
	log.Info("installing the unit files this fogd shipped with; fogd restarts onto them", "socket", socket)
	if err := launch(guard); err != nil {
		// Nothing was changed: the helper never ran. The timer finds no
		// marker and stands down.
		os.Remove(marker)
		return err
	}
	return nil
}

// reportUnitsRollback posts a unit change the guard put back, and refuses
// those unit files from now on so the change isn't retried every start.
// The release itself isn't marked ignored: its binary is running fine.
func (u *Updater) reportUnitsRollback(path string, kv map[string]string, journal string, send func(title, msg string, ok bool)) {
	version := orUnknown(kv["failed_version"])
	title := "Fog's new service files didn't work, so the previous ones are back"
	body := fmt.Sprintf("Fog %s brought newer systemd unit files (fogd.service, and fogd.socket, which holds fog's port across restarts). "+
		"With them installed fog didn't become healthy (%s), so the previous unit files were put back automatically, and fog is running on them.\n\n"+
		"Fog won't try those unit files again; a later release with different ones will. "+
		"Re-running install.sh installs them by hand, and shows any error as it happens.",
		version, orUnknown(kv["why"]))
	if journal != "" {
		body += "\n\nThe last log lines:\n\n" + journal
	}
	if err := u.Store.CreateNotice("selfupdate-units-rolled-back", "", title, body); err != nil {
		return
	}
	if h := kv["units_hash"]; h != "" {
		u.Store.SetSetting(SettingUnitsRefused, h)
	}
	u.Store.Audit("system", "fogd.self-update.units-rolled-back", version, "")
	if send != nil {
		send(title, "The previous unit files were restored automatically. Details are in the Inbox.", false)
	}
	os.Remove(path)
}

// settle is what every fogd start does about self-update before its update
// checks begin: post a rollback the guard performed, confirm a change on
// trial once healthy, then — with nothing on trial — bring the unit files
// to the ones this fogd shipped with.
func (u *Updater) settle(ctx context.Context, send func(title, msg string, ok bool)) {
	u.reportRollback(send)
	u.confirmUpdate(ctx)
	if err := u.syncUnits(ctx); err != nil {
		slog.Default().With("component", "selfupdate").Warn("updating fogd's unit files", "err", err)
	}
}
