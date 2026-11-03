package setup

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitea.lan/matthew/fog/internal/auth"
	"gitea.lan/matthew/fog/internal/firstboot"
)

// fakeSystem simulates the commands the real executor shells out to, with
// just enough filesystem side effects for the executor's own file writes
// and final verification to run for real. What this pins down: the step
// sequencing, the wipe-ordering, the checksum gate, the staged firstboot
// handoff, and that verification actually verifies.
type fakeSystem struct {
	t     *testing.T
	mnt   string
	calls []string
	// failOn, when set, makes that command fail once.
	failOn string
}

func (f *fakeSystem) exec(_ context.Context, name string, args ...string) (string, error) {
	call := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, call)
	if f.failOn != "" && strings.Contains(call, f.failOn) {
		f.failOn = ""
		return "simulated failure", os.ErrInvalid
	}
	touch := func(rel string) {
		path := filepath.Join(f.mnt, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			f.t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			f.t.Fatal(err)
		}
	}
	switch {
	case name == "blockdev":
		return "500107862016\n", nil
	case name == "sh" && strings.Contains(call, "sha256sum -c"):
		// the checksum gate; success unless failOn tripped above
	case name == "unsquashfs":
		// The extracted OS tree, as far as later steps look at it.
		for _, p := range []string{
			"root/usr/local/bin/fogd",
			"root/usr/lib/modules/6.12.30-amd64/vmlinuz",
			"root/etc/os-release",
		} {
			touch(p)
		}
	case name == "cp" && len(args) == 3 && strings.HasSuffix(args[1], "/live") && !strings.Contains(call, "stick-esp"):
		touch("recovery/live/filesystem.squashfs")
	case name == "cp" && len(args) == 3 && strings.HasSuffix(args[1], "/fog") && !strings.Contains(call, "stick-esp"):
		touch("recovery/fog/rootfs.squashfs")
	case name == "cp" && strings.Contains(call, "stick-esp"):
		// recovery kernel/initrd landing on the target ESP
		rel, _ := filepath.Rel(f.mnt, args[len(args)-1])
		touch(rel)
	case name == "chroot" && len(args) > 1 && args[1] == "bootctl":
		touch("esp/EFI/BOOT/BOOTX64.EFI")
		touch("esp/EFI/systemd/systemd-bootx64.efi")
	case name == "chroot" && len(args) > 1 && args[1] == "kernel-install":
		touch("esp/loader/entries/0abc-6.12.30-amd64+3.conf")
	case name == "chroot" && strings.Contains(call, "ls /usr/lib/modules"):
		return "6.12.30-amd64\n", nil
	}
	return "", nil
}

func newRealTest(t *testing.T) (*RealExecutor, *fakeSystem, InstallConfig) {
	t.Helper()
	mnt := t.TempDir()
	payload := t.TempDir()
	os.WriteFile(filepath.Join(payload, "rootfs.squashfs"), []byte("squash"), 0o644)
	os.WriteFile(filepath.Join(payload, "rootfs.squashfs.sha256"), []byte("irrelevant"), 0o644)
	fake := &fakeSystem{t: t, mnt: mnt}
	ex := &RealExecutor{
		PayloadDir: payload,
		MediumDir:  t.TempDir(),
		MountRoot:  mnt,
		StateDir:   t.TempDir(),
		Exec:       fake.exec,
		ExecStdin: func(ctx context.Context, stdin, name string, args ...string) (string, error) {
			return fake.exec(ctx, name, args...)
		},
		Transcript: &strings.Builder{},
	}
	cfg := InstallConfig{
		Mode: "fresh", Disk: "/dev/nvme0n1", ConfirmDisk: "/dev/nvme0n1",
		ServerName: "fog", Username: "matthew", Password: "correct-horse-battery",
		Timezone: "America/New_York",
	}
	return ex, fake, cfg
}

func TestRealExecutorHappyPath(t *testing.T) {
	ex, fake, cfg := newRealTest(t)
	var events []Progress
	res, err := ex.Run(context.Background(), cfg, func(p Progress) { events = append(events, p) })
	if err != nil {
		t.Fatalf("install failed: %v\ntranscript:\n%s", err, ex.Transcript.String())
	}
	if res.ClaimToken == "" || len(res.URLs) == 0 || !strings.Contains(res.URLs[0], "fog.local") {
		t.Fatalf("result: %+v", res)
	}

	// Destruction ordering: the payload checksum comes before first contact
	// with the disk (wipefs), which comes before any mkfs.
	joined := strings.Join(fake.calls, "\n")
	if !strings.Contains(joined, "wipefs -a /dev/nvme0n1") {
		t.Fatal("wipefs never ran")
	}
	if !strings.Contains(joined, "sha256sum -c") {
		t.Fatal("payload checksum never verified")
	}
	if strings.Index(joined, "sha256sum -c") > strings.Index(joined, "wipefs") {
		t.Fatal("wiped the disk before verifying the payload checksum")
	}
	if strings.Index(joined, "wipefs") > strings.Index(joined, "mkfs.vfat") {
		t.Fatal("formatted before wiping/partitioning")
	}

	// The transcript is diagnostics — served to any LAN browser and copied
	// to the stick on failure — so the password must never enter it.
	if strings.Contains(ex.Transcript.String(), cfg.Password) {
		t.Fatal("the admin password leaked into the install transcript")
	}

	// The staged handoff: pre-hashed account + valid claim digest.
	raw, err := os.ReadFile(filepath.Join(ex.mnt, "data", firstboot.FileName))
	if err != nil {
		t.Fatal(err)
	}
	var fb firstboot.Config
	if err := json.Unmarshal(raw, &fb); err != nil {
		t.Fatal(err)
	}
	if fb.Username != "matthew" || !strings.HasPrefix(fb.PasswordHash, "$argon2id$") {
		t.Fatalf("firstboot: %+v", fb)
	}
	if !auth.VerifyPassword("correct-horse-battery", fb.PasswordHash) {
		t.Fatal("staged hash doesn't verify the wizard password")
	}
	if fb.ClaimTokenDigest != auth.DigestToken(res.ClaimToken) {
		t.Fatal("staged claim digest doesn't match the returned token")
	}
	// A duration, never an absolute deadline: the live installer's clock may
	// be wrong, so firstboot stamps the expiry from its own clock at consume.
	if fb.ClaimTokenTTL != firstboot.DefaultClaimTTL {
		t.Fatalf("claim deadline staged wrong: ttl=%q", fb.ClaimTokenTTL)
	}

	// System files the installed OS depends on.
	for rel, want := range map[string]string{
		"root/etc/fstab":                       "LABEL=fog-data  /var/lib/fog",
		"root/etc/hostname":                    "fog",
		"esp/loader/loader.conf":               "timeout 0",
		"esp/loader/entries/fog-recovery.conf": "fog.repair=1",
		"root/etc/docker/daemon.json":          "/var/lib/fog/docker-root",
		"root/etc/containerd/config.toml":      `root = "/var/lib/fog/containerd"`,
	} {
		raw, err := os.ReadFile(filepath.Join(ex.mnt, rel))
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		if !strings.Contains(string(raw), want) {
			t.Fatalf("%s missing %q:\n%s", rel, want, raw)
		}
	}

	// Every step reported done.
	done := map[string]bool{}
	for _, ev := range events {
		if ev.Status == "done" {
			done[ev.ID] = true
		}
	}
	if len(done) != len(InstallSteps) {
		t.Fatalf("steps done = %d, want %d", len(done), len(InstallSteps))
	}
}

func TestRealExecutorChecksumGate(t *testing.T) {
	ex, fake, cfg := newRealTest(t)
	fake.failOn = "sha256sum -c"
	_, err := ex.Run(context.Background(), cfg, func(Progress) {})
	if err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("want checksum failure, got %v", err)
	}
	// A failed checksum must leave the operator's disk exactly as it was:
	// no wipe, no partitioning, nothing extracted.
	joined := strings.Join(fake.calls, "\n")
	for _, forbidden := range []string{"wipefs", "sfdisk", "unsquashfs -f"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("%s ran after the payload failed its checksum", forbidden)
		}
	}
}

func TestInstallRefusesForeignFogLabels(t *testing.T) {
	// An old Fog disk still attached would make the installed system's
	// by-label fstab ambiguous — the install must refuse before wiping.
	ex, fake, cfg := newRealTest(t)
	inner := fake.exec
	ex.Exec = func(ctx context.Context, name string, args ...string) (string, error) {
		if name == "lsblk" {
			return `PKNAME="sdb" LABEL="fog-root"` + "\n" + `PKNAME="sdb" LABEL="fog-data"` + "\n", nil
		}
		return inner(ctx, name, args...)
	}
	_, err := ex.Run(context.Background(), cfg, func(Progress) {})
	if err == nil || !strings.Contains(err.Error(), "unplug") {
		t.Fatalf("install with a foreign fog-labeled disk: %v", err)
	}
	if strings.Contains(strings.Join(fake.calls, "\n"), "wipefs") {
		t.Fatal("wiped the disk despite the label conflict")
	}
	// Reinstalling over the target's own labels stays allowed.
	ex2, fake2, cfg2 := newRealTest(t)
	inner2 := fake2.exec
	ex2.Exec = func(ctx context.Context, name string, args ...string) (string, error) {
		if name == "lsblk" {
			return `PKNAME="nvme0n1" LABEL="fog-root"` + "\n", nil
		}
		return inner2(ctx, name, args...)
	}
	if _, err := ex2.Run(context.Background(), cfg2, func(Progress) {}); err != nil {
		t.Fatalf("reinstall over the target's own labels refused: %v", err)
	}
}

func TestRepairRefusesDuplicateLabels(t *testing.T) {
	ex := &RealExecutor{MountRoot: t.TempDir(), Transcript: &strings.Builder{},
		Exec: func(_ context.Context, name string, _ ...string) (string, error) {
			if name == "lsblk" {
				return `PKNAME="sda" LABEL="fog-root"` + "\n" + `PKNAME="sdb" LABEL="fog-root"` + "\n", nil
			}
			return "", nil
		}}
	err := ex.Repair(context.Background(), "fsck", func(Progress) {})
	if err == nil || !strings.Contains(err.Error(), "unplug") {
		t.Fatalf("repair with duplicate fog-root labels: %v", err)
	}
}

func TestFailedInstallUnmountsEverything(t *testing.T) {
	// A failure late in the install (bootloader) must tear the mount tree
	// down again — "try again" restarts from mkfs and mount, which both
	// fail on leftovers.
	ex, fake, cfg := newRealTest(t)
	fake.failOn = "bootctl"
	_, err := ex.Run(context.Background(), cfg, func(Progress) {})
	if err == nil {
		t.Fatal("want a bootloader failure")
	}
	joined := strings.Join(fake.calls, "\n")
	for _, m := range []string{"root/boot", "data", "recovery", "root", "esp"} {
		if !strings.Contains(joined, "umount "+filepath.Join(ex.mnt, m)) {
			t.Fatalf("%s never unmounted after the failure", m)
		}
	}
}

func TestRealExecutorVerifyCatchesMissingBootEntry(t *testing.T) {
	ex, fake, cfg := newRealTest(t)
	// kernel-install "succeeds" but writes nothing — the verify step must
	// refuse to call this install done.
	failKernel := func(ctx context.Context, name string, args ...string) (string, error) {
		if name == "chroot" && len(args) > 1 && args[1] == "kernel-install" {
			fake.calls = append(fake.calls, "kernel-install (no-op)")
			return "", nil
		}
		return fake.exec(ctx, name, args...)
	}
	ex.Exec = failKernel
	_, err := ex.Run(context.Background(), cfg, func(Progress) {})
	if err == nil || !strings.Contains(err.Error(), "boot entry") {
		t.Fatalf("verify let a bootless install pass: %v", err)
	}
}

func TestSuccessfulInstallWritesMarker(t *testing.T) {
	ex, _, cfg := newRealTest(t)
	ex.Version = "9.9.9"
	if _, err := ex.Run(context.Background(), cfg, func(Progress) {}); err != nil {
		t.Fatalf("install failed: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(ex.MountRoot, "esp", InstallMarkerName))
	if err != nil {
		t.Fatalf("no completed-install marker on the ESP: %v", err)
	}
	m := parseInstallMarker(raw)
	if m == nil {
		t.Fatalf("marker didn't parse: %s", raw)
	}
	if m.ServerName != "fog" || m.Mode != "fresh" || m.Version != "9.9.9" || m.InstalledAt == "" {
		t.Fatalf("marker: %+v", m)
	}
}

func TestInFlightMarkerLifecycle(t *testing.T) {
	// The in-flight marker is how a crash-restarted agent knows an install
	// died mid-write: it must exist before first disk contact and be gone
	// again on both exits that ran their own cleanup.
	ex, fake, cfg := newRealTest(t)
	ex.StateDir = t.TempDir()
	marker := filepath.Join(ex.StateDir, "install-in-flight")
	sawAtDiskContact := false
	inner := fake.exec
	ex.Exec = func(ctx context.Context, name string, args ...string) (string, error) {
		if name == "blockdev" {
			if _, err := os.Stat(marker); err == nil {
				sawAtDiskContact = true
			}
		}
		return inner(ctx, name, args...)
	}
	if _, err := ex.Run(context.Background(), cfg, func(Progress) {}); err != nil {
		t.Fatalf("install failed: %v", err)
	}
	if !sawAtDiskContact {
		t.Fatal("in-flight marker absent when the disk was first touched")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("in-flight marker survived a successful install")
	}

	// The failure path cleans up after itself too — the marker means
	// "cleanup never ran", and here it did.
	ex2, fake2, cfg2 := newRealTest(t)
	ex2.StateDir = t.TempDir()
	fake2.failOn = "bootctl"
	if _, err := ex2.Run(context.Background(), cfg2, func(Progress) {}); err == nil {
		t.Fatal("want a bootloader failure")
	}
	if _, err := os.Stat(filepath.Join(ex2.StateDir, "install-in-flight")); err == nil {
		t.Fatal("in-flight marker survived a failure that ran its cleanup")
	}
}

func TestRecoverInterruptedInstall(t *testing.T) {
	state := t.TempDir()
	// No marker, no mount root: a clean boot recovers nothing.
	if RecoverInterruptedInstall(context.Background(), state, filepath.Join(state, "no-such-mnt"), nil) {
		t.Fatal("clean state reported as an interrupted install")
	}
	// A leftover marker is exactly what a crashed agent leaves: report it,
	// and consume it so the answer is one restart's worth of truth — the
	// next startup is clean again.
	if err := os.WriteFile(filepath.Join(state, "install-in-flight"), []byte("2026-08-24T00:00:00Z\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !RecoverInterruptedInstall(context.Background(), state, filepath.Join(state, "no-such-mnt"), nil) {
		t.Fatal("leftover in-flight marker not reported")
	}
	if _, err := os.Stat(filepath.Join(state, "install-in-flight")); err == nil {
		t.Fatal("marker not consumed — every restart would re-report the same crash")
	}
	if RecoverInterruptedInstall(context.Background(), state, filepath.Join(state, "no-such-mnt"), nil) {
		t.Fatal("consumed marker reported again")
	}
}

func TestFailedInstallLeavesNoMarker(t *testing.T) {
	// The marker is what a later boot reads as "this machine is already a
	// server" — an install that died must never leave one behind.
	ex, fake, cfg := newRealTest(t)
	fake.failOn = "unsquashfs"
	if _, err := ex.Run(context.Background(), cfg, func(Progress) {}); err == nil {
		t.Fatal("install reported success despite a failing step")
	}
	if _, err := os.Stat(filepath.Join(ex.MountRoot, "esp", InstallMarkerName)); err == nil {
		t.Fatal("a failed install wrote the completed-install marker")
	}
}
