package setup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// RepairAction is one entry of the repair menu; the wizard renders these
// verbatim (served in AgentState), so what the engine offers and what the
// screen promises can never drift apart.
type RepairAction struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Keeps string `json:"keeps"`
}

// The repair menu, safest first. Booted from the on-disk recovery partition
// (fog.repair=1), the same agent that installed the machine becomes its
// repair tool — on the attached display and at http://fog.local alike.
// Every action states what it preserves; only what it names is touched.
var RepairActions = []RepairAction{
	{"fsck", "Check & repair filesystems",
		"Everything — this only fixes filesystem bookkeeping after power loss."},
	{"rebuild-docker", "Rebuild the app runtime",
		"All app data and configuration. Container images are re-downloaded when apps start."},
	{"reinstall-os", "Reinstall Fog's operating system",
		"All app data, backups, and settings — only the OS partition is rewritten from the recovery copy."},
}

// Repairer performs repair actions. RealExecutor implements it against the
// machine; MockExecutor simulates for wizard development.
type Repairer interface {
	Repair(ctx context.Context, action string, report func(Progress)) error
}

// StartRepair launches one repair action. Repair mode only — the fresh
// installer has nothing to repair, and repair boots must not run the fresh
// installer (the recovery partition it runs from lives on the target disk).
func (a *Agent) StartRepair(action string) error {
	if a.modeLocked() != "repair" {
		return errors.New("repair actions are only available when booted into Fog Recovery")
	}
	rep, ok := a.Exec.(Repairer)
	if !ok {
		return errors.New("this build has no repair engine")
	}
	valid := false
	for _, ra := range RepairActions {
		if ra.ID == action {
			valid = true
		}
	}
	if !valid {
		return fmt.Errorf("unknown repair action %q", action)
	}
	return a.launch(&InstallConfig{Mode: "repair:" + action}, func(phase string) error {
		if phase == "installing" {
			return errors.New("a repair is already running")
		}
		return nil
	}, func() (*Result, error) {
		// A repair has no result to hand over: the machine reboots into
		// the system it fixed.
		return nil, rep.Repair(context.Background(), action, a.publish)
	}, "repair failed")
}

// Retry after a failed repair reopens the menu (unlike installs, repairs
// are naturally re-runnable). Repair mode only: in install mode a reset
// from "done" would break the one-install-per-boot invariant and let a
// second install re-erase the just-installed disk.
func (a *Agent) resetAfterRepair() error {
	if a.modeLocked() != "repair" {
		return errors.New("reset is only available when booted into Fog Recovery")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.phase == "done" || a.phase == "failed" {
		a.phase = "idle"
		a.events = nil
		a.errMsg = ""
		a.cfg = nil
	}
	return nil
}

// ---- real implementations ----

func (e *RealExecutor) Repair(ctx context.Context, action string, report func(Progress)) error {
	e.prepare()
	// Every repair resolves the machine's partitions by label; with a second
	// Fog disk attached (an old disk brought over for its data, a clone),
	// /dev/disk/by-label points at an arbitrary one of the two and a repair
	// could rewrite the wrong disk. Refuse until the ambiguity is physical
	// history.
	owners, err := e.fogLabelOwners(ctx)
	if err == nil {
		for _, label := range fogLabels {
			if len(owners[label]) > 1 {
				return fmt.Errorf("two disks carry the %q label — unplug the one that isn't this machine's, then try again", label)
			}
		}
	}
	switch action {
	case "fsck":
		return e.repairFsck(ctx, report)
	case "rebuild-docker":
		return e.repairRebuildDocker(ctx, report)
	case "reinstall-os":
		return e.repairReinstallOS(ctx, report)
	}
	return fmt.Errorf("unknown repair action %q", action)
}

func byLabel(label string) string { return "/dev/disk/by-label/" + label }

// The labels an installed Fog disk carries. (The USB stick's own labels —
// FOG-BOOT, uppercase FOG-DATA — are deliberately different.)
var fogLabels = []string{"FOG-ESP", "fog-recovery", "fog-root", "fog-data"}

var lsblkPairRe = regexp.MustCompile(`PKNAME="([^"]*)" LABEL="([^"]*)"`)

// fogLabelOwners maps each filesystem label to the parent disks carrying
// it, so callers can spot a label that exists twice.
func (e *RealExecutor) fogLabelOwners(ctx context.Context) (map[string][]string, error) {
	out, err := e.Exec(ctx, "lsblk", "-P", "-o", "PKNAME,LABEL")
	if err != nil {
		return nil, err
	}
	owners := map[string][]string{}
	for _, m := range lsblkPairRe.FindAllStringSubmatch(out, -1) {
		if m[2] != "" {
			owners[m[2]] = append(owners[m[2]], m[1])
		}
	}
	return owners, nil
}

func (e *RealExecutor) repairFsck(ctx context.Context, report func(Progress)) error {
	do := map[string]stepFunc{}
	for _, label := range fsckLabels {
		do["fsck-"+label] = func(ctx context.Context, s *stepRun) error { return e.fsckOne(ctx, label, s) }
	}
	return runSteps(ctx, RepairSteps("fsck"), do, report, nil)
}

func (e *RealExecutor) fsckOne(ctx context.Context, label string, s *stepRun) error {
	// e2fsck -y against a mounted ext4 can corrupt it. Nothing should be
	// mounted in repair mode, but a previous action's teardown can leave
	// a busy partition behind — verify before touching it. findmnt exits
	// non-zero when the device is mounted nowhere; that (or a missing
	// findmnt) means it is safe to proceed.
	if out, err := e.Exec(ctx, "findmnt", "-rn", "-S", byLabel(label)); err == nil && strings.TrimSpace(out) != "" {
		s.Detail("the partition is still mounted")
		return fmt.Errorf("%s is still mounted (%s) — reboot into recovery and try again", label, strings.Fields(out)[0])
	}
	// e2fsck exits 1/2 for "errors corrected" — that's success here;
	// the whole point is correcting them.
	out, err := e.Exec(ctx, "e2fsck", "-f", "-y", byLabel(label))
	e.log("$ e2fsck -f -y %s\n%s", byLabel(label), out)
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() > 2 {
			s.Detail(tail(out, 8))
			return fmt.Errorf("checking %s: %w", label, err)
		}
		s.Detail("errors were found and repaired")
	}
	return nil
}

func (e *RealExecutor) repairRebuildDocker(ctx context.Context, report func(Progress)) error {
	return runSteps(ctx, RepairSteps("rebuild-docker"), map[string]stepFunc{
		"rebuild-docker": func(ctx context.Context, _ *stepRun) error { return e.rebuildDocker(ctx) },
	}, report, nil)
}

func (e *RealExecutor) rebuildDocker(ctx context.Context) error {
	data := filepath.Join(e.mnt, "repair-data")
	if _, err := e.run(ctx, "mkdir", "-p", data); err != nil {
		return err
	}
	if _, err := e.run(ctx, "mount", byLabel("fog-data"), data); err != nil {
		return err
	}
	// Same busy fallback as unmountAll: a plain umount that fails must not
	// leave fog-data mounted behind us — repairFsck refuses to touch a
	// mounted partition, so a stuck mount would also block the next repair.
	defer func() {
		if _, err := e.run(ctx, "umount", data); err != nil {
			e.run(ctx, "umount", "-l", data)
		}
	}()
	// containerd's store (images and build cache under the containerd
	// image store) holds nothing an app can't pull or build again.
	if _, err := e.run(ctx, "rm", "-rf", "--one-file-system", filepath.Join(data, "containerd")); err != nil {
		return err
	}
	// Everything under docker-root is rebuildable from the materialized
	// compose files — except volumes, which are the apps' actual data.
	// The partition is mounted right here, so enumerate with os.ReadDir
	// rather than parsing `ls` output: a filename with whitespace would
	// split into several rm targets and delete the wrong things.
	dockerRoot := filepath.Join(data, "docker-root")
	entries, err := os.ReadDir(dockerRoot)
	if errors.Is(err, os.ErrNotExist) {
		// No docker-root at all is a fine state to "rebuild" to.
		return nil
	}
	if err != nil {
		return err
	}
	for _, ent := range entries {
		if ent.Name() == "volumes" {
			continue
		}
		// rm --one-file-system, not os.RemoveAll: removal must stop at
		// filesystem boundaries, so a bind mount lingering under docker-root
		// (a container's leftover) can never pull outside data into the rm.
		if _, err := e.run(ctx, "rm", "-rf", "--one-file-system", filepath.Join(dockerRoot, ent.Name())); err != nil {
			return err
		}
	}
	_, err = e.run(ctx, "sync")
	return err
}

// repairReinstallOS rewrites only the root partition from the recovery
// copy: fresh OS, untouched data. The recovery partition this very boot is
// running from is read, never written.
func (e *RealExecutor) repairReinstallOS(ctx context.Context, report func(Progress)) error {
	// The machine's own disk, derived from where its root partition lives.
	pk, err := e.run(ctx, "lsblk", "-no", "pkname", byLabel("fog-root"))
	if err != nil {
		return fmt.Errorf("finding the installed disk: %w", err)
	}
	disk := "/dev/" + strings.TrimSpace(strings.Split(pk, "\n")[0])
	cfg := InstallConfig{Mode: "repair", Disk: disk, ServerName: "fog"}

	do := map[string]stepFunc{
		// The same gate as a fresh install: a corrupt recovery copy must
		// fail before the root partition is reset, while the machine can
		// still boot back into this very recovery environment.
		"payload":     func(ctx context.Context, _ *stepRun) error { return e.verifyPayload(ctx) },
		"format-root": func(ctx context.Context, _ *stepRun) error { return e.reformatRoot(ctx, cfg) },
		"rootfs": func(ctx context.Context, s *stepRun) error {
			return e.extractRootfs(ctx, cfg, s.Percent)
		},
		"bootloader": func(ctx context.Context, _ *stepRun) error {
			// The ESP is not reformatted, and bootloader() mints a fresh
			// machine-id — prune the previous OS entries first or every
			// repair leaves another orphaned kernel set behind.
			if err := e.pruneBootEntries(); err != nil {
				return err
			}
			return e.bootloader(ctx, cfg)
		},
		"configure": func(ctx context.Context, _ *stepRun) error { return e.reconfigureAfterReinstall(ctx, &cfg) },
		"verify":    func(ctx context.Context, _ *stepRun) error { return e.verifyReinstall(ctx) },
	}
	return runSteps(ctx, RepairSteps("reinstall-os"), do, report, func() {
		// Same contract as the install path: a re-run must find clean
		// mountpoints.
		e.unmountAll(ctx, repairMounts)
		e.saveDiagnostics(ctx)
	})
}

func (e *RealExecutor) reformatRoot(ctx context.Context, cfg InstallConfig) error {
	if _, err := e.run(ctx, "mkfs.ext4", "-q", "-F", "-L", "fog-root", byLabel("fog-root")); err != nil {
		return err
	}
	for _, m := range []struct{ label, dir string }{
		{"FOG-ESP", "esp"}, {"fog-root", "root"}, {"fog-data", "data"},
	} {
		dir := filepath.Join(e.mnt, m.dir)
		if _, err := e.run(ctx, "mkdir", "-p", dir); err != nil {
			return err
		}
		if _, err := e.run(ctx, "mount", byLabel(m.label), dir); err != nil {
			return err
		}
	}
	return nil
}

// reconfigureAfterReinstall rebuilds the config the OS partition carried,
// recovering identity from the data partition (the restored fog.db knows
// the server's name and admin).
func (e *RealExecutor) reconfigureAfterReinstall(ctx context.Context, cfg *InstallConfig) error {
	root := filepath.Join(e.mnt, "root")
	if name, _ := peekDataIdentity(filepath.Join(e.mnt, "data", "fog.db")); name != "" {
		cfg.ServerName = name
	}
	if err := writeFileAbs(filepath.Join(root, "etc", "fstab"), Fstab()); err != nil {
		return err
	}
	if err := writeFileAbs(filepath.Join(root, "etc", "hostname"), cfg.ServerName+"\n"); err != nil {
		return err
	}
	hosts := fmt.Sprintf("127.0.0.1\tlocalhost\n127.0.1.1\t%s\n", cfg.ServerName)
	if err := writeFileAbs(filepath.Join(root, "etc", "hosts"), hosts); err != nil {
		return err
	}
	if err := patchDockerDataRoot(filepath.Join(root, "etc", "docker", "daemon.json"), "/var/lib/fog/docker-root"); err != nil {
		return err
	}
	return patchContainerdRoot(filepath.Join(root, "etc", "containerd", "config.toml"), "/var/lib/fog/containerd")
}

var machineIDRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

// pruneBootEntries removes previous OS loader entries and kernel-install's
// per-machine-id kernel trees from the ESP. Without this, repeated
// reinstalls accrete orphaned kernels until the 1 GB ESP fills — and
// systemd-boot's entry sort may prefer a stale entry whose modules no
// longer exist in the freshly extracted root. The recovery entry and its
// kernel are kept: they belong to the partition, not to any OS install.
func (e *RealExecutor) pruneBootEntries() error {
	esp := filepath.Join(e.mnt, "esp")
	entries, err := filepath.Glob(filepath.Join(esp, "loader/entries/*.conf"))
	if err != nil {
		return err
	}
	for _, ent := range entries {
		if filepath.Base(ent) == "fog-recovery.conf" {
			continue
		}
		if err := os.Remove(ent); err != nil {
			return err
		}
	}
	top, err := os.ReadDir(esp)
	if err != nil {
		return err
	}
	for _, d := range top {
		if d.IsDir() && machineIDRe.MatchString(d.Name()) {
			if err := os.RemoveAll(filepath.Join(esp, d.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func (e *RealExecutor) verifyReinstall(ctx context.Context) error {
	root := filepath.Join(e.mnt, "root")
	for _, c := range []string{
		filepath.Join(root, "usr/local/bin/fogd"),
		filepath.Join(root, "etc/fstab"),
	} {
		if _, err := os.Stat(c); err != nil {
			return fmt.Errorf("verification failed — %s is missing", c)
		}
	}
	// Same bar as a fresh install: one real OS entry beside the recovery
	// one, or this "successful" repair wouldn't boot.
	entries, err := filepath.Glob(filepath.Join(e.mnt, "esp", "loader/entries", "*.conf"))
	if err != nil || len(entries) < 2 {
		return fmt.Errorf("verification failed — no OS boot entry was written (%d entries)", len(entries))
	}
	if _, err := e.run(ctx, "sync"); err != nil {
		return err
	}
	e.unmountAll(ctx, repairMounts)
	return nil
}

// peekDataIdentity reads the server name from the data partition's live
// database, best-effort.
func peekDataIdentity(dbPath string) (serverName, adminUser string) {
	raw, err := os.ReadFile(dbPath)
	if err != nil {
		return "", ""
	}
	serverName, adminUser, _ = peekKitDB(raw)
	return serverName, adminUser
}
