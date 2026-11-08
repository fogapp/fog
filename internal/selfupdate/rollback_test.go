package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/store"
)

// The rollback path, exercised as far as it goes without a real systemd:
// Apply installs a fogd that can't start; systemd is played by a loop that,
// like Restart=always with ExecStartPre=-fogd-rollback --boot, runs the
// guard and then the binary until something stays up. The guard is the
// real packaging/fogd-rollback, written beside the binary by Apply itself.
// What this can't show: systemd-run actually arming the timer, and systemd
// honouring ExecStartPre's "-" — those are systemd's own behaviour.

// stand-in binaries: shell scripts, run by "systemd" below.
const (
	oldFogd    = "#!/bin/sh\n# fogd v9.0.0\nexit 0\n"
	brokenFogd = "#!/bin/sh\n# fogd v9.9.9, which can't start\necho 'fogd: panic: cannot start' >&2\nexit 1\n"
	goodFogd   = "#!/bin/sh\n# fogd v9.9.9\nexit 0\n"
)

// guardFixture runs Apply with a short trial, and returns the updater and
// the executable's path; the guard lands beside it.
func guardFixture(t *testing.T, newBinary string, trial time.Duration) (*Updater, string) {
	t.Helper()
	skipShell(t)
	u, exe := applyFixture(t, oldFogd, newBinary, nil)
	u.delay = trial
	var armed string
	u.arm = func(guard string, after time.Duration) error {
		armed = guard
		if after != trial {
			t.Errorf("guard armed for %v, want %v", after, trial)
		}
		return nil
	}
	if err := u.Apply(context.Background(), func(int) {}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if armed != filepath.Join(filepath.Dir(exe), guardName) {
		t.Fatalf("guard armed at %q", armed)
	}
	if b, err := os.ReadFile(armed); err != nil || !strings.Contains(string(b), "fogd-rollback") {
		t.Fatalf("the guard script wasn't written beside fogd: %v", err)
	}
	if _, err := os.Stat(exe + pendingSuffix); err != nil {
		t.Fatalf("no trial marker after Apply: %v", err)
	}
	return u, exe
}

// stubPath puts a recording systemctl and journalctl first on PATH.
func stubPath(t *testing.T) (log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "calls")
	writeStub(t, dir, "systemctl", "#!/bin/sh\necho \"systemctl $*\" >> "+log+"\n")
	writeStub(t, dir, "journalctl", "#!/bin/sh\necho 'fogd[1]: panic: cannot start'\n")
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	return log
}

func runGuard(t *testing.T, exe, mode string) {
	t.Helper()
	out, err := exec.Command("sh", filepath.Join(filepath.Dir(exe), guardName), mode).CombinedOutput()
	if err != nil {
		t.Fatalf("fogd-rollback %s: %v\n%s", mode, err, out)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("%s: %v", p, err)
	}
	return string(b)
}

func TestSelfUpdateRollsBackAFogdThatCannotStart(t *testing.T) {
	u, exe := guardFixture(t, brokenFogd, time.Second)
	stubPath(t)

	// systemd: ExecStartPre, then ExecStart, restarted every RestartSec
	// until the service stays up.
	starts, stayedUp := 0, false
	for i := 0; i < 40 && !stayedUp; i++ {
		runGuard(t, exe, "--boot")
		starts++
		stayedUp = exec.Command("sh", exe).Run() == nil
		if !stayedUp {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if !stayedUp {
		t.Fatal("fogd never came back up: the guard didn't roll back")
	}
	if starts < 2 {
		t.Errorf("the new fogd was never tried (%d starts)", starts)
	}
	if got := readFile(t, exe); got != oldFogd {
		t.Errorf("fogd = %q, want the previous binary", got)
	}
	if _, err := os.Stat(exe + pendingSuffix); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the trial marker outlived the rollback: %v", err)
	}
	record := readFile(t, exe+rolledBackSuffix)
	for _, want := range []string{"failed_version=v9.9.9", "restored_version=v9.0.0", "by=boot", "cannot start"} {
		if !strings.Contains(record, want) {
			t.Errorf("rollback record lacks %q:\n%s", want, record)
		}
	}

	// The restored fogd starts, and the Inbox hears about it.
	var sent []string
	u.version = "v9.0.0"
	u.reportRollback(func(title, msg string, ok bool) { sent = append(sent, title) })
	notices, err := u.Store.ActiveNotices()
	if err != nil {
		t.Fatal(err)
	}
	var found *store.Notice
	for i := range notices {
		if notices[i].Kind == "selfupdate-rolled-back" {
			found = &notices[i]
		}
	}
	if found == nil {
		t.Fatalf("no rollback notice in the Inbox: %+v", notices)
	}
	if !strings.Contains(found.Title, "v9.9.9") || !strings.Contains(found.Body, "v9.0.0") || !strings.Contains(found.Body, "cannot start") {
		t.Errorf("notice = %q / %q", found.Title, found.Body)
	}
	if len(sent) != 1 {
		t.Errorf("notifications sent: %v", sent)
	}
	if v, _ := u.Store.GetSetting(store.SettingIgnoredUpdateVersion); v != "v9.9.9" {
		t.Errorf("ignored version = %q, want the failed v9.9.9 so the prompt stops offering it", v)
	}
	if _, err := os.Stat(exe + rolledBackSuffix); !errors.Is(err, os.ErrNotExist) {
		t.Error("the rollback record was posted but not removed: it would be posted again")
	}

	// No loop: every later start, and a late timer, leaves the restored
	// binary alone and posts nothing new.
	for i := 0; i < 3; i++ {
		runGuard(t, exe, "--boot")
	}
	runGuard(t, exe, "--timer")
	if got := readFile(t, exe); got != oldFogd {
		t.Errorf("fogd = %q after later starts, want it left alone", got)
	}
	if _, err := os.Stat(exe + rolledBackSuffix); !errors.Is(err, os.ErrNotExist) {
		t.Error("a later guard run wrote another rollback record")
	}
}

// A fogd that hangs is never restarted by itself; the timer the updater
// armed finds the marker past its deadline, restores, and restarts fogd.
func TestRollbackTimerRestoresAndRestartsAHungFogd(t *testing.T) {
	_, exe := guardFixture(t, brokenFogd, time.Second)
	calls := stubPath(t)

	runGuard(t, exe, "--timer") // before the deadline: the new fogd's chance
	if got := readFile(t, exe); got != brokenFogd {
		t.Fatalf("rolled back before the deadline: fogd = %q", got)
	}
	waitPastDeadline(t, exe)
	runGuard(t, exe, "--timer")
	if got := readFile(t, exe); got != oldFogd {
		t.Fatalf("fogd = %q, want the previous binary", got)
	}
	if got := readFile(t, calls); !strings.Contains(got, "systemctl restart fogd.service") {
		t.Errorf("the timer didn't restart fogd: %q", got)
	}
	if !strings.Contains(readFile(t, exe+rolledBackSuffix), "by=timer") {
		t.Error("record doesn't say the timer acted")
	}
}

// The healthy path: the new fogd confirms itself, and the guard then never
// touches it.
func TestNewFogdConfirmsItselfAndTheGuardStandsDown(t *testing.T) {
	u, exe := guardFixture(t, goodFogd, 500*time.Millisecond)
	stubPath(t)
	health := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{}`)) }))
	defer health.Close()

	// This is now the new fogd, starting up.
	u.version, u.healthURL, u.healthTick, u.healthWindow = "v9.9.9", health.URL, 10*time.Millisecond, 100*time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	u.confirmUpdate(ctx)
	if _, err := os.Stat(exe + pendingSuffix); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a healthy fogd didn't confirm its update: %v", err)
	}
	time.Sleep(600 * time.Millisecond)
	runGuard(t, exe, "--boot")
	runGuard(t, exe, "--timer")
	if got := readFile(t, exe); got != goodFogd {
		t.Errorf("fogd = %q, want the confirmed update left in place", got)
	}
}

// An unhealthy new fogd never confirms, however long it runs: the marker
// stays for the guard.
func TestUnhealthyFogdDoesNotConfirm(t *testing.T) {
	t.Parallel()

	u, exe := guardFixture(t, goodFogd, time.Minute)
	sick := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", 500) }))
	defer sick.Close()
	u.version, u.healthURL, u.healthTick, u.healthWindow = "v9.9.9", sick.URL, 10*time.Millisecond, 50*time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	u.confirmUpdate(ctx)
	if _, err := os.Stat(exe + pendingSuffix); err != nil {
		t.Fatalf("an unhealthy fogd removed its trial marker: %v", err)
	}
}

// The guard only ever swaps the exact pair the marker names.
func TestRollbackGuardLeavesUnknownBinariesAlone(t *testing.T) {
	t.Run("fogd replaced since", func(t *testing.T) {
		_, exe := guardFixture(t, brokenFogd, 10*time.Millisecond)
		stubPath(t)
		os.WriteFile(exe, []byte("#!/bin/sh\n# installed by hand\n"), 0o755)
		waitPastDeadline(t, exe)
		runGuard(t, exe, "--boot")
		if got := readFile(t, exe); !strings.Contains(got, "by hand") {
			t.Errorf("fogd = %q, want the hand-installed binary kept", got)
		}
	})
	t.Run("fogd.prev isn't the replaced binary", func(t *testing.T) {
		_, exe := guardFixture(t, brokenFogd, 10*time.Millisecond)
		stubPath(t)
		os.WriteFile(exe+".prev", []byte("#!/bin/sh\n# something else\n"), 0o755)
		waitPastDeadline(t, exe)
		runGuard(t, exe, "--boot")
		if got := readFile(t, exe); got != brokenFogd {
			t.Errorf("fogd = %q, want it left alone rather than replaced by an unknown .prev", got)
		}
		if _, err := os.Stat(exe + pendingSuffix); !errors.Is(err, os.ErrNotExist) {
			t.Error("marker kept: the guard would retry on every start")
		}
	})
}

// A marker about a version that isn't running is stale; the new start
// clears it so it can't block later updates or trigger a rollback.
func TestConfirmClearsAMarkerForAnotherVersion(t *testing.T) {
	t.Parallel()

	u, exe := guardFixture(t, goodFogd, time.Minute)
	u.version = "v9.0.0" // the guard (or a person) already put the old one back
	u.confirmUpdate(context.Background())
	if _, err := os.Stat(exe + pendingSuffix); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stale marker kept: %v", err)
	}
}

func TestApplyRefusesWhileAnUpdateIsOnTrial(t *testing.T) {
	t.Parallel()

	u, exe := guardFixture(t, goodFogd, time.Minute)
	u.version = "v9.0.0"
	err := u.Apply(context.Background(), func(int) { t.Error("exit requested") })
	if err == nil || !strings.Contains(err.Error(), "still proving itself") {
		t.Fatalf("err = %v, want a refusal while v9.9.9 is on trial", err)
	}
	if got := readFile(t, exe); got != goodFogd {
		t.Errorf("fogd = %q", got)
	}
}

// When the guard can't be armed, nothing changes: no update without a way
// back.
func TestApplyWithoutAGuardChangesNothing(t *testing.T) {
	t.Parallel()

	u, exe := applyFixture(t, "old binary", "new binary", nil)
	u.arm = func(string, time.Duration) error { return errors.New("systemd-run: no such file") }
	err := u.Apply(context.Background(), func(int) { t.Error("exit requested") })
	if err == nil || !strings.Contains(err.Error(), "systemd-run") {
		t.Fatalf("err = %v", err)
	}
	if got := readFile(t, exe); got != "old binary" {
		t.Errorf("fogd = %q, want it untouched", got)
	}
	if _, err := os.Stat(exe + pendingSuffix); !errors.Is(err, os.ErrNotExist) {
		t.Error("marker left behind by a refused update")
	}
	if temps := leftoverTemps(t, exe); len(temps) != 0 {
		t.Errorf("temp files left behind: %v", temps)
	}
}

// Updates only move forward: a manifest naming the running version or an
// older one is never installed, however validly signed.
func TestApplyRefusesADowngrade(t *testing.T) {
	t.Parallel()

	for _, running := range []string{"v9.9.9", "v10.0.0"} {
		u, exe := applyFixture(t, "old binary", "new binary", nil)
		u.version = running
		err := u.Apply(context.Background(), func(int) { t.Error("exit requested") })
		if err == nil || !strings.Contains(err.Error(), "isn't newer") {
			t.Fatalf("running %s: err = %v, want a downgrade refusal", running, err)
		}
		if got := readFile(t, exe); got != "old binary" {
			t.Errorf("fogd = %q", got)
		}
		if res := u.Check(context.Background()); res.UpdateAvailable {
			t.Errorf("running %s: Check offers v9.9.9", running)
		}
	}
}

func TestSelfURL(t *testing.T) {
	t.Parallel()

	for in, want := range map[string]string{
		":8080":          "http://127.0.0.1:8080/api/setup/status",
		"0.0.0.0:9000":   "http://127.0.0.1:9000/api/setup/status",
		"[::]:8080":      "http://127.0.0.1:8080/api/setup/status",
		"192.0.2.5:8080": "http://192.0.2.5:8080/api/setup/status",
		"nonsense":       "",
	} {
		if got := selfURL(in); got != want {
			t.Errorf("selfURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// A rollback because another process took fog's port in the restart gap is
// not the release's fault: the notice says so, and the release isn't
// marked ignored.
func TestRollbackAfterAPortTheftDoesNotIgnoreTheRelease(t *testing.T) {
	t.Parallel()

	u, exe := applyFixture(t, oldFogd, goodFogd, nil)
	os.WriteFile(exe+rolledBackSuffix, []byte("failed_version=v9.9.9\nrestored_version=v9.0.0\nat=1\nby=boot\n--- journal\n"+
		"fogd[7]: level=ERROR msg=\"http server\" err=\"listen tcp :8080: bind: address already in use\"\n"), 0o644)
	u.reportRollback(nil)
	if v, _ := u.Store.GetSetting(store.SettingIgnoredUpdateVersion); v != "" {
		t.Errorf("release marked ignored (%q) after a port theft", v)
	}
	ns, _ := u.Store.ActiveNotices()
	if len(ns) != 1 || !strings.Contains(ns[0].Title, "couldn't open its port") || !strings.Contains(ns[0].Body, "still offered") {
		t.Fatalf("notices = %+v", ns)
	}
}

// syncUnits' decisions, without systemd: when it leaves a server's units
// alone, and what it stages and hands to the helper when it doesn't.
func TestSyncUnitsDecides(t *testing.T) {
	t.Setenv("INVOCATION_ID", "test")
	newSvc, newSock := []byte("[Service]\nExecStart=NEW\n"), []byte("[Socket]\nListenStream=8080\n")
	setup := func(t *testing.T, installed string) (*Updater, string, *[]string) {
		u, exe := applyFixture(t, oldFogd, goodFogd, nil)
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "fogd.service"), []byte(strings.ReplaceAll(installed, "EXE", exe)), 0o644)
		var launched []string
		u.units = &unitSeams{dir: dir, service: "fogd.service", socket: "fogd.socket",
			text:   func() ([]byte, []byte) { return newSvc, newSock },
			launch: func(g string) error { launched = append(launched, g); return nil }}
		return u, exe, &launched
	}
	t.Run("an old unit for this binary is migrated", func(t *testing.T) {
		u, exe, launched := setup(t, "[Service]\nExecStart=EXE\n")
		if err := u.syncUnits(context.Background()); err != nil {
			t.Fatal(err)
		}
		p, err := readPending(exe + pendingSuffix)
		if err != nil || p.Kind != "units" || p.Socket != "fogd.socket" || p.UnitsHash != unitsHash(newSvc, newSock) {
			t.Fatalf("marker = %+v, %v", p, err)
		}
		if b, _ := os.ReadFile(filepath.Join(exe+".units", "new", "fogd.socket")); string(b) != string(newSock) {
			t.Error("new socket not staged")
		}
		if len(*launched) != 1 {
			t.Errorf("helper launched %d times", len(*launched))
		}
	})
	t.Run("a custom layout is left alone", func(t *testing.T) {
		u, exe, launched := setup(t, "[Service]\nExecStart=/opt/elsewhere/fogd\n")
		u.syncUnits(context.Background())
		if _, err := os.Stat(exe + pendingSuffix); err == nil || len(*launched) != 0 {
			t.Error("a unit not running this binary was changed")
		}
	})
	t.Run("refused units aren't retried", func(t *testing.T) {
		u, exe, launched := setup(t, "[Service]\nExecStart=EXE\n")
		u.Store.SetSetting(SettingUnitsRefused, unitsHash(newSvc, newSock))
		u.syncUnits(context.Background())
		if _, err := os.Stat(exe + pendingSuffix); err == nil || len(*launched) != 0 {
			t.Error("refused unit files were tried again")
		}
	})
	t.Run("a helper that can't start changes nothing", func(t *testing.T) {
		u, exe, _ := setup(t, "[Service]\nExecStart=EXE\n")
		u.units.launch = func(string) error { return errors.New("systemd-run: no") }
		if err := u.syncUnits(context.Background()); err == nil {
			t.Fatal("no error")
		}
		if _, err := os.Stat(exe + pendingSuffix); err == nil {
			t.Error("marker left behind")
		}
	})
}

// The guard never acts before a change's deadline. The marker holds whole
// seconds and the guard reads the clock in whole seconds (date +%s), so a
// deadline of 100.9s written as 100 let it act at 100.0 — up to a second
// early, and with a short trial, at once. With the clock pinned (a date
// stub), a deadline inside second 100 holds at 100 and acts at 101.
func TestRollbackGuardNeverActsEarly(t *testing.T) {
	if got := ceilUnix(time.Unix(100, 900_000_000)); got != 101 {
		t.Errorf("ceilUnix(100.9s) = %d, want 101", got)
	}
	if got := ceilUnix(time.Unix(100, 0)); got != 100 {
		t.Errorf("ceilUnix(100.0s) = %d, want 100", got)
	}

	_, exe := guardFixture(t, brokenFogd, time.Minute)
	stubPath(t)
	p, err := readPending(exe + pendingSuffix)
	if err != nil {
		t.Fatal(err)
	}
	p.Deadline = time.Unix(100, 900_000_000)
	os.WriteFile(exe+pendingSuffix, p.encode(), 0o644)

	clock := t.TempDir()
	dateAt := func(sec int) {
		writeStub(t, clock, "date", fmt.Sprintf("#!/bin/sh\necho %d\n", sec))
	}
	t.Setenv("PATH", clock+":"+os.Getenv("PATH"))
	dateAt(100)
	runGuard(t, exe, "--timer")
	if got := readFile(t, exe); got != brokenFogd {
		t.Fatalf("rolled back at second 100, before the 100.9s deadline: fogd = %q", got)
	}
	dateAt(101)
	runGuard(t, exe, "--timer")
	if got := readFile(t, exe); got != oldFogd {
		t.Fatalf("not rolled back at second 101, after the deadline: fogd = %q", got)
	}
}

// waitPastDeadline waits until the guard, reading the clock in whole
// seconds as it does, sees the marker's deadline as passed.
func waitPastDeadline(t *testing.T, exe string) {
	t.Helper()
	p, err := readPending(exe + pendingSuffix)
	if err != nil {
		t.Fatalf("no marker to wait out: %v", err)
	}
	for time.Now().Unix() < p.Deadline.Unix() {
		time.Sleep(50 * time.Millisecond)
	}
}
