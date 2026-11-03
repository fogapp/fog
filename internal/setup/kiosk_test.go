package setup

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeKioskFile(t *testing.T, dir, name, val string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(val+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestKioskDirNoKiosk(t *testing.T) {
	k := KioskDir{Dir: t.TempDir()}
	if _, err := k.Scale(); !errors.Is(err, ErrNoKiosk) {
		t.Fatalf("Scale with no state files: %v, want ErrNoKiosk", err)
	}
	if _, err := k.SetScale(2); !errors.Is(err, ErrNoKiosk) {
		t.Fatalf("SetScale with no state files: %v, want ErrNoKiosk", err)
	}
}

func TestKioskDirOverride(t *testing.T) {
	dir := t.TempDir()
	writeKioskFile(t, dir, "scale", "2.5")
	writeKioskFile(t, dir, "auto", "2.5")
	rescales := 0
	k := KioskDir{Dir: dir, Rescale: func() error { rescales++; return nil }}

	st, err := k.Scale()
	if err != nil || st != (KioskScale{Scale: 2.5, Auto: 2.5}) {
		t.Fatalf("Scale = %+v, %v", st, err)
	}

	restarting, err := k.SetScale(1.75)
	if err != nil || !restarting || rescales != 1 {
		t.Fatalf("SetScale(1.75) = %v, %v (rescales %d), want a restart", restarting, err, rescales)
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, "override")); string(raw) != "1.75\n" {
		t.Fatalf("override file = %q", raw)
	}
	// Until the kiosk comes back, it is still drawing at the old scale — the
	// state must say so rather than claim the new one.
	st, _ = k.Scale()
	if st != (KioskScale{Scale: 2.5, Auto: 2.5, Manual: true}) {
		t.Fatalf("Scale after override = %+v", st)
	}

	// Handing it back to automatic when automatic is what's running: nothing
	// to restart, and the override is gone.
	restarting, err = k.SetScale(0)
	if err != nil || restarting {
		t.Fatalf("SetScale(0) = %v, %v, want no restart", restarting, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "override")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("override still present: %v", err)
	}
	if st, _ = k.Scale(); st.Manual {
		t.Fatalf("Scale after automatic = %+v, want not manual", st)
	}
}

func TestKioskDirConsole(t *testing.T) {
	// No kiosk ever came up: there is no screen to hand over.
	none := KioskDir{Dir: t.TempDir(), Kiosk: func(bool) error { t.Fatal("stopped a kiosk that never ran"); return nil }}
	if err := none.Console(true); !errors.Is(err, ErrNoKiosk) {
		t.Fatalf("Console with no state files: %v, want ErrNoKiosk", err)
	}

	dir := t.TempDir()
	writeKioskFile(t, dir, "scale", "1")
	var running []bool
	k := KioskDir{Dir: dir, Kiosk: func(on bool) error { running = append(running, on); return nil }}
	// The command line on means the kiosk off, and back.
	if err := k.Console(true); err != nil {
		t.Fatal(err)
	}
	if err := k.Console(false); err != nil {
		t.Fatal(err)
	}
	if len(running) != 2 || running[0] || !running[1] {
		t.Fatalf("kiosk running states = %v, want [false true]", running)
	}
}

func TestConsoleEndpoint(t *testing.T) {
	post := func(a *Agent, body string) *httptest.ResponseRecorder {
		t.Helper()
		return doHandler(a, newTestRequest(t, "POST", "/api/setup/console", strings.NewReader(body), "application/json"))
	}
	// Install mode never offers it, even with a display: the wizard is the
	// only thing the USB stick is for.
	if res := post(&Agent{Display: NewMockKiosk(1)}, `{"on": true}`); res.Code != http.StatusBadRequest {
		t.Fatalf("console in install mode: %d, want 400", res.Code)
	}
	// Headless recovery: no screen to hand over.
	if res := post(&Agent{Mode: "repair"}, `{"on": true}`); res.Code != http.StatusNotFound {
		t.Fatalf("console with no kiosk: %d, want 404", res.Code)
	}
	disp := NewMockKiosk(1)
	r := &Agent{Inventory: MockInventory, Exec: MockExecutor{StepDelay: 2 * time.Second}, Mode: "repair", Display: disp}
	for _, bad := range []string{`{}`, `{"on": "yes"}`, `nope`} {
		if res := post(r, bad); res.Code != http.StatusBadRequest {
			t.Errorf("console %s: %d, want 400", bad, res.Code)
		}
	}
	// Mid-repair the screen stays: the shell must not appear over a
	// half-rewritten OS partition.
	if err := r.StartRepair("reinstall-os"); err != nil {
		t.Fatal(err)
	}
	if res := post(r, `{"on": true}`); res.Code != http.StatusBadRequest {
		t.Fatalf("console mid-repair: %d, want 400", res.Code)
	}
	if disp.console {
		t.Fatal("display handed over while a repair ran")
	}
	// An idle recovery menu hands over, and takes back.
	idle := &Agent{Inventory: MockInventory, Exec: MockExecutor{}, Mode: "repair", Display: disp}
	if res := post(idle, `{"on": true}`); res.Code != http.StatusOK || !disp.console {
		t.Fatalf("console from idle menu: %d (console %v), want 200 and the command line", res.Code, disp.console)
	}
	if res := post(idle, `{"on": false}`); res.Code != http.StatusOK || disp.console {
		t.Fatalf("screen back from idle menu: %d (console %v), want 200 and the kiosk", res.Code, disp.console)
	}
}

func TestDisplayEndpoints(t *testing.T) {
	post := func(srv *httptest.Server, body string) *http.Response {
		t.Helper()
		res, err := srv.Client().Post(srv.URL+"/api/setup/display", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { res.Body.Close() })
		return res
	}

	headless := httptest.NewServer((&Agent{}).Handler(nil))
	t.Cleanup(headless.Close)
	res, err := headless.Client().Get(headless.URL + "/api/setup/display")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("GET display with no kiosk: %d, want 404", res.StatusCode)
	}
	if res := post(headless, `{"scale": 2}`); res.StatusCode != http.StatusNotFound {
		t.Fatalf("POST display with no kiosk: %d, want 404", res.StatusCode)
	}

	srv := httptest.NewServer((&Agent{Display: NewMockKiosk(1.25)}).Handler(nil))
	t.Cleanup(srv.Close)
	for _, bad := range []string{`{}`, `{"scale": 9}`, `{"scale": 0.25}`, `{"scale": -1}`, `nope`} {
		if res := post(srv, bad); res.StatusCode != http.StatusBadRequest {
			t.Errorf("POST display %s: %d, want 400", bad, res.StatusCode)
		}
	}
	if res := post(srv, `{"scale": 2}`); res.StatusCode != http.StatusOK {
		t.Fatalf("POST display 2: %d", res.StatusCode)
	}
	res, err = srv.Client().Get(srv.URL + "/api/setup/display")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var st KioskScale
	if err := json.NewDecoder(res.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if st != (KioskScale{Scale: 2, Auto: 1.25, Manual: true}) {
		t.Fatalf("display after override = %+v", st)
	}
}
