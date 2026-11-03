package setup

import (
	"context"
	"fmt"
	"os"
	"time"

	"gitea.lan/matthew/fog/internal/auth"
)

// MockExecutor plays the install script against nothing: real steps, real
// pacing, real progress events, no hardware. It is how the wizard is
// developed and demoed before (and beside) the real engine.
type MockExecutor struct {
	StepDelay time.Duration // per-step; rootfs/recovery take 5 ticks
}

func (m MockExecutor) Run(ctx context.Context, cfg InstallConfig, report func(Progress)) (Result, error) {
	if err := runSteps(ctx, InstallSteps, m.play(InstallSteps), report, nil); err != nil {
		return Result{}, err
	}
	token, _ := auth.NewToken()
	return Result{
		ClaimToken: token,
		URLs: []string{
			fmt.Sprintf("http://%s.local:8080", cfg.ServerName),
			"http://192.168.1.42:8080", // the mock machine's pretend address
		},
	}, nil
}

// Repair simulates each repair action so the repair UI is drivable in dev
// (FOG_SETUP_MOCK=1 FOG_SETUP_REPAIR=1).
func (m MockExecutor) Repair(ctx context.Context, action string, report func(Progress)) error {
	steps := RepairSteps(action)
	if steps == nil {
		return fmt.Errorf("unknown repair action %q", action)
	}
	return runSteps(ctx, steps, m.play(steps), report, nil)
}

// play stands in for every step's work: a pause, with the long copies
// streaming five progress ticks the way the real ones do.
func (m MockExecutor) play(steps []Step) map[string]stepFunc {
	delay := m.StepDelay
	if delay == 0 {
		delay = 700 * time.Millisecond
	}
	do := make(map[string]stepFunc, len(steps))
	for _, st := range steps {
		ticks := 1
		if st.ID == "rootfs" || st.ID == "recovery" {
			ticks = 5
		}
		do[st.ID] = func(ctx context.Context, s *stepRun) error {
			for t := 1; t <= ticks; t++ {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(delay / time.Duration(ticks)):
				}
				if ticks > 1 {
					s.Percent(t * 100 / ticks)
				}
			}
			return nil
		}
	}
	return do
}

// MockInventory is the dev-machine stand-in: one internal NVMe carrying an
// old Linux install, the USB stick we "booted" from, and a healthy wired
// network.
func MockInventory() (Inventory, error) {
	disks := []Disk{
		{
			Path: "/dev/nvme0n1", Model: "Samsung SSD 970 EVO 500GB",
			SizeBytes: 500_107_862_016, Bus: "nvme",
			Contents: "an existing Linux system (ext4)",
		},
		{
			Path: "/dev/sda", Model: "SanDisk Ultra USB 3.0",
			SizeBytes: 30_752_636_928, Bus: "usb", Removable: true, BootMedium: true,
			Contents: "the Fog installer",
		},
	}
	inv := Inventory{
		Disks:     disks,
		Candidate: pickCandidate(disks),
		Network:   Network{Online: true, Interface: "enp1s0", IP: "192.168.1.42"},
		Machine:   "Intel NUC11ATKC4",
	}
	// FOG_SETUP_MOCK_INSTALLED=1 pretends this machine's disk already holds
	// a finished install: the state the installer boots into when the stick
	// was left plugged in. It is how that screen is developed.
	if os.Getenv("FOG_SETUP_MOCK_INSTALLED") == "1" {
		inv.Disks[0].FogDisk = true
		inv.Disks[0].Contents = "an existing Fog server"
		inv.Existing = &ExistingInstall{
			Disk: inv.Disks[0].Path, ServerName: "fog", Mode: "fresh",
			Version: "0.6.15", InstalledAt: "2026-08-22T18:04:00Z",
		}
	}
	return inv, nil
}
