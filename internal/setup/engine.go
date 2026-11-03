package setup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sync"

	"gitea.lan/matthew/fog/internal/auth"
)

// InstallConfig is everything the wizard collects. It is validated against
// the live inventory before a single block is touched.
type InstallConfig struct {
	Mode string `json:"mode"` // fresh | restore
	Disk string `json:"disk"` // /dev/… target, will be erased
	// ConfirmDisk must repeat Disk exactly: the API-level belt matching the
	// wizard's explicit erase confirmation. No accidental one-field install.
	ConfirmDisk string `json:"confirmDisk"`
	// ConfirmModel/ConfirmSizeBytes pin the identity of the disk the
	// operator actually saw and confirmed: device names can re-enumerate
	// between the picker and Start, and a name alone could then point at a
	// different disk. Optional — when set, the disk at that name must match.
	ConfirmModel     string `json:"confirmModel"`
	ConfirmSizeBytes int64  `json:"confirmSizeBytes"`
	ServerName       string `json:"serverName"`
	Username         string `json:"username"`
	Password         string `json:"password"`
	Timezone         string `json:"timezone"`

	// kit carries the verified recovery kit on restore installs. Loaded via
	// the agent (POST /api/setup/kit), never from the request body.
	kit *KitPayload
}

var serverNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

func (c *InstallConfig) Validate(inv Inventory) error {
	if c.Mode != "fresh" && c.Mode != "restore" {
		return errors.New("mode must be fresh or restore")
	}
	var target *Disk
	for i := range inv.Disks {
		if inv.Disks[i].Path == c.Disk {
			target = &inv.Disks[i]
		}
	}
	if target == nil {
		return fmt.Errorf("disk %q is not in the inventory", c.Disk)
	}
	if target.BootMedium {
		return errors.New("refusing to install onto the installer's own boot medium")
	}
	if target.Removable && !inv.bootMediumKnown() {
		// The one state where a removable target is never safe: if the
		// inventory couldn't identify which disk the installer booted from,
		// any removable disk could be the stick itself.
		return errors.New("refusing to erase removable media — the installer could not identify its own boot stick, and this could be it")
	}
	if target.SizeBytes < MinDiskBytes {
		return fmt.Errorf("%s is too small — Fog needs at least %d GB", c.Disk, MinDiskBytes>>30)
	}
	if c.ConfirmDisk != c.Disk {
		return errors.New("the erase confirmation must name the target disk")
	}
	if (c.ConfirmSizeBytes != 0 && c.ConfirmSizeBytes != target.SizeBytes) ||
		(c.ConfirmModel != "" && c.ConfirmModel != target.Model) {
		return fmt.Errorf("the disk now at %s (%s, %d GB) is not the one that was confirmed — device names changed; go back and pick the disk again", c.Disk, target.Model, target.SizeBytes>>30)
	}
	if c.ServerName == "" {
		c.ServerName = "fog"
	}
	if !serverNameRe.MatchString(c.ServerName) {
		return errors.New("server name must be lowercase letters, digits, and hyphens (max 32)")
	}
	if c.Mode == "fresh" {
		if c.Username == "" {
			return errors.New("username is required")
		}
		if err := auth.ValidatePassword(c.Password); err != nil {
			return err
		}
	}
	return nil
}

// Progress is one line of the install's story, streamed to every connected
// wizard (screen and phones alike).
type Progress struct {
	Step    int    `json:"step"`  // 1-based index into Steps
	Steps   int    `json:"steps"` // total
	ID      string `json:"id"`
	Title   string `json:"title"`
	Status  string `json:"status" ts:"\"running\" | \"done\" | \"failed\""`
	Detail  string `json:"detail,omitempty"`
	Percent int    `json:"percent"` // within the step
}

// Result is what a finished install hands the wizard: where the installed
// system will answer, and the one-shot token that signs the operator in
// there without retyping anything.
type Result struct {
	ClaimToken string   `json:"claimToken"`
	URLs       []string `json:"urls"`
}

// Executor performs the install. The mock executor stands in until the real
// engine lands, keeping the whole wizard drivable on a dev machine.
type Executor interface {
	Run(ctx context.Context, cfg InstallConfig, report func(Progress)) (Result, error)
}

// Agent owns the installer's state machine: one install per boot, its event
// log replayable so a wizard that connects late (or reconnects) still sees
// the whole story.
type Agent struct {
	Inventory func() (Inventory, error)
	Exec      Executor
	Reboot    func() error
	// Display is the kiosk on the machine's own screen; nil where there is
	// none to size.
	Display KioskDisplay
	Log     *slog.Logger
	Version string
	// Mode is "install" (from the USB stick) or "repair" (booted from the
	// on-disk recovery partition, fog.repair=1 on the kernel cmdline).
	Mode string

	mu     sync.Mutex
	kit    *KitPayload
	phase  string // idle | installing | done | failed
	cfg    *InstallConfig
	events []Progress
	result *Result
	errMsg string
	subs   map[chan Progress]struct{}
}

type AgentState struct {
	Phase       string     `json:"phase" ts:"\"idle\" | \"installing\" | \"done\" | \"failed\""`
	Mode        string     `json:"agentMode" ts:"\"install\" | \"repair\""`
	Kit         *KitInfo   `json:"kit,omitempty"`
	ServerName  string     `json:"serverName,omitempty"`
	InstallMode string     `json:"mode,omitempty"`
	Events      []Progress `json:"events"`
	Result      *Result    `json:"result,omitempty"`
	Error       string     `json:"error,omitempty"`
	Version     string     `json:"version"`
	// The repair menu, served so the wizard renders exactly what the
	// engine offers (repair mode only).
	RepairActions []RepairAction `json:"repairActions,omitempty"`
}

func (a *Agent) State() AgentState {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := AgentState{Phase: a.phaseLocked(), Mode: a.modeLocked(), Events: append([]Progress(nil), a.events...), Version: a.Version}
	if st.Mode == "repair" {
		st.RepairActions = RepairActions
	}
	if a.kit != nil {
		st.Kit = &KitInfo{ServerName: a.kit.ServerName, AdminUser: a.kit.AdminUser}
	}
	if a.cfg != nil {
		st.ServerName = a.cfg.ServerName
		st.InstallMode = a.cfg.Mode
	}
	// The claim token never rides along: /api/setup/state is readable by
	// anyone on the LAN, and the token is a ready-made admin session on the
	// installed server. It is handed out exactly once, via TakeClaimToken.
	if a.result != nil {
		r := *a.result
		r.ClaimToken = ""
		st.Result = &r
	}
	st.Error = a.errMsg
	return st
}

// TakeClaimToken hands out the one-shot claim token exactly once — to the
// wizard surface actually driving the restart — then forgets it. A second
// caller (or anyone scraping the API) gets nothing.
func (a *Agent) TakeClaimToken() (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.result == nil || a.result.ClaimToken == "" {
		return "", false
	}
	t := a.result.ClaimToken
	a.result.ClaimToken = ""
	return t, true
}

func (a *Agent) phaseLocked() string {
	if a.phase == "" {
		return "idle"
	}
	return a.phase
}

func (a *Agent) modeLocked() string {
	if a.Mode == "" {
		return "install"
	}
	return a.Mode
}

// KitInfo is what the wizard may know about a loaded kit.
type KitInfo struct {
	ServerName string `json:"serverName"`
	AdminUser  string `json:"adminUser"`
}

// LoadKit decrypts and verifies a recovery kit, holding it (in memory only)
// for a subsequent restore install. Verification happens here — before the
// wizard lets anyone near the erase step.
func (a *Agent) LoadKit(sealed []byte, passphrase string) (KitInfo, error) {
	kit, err := ParseKit(sealed, passphrase)
	if err != nil {
		return KitInfo{}, err
	}
	a.mu.Lock()
	a.kit = kit
	a.mu.Unlock()
	return KitInfo{ServerName: kit.ServerName, AdminUser: kit.AdminUser}, nil
}

// Start validates and launches the install. Exactly one install may ever
// start per boot of the installer — a failed one holds its error until the
// operator retries from a rebooted (pristine) environment or the engine is
// reset explicitly.
func (a *Agent) Start(cfg InstallConfig) error {
	// Repair boots must never run the fresh installer: the recovery
	// partition they run from lives on the very disk an install would erase
	// (the mirror of StartRepair's gate).
	if a.modeLocked() == "repair" {
		return errors.New("installing is not available when booted into Fog Recovery — use a repair action, or boot from the installer stick")
	}
	inv, err := a.Inventory()
	if err != nil {
		return fmt.Errorf("inventory: %w", err)
	}
	if cfg.Mode == "restore" {
		a.mu.Lock()
		cfg.kit = a.kit
		a.mu.Unlock()
		if cfg.kit == nil {
			return errors.New("no recovery kit loaded — upload and verify one first")
		}
		// The kit already knows the server's name; only override when the
		// operator typed one.
		if cfg.ServerName == "" {
			cfg.ServerName = cfg.kit.ServerName
		}
		// A restore may carry the admin's password so the installed system
		// gets a matching unix account (console/ssh recourse). It must be
		// the password of the account being restored — the kit's own hash
		// is the referee.
		cfg.Username = cfg.kit.AdminUser
		if cfg.Password != "" && (cfg.kit.AdminHash == "" || !auth.VerifyPassword(cfg.Password, cfg.kit.AdminHash)) {
			return errors.New("that isn't the restored server's admin password — leave it blank to skip console access")
		}
	}
	if err := cfg.Validate(inv); err != nil {
		return err
	}
	// One install per boot: "done" refuses too, or a second install could
	// re-erase the disk that was just installed.
	return a.launch(&cfg, func(phase string) error {
		if phase == "installing" || phase == "done" {
			return errors.New("an install is already " + phase)
		}
		return nil
	}, func() (*Result, error) {
		res, err := a.Exec.Run(context.Background(), cfg, a.publish)
		return &res, err
	}, "install failed")
}

// launch claims the agent for one operation (an install or a repair):
// refused if busy says the current phase forbids it, otherwise the story so
// far is cleared and op runs in the background. failMsg is the log line for
// a failure.
func (a *Agent) launch(cfg *InstallConfig, busy func(phase string) error, op func() (*Result, error), failMsg string) error {
	a.mu.Lock()
	if err := busy(a.phase); err != nil {
		a.mu.Unlock()
		return err
	}
	a.phase = "installing"
	a.cfg = cfg
	a.events = nil
	a.errMsg = ""
	a.result = nil
	a.mu.Unlock()

	go func() {
		res, err := op()
		a.mu.Lock()
		defer a.mu.Unlock()
		if err != nil {
			a.phase = "failed"
			a.errMsg = err.Error()
			if a.Log != nil {
				a.Log.Error(failMsg, "mode", cfg.Mode, "err", err)
			}
			return
		}
		a.phase = "done"
		a.result = res
	}()
	return nil
}

// MarkInterrupted puts a freshly started agent straight into the failed
// phase. It is called once at startup when the previous agent died with an
// install in flight (Restart=always brings the process back blank): opening
// on the welcome screen as if nothing happened would be a lie, and the
// failed phase is the one the wizard already knows how to render — message
// plus a working "try again" (Retry clears it).
func (a *Agent) MarkInterrupted(msg string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.phase == "" || a.phase == "idle" {
		a.phase = "failed"
		a.errMsg = msg
	}
}

// Retry clears a failed install so the wizard's "try again" works without a
// reboot. Only a failure is clearable — never a success.
func (a *Agent) Retry() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.phase != "failed" {
		return errors.New("nothing to retry")
	}
	a.phase = "idle"
	a.errMsg = ""
	a.events = nil
	a.cfg = nil
	return nil
}

func (a *Agent) publish(p Progress) {
	a.mu.Lock()
	a.events = append(a.events, p)
	for ch := range a.subs {
		select {
		case ch <- p:
		default:
			// A subscriber this far behind (a phone that slept mid-install)
			// can never be caught up in-band: dropping arbitrary events could
			// discard the terminal one, and that surface would spin forever.
			// Close it out instead — the SSE handler ends the response, the
			// browser's EventSource reconnects, and Subscribe's replay hands
			// it the complete story. A stalled subscriber still never stalls
			// the install.
			delete(a.subs, ch)
			close(ch)
		}
	}
	a.mu.Unlock()
}

// Subscribe returns a channel replaying every event so far, then live ones.
func (a *Agent) Subscribe() (<-chan Progress, func()) {
	a.mu.Lock()
	// Sized for the full replay plus live headroom, so seeding under the
	// lock can never block.
	ch := make(chan Progress, len(a.events)+256)
	for _, p := range a.events {
		ch <- p
	}
	if a.subs == nil {
		a.subs = map[chan Progress]struct{}{}
	}
	a.subs[ch] = struct{}{}
	a.mu.Unlock()
	return ch, func() {
		a.mu.Lock()
		delete(a.subs, ch)
		a.mu.Unlock()
	}
}
