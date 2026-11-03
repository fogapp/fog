package issues

import (
	"context"
	"testing"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/store"
)

func TestOffsiteUnconfiguredOnlyWhenBackupsRun(t *testing.T) {
	h := 2
	scheduled := &snapshot{apps: []appInfo{{app: store.App{ID: 1, Name: "hub"}, spec: &apps.AppSpec{Backup: &apps.BackupSpec{Enabled: true, HourUTC: &h}}}}}
	if rep := checkOffsiteUnconfigured(context.Background(), scheduled); len(rep.findings) != 1 || rep.findings[0].Kind != "offsite-unconfigured" {
		t.Fatalf("scheduled, no target: %+v", rep)
	}
	nothing := &snapshot{apps: []appInfo{{app: store.App{ID: 1, Name: "hub"}, spec: &apps.AppSpec{}}}}
	if rep := checkOffsiteUnconfigured(context.Background(), nothing); len(rep.findings) != 0 || len(rep.evaluated) != 1 {
		t.Fatalf("no backups scheduled: %+v", rep)
	}
	withTarget := &snapshot{backupHourGlobal: "1", offsiteTargets: []store.OffsiteTarget{{Name: "cloud", Role: "active", Grade: "A"}}}
	if rep := checkOffsiteUnconfigured(context.Background(), withTarget); len(rep.findings) != 0 {
		t.Fatalf("with a target: %+v", rep)
	}
	retiredOnly := &snapshot{backupHourGlobal: "1", offsiteTargets: []store.OffsiteTarget{{Name: "old", Role: "retired", Grade: "A"}}}
	if rep := checkOffsiteUnconfigured(context.Background(), retiredOnly); len(rep.findings) != 1 {
		t.Fatalf("a retired target does not count: %+v", rep)
	}
}

func TestOffsiteGradeFilesBelowA(t *testing.T) {
	sn := &snapshot{offsiteTargets: []store.OffsiteTarget{
		{Name: "good", Role: "active", Grade: "A"},
		{Name: "append", Role: "active", Grade: "B", Probe: `{"grade":"B","fix":"Enable Object Lock."}`},
		{Name: "leaky", Role: "active", Grade: "C"},
		{Name: "gone", Role: "retired", Grade: "D"},
		{Name: "unknown", Role: "active", Grade: ""},
	}}
	rep := checkOffsiteGrade(context.Background(), sn)
	if len(rep.findings) != 2 {
		t.Fatalf("findings: %+v", rep.findings)
	}
	bySubject := map[string]store.IssueFinding{}
	for _, f := range rep.findings {
		bySubject[f.Subject] = f
	}
	if f := bySubject["append"]; f.Severity != "info" || f.Fix != "Enable Object Lock." {
		t.Fatalf("append-only: %+v", f)
	}
	if f := bySubject["leaky"]; f.Severity != "warning" || f.FixURL != "/settings/storage/offsite" {
		t.Fatalf("versioned: %+v", f)
	}
}
