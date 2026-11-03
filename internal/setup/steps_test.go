package setup

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestRunStepsRefusesAStepWithNoCode(t *testing.T) {
	steps := []Step{{"a", "First"}, {"b", "Second"}}
	var got []Progress
	err := runSteps(context.Background(), steps, map[string]stepFunc{
		"a": func(context.Context, *stepRun) error { return nil },
	}, func(p Progress) { got = append(got, p) }, nil)
	if err == nil || !strings.Contains(err.Error(), "Second") {
		t.Fatalf("err = %v, want a failure naming the missing step", err)
	}
	last := got[len(got)-1]
	if last.ID != "b" || last.Status != "failed" {
		t.Fatalf("last progress = %+v, want b failed", last)
	}
}

func TestRunStepsKeepsAStepsOwnFailureDetail(t *testing.T) {
	var got []Progress
	failed := false
	err := runSteps(context.Background(), []Step{{"x", "Doing x"}}, map[string]stepFunc{
		"x": func(_ context.Context, s *stepRun) error {
			s.Percent(40)
			s.Detail("last lines of output")
			return errors.New("exit status 8")
		},
	}, func(p Progress) { got = append(got, p) }, func() { failed = true })
	if err == nil || !failed {
		t.Fatalf("err = %v, onFail ran = %v", err, failed)
	}
	want := []string{"running", "running", "failed"}
	for i, p := range got {
		if p.Status != want[i] {
			t.Fatalf("progress %d = %+v, want %s", i, p, want[i])
		}
	}
	if got[1].Percent != 40 || got[2].Detail != "last lines of output" {
		t.Fatalf("progress = %+v", got)
	}
}

// The mock and the real engine play the same scripts, so the repair screen
// shows in dev exactly the lines it shows on a machine.
func TestMockRepairPlaysTheRealScript(t *testing.T) {
	for _, ra := range RepairActions {
		var done []string
		err := MockExecutor{StepDelay: 1}.Repair(context.Background(), ra.ID, func(p Progress) {
			if p.Status == "done" {
				done = append(done, p.ID+"|"+p.Title)
			}
		})
		if err != nil {
			t.Fatalf("%s: %v", ra.ID, err)
		}
		steps := RepairSteps(ra.ID)
		if len(steps) == 0 || len(done) != len(steps) {
			t.Fatalf("%s: done %v, script %v", ra.ID, done, steps)
		}
		for i, st := range steps {
			if done[i] != st.ID+"|"+st.Title {
				t.Fatalf("%s step %d = %s, want %s|%s", ra.ID, i, done[i], st.ID, st.Title)
			}
		}
	}
	if err := (MockExecutor{StepDelay: 1}).Repair(context.Background(), "format-c", func(Progress) {}); err == nil {
		t.Fatal("unknown action played")
	}
}
