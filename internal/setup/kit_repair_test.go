package setup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/auth"
	"gitea.lan/matthew/fog/internal/backup"
	"gitea.lan/matthew/fog/internal/store"
	"gitea.lan/matthew/fog/internal/store/storetest"
)

// makeKit builds a real sealed kit: master.key + an actual fog.db carrying
// identity, sealed with the real crypto.
func makeKit(t *testing.T, passphrase string) []byte {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "fog.db")
	st, err := storetest.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	st.SetSetting(store.SettingServerName, "attic")
	hash, err := auth.HashPassword("attic-admin-password")
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.CreatePerson("matthew", "matthew", store.PersonRoleOperator)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddPersonCredential(id, store.CredentialPassword, hash, "password"); err != nil {
		t.Fatal(err)
	}
	st.Close()
	db, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range []struct {
		name string
		body []byte
	}{
		{"master.key", []byte("thirty-two-byte-master-key......")},
		{"fog.db", db},
	} {
		tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o600, Size: int64(len(f.body))})
		tw.Write(f.body)
	}
	tw.Close()
	gz.Close()
	sealed, err := backup.EncryptKit(buf.Bytes(), passphrase)
	if err != nil {
		t.Fatal(err)
	}
	return sealed
}

func TestParseKit(t *testing.T) {
	sealed := makeKit(t, "kit pass phrase")
	kit, err := ParseKit(sealed, "kit pass phrase")
	if err != nil {
		t.Fatal(err)
	}
	if kit.ServerName != "attic" || kit.AdminUser != "matthew" {
		t.Fatalf("peeked identity: %q/%q", kit.ServerName, kit.AdminUser)
	}
	if _, err := ParseKit(sealed, "wrong"); err == nil {
		t.Fatal("wrong passphrase accepted")
	}
}

func TestRestoreInstallStagesKit(t *testing.T) {
	ex, _, cfg := newRealTest(t)
	sealed := makeKit(t, "kit pass phrase")

	a := &Agent{Inventory: MockInventory, Exec: ex}
	info, err := a.LoadKit(sealed, "kit pass phrase")
	if err != nil {
		t.Fatal(err)
	}
	if info.ServerName != "attic" {
		t.Fatalf("kit info: %+v", info)
	}

	cfg.Mode = "restore"
	cfg.ServerName = "" // the kit's own name should win
	cfg.Username, cfg.Password = "", ""
	if err := a.Start(cfg); err != nil {
		t.Fatal(err)
	}
	st := waitPhase(t, a, "done")
	if st.Result == nil || st.Result.ClaimToken != "" {
		t.Fatalf("restore must not mint a claim token: %+v", st.Result)
	}
	if !strings.Contains(st.Result.URLs[0], "attic.local") {
		t.Fatalf("restore URLs should carry the kit's server name: %v", st.Result.URLs)
	}
	// The kit landed exactly where fogd's boot consumes it.
	for _, f := range []string{"fog.db", "master.key"} {
		if _, err := os.Stat(filepath.Join(ex.mnt, "data", "restore-pending", f)); err != nil {
			t.Fatalf("restore-pending/%s: %v", f, err)
		}
	}
	// And no account preseed — the restored database owns identity.
	if _, err := os.Stat(filepath.Join(ex.mnt, "data", "firstboot.json")); !os.IsNotExist(err) {
		t.Fatal("restore wrote a firstboot preseed over the restored identity")
	}
}

func TestRestoreWithPasswordMirrorsUnixAccount(t *testing.T) {
	ex, fake, cfg := newRealTest(t)
	sealed := makeKit(t, "kit pass phrase")
	a := &Agent{Inventory: MockInventory, Exec: ex}
	if _, err := a.LoadKit(sealed, "kit pass phrase"); err != nil {
		t.Fatal(err)
	}

	// The wrong password is refused before anything starts — the kit's own
	// hash is the referee.
	cfg.Mode = "restore"
	cfg.Username = ""
	cfg.Password = "not-the-admin-password"
	if err := a.Start(cfg); err == nil || !strings.Contains(err.Error(), "admin password") {
		t.Fatalf("wrong restore password: %v", err)
	}

	// The right password creates the console/ssh account for the restored
	// admin — and still stages no firstboot preseed.
	cfg.Password = "attic-admin-password"
	if err := a.Start(cfg); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, a, "done")
	joined := strings.Join(fake.calls, "\n")
	if !strings.Contains(joined, "useradd -m -s /bin/bash -G sudo,docker matthew") {
		t.Fatal("restore with a verified password created no unix account")
	}
	if strings.Contains(ex.Transcript.String(), "attic-admin-password") {
		t.Fatal("the restore password leaked into the transcript")
	}
	if _, err := os.Stat(filepath.Join(ex.mnt, "data", "firstboot.json")); !os.IsNotExist(err) {
		t.Fatal("restore wrote a firstboot preseed")
	}
}

func TestRestoreWithoutKitRefused(t *testing.T) {
	a := &Agent{Inventory: MockInventory, Exec: MockExecutor{StepDelay: time.Millisecond}}
	cfg := goodCfg()
	cfg.Mode = "restore"
	if err := a.Start(cfg); err == nil || !strings.Contains(err.Error(), "kit") {
		t.Fatalf("restore without kit: %v", err)
	}
}

func TestInstallRefusedInRepairMode(t *testing.T) {
	// The recovery partition a repair boot runs from lives on the target
	// disk — the fresh installer must never run there.
	r := &Agent{Inventory: MockInventory, Exec: MockExecutor{StepDelay: time.Millisecond}, Mode: "repair"}
	if err := r.Start(goodCfg()); err == nil || !strings.Contains(err.Error(), "Fog Recovery") {
		t.Fatalf("install in repair mode: %v", err)
	}
	if r.State().Phase != "idle" {
		t.Fatalf("phase = %q after refused install, want idle", r.State().Phase)
	}
}

func TestRepairModeGates(t *testing.T) {
	// Install mode: repairs refused.
	a := &Agent{Inventory: MockInventory, Exec: MockExecutor{StepDelay: time.Millisecond}}
	if err := a.StartRepair("fsck"); err == nil {
		t.Fatal("repair allowed outside repair mode")
	}
	// Repair mode: unknown actions refused, real ones run and re-run.
	r := &Agent{Inventory: MockInventory, Exec: MockExecutor{StepDelay: time.Millisecond}, Mode: "repair"}
	if err := r.StartRepair("format-c"); err == nil {
		t.Fatal("unknown repair action accepted")
	}
	if err := r.StartRepair("fsck"); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, r, "done")
	if err := r.resetAfterRepair(); err != nil {
		t.Fatal(err)
	}
	if err := r.StartRepair("rebuild-docker"); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, r, "done")
}

func TestResetRefusedOutsideRepairMode(t *testing.T) {
	// One install per boot: in install mode, a reset from "done" would let
	// a second install re-erase the just-installed disk.
	a := &Agent{Inventory: MockInventory, Exec: MockExecutor{StepDelay: time.Millisecond}}
	if err := a.Start(goodCfg()); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, a, "done")
	if err := a.resetAfterRepair(); err == nil {
		t.Fatal("reset allowed in install mode")
	}
	if a.State().Phase != "done" {
		t.Fatalf("phase = %q after refused reset, want done", a.State().Phase)
	}
	if err := a.Start(goodCfg()); err == nil {
		t.Fatal("second install allowed after refused reset")
	}
}

func TestRebootRefusedMidRepair(t *testing.T) {
	// A reboot between mkfs and verify during reinstall-os would leave a
	// half-written OS partition — the gate must hold while a repair runs.
	r := &Agent{Inventory: MockInventory, Exec: MockExecutor{StepDelay: 2 * time.Second}, Mode: "repair"}
	if err := r.StartRepair("reinstall-os"); err != nil {
		t.Fatal(err)
	}
	res := doHandler(r, newTestRequest(t, "POST", "/api/setup/reboot", nil, "application/json"))
	if res.Code != 400 {
		t.Fatalf("reboot mid-repair: %d, want 400", res.Code)
	}
	// An idle repair menu still offers restart.
	idle := &Agent{Inventory: MockInventory, Exec: MockExecutor{}, Mode: "repair"}
	res = doHandler(idle, newTestRequest(t, "POST", "/api/setup/reboot", nil, "application/json"))
	if res.Code != 200 {
		t.Fatalf("reboot from idle repair menu: %d, want 200", res.Code)
	}
}

func TestRebootRefusedMidInstall(t *testing.T) {
	// The one state a restart must never be granted from: the installer is
	// writing to the disk.
	a := &Agent{Inventory: MockInventory, Exec: MockExecutor{StepDelay: 2 * time.Second}}
	if err := a.Start(InstallConfig{
		Mode: "fresh", Disk: "/dev/nvme0n1", ConfirmDisk: "/dev/nvme0n1",
		ServerName: "fog", Username: "matthew", Password: "correct-horse-battery",
	}); err != nil {
		t.Fatal(err)
	}
	res := doHandler(a, newTestRequest(t, "POST", "/api/setup/reboot", nil, "application/json"))
	if res.Code != 400 {
		t.Fatalf("reboot mid-install: %d, want 400", res.Code)
	}
}

func TestRepairRebuildDockerKeepsVolumes(t *testing.T) {
	mnt := t.TempDir()
	// Simulated mounted data partition with a docker-root.
	dataDir := filepath.Join(mnt, "repair-data")
	for _, d := range []string{"docker-root/volumes/appdata", "docker-root/overlay2/xyz", "docker-root/image", "containerd/io.containerd.content.v1.content/blobs"} {
		if err := os.MkdirAll(filepath.Join(dataDir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fake := func(_ context.Context, name string, args ...string) (string, error) {
		switch name {
		case "ls":
			entries, err := os.ReadDir(args[0])
			if err != nil {
				return "", err
			}
			var names []string
			for _, e := range entries {
				names = append(names, e.Name())
			}
			return strings.Join(names, "\n"), nil
		case "rm":
			return "", os.RemoveAll(args[len(args)-1])
		}
		return "", nil
	}
	ex := &RealExecutor{MountRoot: mnt, Exec: fake, Transcript: &strings.Builder{}}
	if err := ex.Repair(context.Background(), "rebuild-docker", func(Progress) {}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "docker-root/volumes/appdata")); err != nil {
		t.Fatal("volumes did not survive the rebuild")
	}
	for _, gone := range []string{"docker-root/overlay2", "docker-root/image", "containerd"} {
		if _, err := os.Stat(filepath.Join(dataDir, gone)); !os.IsNotExist(err) {
			t.Fatalf("%s survived the rebuild", gone)
		}
	}
}

func TestPruneBootEntriesKeepsRecovery(t *testing.T) {
	mnt := t.TempDir()
	esp := filepath.Join(mnt, "esp")
	// Two generations of orphaned OS entries plus the recovery set.
	for _, d := range []string{
		"loader/entries", "0123456789abcdef0123456789abcdef/6.12.30", "fedcba9876543210fedcba9876543210/6.12.9", "fog-recovery", "EFI/BOOT",
	} {
		if err := os.MkdirAll(filepath.Join(esp, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{
		"loader/entries/0123456789abcdef0123456789abcdef-6.12.30+3.conf",
		"loader/entries/fedcba9876543210fedcba9876543210-6.12.9.conf",
		"loader/entries/fog-recovery.conf",
	} {
		if err := os.WriteFile(filepath.Join(esp, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ex := &RealExecutor{MountRoot: mnt}
	ex.mnt = mnt
	if err := ex.pruneBootEntries(); err != nil {
		t.Fatal(err)
	}
	left, _ := filepath.Glob(filepath.Join(esp, "loader/entries/*.conf"))
	if len(left) != 1 || filepath.Base(left[0]) != "fog-recovery.conf" {
		t.Fatalf("entries after prune: %v", left)
	}
	for _, gone := range []string{"0123456789abcdef0123456789abcdef", "fedcba9876543210fedcba9876543210"} {
		if _, err := os.Stat(filepath.Join(esp, gone)); !os.IsNotExist(err) {
			t.Fatalf("stale kernel tree %s survived", gone)
		}
	}
	// Non-machine-id directories (recovery kernel, EFI) are untouched.
	for _, kept := range []string{"fog-recovery", "EFI/BOOT"} {
		if _, err := os.Stat(filepath.Join(esp, kept)); err != nil {
			t.Fatalf("%s should survive the prune: %v", kept, err)
		}
	}
}

func TestVerifyReinstallDemandsBootEntry(t *testing.T) {
	mnt := t.TempDir()
	for _, f := range []string{"root/usr/local/bin/fogd", "root/etc/fstab", "esp/loader/entries/fog-recovery.conf"} {
		p := filepath.Join(mnt, f)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("x"), 0o644)
	}
	ex := &RealExecutor{MountRoot: mnt, Transcript: &strings.Builder{},
		Exec: func(context.Context, string, ...string) (string, error) { return "", nil }}
	ex.mnt = mnt
	if err := ex.verifyReinstall(context.Background()); err == nil || !strings.Contains(err.Error(), "boot entry") {
		t.Fatalf("reinstall verified with only the recovery entry: %v", err)
	}
}

func TestKitEndpoint(t *testing.T) {
	a := &Agent{Inventory: MockInventory, Exec: MockExecutor{StepDelay: time.Millisecond}}
	sealed := makeKit(t, "pp")
	var body bytes.Buffer
	contentType := newMultipart(t, &body, sealed, "pp")
	req := newTestRequest(t, "POST", "/api/setup/kit", &body, contentType)
	res := doHandler(a, req)
	if res.Code != 200 {
		t.Fatalf("kit upload: %d %s", res.Code, res.Body.String())
	}
	var out struct {
		Kit KitInfo `json:"kit"`
	}
	json.Unmarshal(res.Body.Bytes(), &out)
	if out.Kit.ServerName != "attic" {
		t.Fatalf("kit info over http: %+v", out)
	}
	// Wrong passphrase surfaces as a 400 with the honest message.
	var bad bytes.Buffer
	contentType = newMultipart(t, &bad, sealed, "nope")
	res = doHandler(a, newTestRequest(t, "POST", "/api/setup/kit", &bad, contentType))
	if res.Code != 400 || !strings.Contains(res.Body.String(), "passphrase") {
		t.Fatalf("wrong passphrase: %d %s", res.Code, res.Body.String())
	}
}

// An upload past the cap is refused with the limit stated — before the
// tmpfs-rooted live installer buffers it into the RAM the install itself
// needs. Recovery kits are a few MB; anything bigger is the wrong file.
func TestKitEndpointRefusesOversizedUpload(t *testing.T) {
	a := &Agent{Inventory: MockInventory, Exec: MockExecutor{StepDelay: time.Millisecond}}
	var body bytes.Buffer
	contentType := newMultipart(t, &body, make([]byte, maxKitFile+1), "pp")
	res := doHandler(a, newTestRequest(t, "POST", "/api/setup/kit", &body, contentType))
	if res.Code != 413 || !strings.Contains(res.Body.String(), "64MB limit") {
		t.Fatalf("oversized upload: %d %s", res.Code, res.Body.String())
	}
}
