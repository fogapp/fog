package backup

import (
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/store"
)

// FleetStatus applies the staleness alarm's own rule: only a scheduled app
// can be stale, and it is stale when its newest backup is missing or older
// than the schedule plus grace. The resolved hour follows the global
// default when the spec sets none.
func TestFleetStatus(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.SetSetting(SettingBackupHourUTC, "3")

	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

	// installedAt places the app (and the revision carrying its backup
	// settings) on the fixture clock: staleness is measured from when the
	// app started owing backups, so the store's wall-clock stamps would
	// otherwise put every fixture inside its first-backup grace.
	mk := func(name, spec string, installedAt time.Time) *store.App {
		a, err := st.CreateApp(name)
		if err != nil {
			t.Fatal(err)
		}
		r, err := st.CreateRevision(a.ID, spec, "", "test")
		if err != nil {
			t.Fatal(err)
		}
		at := installedAt.Format(time.RFC3339)
		if _, err := st.DB().Exec(`UPDATE apps SET created_at = ? WHERE id = ?`, at, a.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB().Exec(`UPDATE app_revisions SET created_at = ? WHERE id = ?`, at, r.ID); err != nil {
			t.Fatal(err)
		}
		return a
	}
	week := now.Add(-7 * 24 * time.Hour)

	// Installed a week ago, follows the global hour, never backed up: stale.
	mk("never", `{"source":{"type":"image","image":"x"},"backup":{"enabled":true}}`, week)
	// Enabled with its own hour, backed up an hour ago: fine.
	fresh := mk("fresh", `{"source":{"type":"image","image":"x"},"backup":{"enabled":true,"hourUTC":5,"mode":"delegated"}}`, week)
	// Enabled, backed up three days ago: stale.
	old := mk("old", `{"source":{"type":"image","image":"x"},"backup":{"enabled":true}}`, week)
	// Backups off: never stale, no schedule.
	mk("off", `{"source":{"type":"image","image":"x"}}`, week)
	// Installed an hour ago under the 03:00 schedule: its first backup is
	// not due until tomorrow, so it owes nothing yet.
	mk("justinstalled", `{"source":{"type":"image","image":"x"},"backup":{"enabled":true}}`, now.Add(-time.Hour))

	// CreateBackup stamps now(); rewrite the timestamps to the fixture clock.
	freshID, _ := st.CreateBackup(fresh.ID, "scheduled", "/x", 10)
	oldID, _ := st.CreateBackup(old.ID, "manual", "/y", 20)
	for id, at := range map[int64]time.Time{freshID: now.Add(-time.Hour), oldID: now.Add(-72 * time.Hour)} {
		if _, err := st.DB().Exec(`UPDATE backups SET created_at = ? WHERE id = ?`, at.Format(time.RFC3339), id); err != nil {
			t.Fatal(err)
		}
	}

	rows, err := FleetStatus(st, now)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]AppBackupStatus{}
	for _, r := range rows {
		byName[r.App] = r
	}
	if len(byName) != 5 {
		t.Fatalf("want a row per app, got %+v", rows)
	}

	never := byName["never"]
	if !never.Enabled || never.HourUTC == nil || *never.HourUTC != 3 || !never.Stale || never.NextAt != "2026-08-26T03:00:00Z" {
		t.Errorf("never: %+v", never)
	}
	f := byName["fresh"]
	if f.Stale || f.HourUTC == nil || *f.HourUTC != 5 || f.Mode != "delegated" || f.LastSizeBytes != 10 || f.NextAt != "2026-08-26T05:00:00Z" {
		t.Errorf("fresh: %+v", f)
	}
	if o := byName["old"]; !o.Stale || o.LastKind != "manual" {
		t.Errorf("old: %+v", o)
	}
	if off := byName["off"]; off.Enabled || off.Stale || off.HourUTC != nil || off.NextAt != "" || off.Mode != "volumes" {
		t.Errorf("off: %+v", off)
	}
	if ji := byName["justinstalled"]; ji.Stale || ji.LastAt != "" || ji.NextAt != "2026-08-26T03:00:00Z" {
		t.Errorf("just installed, first backup not yet due: %+v", ji)
	}

	// Once that first run has come and gone without a backup — 03:00 plus
	// the same slack an established app gets — the alarm engages.
	rows, err = FleetStatus(st, time.Date(2026, 8, 26, 5, 30, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.App == "justinstalled" && !r.Stale {
			t.Errorf("first backup missed its window, still not stale: %+v", r)
		}
	}

	// With no global hour, an enabled app without its own is unscheduled —
	// nothing to be late against, so not stale either.
	st.SetSetting(SettingBackupHourUTC, "")
	rows, _ = FleetStatus(st, now)
	for _, r := range rows {
		if r.App == "never" && (r.HourUTC != nil || r.Stale) {
			t.Errorf("unscheduled app reported as scheduled/stale: %+v", r)
		}
	}
}

// A grace period keyed on installation alone would still cry wolf the
// moment an operator switches backups on for an app that has had them off
// for months. The expectation starts when they are switched on.
func TestStalenessGracesNewlyEnabledBackups(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.SetSetting(SettingBackupHourUTC, "3")
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

	a, err := st.CreateApp("hub")
	if err != nil {
		t.Fatal(err)
	}
	off, _ := st.CreateRevision(a.ID, `{"source":{"type":"image","image":"x"}}`, "", "test")
	on, _ := st.CreateRevision(a.ID, `{"source":{"type":"image","image":"x"},"backup":{"enabled":true}}`, "", "test")
	st.DB().Exec(`UPDATE apps SET created_at = ? WHERE id = ?`, now.Add(-30*24*time.Hour).Format(time.RFC3339), a.ID)
	st.DB().Exec(`UPDATE app_revisions SET created_at = ? WHERE id = ?`, now.Add(-30*24*time.Hour).Format(time.RFC3339), off.ID)
	st.DB().Exec(`UPDATE app_revisions SET created_at = ? WHERE id = ?`, now.Add(-time.Hour).Format(time.RFC3339), on.ID)

	rows, err := FleetStatus(st, now)
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Stale {
		t.Errorf("backups switched on an hour ago, first run not due: %+v", rows[0])
	}
	// The next morning's run came and went with nothing: now it is late.
	rows, _ = FleetStatus(st, time.Date(2026, 8, 26, 5, 30, 0, 0, time.UTC))
	if !rows[0].Stale {
		t.Errorf("first run missed, still not stale: %+v", rows[0])
	}
}

// The switch-on moment is found however many revisions ago it was: a run of
// enabled revisions longer than the walk's first read still reaches back to
// the one that turned backups on, and an app installed after that stays
// graced by its install time.
func TestStalenessFindsTheSwitchOnPastManyRevisions(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.SetSetting(SettingBackupHourUTC, "3")
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	stamp := func(table string, id int64, at time.Time) {
		t.Helper()
		if _, err := st.DB().Exec(`UPDATE `+table+` SET created_at = ? WHERE id = ?`, at.Format(time.RFC3339), id); err != nil {
			t.Fatal(err)
		}
	}
	const on = `{"source":{"type":"image","image":"x"},"backup":{"enabled":true}}`
	// Installed a month ago with backups off, switched on at switchedOn,
	// then edited many times in the last hour.
	mk := func(name string, switchedOn time.Time) *store.App {
		a, _ := st.CreateApp(name)
		stamp("apps", a.ID, now.Add(-30*24*time.Hour))
		off, _ := st.CreateRevision(a.ID, `{"source":{"type":"image","image":"x"}}`, "", "test")
		stamp("app_revisions", off.ID, now.Add(-30*24*time.Hour))
		r, _ := st.CreateRevision(a.ID, on, "", "test")
		stamp("app_revisions", r.ID, switchedOn)
		for i := range 2*enabledWalkFirst + 3 {
			r, _ := st.CreateRevision(a.ID, on, "", "test")
			stamp("app_revisions", r.ID, now.Add(-time.Hour+time.Duration(i)*time.Minute))
		}
		return a
	}
	mk("long", now.Add(-3*24*time.Hour))
	recent := mk("recent", now.Add(-3*time.Hour))

	stale := func(at time.Time) map[string]bool {
		t.Helper()
		rows, err := FleetStatus(st, at)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, r := range rows {
			out[r.App] = r.Stale
		}
		return out
	}
	got := stale(now)
	if !got["long"] {
		t.Error("switched on three days ago and never backed up, not stale")
	}
	if got["recent"] {
		t.Error("switched on three hours ago, first run not due, yet stale")
	}
	tomorrow := time.Date(2026, 8, 26, 5, 30, 0, 0, time.UTC)
	if !stale(tomorrow)["recent"] {
		t.Error("first run missed, still not stale")
	}

	// Installed after the switch-on, just past the next 03:00 run: install
	// time is the later start, so the first run owed is a day later.
	stamp("apps", recent.ID, time.Date(2026, 8, 26, 4, 0, 0, 0, time.UTC))
	if stale(tomorrow)["recent"] {
		t.Error("installed at 04:00, first run not due until tomorrow, yet stale")
	}
}

// The dead-man's-switch notice states an ongoing condition: it has to
// appear when an app falls behind and go when it catches up.
func TestStalenessNoticeFollowsTheCondition(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.SetSetting(SettingBackupHourUTC, "3")
	a, err := st.CreateApp("hub")
	if err != nil {
		t.Fatal(err)
	}
	r, _ := st.CreateRevision(a.ID, `{"source":{"type":"image","image":"x"},"backup":{"enabled":true}}`, "", "test")
	week := time.Now().Add(-7 * 24 * time.Hour).UTC().Format(time.RFC3339)
	st.DB().Exec(`UPDATE apps SET created_at = ?`, week)
	st.DB().Exec(`UPDATE app_revisions SET created_at = ? WHERE id = ?`, week, r.ID)

	m := &Manager{Store: st, Log: slog.New(slog.DiscardHandler), Notify: func(string, string, bool) {}}
	m.checkStaleness()
	notices, _ := st.ActiveNotices()
	if len(notices) != 1 || notices[0].Kind != NoticeStale || notices[0].App != "hub" {
		t.Fatalf("installed a week ago, never backed up: %+v", notices)
	}

	if _, err := st.CreateBackup(a.ID, "scheduled", "/x", 10); err != nil {
		t.Fatal(err)
	}
	m.checkStaleness()
	if notices, _ = st.ActiveNotices(); len(notices) != 0 {
		t.Fatalf("notice outlived the condition: %+v", notices)
	}
}
