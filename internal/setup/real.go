package setup

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gitea.lan/matthew/fog/internal/auth"
	"gitea.lan/matthew/fog/internal/firstboot"
)

// RealExecutor installs Fog onto actual hardware: partition, format, extract
// the known-good rootfs, write the recovery copy, install the bootloader,
// stage the first-boot configuration, verify. Every external command runs
// through Run so the whole session is captured in a transcript — on failure
// the transcript is the diagnostics bundle, copied to the stick's FOG-DATA
// partition where any computer can read it.
type RealExecutor struct {
	// PayloadDir holds rootfs.squashfs (+ .sha256) — on the booted stick,
	// /run/live/medium/fog.
	PayloadDir string
	// MediumDir is the live medium root (live/filesystem.squashfs lives
	// under it); its contents become the recovery partition.
	MediumDir string
	// Exec runs a command and returns combined output. Overridden in tests;
	// nil gets the real thing. ExecStdin is the same for commands fed via
	// stdin (sfdisk scripts, chpasswd).
	Exec      func(ctx context.Context, name string, args ...string) (string, error)
	ExecStdin func(ctx context.Context, stdin, name string, args ...string) (string, error)
	// Transcript receives every command and its output.
	Transcript *strings.Builder
	// MountRoot for the target's partitions (tests point it at a temp dir).
	MountRoot string
	// StateDir holds the installer's own runtime state — the in-flight
	// marker. Default /run/fog-installer; tests point it at a temp dir.
	StateDir string
	// Version stamps the completed-install marker, so a later boot of the
	// installer can say which Fog put the system there.
	Version string

	mnt string
}

const (
	defaultMnt      = "/run/fog-installer/mnt"
	defaultStateDir = "/run/fog-installer"
	// inFlightName is the marker recording that an install started and has
	// not yet reached its own cleanup. The unit runs Restart=always, so a
	// crashed agent comes back blank — this file is how the next process
	// knows the blankness is a lie.
	inFlightName = "install-in-flight"
)

func (e *RealExecutor) stateDir() string {
	if e.StateDir != "" {
		return e.StateDir
	}
	return defaultStateDir
}

// markInFlight records that the disk is about to be (or being) written.
// Best-effort: an unwritable /run must not stop an install, it only costs
// the crash diagnosis.
func (e *RealExecutor) markInFlight() {
	if err := os.MkdirAll(e.stateDir(), 0o755); err != nil {
		e.log("! writing the in-flight marker: %v", err)
		return
	}
	if err := os.WriteFile(filepath.Join(e.stateDir(), inFlightName), []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644); err != nil {
		e.log("! writing the in-flight marker: %v", err)
	}
}

// clearInFlight is called only once the run has cleaned up after itself —
// success, or the failure path's unmountAll. Anything else (panic, OOM kill)
// leaves the marker for RecoverInterruptedInstall to find.
func (e *RealExecutor) clearInFlight() {
	os.Remove(filepath.Join(e.stateDir(), inFlightName))
}

// RecoverInterruptedInstall runs once at agent startup (live boots only).
// The unit's Restart=always means a crashed installer comes back blank:
// systemd killed the in-flight commands with it, nothing ran the failure
// path's unmountAll, and the kiosk would show the welcome screen as if
// nothing happened — with a retry doomed to fail on the leftovers (mkfs
// refuses mounted devices). This sweeps those stale mounts and reports
// whether an install was in flight when the previous agent died, so the
// caller can start errored instead of idle.
func RecoverInterruptedInstall(ctx context.Context, stateDir, mountRoot string, log *slog.Logger) bool {
	if stateDir == "" {
		stateDir = defaultStateDir
	}
	if mountRoot == "" {
		mountRoot = defaultMnt
	}
	marker := filepath.Join(stateDir, inFlightName)
	_, err := os.Stat(marker)
	interrupted := err == nil
	// The mount root only exists once an install has run this boot (it lives
	// on tmpfs) — a clean startup skips the sweep entirely.
	if _, err := os.Stat(mountRoot); err == nil {
		e := &RealExecutor{MountRoot: mountRoot, Exec: runCmd, ExecStdin: runCmdStdin, Transcript: &strings.Builder{}}
		e.mnt = mountRoot
		e.unmountAll(ctx, installMounts)
		if log != nil {
			log.Info("swept stale installer mounts", "root", mountRoot, "interrupted", interrupted)
		}
	}
	if interrupted {
		os.Remove(marker)
	}
	return interrupted
}

func (e *RealExecutor) log(format string, args ...any) {
	if e.Transcript != nil {
		fmt.Fprintf(e.Transcript, format+"\n", args...)
	}
}

func (e *RealExecutor) run(ctx context.Context, name string, args ...string) (string, error) {
	e.log("$ %s %s", name, strings.Join(args, " "))
	out, err := e.Exec(ctx, name, args...)
	if out != "" {
		e.log("%s", strings.TrimRight(out, "\n"))
	}
	if err != nil {
		e.log("! %v", err)
		return out, fmt.Errorf("%s: %w\n%s", name, err, tail(out, 12))
	}
	return out, nil
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// prepare fills in the defaults every entry point (install, repair) needs.
func (e *RealExecutor) prepare() {
	e.mnt = e.MountRoot
	if e.mnt == "" {
		e.mnt = defaultMnt
	}
	if e.Exec == nil {
		e.Exec = runCmd
	}
	if e.ExecStdin == nil {
		e.ExecStdin = runCmdStdin
	}
	if e.Transcript == nil {
		e.Transcript = &strings.Builder{}
	}
}

// Run implements Executor.
func (e *RealExecutor) Run(ctx context.Context, cfg InstallConfig, report func(Progress)) (Result, error) {
	e.prepare()
	// From here until cleanup, a dead agent means leftover mounts and a
	// half-written disk. The marker outlives a crash (tmpfs survives a
	// service restart) so the next agent can tell the operator the truth.
	e.markInFlight()
	var result Result
	do := map[string]stepFunc{
		"payload":   func(ctx context.Context, _ *stepRun) error { return e.verifyPayload(ctx) },
		"partition": func(ctx context.Context, _ *stepRun) error { return e.partition(ctx, cfg) },
		"format":    func(ctx context.Context, _ *stepRun) error { return e.format(ctx, cfg) },
		"rootfs": func(ctx context.Context, s *stepRun) error {
			return e.extractRootfs(ctx, cfg, s.Percent)
		},
		"recovery":   func(ctx context.Context, _ *stepRun) error { return e.writeRecovery(ctx, cfg) },
		"bootloader": func(ctx context.Context, _ *stepRun) error { return e.bootloader(ctx, cfg) },
		"configure": func(ctx context.Context, _ *stepRun) (err error) {
			result, err = e.configure(ctx, cfg)
			return err
		},
		"verify": func(ctx context.Context, _ *stepRun) error { return e.verify(ctx, cfg) },
	}
	err := runSteps(ctx, InstallSteps, do, report, func() {
		// Leave no mounts behind: the wizard's "try again" restarts from
		// mkfs and mount, both of which fail on the leftovers — without
		// this, the advertised retry could never actually succeed.
		e.unmountAll(ctx, installMounts)
		e.saveDiagnostics(ctx)
	})
	// On success verify already unmounted everything; the environment is
	// clean again either way.
	e.clearInFlight()
	if err != nil {
		return Result{}, err
	}
	return result, nil
}

// The mount tree each flow builds under e.mnt, teardown-ordered (innermost
// binds first).
var (
	installMounts = []string{"root/dev", "root/sys", "root/proc", "root/boot", "data", "recovery", "root", "esp"}
	repairMounts  = []string{"root/dev", "root/sys", "root/proc", "root/boot", "data", "root", "esp"}
)

// unmountAll tears the tree down, lazily when busy — used before reboot and
// on every failure path. Paths that never got mounted just log a failed
// umount into the transcript, which is honest and harmless.
func (e *RealExecutor) unmountAll(ctx context.Context, names []string) {
	for _, m := range names {
		path := filepath.Join(e.mnt, m)
		if _, err := e.run(ctx, "umount", path); err != nil {
			e.run(ctx, "umount", "-l", path)
		}
	}
}

func (e *RealExecutor) part(cfg InstallConfig, n int) string { return PartitionPath(cfg.Disk, n) }

// installedKernelVersion finds the (single) kernel the rootfs ships.
func installedKernelVersion(root string) (string, error) {
	entries, err := os.ReadDir(filepath.Join(root, "usr", "lib", "modules"))
	if err != nil || len(entries) == 0 {
		return "", fmt.Errorf("no kernel modules in the extracted rootfs: %v", err)
	}
	return entries[0].Name(), nil
}

func (e *RealExecutor) partition(ctx context.Context, cfg InstallConfig) error {
	sizeOut, err := e.run(ctx, "blockdev", "--getsize64", cfg.Disk)
	if err != nil {
		return err
	}
	var bytes int64
	if _, err := fmt.Sscanf(strings.TrimSpace(sizeOut), "%d", &bytes); err != nil {
		return fmt.Errorf("reading the size of %s: blockdev said %q: %w", cfg.Disk, strings.TrimSpace(sizeOut), err)
	}
	plan, err := PlanPartitions(bytes)
	if err != nil {
		return err
	}
	// The installed system finds its partitions by label (fstab, recovery).
	// A second disk already carrying fog labels — the old server's disk,
	// still attached — would make those lookups ambiguous after reboot, so
	// it has to be unplugged before this disk is touched. Reinstalling over
	// the target's own labels is fine.
	if owners, lerr := e.fogLabelOwners(ctx); lerr == nil {
		base := filepath.Base(cfg.Disk)
		for _, label := range fogLabels {
			for _, owner := range owners[label] {
				if owner != "" && owner != base {
					return fmt.Errorf("another attached disk (/dev/%s) already carries the %q label — unplug the old Fog disk before installing", owner, label)
				}
			}
		}
	}
	if _, err := e.run(ctx, "wipefs", "-a", cfg.Disk); err != nil {
		return err
	}
	if err := e.runWithStdin(ctx, plan.SfdiskScript(), "sfdisk", "--wipe", "always", cfg.Disk); err != nil {
		return err
	}
	// The kernel needs a beat to publish the new partition nodes.
	if _, err := e.run(ctx, "udevadm", "settle"); err != nil {
		return err
	}
	return nil
}

func (e *RealExecutor) runWithStdin(ctx context.Context, stdin, name string, args ...string) error {
	e.log("$ %s %s <<EOF\n%sEOF", name, strings.Join(args, " "), stdin)
	return e.execStdinLogged(ctx, stdin, name, args...)
}

// runWithSecretStdin logs the command but never its input: the transcript
// is served to the LAN as diagnostics and copied to the stick, so
// credentials must not enter it.
func (e *RealExecutor) runWithSecretStdin(ctx context.Context, stdin, name string, args ...string) error {
	e.log("$ %s %s <<EOF\n[redacted]\nEOF", name, strings.Join(args, " "))
	return e.execStdinLogged(ctx, stdin, name, args...)
}

func (e *RealExecutor) execStdinLogged(ctx context.Context, stdin, name string, args ...string) error {
	out, err := e.ExecStdin(ctx, stdin, name, args...)
	if out != "" {
		e.log("%s", strings.TrimRight(out, "\n"))
	}
	if err != nil {
		e.log("! %v", err)
		return fmt.Errorf("%s: %w\n%s", name, err, tail(out, 12))
	}
	return nil
}

func (e *RealExecutor) format(ctx context.Context, cfg InstallConfig) error {
	steps := [][]string{
		{"mkfs.vfat", "-F", "32", "-n", "FOG-ESP", e.part(cfg, 1)},
		{"mkfs.ext4", "-q", "-F", "-L", "fog-recovery", e.part(cfg, 2)},
		{"mkfs.ext4", "-q", "-F", "-L", "fog-root", e.part(cfg, 3)},
		{"mkfs.ext4", "-q", "-F", "-L", "fog-data", e.part(cfg, 4)},
	}
	for _, s := range steps {
		if _, err := e.run(ctx, s[0], s[1:]...); err != nil {
			return err
		}
	}
	for i, name := range []string{"esp", "recovery", "root", "data"} {
		dir := filepath.Join(e.mnt, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if _, err := e.run(ctx, "mount", e.part(cfg, i+1), dir); err != nil {
			return err
		}
	}
	return nil
}

// verifyPayload checksums the OS payload BEFORE anything touches the disk:
// a corrupted download or dying stick must fail while the operator's old
// system is still intact, not as a half-written one after the wipe.
// sha256sum resolves the listed name relative to the checksum file's
// directory only if we cd there.
func (e *RealExecutor) verifyPayload(ctx context.Context) error {
	if _, err := e.run(ctx, "sh", "-c",
		fmt.Sprintf("cd %q && sha256sum -c rootfs.squashfs.sha256", e.PayloadDir)); err != nil {
		return fmt.Errorf("the OS payload failed its checksum — reflash the installer stick: %w", err)
	}
	return nil
}

func (e *RealExecutor) extractRootfs(ctx context.Context, cfg InstallConfig, progress func(int)) error {
	squash := filepath.Join(e.PayloadDir, "rootfs.squashfs")
	root := filepath.Join(e.mnt, "root")
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		e.sampleProgress(root, squash, progress, done)
		close(stopped)
	}()
	_, err := e.run(ctx, "unsquashfs", "-f", "-q", "-no-progress", "-d", root, squash)
	close(done)
	// Join the sampler: an in-flight tick still owns the caller's Progress
	// struct, and the step loop writes to it the moment this returns.
	<-stopped
	return err
}

// sampleProgress reports extraction percent from disk usage vs the
// squashfs's declared uncompressed size — coarse, honest, dependency-free.
func (e *RealExecutor) sampleProgress(root, squash string, progress func(int), done chan struct{}) {
	expected := squashUncompressedBytes(squash)
	if expected <= 0 {
		return
	}
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			used := duBytes(root)
			pct := int(used * 100 / expected)
			if pct > 99 {
				pct = 99
			}
			if pct > 0 {
				progress(pct)
			}
		}
	}
}

func (e *RealExecutor) writeRecovery(ctx context.Context, cfg InstallConfig) error {
	rec := filepath.Join(e.mnt, "recovery")
	// The recovery partition is a byte-for-byte copy of the installer
	// medium's payload: live environment where live-boot finds it, OS
	// payload beside it. The machine can rebuild itself from this forever.
	for _, cp := range [][2]string{
		{filepath.Join(e.MediumDir, "live"), rec},
		{filepath.Join(e.MediumDir, "fog"), rec},
	} {
		if _, err := e.run(ctx, "cp", "-a", cp[0], cp[1]); err != nil {
			return err
		}
	}
	// Its kernel/initrd must live on the ESP — systemd-boot reads nothing
	// else. They come from the running live environment's own boot files.
	espRec := filepath.Join(e.mnt, "esp", "fog-recovery")
	if err := os.MkdirAll(espRec, 0o755); err != nil {
		return err
	}
	return e.copyLiveKernel(ctx, espRec)
}

// copyLiveKernel places the live kernel+initrd — the pair that booted this
// very installer, so they are proven on this hardware — into the target's
// ESP for the recovery entry. They live on the stick's own ESP (systemd-boot
// reads nothing else), found by its label.
func (e *RealExecutor) copyLiveKernel(ctx context.Context, espRec string) error {
	if _, err := e.run(ctx, "mkdir", "-p", "/run/fog-installer/stick-esp"); err != nil {
		return err
	}
	if _, err := e.run(ctx, "mount", "-o", "ro", "/dev/disk/by-label/FOG-BOOT", "/run/fog-installer/stick-esp"); err != nil {
		return err
	}
	defer e.run(ctx, "umount", "/run/fog-installer/stick-esp")
	if _, err := e.run(ctx, "cp", "/run/fog-installer/stick-esp/vmlinuz", filepath.Join(espRec, "vmlinuz")); err != nil {
		return err
	}
	_, err := e.run(ctx, "cp", "/run/fog-installer/stick-esp/initrd.img", filepath.Join(espRec, "initrd.img"))
	return err
}

func (e *RealExecutor) bootloader(ctx context.Context, cfg InstallConfig) error {
	root := filepath.Join(e.mnt, "root")
	esp := filepath.Join(e.mnt, "esp")
	// Stash the shipped kernel + initrd while the extracted /boot is still
	// visible — the ESP bind mount below shadows it, and Debian keeps the
	// kernel image nowhere else (there is no /usr/lib/modules/<v>/vmlinuz
	// here, unlike Fedora-style layouts).
	kver, err := installedKernelVersion(root)
	if err != nil {
		return err
	}
	if _, err := e.run(ctx, "cp", filepath.Join(root, "boot", "vmlinuz-"+kver), filepath.Join(root, "tmp", "fog-vmlinuz")); err != nil {
		return err
	}
	if _, err := e.run(ctx, "cp", filepath.Join(root, "boot", "initrd.img-"+kver), filepath.Join(root, "tmp", "fog-initrd.img")); err != nil {
		return err
	}
	// The target's /boot is the ESP; bind what a chrooted bootctl and
	// kernel hooks expect.
	if _, err := e.run(ctx, "mount", "--bind", esp, filepath.Join(root, "boot")); err != nil {
		return err
	}
	for _, m := range [][]string{
		{"-t", "proc", "proc", filepath.Join(root, "proc")},
		{"-t", "sysfs", "sys", filepath.Join(root, "sys")},
		{"--bind", "/dev", filepath.Join(root, "dev")},
	} {
		if _, err := e.run(ctx, "mount", m...); err != nil {
			return err
		}
	}
	// The installed system's kernel command line. Without this,
	// kernel-install's entry generator falls back to /proc/cmdline — the
	// LIVE environment's boot=live arguments — and the installed entry
	// would try to boot the installer forever.
	if err := writeFileAbs(filepath.Join(root, "etc", "kernel", "cmdline"), "root=LABEL=fog-root rw quiet loglevel=4"+"\n"); err != nil {
		return err
	}
	// A unique machine identity, minted at install time — kernel-install
	// files entries under it.
	if _, err := e.run(ctx, "chroot", root, "systemd-machine-id-setup"); err != nil {
		return err
	}
	if _, err := e.run(ctx, "chroot", root, "bootctl", "install", "--no-variables"); err != nil {
		return err
	}
	// Register the shipped kernel with boot counting (/etc/kernel/tries
	// baked into the rootfs) — this writes the +3 entry systemd-boot counts
	// down and systemd-bless-boot confirms. The initrd is passed
	// explicitly: the copy under the (now shadowed) extracted /boot.
	if _, err := e.run(ctx, "chroot", root, "kernel-install", "add", kver,
		"/tmp/fog-vmlinuz", "/tmp/fog-initrd.img"); err != nil {
		return err
	}
	if _, err := e.run(ctx, "rm", "-f",
		filepath.Join(root, "tmp", "fog-vmlinuz"),
		filepath.Join(root, "tmp", "fog-initrd.img")); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(esp, "loader", "entries"), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(esp, "loader", "loader.conf"), []byte(LoaderConf()), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(esp, "loader", "entries", "fog-recovery.conf"), []byte(RecoveryLoaderEntry()), 0o644); err != nil {
		return err
	}
	// Best-effort NVRAM entry; the removable-path BOOTX64.EFI fallback
	// (bootctl installed it) carries machines where efivars are fussy.
	e.run(ctx, "efibootmgr", "--create", "--disk", cfg.Disk, "--part", "1",
		"--label", "Fog", "--loader", `\EFI\systemd\systemd-bootx64.efi`)
	return nil
}

func (e *RealExecutor) configure(ctx context.Context, cfg InstallConfig) (Result, error) {
	root := filepath.Join(e.mnt, "root")
	data := filepath.Join(e.mnt, "data")

	if err := os.WriteFile(filepath.Join(root, "etc", "fstab"), []byte(Fstab()), 0o644); err != nil {
		return Result{}, err
	}
	if err := os.WriteFile(filepath.Join(root, "etc", "hostname"), []byte(cfg.ServerName+"\n"), 0o644); err != nil {
		return Result{}, err
	}
	hosts := fmt.Sprintf("127.0.0.1\tlocalhost\n127.0.1.1\t%s\n", cfg.ServerName)
	if err := os.WriteFile(filepath.Join(root, "etc", "hosts"), []byte(hosts), 0o644); err != nil {
		return Result{}, err
	}
	if cfg.Timezone != "" {
		if _, err := os.Stat(filepath.Join(root, "usr/share/zoneinfo", cfg.Timezone)); err == nil {
			os.Remove(filepath.Join(root, "etc", "localtime"))
			if err := os.Symlink("/usr/share/zoneinfo/"+cfg.Timezone, filepath.Join(root, "etc", "localtime")); err != nil {
				return Result{}, err
			}
		}
	}
	// Docker's writable world belongs on the data partition with everything
	// else precious-or-bulky; merge data-root into the baked daemon.json.
	if err := patchDockerDataRoot(filepath.Join(root, "etc", "docker", "daemon.json"), "/var/lib/fog/docker-root"); err != nil {
		return Result{}, err
	}
	if err := patchContainerdRoot(filepath.Join(root, "etc", "containerd", "config.toml"), "/var/lib/fog/containerd"); err != nil {
		return Result{}, err
	}

	urls := []string{fmt.Sprintf("http://%s.local:8080", cfg.ServerName)}
	if ip := currentIPv4(); ip != "" {
		urls = append(urls, fmt.Sprintf("http://%s:8080", ip))
	}

	if cfg.Mode == "restore" {
		// Bring back the old server's identity: stage the kit exactly where
		// fogd's boot looks for it (cmd/fogd/main.go, restore-pending). No
		// account preseed, no claim token — the operator signs in with the
		// credentials the restored server always had.
		pending := filepath.Join(data, "restore-pending")
		if err := os.MkdirAll(pending, 0o700); err != nil {
			return Result{}, err
		}
		if err := os.WriteFile(filepath.Join(pending, "fog.db"), cfg.kit.DB, 0o600); err != nil {
			return Result{}, err
		}
		if err := os.WriteFile(filepath.Join(pending, "master.key"), cfg.kit.MasterKey, 0o600); err != nil {
			return Result{}, err
		}
		// Console/ssh recourse for restores too, when the operator supplied
		// the admin password (verified against the kit's own hash in Start).
		if cfg.Username != "" && cfg.Password != "" {
			if err := e.createUnixAccount(ctx, root, cfg); err != nil {
				return Result{}, err
			}
		}
		return Result{URLs: urls}, nil
	}

	// A unix account mirroring the Fog admin: console/ssh recourse with the
	// same credentials the operator already knows.
	if err := e.createUnixAccount(ctx, root, cfg); err != nil {
		return Result{}, err
	}

	// The handoff: pre-hashed account + one-shot claim token, staged where
	// fogd's data dir will mount.
	hash, err := auth.HashPassword(cfg.Password)
	if err != nil {
		return Result{}, err
	}
	token, digest := auth.NewToken()
	fb := firstboot.Config{
		Username:         cfg.Username,
		PasswordHash:     hash,
		ServerName:       cfg.ServerName,
		ClaimTokenDigest: digest,
		// A duration, not an absolute deadline: the live installer's clock may
		// never have seen NTP, and a wrong stamp here would shrink or void the
		// claim window as judged by the installed system's chrony-synced
		// clock. firstboot stamps the deadline from its own clock at consume
		// time instead.
		ClaimTokenTTL: firstboot.DefaultClaimTTL,
	}
	raw, err := json.MarshalIndent(fb, "", "  ")
	if err != nil {
		return Result{}, err
	}
	if err := os.WriteFile(filepath.Join(data, firstboot.FileName), raw, 0o600); err != nil {
		return Result{}, err
	}
	return Result{ClaimToken: token, URLs: urls}, nil
}

func (e *RealExecutor) createUnixAccount(ctx context.Context, root string, cfg InstallConfig) error {
	if _, err := e.run(ctx, "chroot", root, "useradd", "-m", "-s", "/bin/bash", "-G", "sudo,docker", cfg.Username); err != nil {
		return err
	}
	return e.runWithSecretStdin(ctx, cfg.Username+":"+cfg.Password+"\n", "chroot", root, "chpasswd")
}

func (e *RealExecutor) verify(ctx context.Context, cfg InstallConfig) error {
	root := filepath.Join(e.mnt, "root")
	esp := filepath.Join(e.mnt, "esp")
	checks := []string{
		filepath.Join(esp, "EFI/BOOT/BOOTX64.EFI"),
		filepath.Join(esp, "loader/entries/fog-recovery.conf"),
		filepath.Join(root, "usr/local/bin/fogd"),
		filepath.Join(root, "etc/fstab"),
		filepath.Join(e.mnt, "recovery", "live/filesystem.squashfs"),
	}
	if cfg.Mode == "restore" {
		// A restore's handoff is the staged kit, not an account preseed.
		checks = append(checks,
			filepath.Join(e.mnt, "data", "restore-pending", "fog.db"),
			filepath.Join(e.mnt, "data", "restore-pending", "master.key"))
	} else {
		checks = append(checks, filepath.Join(e.mnt, "data", firstboot.FileName))
	}
	for _, c := range checks {
		if _, err := os.Stat(c); err != nil {
			return fmt.Errorf("verification failed — %s is missing", c)
		}
	}
	// One real boot entry must exist beside the recovery one.
	entries, err := filepath.Glob(filepath.Join(esp, "loader/entries", "*.conf"))
	if err != nil || len(entries) < 2 {
		return fmt.Errorf("verification failed — no OS boot entry was written (%d entries)", len(entries))
	}
	// The completed-install marker, written only once everything above has
	// passed. From here on, an installer booted on this machine again — the
	// USB stick left plugged in — reads it and says "this is already your
	// server" instead of opening with an offer to erase it.
	if err := writeInstallMarker(filepath.Join(esp, InstallMarkerName), InstallMarker{
		ServerName: cfg.ServerName,
		Mode:       cfg.Mode,
		Version:    e.Version,
	}); err != nil {
		return fmt.Errorf("writing the install marker: %w", err)
	}
	if _, err := e.run(ctx, "sync"); err != nil {
		return err
	}
	// Unmount everything so the reboot is clean.
	e.unmountAll(ctx, installMounts)
	return nil
}

// saveDiagnostics copies the transcript to the stick's FOG-DATA partition —
// pluggable into any computer, no tools needed to read a plain .log.
func (e *RealExecutor) saveDiagnostics(ctx context.Context) {
	dir := "/run/fog-installer/data"
	if _, err := e.run(ctx, "mkdir", "-p", dir); err != nil {
		return
	}
	if _, err := e.run(ctx, "mount", "/dev/disk/by-label/FOG-DATA", dir); err != nil {
		return
	}
	defer e.run(ctx, "umount", dir)
	name := filepath.Join(dir, fmt.Sprintf("fog-install-%s.log", time.Now().UTC().Format("20060102-150405")))
	if err := os.WriteFile(name, []byte(e.Transcript.String()), 0o644); err != nil {
		// The stick copy is best-effort; the downloadable transcript at
		// least records that it didn't happen.
		e.log("! saving diagnostics to FOG-DATA: %v", err)
		return
	}
	e.run(ctx, "sync")
}

func patchDockerDataRoot(path, dataRoot string) error {
	conf := map[string]any{}
	if raw, err := os.ReadFile(path); err == nil {
		json.Unmarshal(raw, &conf)
	}
	conf["data-root"] = dataRoot
	raw, err := json.MarshalIndent(conf, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o644)
}

// patchContainerdRoot points containerd's root at the data partition.
// Docker's data-root doesn't reach it: with the containerd image store —
// Docker 29's default — image layers and build cache live under
// containerd's own root, which otherwise stays on the OS partition
// and fills it while the data partition sits empty.
//
// Only a top-level key is touched (before the first [table]); an existing
// root line is replaced, else the stock commented one, else one is added.
func patchContainerdRoot(path, dir string) error {
	line := fmt.Sprintf("root = %q", dir)
	var lines []string
	if raw, err := os.ReadFile(path); err == nil && len(raw) > 0 {
		lines = strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	}
	isRoot := func(s string) bool {
		key, _, ok := strings.Cut(s, "=")
		return ok && strings.TrimSpace(key) == "root"
	}
	at, commented := len(lines), -1
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "[") {
			at = i
			break
		}
		if isRoot(t) {
			commented = -2
			lines[i] = line
			break
		}
		if c := strings.TrimSpace(strings.TrimPrefix(t, "#")); commented == -1 && strings.HasPrefix(t, "#") && isRoot(c) {
			commented = i
		}
	}
	switch {
	case commented >= 0:
		lines[commented] = line
	case commented == -1:
		lines = append(lines[:at], append([]string{line}, lines[at:]...)...)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}
