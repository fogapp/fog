package setup

import (
	"context"
	"fmt"
)

// Step is one line of a scripted flow, as the wizard renders it.
type Step struct{ ID, Title string }

// InstallSteps is the shared script: the mock executor plays it, the real
// engine (imaging phase) performs it, and the wizard renders it. One source
// of truth so the UI can never promise a step the engine doesn't have.
var InstallSteps = []Step{
	{"payload", "Checking the installer payload"},
	{"partition", "Partitioning the disk"},
	{"format", "Creating filesystems"},
	{"rootfs", "Copying the Fog operating system"},
	{"recovery", "Writing the recovery partition"},
	{"bootloader", "Installing the boot loader"},
	{"configure", "Writing system configuration"},
	{"verify", "Verifying the installation"},
}

// fsckLabels are the partitions "Check & repair filesystems" walks, in
// order. The ESP and recovery partition are left alone: neither is written
// in normal operation.
var fsckLabels = []string{"fog-root", "fog-data"}

// RepairSteps is each repair action's script, shared by the real engine and
// the mock the same way InstallSteps is.
func RepairSteps(action string) []Step {
	switch action {
	case "fsck":
		steps := make([]Step, len(fsckLabels))
		for i, label := range fsckLabels {
			steps[i] = Step{"fsck-" + label, "Checking " + label}
		}
		return steps
	case "rebuild-docker":
		return []Step{{"rebuild-docker", "Rebuilding the app runtime"}}
	case "reinstall-os":
		return []Step{
			{"payload", "Checking the recovery copy"},
			{"format-root", "Resetting the OS partition"},
			{"rootfs", "Copying the Fog operating system"},
			{"bootloader", "Reinstalling the boot loader"},
			{"configure", "Restoring system configuration"},
			{"verify", "Verifying"},
		}
	}
	return nil
}

// stepRun is what a running step may say about itself while it works.
type stepRun struct {
	p      Progress
	report func(Progress)
}

// Percent streams progress within the step.
func (s *stepRun) Percent(pct int) {
	s.p.Percent = pct
	s.report(s.p)
}

// Detail sets the line shown under the step; on failure it replaces the
// error text (a command's output tail says more than its exit status).
func (s *stepRun) Detail(d string) { s.p.Detail = d }

type stepFunc func(ctx context.Context, s *stepRun) error

// runSteps plays steps in order, reporting each one running and then done
// or failed. Every step needs an entry in do: a script line with no code
// behind it fails rather than reporting "done" for work never done. onFail
// (optional) runs once, after the failing step is reported.
func runSteps(ctx context.Context, steps []Step, do map[string]stepFunc, report func(Progress), onFail func()) error {
	for i, st := range steps {
		s := &stepRun{report: report, p: Progress{
			Step: i + 1, Steps: len(steps), ID: st.ID, Title: st.Title, Status: "running",
		}}
		report(s.p)
		var err error
		if fn, ok := do[st.ID]; ok {
			err = fn(ctx, s)
		} else {
			err = fmt.Errorf("no step %q in this engine", st.ID)
		}
		if err != nil {
			s.p.Status = "failed"
			if s.p.Detail == "" {
				s.p.Detail = err.Error()
			}
			report(s.p)
			if onFail != nil {
				onFail()
			}
			return fmt.Errorf("%s: %w", st.Title, err)
		}
		s.p.Status = "done"
		s.p.Percent = 100
		report(s.p)
	}
	return nil
}
