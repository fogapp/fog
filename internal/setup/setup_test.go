package setup

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ---- inventory ----

const lsblkFixture = `{
  "blockdevices": [
    {"name":"nvme0n1","type":"disk","size":500107862016,"model":"Samsung SSD 970 EVO 500GB","tran":"nvme","rm":false,"fstype":null,
     "children":[
       {"name":"nvme0n1p1","type":"part","size":536870912,"model":null,"tran":"nvme","rm":false,"fstype":"vfat"},
       {"name":"nvme0n1p2","type":"part","size":499000000000,"model":null,"tran":"nvme","rm":false,"fstype":"ext4"}
     ]},
    {"name":"sda","type":"disk","size":30752636928,"model":"SanDisk Ultra","tran":"usb","rm":true,"fstype":null,
     "children":[{"name":"sda1","type":"part","size":30752636928,"model":null,"tran":"usb","rm":true,"fstype":"vfat","label":"FOG-BOOT"}]},
    {"name":"sr0","type":"rom","size":0,"model":"DVD","tran":"sata","rm":true,"fstype":null}
  ]
}`

func TestParseLsblk(t *testing.T) {
	disks, err := parseLsblk([]byte(lsblkFixture), "sda")
	if err != nil {
		t.Fatal(err)
	}
	if len(disks) != 2 {
		t.Fatalf("disks = %d, want 2 (rom skipped)", len(disks))
	}
	nvme := disks[0]
	if nvme.Path != "/dev/nvme0n1" || nvme.Removable || nvme.BootMedium {
		t.Fatalf("nvme parsed wrong: %+v", nvme)
	}
	if !strings.Contains(nvme.Contents, "Linux") {
		t.Fatalf("contents = %q, want a Linux description", nvme.Contents)
	}
	usb := disks[1]
	if !usb.Removable || !usb.BootMedium {
		t.Fatalf("usb stick parsed wrong: %+v", usb)
	}
	if got := pickCandidate(disks); got != "/dev/nvme0n1" {
		t.Fatalf("candidate = %q", got)
	}
}

func TestParseLsblkRecognizesStickByLabel(t *testing.T) {
	// Even when the boot-disk recording failed (""), the stick's own
	// FOG-BOOT label must mark it as the boot medium.
	disks, err := parseLsblk([]byte(lsblkFixture), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range disks {
		if d.Path == "/dev/sda" && !d.BootMedium {
			t.Fatal("FOG-BOOT-labeled stick not recognized as the boot medium")
		}
	}
}

// A machine that already went through the installer: its disk carries all
// four fog labels, plus the stick that's still plugged in.
const lsblkInstalledFixture = `{
  "blockdevices": [
    {"name":"nvme0n1","type":"disk","size":500107862016,"model":"Samsung SSD 970 EVO 500GB","tran":"nvme","rm":false,"fstype":null,
     "children":[
       {"name":"nvme0n1p1","type":"part","size":1073741824,"fstype":"vfat","label":"FOG-ESP"},
       {"name":"nvme0n1p2","type":"part","size":6442450944,"fstype":"ext4","label":"fog-recovery"},
       {"name":"nvme0n1p3","type":"part","size":17179869184,"fstype":"ext4","label":"fog-root"},
       {"name":"nvme0n1p4","type":"part","size":470000000000,"fstype":"ext4","label":"fog-data"}
     ]},
    {"name":"sda","type":"disk","size":30752636928,"model":"SanDisk Ultra","tran":"usb","rm":true,"fstype":null,
     "children":[{"name":"sda1","type":"part","size":30752636928,"fstype":"vfat","label":"FOG-BOOT"}]}
  ]
}`

func TestFindsExistingInstall(t *testing.T) {
	disks, err := parseLsblk([]byte(lsblkInstalledFixture), "sda")
	if err != nil {
		t.Fatal(err)
	}
	target := disks[0]
	if !target.FogDisk || target.espDev != "/dev/nvme0n1p1" {
		t.Fatalf("installed disk not recognized: %+v", target)
	}
	if target.Contents != "an existing Fog server" {
		t.Fatalf("contents = %q, want the disk named as a Fog server", target.Contents)
	}
	marker := &InstallMarker{ServerName: "attic", Mode: "fresh", Version: "0.6.15",
		InstalledAt: "2026-08-22T18:04:00Z"}
	var read []string
	existing := findExisting(disks, func(esp string) *InstallMarker {
		read = append(read, esp)
		return marker
	})
	if existing == nil {
		t.Fatal("no existing install reported for a disk carrying a marker")
	}
	if existing.Disk != "/dev/nvme0n1" || existing.ServerName != "attic" || existing.Version != "0.6.15" {
		t.Fatalf("existing: %+v", existing)
	}
	// Only the machine's own disk is probed — never the stick we booted.
	if len(read) != 1 || read[0] != "/dev/nvme0n1p1" {
		t.Fatalf("probed %v", read)
	}
}

func TestNoMarkerIsNotAnExistingInstall(t *testing.T) {
	// All four labels but no marker: what an install that died between mkfs
	// and the finish line leaves. Calling that "already a Fog server" would
	// send the operator away from a machine that needs installing.
	disks, err := parseLsblk([]byte(lsblkInstalledFixture), "sda")
	if err != nil {
		t.Fatal(err)
	}
	if e := findExisting(disks, func(string) *InstallMarker { return nil }); e != nil {
		t.Fatalf("markerless disk reported as installed: %+v", e)
	}
}

func TestPlainDiskIsNotAFogDisk(t *testing.T) {
	disks, err := parseLsblk([]byte(lsblkFixture), "sda")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range disks {
		if d.FogDisk {
			t.Fatalf("%s claimed to hold a Fog install: %+v", d.Path, d)
		}
	}
	probed := false
	if e := findExisting(disks, func(string) *InstallMarker { probed = true; return nil }); e != nil || probed {
		t.Fatalf("probed a disk with no fog labels (existing=%+v, probed=%v)", e, probed)
	}
}

func TestParseInstallMarkerRejectsGarbage(t *testing.T) {
	for _, raw := range []string{"", "{", "{}", `{"serverName":"fog"}`} {
		if m := parseInstallMarker([]byte(raw)); m != nil {
			t.Fatalf("%q read as a finished install: %+v", raw, m)
		}
	}
}

func TestValidateRefusesRemovableWhenBootMediumUnknown(t *testing.T) {
	// A big removable disk in an inventory that identified no boot medium
	// could be the installer's own stick — Validate must refuse it.
	inv := Inventory{Disks: []Disk{
		{Path: "/dev/sdb", SizeBytes: 128 << 30, Bus: "usb", Removable: true},
	}}
	c := InstallConfig{Mode: "fresh", Disk: "/dev/sdb", ConfirmDisk: "/dev/sdb",
		ServerName: "fog", Username: "m", Password: "long-enough-pw"}
	if err := c.Validate(inv); err == nil || !strings.Contains(err.Error(), "removable") {
		t.Fatalf("removable disk accepted with no known boot medium: %v", err)
	}
	// Once the boot medium is identified elsewhere, a deliberate removable
	// target (USB-attached SSD) stays allowed.
	inv.Disks = append(inv.Disks, Disk{Path: "/dev/sda", Removable: true, BootMedium: true})
	if err := c.Validate(inv); err != nil {
		t.Fatalf("removable target refused despite a known boot medium: %v", err)
	}
}

func TestPickCandidateAmbiguous(t *testing.T) {
	disks := []Disk{
		{Path: "/dev/sda", SizeBytes: 1000},
		{Path: "/dev/sdb", SizeBytes: 2000},
	}
	if got := pickCandidate(disks); got != "" {
		t.Fatalf("two internal disks must not auto-pick, got %q", got)
	}
}

// ---- validation ----

func mockInv(t *testing.T) Inventory {
	t.Helper()
	inv, err := MockInventory()
	if err != nil {
		t.Fatal(err)
	}
	return inv
}

func TestValidate(t *testing.T) {
	inv := mockInv(t)
	good := InstallConfig{
		Mode: "fresh", Disk: "/dev/nvme0n1", ConfirmDisk: "/dev/nvme0n1",
		ConfirmModel: "Samsung SSD 970 EVO 500GB", ConfirmSizeBytes: 500_107_862_016,
		ServerName: "fog", Username: "matthew", Password: "correct-horse-battery",
	}
	if err := good.Validate(inv); err != nil {
		t.Fatal(err)
	}

	cases := map[string]func(c *InstallConfig){
		"unknown disk":    func(c *InstallConfig) { c.Disk = "/dev/sdz"; c.ConfirmDisk = "/dev/sdz" },
		"boot medium":     func(c *InstallConfig) { c.Disk = "/dev/sda"; c.ConfirmDisk = "/dev/sda" },
		"missing confirm": func(c *InstallConfig) { c.ConfirmDisk = "" },
		"wrong confirm":   func(c *InstallConfig) { c.ConfirmDisk = "/dev/sda" },
		// A device that re-enumerated between the picker and Start: the name
		// matches, the disk behind it doesn't.
		"changed size":    func(c *InstallConfig) { c.ConfirmSizeBytes = 128 << 30 },
		"changed model":   func(c *InstallConfig) { c.ConfirmModel = "Some Other Disk" },
		"bad server name": func(c *InstallConfig) { c.ServerName = "Fog Server!" },
		"short password":  func(c *InstallConfig) { c.Password = "short" },
		"no username":     func(c *InstallConfig) { c.Username = "" },
		"bad mode":        func(c *InstallConfig) { c.Mode = "yolo" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := good
			mutate(&c)
			if err := c.Validate(inv); err == nil {
				t.Fatal("want validation error")
			}
		})
	}
}

func TestValidateDefaultsServerName(t *testing.T) {
	inv := mockInv(t)
	c := InstallConfig{Mode: "fresh", Disk: "/dev/nvme0n1", ConfirmDisk: "/dev/nvme0n1",
		Username: "m", Password: "long-enough-pw"}
	if err := c.Validate(inv); err != nil {
		t.Fatal(err)
	}
	if c.ServerName != "fog" {
		t.Fatalf("serverName = %q, want fog default", c.ServerName)
	}
}

// ---- engine ----

func fastAgent() *Agent {
	return &Agent{
		Inventory: MockInventory,
		Exec:      MockExecutor{StepDelay: 2 * time.Millisecond},
	}
}

func goodCfg() InstallConfig {
	return InstallConfig{
		Mode: "fresh", Disk: "/dev/nvme0n1", ConfirmDisk: "/dev/nvme0n1",
		ServerName: "fog", Username: "matthew", Password: "correct-horse-battery",
	}
}

func waitPhase(t *testing.T, a *Agent, want string) AgentState {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st := a.State()
		if st.Phase == want {
			return st
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("phase never reached %q (now %q, err %q)", want, a.State().Phase, a.State().Error)
	return AgentState{}
}

func TestMockInstallRunsToDone(t *testing.T) {
	a := fastAgent()
	if err := a.Start(goodCfg()); err != nil {
		t.Fatal(err)
	}
	st := waitPhase(t, a, "done")
	if st.Result == nil || len(st.Result.URLs) == 0 {
		t.Fatalf("done without a usable result: %+v", st.Result)
	}
	// State never carries the token; TakeClaimToken hands it out once.
	if st.Result.ClaimToken != "" {
		t.Fatal("claim token leaked into agent state")
	}
	if tok, ok := a.TakeClaimToken(); !ok || tok == "" {
		t.Fatal("no claim token to take after a fresh install")
	}
	if _, ok := a.TakeClaimToken(); ok {
		t.Fatal("claim token taken twice")
	}
	// Every step must appear and finish, in order.
	doneSteps := map[string]bool{}
	for _, e := range st.Events {
		if e.Status == "done" {
			doneSteps[e.ID] = true
		}
	}
	for _, step := range InstallSteps {
		if !doneSteps[step.ID] {
			t.Fatalf("step %s never completed", step.ID)
		}
	}
	// One install per boot: a second start must refuse.
	if err := a.Start(goodCfg()); err == nil {
		t.Fatal("second install allowed after done")
	}
	// And a success is not retryable.
	if err := a.Retry(); err == nil {
		t.Fatal("retry allowed after success")
	}
}

func TestPublishOverflowClosesLaggingSubscriber(t *testing.T) {
	// A subscriber that stops reading (a phone asleep mid-install) must be
	// closed out once its buffer fills — silently dropping events could
	// discard the terminal one and leave that surface spinning forever. The
	// close is what forces EventSource to reconnect and replay.
	a := &Agent{}
	ch, cancel := a.Subscribe()
	defer cancel()
	sent := 300 // capacity is len(events)+256 = 256 at subscribe time
	for i := 0; i < sent; i++ {
		a.publish(Progress{Step: i + 1, Steps: sent})
	}
	got, closed := 0, false
	for !closed {
		select {
		case _, ok := <-ch:
			if !ok {
				closed = true
				break
			}
			got++
		default:
			t.Fatalf("channel neither closed nor full after %d reads", got)
		}
	}
	if got != 256 {
		t.Fatalf("buffered events = %d, want the full 256-slot buffer", got)
	}
	// The transcript itself lost nothing: a reconnecting subscriber replays
	// every event, including the ones the dead channel never carried.
	ch2, cancel2 := a.Subscribe()
	defer cancel2()
	replayed := 0
	for i := 0; i < sent; i++ {
		select {
		case <-ch2:
			replayed++
		default:
		}
	}
	if replayed != sent {
		t.Fatalf("replay = %d events, want %d", replayed, sent)
	}
	// And a live event still flows to the healthy subscriber.
	a.publish(Progress{ID: "verify", Status: "done"})
	select {
	case p := <-ch2:
		if p.ID != "verify" {
			t.Fatalf("live event = %+v", p)
		}
	default:
		t.Fatal("live event never reached the surviving subscriber")
	}
}

func TestMarkInterruptedStartsFailedAndRetryable(t *testing.T) {
	// A crash-restarted agent must not open on the welcome screen: it starts
	// failed with the interruption message, and Retry (the wizard's "try
	// again") clears it back to a workable idle.
	a := fastAgent()
	a.MarkInterrupted("the previous install was interrupted")
	st := a.State()
	if st.Phase != "failed" || st.Error == "" {
		t.Fatalf("state after MarkInterrupted: %+v", st)
	}
	if err := a.Retry(); err != nil {
		t.Fatal(err)
	}
	if err := a.Start(goodCfg()); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, a, "done")
	// Once an install ran to done, a late MarkInterrupted (programming
	// error) must not clobber the result.
	a.MarkInterrupted("nope")
	if got := a.State(); got.Phase != "done" {
		t.Fatalf("MarkInterrupted overwrote phase %q", got.Phase)
	}
}

type failExec struct{}

func (failExec) Run(_ context.Context, _ InstallConfig, report func(Progress)) (Result, error) {
	report(Progress{Step: 1, Steps: len(InstallSteps), ID: "partition", Title: "Partitioning the disk", Status: "failed", Detail: "no such device"})
	return Result{}, context.DeadlineExceeded
}

func TestFailedInstallIsRetryable(t *testing.T) {
	a := fastAgent()
	a.Exec = failExec{}
	if err := a.Start(goodCfg()); err != nil {
		t.Fatal(err)
	}
	st := waitPhase(t, a, "failed")
	if st.Error == "" {
		t.Fatal("failed phase without an error message")
	}
	if err := a.Retry(); err != nil {
		t.Fatal(err)
	}
	a.Exec = MockExecutor{StepDelay: 2 * time.Millisecond}
	if err := a.Start(goodCfg()); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, a, "done")
}

// ---- HTTP surface ----

func TestHandlerEndToEnd(t *testing.T) {
	a := fastAgent()
	srv := httptest.NewServer(a.Handler(nil))
	t.Cleanup(srv.Close)

	get := func(path string) *json.Decoder {
		res, err := srv.Client().Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { res.Body.Close() })
		if res.StatusCode != 200 {
			t.Fatalf("GET %s: %d", path, res.StatusCode)
		}
		return json.NewDecoder(res.Body)
	}

	var inv Inventory
	if err := get("/api/setup/inventory").Decode(&inv); err != nil {
		t.Fatal(err)
	}
	if inv.Candidate != "/dev/nvme0n1" {
		t.Fatalf("candidate = %q", inv.Candidate)
	}

	// Nothing is running, so a restart is offered: it is the whole
	// instruction on the "this machine is already a Fog server" screen, and
	// the guard below is about disks being written, not about the wizard
	// having been walked. (Mid-install it must refuse —
	// TestRebootRefusedMidInstall.)
	res, err := srv.Client().Post(srv.URL+"/api/setup/reboot", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("reboot from an idle installer: %d, want 200", res.StatusCode)
	}

	body, _ := json.Marshal(goodCfg())
	res, err = srv.Client().Post(srv.URL+"/api/setup/install", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("install: %d", res.StatusCode)
	}

	// The SSE stream must tell the whole story through to the final step.
	sres, err := srv.Client().Get(srv.URL + "/api/setup/events")
	if err != nil {
		t.Fatal(err)
	}
	defer sres.Body.Close()
	scanner := bufio.NewScanner(sres.Body)
	sawFinal := false
	deadline := time.After(5 * time.Second)
	lines := make(chan string)
	go func() {
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		close(lines)
	}()
	for !sawFinal {
		select {
		case <-deadline:
			t.Fatal("SSE stream never delivered the final step")
		case line, ok := <-lines:
			if !ok {
				t.Fatal("SSE stream closed early")
			}
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var p Progress
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &p); err != nil {
				t.Fatal(err)
			}
			if p.ID == "verify" && p.Status == "done" {
				sawFinal = true
			}
		}
	}

	waitPhase(t, a, "done")
	var st AgentState
	if err := get("/api/setup/state").Decode(&st); err != nil {
		t.Fatal(err)
	}
	if st.Phase != "done" || st.Result == nil || st.Result.ClaimToken != "" {
		t.Fatalf("state after install (token must not be in state): %+v", st)
	}

	// The token comes from its own one-shot endpoint: once, then 404.
	res, err = srv.Client().Post(srv.URL+"/api/setup/claim-token", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	var tok struct {
		ClaimToken string `json:"claimToken"`
	}
	json.NewDecoder(res.Body).Decode(&tok)
	res.Body.Close()
	if res.StatusCode != 200 || tok.ClaimToken == "" {
		t.Fatalf("claim-token: %d %+v", res.StatusCode, tok)
	}
	res, err = srv.Client().Post(srv.URL+"/api/setup/claim-token", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 404 {
		t.Fatalf("second claim-token take: %d, want 404", res.StatusCode)
	}
}

func TestHandlerRefusesCrossOrigin(t *testing.T) {
	// The API is unauthenticated on the LAN — a malicious page in a LAN
	// browser must not be able to fire a no-preflight POST at it.
	a := fastAgent()
	body, _ := json.Marshal(goodCfg())
	req := newTestRequest(t, "POST", "/api/setup/install", strings.NewReader(string(body)), "text/plain")
	req.Header.Set("Origin", "http://evil.example")
	if res := doHandler(a, req); res.Code != 403 {
		t.Fatalf("cross-origin install: %d, want 403", res.Code)
	}
	// Same-origin traffic (the wizard itself, at an address of this
	// machine's) passes.
	req = newTestRequest(t, "POST", "/api/setup/install", strings.NewReader(string(body)), "application/json")
	req.Host = "localhost:8080"
	req.Header.Set("Origin", "http://"+req.Host)
	if res := doHandler(a, req); res.Code != 200 {
		t.Fatalf("same-origin install: %d %s", res.Code, res.Body.String())
	}
}
