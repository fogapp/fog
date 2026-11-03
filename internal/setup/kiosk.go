package setup

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// KioskScale is how big the wizard is drawn on the machine's own display.
// fog-kiosk (imaging/overlay/live/usr/local/bin) decides it from the panel's
// resolution; the wizard's screen-size buttons overrule it when that guess is
// wrong for the screen in front of someone.
type KioskScale struct {
	Scale  float64 `json:"scale"`  // what the running kiosk was started at
	Auto   float64 `json:"auto"`   // what fog-kiosk picks with no override
	Manual bool    `json:"manual"` // an override from the buttons is in force
}

// KioskDisplay reads and overrules the kiosk's scale.
type KioskDisplay interface {
	Scale() (KioskScale, error)
	// SetScale overrules the automatic scale (0 hands it back) and reports
	// whether the kiosk restarts to apply it — the screen blanks and the page
	// comes back at the new size.
	SetScale(scale float64) (restarting bool, err error)
	// Console hands the machine's display to the command line (on=true): the
	// kiosk stops and the root shell that was on tty1 underneath it all
	// along shows. on=false starts the kiosk again. Nothing else changes —
	// no partition is mounted or touched; it is the shell that does things.
	Console(on bool) error
}

// ErrNoKiosk means nothing is drawing the wizard on a local display: a
// headless machine, or a kiosk that never came up.
var ErrNoKiosk = errors.New("no kiosk display on this machine")

// The range fog-kiosk accepts; anything outside it is ignored there, so it is
// refused here rather than silently doing nothing.
const (
	MinKioskScale = 0.5
	MaxKioskScale = 4.0
)

func validKioskScale(s float64) bool {
	return s == 0 || (s >= MinKioskScale && s <= MaxKioskScale)
}

// KioskDir is the real display: the files fog-kiosk keeps in its runtime
// directory, plus the rescale unit that restarts the kiosk when the answer
// changed.
type KioskDir struct {
	Dir     string // /run/fog-kiosk
	Rescale func() error
	// Kiosk starts (true) or stops (false) fog-kiosk.service.
	Kiosk func(running bool) error
}

func (k KioskDir) Scale() (KioskScale, error) {
	cur, ok := k.read("scale")
	if !ok {
		return KioskScale{}, ErrNoKiosk
	}
	st := KioskScale{Scale: cur, Auto: cur}
	if auto, ok := k.read("auto"); ok {
		st.Auto = auto
	}
	if o, ok := k.read("override"); ok && o != 0 && validKioskScale(o) {
		st.Manual = true
	}
	return st, nil
}

func (k KioskDir) SetScale(scale float64) (bool, error) {
	st, err := k.Scale()
	if err != nil {
		return false, err
	}
	path := filepath.Join(k.Dir, "override")
	if scale == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		scale = st.Auto
	} else if err := os.WriteFile(path, []byte(strconv.FormatFloat(scale, 'f', -1, 64)+"\n"), 0o644); err != nil {
		return false, err
	}
	if k.Rescale != nil {
		if err := k.Rescale(); err != nil {
			return false, err
		}
	}
	return scale != st.Scale, nil
}

func (k KioskDir) Console(on bool) error {
	// No state files means the kiosk never came up: there is no screen to
	// hand over, so say so rather than stopping a unit that isn't running.
	if _, err := k.Scale(); err != nil {
		return err
	}
	if k.Kiosk == nil {
		return errors.New("this build cannot stop the kiosk")
	}
	return k.Kiosk(!on)
}

func (k KioskDir) read(name string) (float64, bool) {
	raw, err := os.ReadFile(filepath.Join(k.Dir, name))
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(string(raw)), 64)
	return v, err == nil
}

// MockKiosk is a display for wizard dev. Its "restart" is instant, so the
// page stays up and simply reads the new scale back.
type MockKiosk struct {
	mu       sync.Mutex
	auto     float64
	override float64
	console  bool // the pretend display shows the command line
}

func NewMockKiosk(auto float64) *MockKiosk { return &MockKiosk{auto: auto} }

func (m *MockKiosk) Scale() (KioskScale, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := KioskScale{Scale: m.auto, Auto: m.auto, Manual: m.override != 0}
	if st.Manual {
		st.Scale = m.override
	}
	return st, nil
}

func (m *MockKiosk) SetScale(scale float64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.override = scale
	return false, nil
}

func (m *MockKiosk) Console(on bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.console = on
	return nil
}

// Console hands the machine's display to the command line, or back. Repair
// mode only — it is the recovery menu's "nothing automatic, I'll do it
// myself" exit — and never while a repair is writing to the disk: the shell
// would appear over a half-finished rewrite with no screen left to say so.
func (a *Agent) Console(on bool) error {
	if a.modeLocked() != "repair" {
		return errors.New("the command line is only offered when booted into Fog Recovery")
	}
	if a.Display == nil {
		return ErrNoKiosk
	}
	a.mu.Lock()
	phase := a.phaseLocked()
	a.mu.Unlock()
	if phase == "installing" {
		return errors.New("a repair is running — wait for it to finish before leaving this screen")
	}
	return a.Display.Console(on)
}
