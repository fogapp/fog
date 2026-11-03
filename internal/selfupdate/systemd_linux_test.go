//go:build linux

package selfupdate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/sdnotify"
	"gitea.lan/matthew/fog/internal/store"
	"gitea.lan/matthew/fog/packaging"
)

// Under real systemd (scripts/test-fogd-unit runs this as root in a Linux
// VM): packaging/fogd.service, with its hardening and its ExecStartPre
// guard, runs a stand-in fogd — this test binary, re-executed — and the
// rollback paths are driven through systemd itself rather than a loop
// playing it. The stand-in is told apart by a trailer on its executable:
// "FOGSTUB-GOOD" starts (READY=1, then probes what fogd relies on inside the
// sandbox), "FOGSTUB-BAD" exits 1 like a fogd that can't start.

func TestMain(m *testing.M) {
	if os.Getenv("FOGSTUB") == "1" {
		stubMain()
		return
	}
	os.Exit(m.Run())
}

func stubMain() {
	self, _ := os.ReadFile("/proc/self/exe")
	if bytes.HasSuffix(self, []byte("\nFOGSTUB-BAD\n")) {
		fmt.Fprintln(os.Stderr, "fogd stub: panic: cannot start")
		os.Exit(1)
	}
	exe, _ := os.Executable()
	var probe []string
	report := func(name string, err error) {
		if err != nil {
			probe = append(probe, name+"=fail: "+err.Error())
		} else {
			probe = append(probe, name+"=ok")
		}
	}
	report("tmp", os.WriteFile("/tmp/fogstub-private", []byte("x"), 0o600))
	report("bin-writable", os.WriteFile(filepath.Join(filepath.Dir(exe), ".probe"), []byte("x"), 0o600))
	report("data-writable", os.WriteFile(filepath.Join(os.Getenv("FOG_DATA_DIR"), ".probe"), []byte("x"), 0o600))
	// hostpkg (apt, mounts) and the rollback timer go through systemd-run.
	report("systemd-run", exec.Command("systemd-run", "--quiet", "--wait", "--collect",
		fmt.Sprintf("--unit=fogstub-probe-%d", os.Getpid()), "true").Run())
	// tailscaled, a child of fogd, opens the tun device.
	if _, err := os.Stat("/dev/net/tun"); err == nil {
		f, err := os.OpenFile("/dev/net/tun", os.O_RDWR, 0)
		if err == nil {
			f.Close()
		}
		report("tun", err)
	}
	// Serve like fogd: on the socket systemd holds, else bind FOG_LISTEN.
	ln, err := sdnotify.Listener()
	activated := "yes"
	if err == nil && ln == nil {
		activated = "no"
		ln, err = net.Listen("tcp", os.Getenv("FOG_LISTEN"))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "fogd stub: listen:", err)
		os.Exit(1)
	}
	probe = append(probe, "activated="+activated)
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "stub %d", os.Getpid())
	}))
	os.WriteFile(filepath.Join(os.Getenv("FOG_DATA_DIR"), "probe"), []byte(strings.Join(probe, "\n")+"\n"), 0o644)
	sdnotify.Notify("READY=1")
	// Then what fogd does at every start (Loop): settle — post a rollback,
	// confirm a trial, bring the unit files to the ones it shipped with.
	if os.Getenv("FOGSTUB_SETTLE") == "1" {
		st, err := store.Open(filepath.Join(os.Getenv("FOG_DATA_DIR"), "fog.db"))
		if err != nil {
			fmt.Fprintln(os.Stderr, "fogd stub: store:", err)
			os.Exit(1)
		}
		delay, _ := time.ParseDuration(os.Getenv("FOGSTUB_DELAY"))
		port := ln.Addr().(*net.TCPAddr).Port
		bad := os.Getenv("FOGSTUB_BADUNITS") == "1"
		u := &Updater{Store: st, delay: delay, healthURL: selfURL(fmt.Sprintf(":%d", port)),
			healthTick: 200 * time.Millisecond, healthWindow: 2 * time.Second,
			units: &unitSeams{dir: "/etc/systemd/system", service: sdUnit, socket: sdSocket,
				text: func() ([]byte, []byte) { return sdUnits(bad) }}}
		go u.settle(context.Background(), nil)
	}
	for {
		sdnotify.Notify("WATCHDOG=1")
		time.Sleep(5 * time.Second)
	}
}

const (
	sdRoot   = "/opt/fogtest"
	sdUnit   = "fogd-test.service"
	sdSocket = "fogd-test.socket"
	sdPort   = "18080"
)

type sdFixture struct {
	t              *testing.T
	bin, data      string
	exe            string
	good, bad      []byte
	goodSHA, badSH string
}

func sh256(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func newSDFixture(t *testing.T) *sdFixture {
	t.Helper()
	if os.Getenv("FOG_TEST_SYSTEMD") != "1" || os.Getuid() != 0 {
		t.Skip("needs root and systemd: scripts/test-fogd-unit")
	}
	self, err := os.ReadFile("/proc/self/exe")
	if err != nil {
		t.Fatal(err)
	}
	f := &sdFixture{t: t, bin: sdRoot + "/bin", data: sdRoot + "/data",
		good: append(append([]byte{}, self...), "\nFOGSTUB-GOOD\n"...),
		bad:  append(append([]byte{}, self...), "\nFOGSTUB-BAD\n"...)}
	f.exe = f.bin + "/fogd"
	f.goodSHA, f.badSH = sh256(f.good), sh256(f.bad)
	t.Cleanup(f.teardown)
	f.teardown()
	for _, d := range []string{f.bin, f.data} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(f.bin+"/fogd-rollback", packaging.Rollback, 0o755)
	// The real units, moved to scratch paths and a scratch port; the stub
	// is told it's the stub by a drop-in, as a server's own settings are.
	unit, socket := sdUnits(false)
	if err := os.WriteFile(sdDir+"/"+sdUnit, unit, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sdDir+"/"+sdSocket, socket, 0o644); err != nil {
		t.Fatal(err)
	}
	f.dropIn(sdUnit, "20-stub.conf", "[Service]\nEnvironment=FOGSTUB=1\n")
	f.systemctl("daemon-reload")
	return f
}

const sdDir = "/etc/systemd/system"

var sdMoved = strings.NewReplacer("/usr/local/bin", sdRoot+"/bin", "/var/lib/fog", sdRoot+"/data",
	"fogd.socket", sdSocket, "8080", sdPort)

// sdUnits is packaging's fogd.service and fogd.socket, moved to the scratch
// paths, names and port — what a release ships, for fogd-test. bad breaks
// the service (its ExecStart names nothing), for a unit change that fails.
func sdUnits(bad bool) ([]byte, []byte) {
	unit := sdMoved.Replace(string(packaging.Unit))
	if bad {
		unit = strings.Replace(unit, "ExecStart="+sdRoot+"/bin/fogd\n", "ExecStart="+sdRoot+"/bin/missing\n", 1)
	}
	return []byte(unit), []byte(sdMoved.Replace(string(packaging.Socket)))
}

// sdOldUnit is fogd.service as servers were installed before fogd.socket,
// the hardening and the ExecStartPre guard.
var sdOldUnit = sdMoved.Replace(`[Unit]
Description=Fog - home server control plane
After=network-online.target
Wants=network-online.target

[Service]
Type=notify
WatchdogSec=90
ExecStart=/usr/local/bin/fogd
Restart=always
RestartSec=3
OOMScoreAdjust=-900
KillMode=mixed
Environment=FOG_DATA_DIR=/var/lib/fog
Environment=FOG_LISTEN=:8080
NoNewPrivileges=false
ProtectHome=read-only
ProtectSystem=full
ReadWritePaths=/var/lib/fog /usr/local/bin

[Install]
WantedBy=multi-user.target
`)

func (f *sdFixture) dropIn(unit, name, body string) {
	f.t.Helper()
	os.MkdirAll(sdDir+"/"+unit+".d", 0o755)
	if err := os.WriteFile(sdDir+"/"+unit+".d/"+name, []byte(body), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// oldStyle makes fogd-test an install from before the socket: the old unit,
// no socket, and the drop-ins such a server carries — a Proxmox port, the
// install source — plus the stub's own.
func (f *sdFixture) oldStyle(stub string) map[string]string {
	f.t.Helper()
	os.Remove(sdDir + "/" + sdSocket)
	if err := os.WriteFile(sdDir+"/"+sdUnit, []byte(sdOldUnit), 0o644); err != nil {
		f.t.Fatal(err)
	}
	dropins := map[string]string{
		"10-listen.conf":        "[Service]\nEnvironment=FOG_LISTEN=:" + sdAltPort + "\n",
		"10-update-source.conf": "[Service]\nEnvironment=\"FOG_UPDATE_BASE_URL=http://gitea.lan/matthew/fog\"\nEnvironment=\"FOG_UPDATE_BRANCH=main\"\n",
		"20-stub.conf":          "[Service]\nEnvironment=FOGSTUB=1\n" + stub,
	}
	for name, body := range dropins {
		f.dropIn(sdUnit, name, body)
	}
	f.systemctl("daemon-reload")
	return dropins
}

// The server's port before and after: a Proxmox install on another port.
const sdAltPort = "18081"

func (f *sdFixture) waitFor(within time.Duration, what string, ok func() bool) {
	f.t.Helper()
	end := time.Now().Add(within)
	for time.Now().Before(end) {
		if ok() {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	out, _ := exec.Command("journalctl", "-u", sdUnit, "-u", sdSocket, "-n", "60", "--no-pager").CombinedOutput()
	f.t.Fatalf("never %s within %v\n%s", what, within, out)
}

func fileIs(path string, want []byte) bool {
	b, err := os.ReadFile(path)
	return err == nil && bytes.Equal(b, want)
}

func getPort(port string) (string, error) {
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Get("http://127.0.0.1:" + port + "/")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}

// The gap the coordinator named: a server installed before fogd.socket
// self-updates and ends up on the release's units — socket active on its
// own port, the hardened unit, the ExecStartPre guard — with every drop-in
// it had exactly as it was.
func TestUnderSystemdSelfUpdateMigratesAnOldInstall(t *testing.T) {
	f := newSDFixture(t)
	f.install(f.exe, f.good)
	dropins := f.oldStyle("Environment=FOGSTUB_SETTLE=1\nEnvironment=FOGSTUB_DELAY=40s\n")
	if err := f.systemctl("start", sdUnit); err != nil {
		t.Fatal(err)
	}
	newUnit, newSocket := sdUnits(false)
	f.waitFor(90*time.Second, "migrated to the release's unit files, confirmed", func() bool {
		_, pend := os.Stat(f.exe + pendingSuffix)
		return fileIs(sdDir+"/"+sdUnit, newUnit) && fileIs(sdDir+"/"+sdSocket, newSocket) &&
			exec.Command("systemctl", "is-active", "--quiet", sdSocket).Run() == nil &&
			exec.Command("systemctl", "is-active", "--quiet", sdUnit).Run() == nil &&
			os.IsNotExist(pend)
	})
	if probe, _ := os.ReadFile(f.data + "/probe"); !strings.Contains(string(probe), "activated=yes") {
		t.Errorf("fogd isn't on the socket:\n%s", probe)
	}
	if body, err := getPort(sdAltPort); err != nil || !strings.HasPrefix(body, "stub ") {
		t.Errorf("not answering on the server's own port %s: %q %v", sdAltPort, body, err)
	}
	for name, body := range dropins {
		if !fileIs(sdDir+"/"+sdUnit+".d/"+name, []byte(body)) {
			t.Errorf("drop-in %s changed", name)
		}
	}
	if !fileIs(sdDir+"/"+sdSocket+".d/10-listen.conf", []byte("[Socket]\nListenStream=\nListenStream="+sdAltPort+"\n")) {
		t.Error("the socket didn't get the server's port")
	}
	out, _ := exec.Command("systemctl", "show", sdUnit, "-p", "ProtectControlGroups", "-p", "ExecStartPre").CombinedOutput()
	if !strings.Contains(string(out), "ProtectControlGroups=yes") || !strings.Contains(string(out), "fogd-rollback") {
		t.Errorf("not the hardened unit with its guard:\n%s", out)
	}
	// Settled: later starts change nothing.
	f.systemctl("restart", sdUnit)
	time.Sleep(4 * time.Second)
	if _, err := os.Stat(f.exe + pendingSuffix); err == nil {
		t.Error("a settled server started another unit change")
	}
}

// New unit files that don't work are put back — unit, socket, port drop-in —
// and refused from then on, so fog doesn't retry them every start.
func TestUnderSystemdFailedUnitChangeRollsBack(t *testing.T) {
	f := newSDFixture(t)
	f.install(f.exe, f.good)
	dropins := f.oldStyle("Environment=FOGSTUB_SETTLE=1\nEnvironment=FOGSTUB_DELAY=10s\nEnvironment=FOGSTUB_BADUNITS=1\n")
	if err := f.systemctl("start", sdUnit); err != nil {
		t.Fatal(err)
	}
	f.waitFor(30*time.Second, "tried the broken unit files", func() bool {
		b, _ := os.ReadFile(sdDir + "/" + sdUnit)
		return bytes.Contains(b, []byte("bin/missing"))
	})
	f.waitFor(90*time.Second, "rolled back to the old unit files", func() bool {
		_, sock := os.Stat(sdDir + "/" + sdSocket)
		return fileIs(sdDir+"/"+sdUnit, []byte(sdOldUnit)) && os.IsNotExist(sock) &&
			exec.Command("systemctl", "is-active", "--quiet", sdUnit).Run() == nil
	})
	for name, body := range dropins {
		if !fileIs(sdDir+"/"+sdUnit+".d/"+name, []byte(body)) {
			t.Errorf("drop-in %s changed", name)
		}
	}
	if _, err := os.Stat(sdDir + "/" + sdSocket + ".d/10-listen.conf"); err == nil {
		t.Error("the socket port drop-in the change added is still there")
	}
	f.waitFor(30*time.Second, "posted the rollback and refused those units", func() bool {
		st, err := store.Open(f.data + "/fog.db")
		if err != nil {
			return false
		}
		defer st.Close()
		v, _ := st.GetSetting(SettingUnitsRefused)
		ns, _ := st.ActiveNotices()
		return v != "" && len(ns) > 0
	})
	// No loop: the refused units aren't tried again.
	f.systemctl("restart", sdUnit)
	time.Sleep(6 * time.Second)
	if !fileIs(sdDir+"/"+sdUnit, []byte(sdOldUnit)) {
		t.Error("the refused unit files were tried again")
	}
	if body, err := getPort(sdAltPort); err != nil || !strings.HasPrefix(body, "stub ") {
		t.Errorf("not answering after the rollback: %q %v", body, err)
	}
}

// A new binary that fails on an old-style install: the binary is put back,
// and the unit files are never touched — they only change once a binary has
// proven itself on the old ones.
func TestUnderSystemdFailedBinaryOnAnOldInstallLeavesTheUnits(t *testing.T) {
	f := newSDFixture(t)
	dropins := f.oldStyle("")
	f.trial(time.Now().Add(3 * time.Second))
	if err := armTimer(context.Background(), f.bin+"/fogd-rollback", 3*time.Second); err != nil {
		t.Fatal(err)
	}
	f.systemctl("start", "--no-block", sdUnit)
	f.waitActive(40 * time.Second)
	f.record("timer")
	if !fileIs(sdDir+"/"+sdUnit, []byte(sdOldUnit)) {
		t.Error("the unit file changed during a binary rollback")
	}
	if _, err := os.Stat(sdDir + "/" + sdSocket); err == nil {
		t.Error("a socket appeared during a binary rollback")
	}
	for name, body := range dropins {
		if !fileIs(sdDir+"/"+sdUnit+".d/"+name, []byte(body)) {
			t.Errorf("drop-in %s changed", name)
		}
	}
}

func (f *sdFixture) teardown() {
	// Guard timers this test armed (and only those: their command is under
	// sdRoot) would otherwise wait out their deadline.
	out, _ := exec.Command("systemctl", "list-units", "--all", "--plain", "--no-legend", "fogd-update-guard-*.service", "fogd-unit-rollback-*.service").Output()
	for _, line := range strings.Split(string(out), "\n") {
		name, _, _ := strings.Cut(strings.TrimSpace(line), " ")
		if name == "" {
			continue
		}
		show, _ := exec.Command("systemctl", "show", "-p", "ExecStart", name).Output()
		if strings.Contains(string(show), sdRoot+"/") {
			exec.Command("systemctl", "stop", strings.TrimSuffix(name, ".service")+".timer", name).Run()
			exec.Command("systemctl", "reset-failed", name).Run()
		}
	}
	exec.Command("systemctl", "stop", sdSocket, sdUnit).Run()
	exec.Command("systemctl", "disable", sdSocket).Run()
	exec.Command("systemctl", "reset-failed", sdUnit, sdSocket).Run()
	os.Remove("/etc/systemd/system/" + sdUnit)
	os.Remove("/etc/systemd/system/" + sdSocket)
	os.RemoveAll("/etc/systemd/system/" + sdSocket + ".d")
	os.RemoveAll("/etc/systemd/system/" + sdUnit + ".d")
	exec.Command("systemctl", "daemon-reload").Run()
	os.RemoveAll(sdRoot)
	os.Remove("/tmp/fogstub-private")
}

func (f *sdFixture) systemctl(args ...string) error {
	out, err := exec.Command("systemctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %v: %v: %s", args, err, out)
	}
	return nil
}

func (f *sdFixture) install(path string, b []byte) {
	if err := os.WriteFile(path, b, 0o755); err != nil {
		f.t.Fatal(err)
	}
}

// trial puts the broken fogd in place as a self-update would leave it.
func (f *sdFixture) trial(deadline time.Time) {
	f.install(f.exe+".prev", f.good)
	f.install(f.exe, f.bad)
	p := pending{ToVersion: "v2.0.0", FromVersion: "v1.0.0", ToSHA: f.badSH, FromSHA: f.goodSHA, Deadline: deadline, Unit: sdUnit}
	os.WriteFile(f.exe+pendingSuffix, p.encode(), 0o644)
}

func (f *sdFixture) waitActive(within time.Duration) {
	f.t.Helper()
	end := time.Now().Add(within)
	for time.Now().Before(end) {
		if exec.Command("systemctl", "is-active", "--quiet", sdUnit).Run() == nil {
			if b, _ := os.ReadFile(f.exe); bytes.Equal(b, f.good) {
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	out, _ := exec.Command("journalctl", "-u", sdUnit, "-n", "40", "--no-pager").CombinedOutput()
	f.t.Fatalf("%s never came up on the previous fogd within %v\n%s", sdUnit, within, out)
}

func (f *sdFixture) record(by string) {
	f.t.Helper()
	b, err := os.ReadFile(f.exe + rolledBackSuffix)
	if err != nil {
		f.t.Fatalf("no rollback record: %v", err)
	}
	for _, want := range []string{"failed_version=v2.0.0", "restored_version=v1.0.0", "by=" + by, "cannot start"} {
		if !strings.Contains(string(b), want) {
			f.t.Errorf("record lacks %q:\n%s", want, b)
		}
	}
	os.Remove(f.exe + rolledBackSuffix)
}

func TestUnderSystemdHardenedUnitRunsFogd(t *testing.T) {
	f := newSDFixture(t)
	f.install(f.exe, f.good)
	if err := f.systemctl("start", sdUnit); err != nil {
		t.Fatal(err)
	}
	probe, err := os.ReadFile(f.data + "/probe")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("inside the sandbox:\n%s", probe)
	if strings.Contains(string(probe), "fail") {
		t.Errorf("something fogd relies on is blocked by the unit's hardening:\n%s", probe)
	}
	if _, err := os.Stat("/tmp/fogstub-private"); err == nil {
		t.Error("PrivateTmp didn't take: the stub's /tmp file is on the host")
	}
	if !strings.Contains(string(probe), "activated=yes") {
		t.Error("fogd wasn't handed fogd.socket")
	}
}

func get(t *testing.T) (string, error) {
	t.Helper()
	c := &http.Client{Timeout: 10 * time.Second}
	resp, err := c.Get("http://127.0.0.1:" + sdPort + "/api/setup/status")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}

// The port race: while fogd restarts (a self-update's exit), the port is
// never free — systemd holds it — so nothing else on the machine can take
// it and block the new fogd or answer in its place. Probed throughout a
// restart, binding the port fails every time; and with fogd stopped
// outright, a request starts it.
func TestUnderSystemdSocketHoldsThePortAcrossRestarts(t *testing.T) {
	f := newSDFixture(t)
	f.install(f.exe, f.good)
	if err := f.systemctl("start", sdUnit); err != nil {
		t.Fatal(err)
	}
	before, err := get(t)
	if err != nil || !strings.HasPrefix(before, "stub ") {
		t.Fatalf("GET: %q %v", before, err)
	}

	stop := make(chan struct{})
	stolen := make(chan string, 1)
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			if ln, err := net.Listen("tcp", ":"+sdPort); err == nil {
				ln.Close()
				stolen <- "bound the port mid-restart"
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	for i := 0; i < 3; i++ {
		if err := f.systemctl("restart", sdUnit); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.systemctl("stop", sdUnit); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	close(stop)
	select {
	case msg := <-stolen:
		t.Fatalf("another process %s: the race is open", msg)
	default:
	}

	// fogd stopped, socket still listening: a request starts fogd.
	after, err := get(t)
	if err != nil || !strings.HasPrefix(after, "stub ") || after == before {
		t.Fatalf("GET after stop: %q %v (before %q)", after, err, before)
	}
}

// A unit without the socket (an install from before it) still works: fogd binds FOG_LISTEN itself.
func TestUnderSystemdFogdBindsItselfWithoutTheSocket(t *testing.T) {
	f := newSDFixture(t)
	f.install(f.exe, f.good)
	// An install from before the socket: the unit names it, but it isn't there.
	os.Remove("/etc/systemd/system/" + sdSocket)
	if err := f.systemctl("daemon-reload"); err != nil {
		t.Fatal(err)
	}
	if err := f.systemctl("start", sdUnit); err != nil {
		t.Fatal(err)
	}
	probe, _ := os.ReadFile(f.data + "/probe")
	if !strings.Contains(string(probe), "activated=no") {
		t.Errorf("probe:\n%s", probe)
	}
	if body, err := get(t); err != nil || !strings.HasPrefix(body, "stub ") {
		t.Fatalf("GET: %q %v", body, err)
	}
}

func TestUnderSystemdExecStartPreRestoresAtStart(t *testing.T) {
	f := newSDFixture(t)
	f.trial(time.Now().Add(-time.Second))
	if err := f.systemctl("start", sdUnit); err != nil {
		t.Fatal(err)
	}
	f.waitActive(10 * time.Second)
	if _, err := os.Stat(f.exe + rolledBackSuffix); err != nil {
		t.Fatal("restored, but no record")
	}
	b, _ := os.ReadFile(f.exe + rolledBackSuffix)
	if !strings.Contains(string(b), "by=boot") {
		t.Errorf("record:\n%s", b)
	}
}

// A new fogd that crash-loops: systemd restarts it every RestartSec, and the
// first start after the deadline runs the previous binary.
func TestUnderSystemdCrashLoopRollsBackAtTheDeadline(t *testing.T) {
	f := newSDFixture(t)
	f.trial(time.Now().Add(8 * time.Second))
	f.systemctl("start", "--no-block", sdUnit)
	time.Sleep(4 * time.Second)
	if b, _ := os.ReadFile(f.exe); !bytes.Equal(b, f.bad) {
		t.Fatal("rolled back before the deadline")
	}
	f.waitActive(40 * time.Second)
	f.record("boot")
}

// A unit from before the ExecStartPre line: the timer the updater arms
// (armTimer, the real systemd-run call) does the rollback and restarts fogd.
func TestUnderSystemdTimerRollsBackWithoutExecStartPre(t *testing.T) {
	f := newSDFixture(t)
	os.MkdirAll("/etc/systemd/system/"+sdUnit+".d", 0o755)
	os.WriteFile("/etc/systemd/system/"+sdUnit+".d/old.conf", []byte("[Service]\nExecStartPre=\n"), 0o644)
	f.systemctl("daemon-reload")
	f.trial(time.Now().Add(3 * time.Second))
	if err := armTimer(context.Background(), f.bin+"/fogd-rollback", 3*time.Second); err != nil {
		t.Fatal(err)
	}
	f.systemctl("start", "--no-block", sdUnit)
	f.waitActive(40 * time.Second)
	f.record("timer")
}
