// The fleet view of backups: one row per app answering "is this backed up,
// when was it last, and is that recent enough?" — the same rule the
// staleness alarm applies, so the table and the alarm can never disagree.
package backup

import (
	"strconv"
	"time"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/store"
)

type AppBackupStatus struct {
	App     string `json:"app"`
	Enabled bool   `json:"enabled"`
	// Mode is "volumes" or "delegated" — the empty spec value is normalized
	// so the UI never has to know that "" means volumes.
	Mode string `json:"mode" ts:"\"volumes\" | \"delegated\""`
	// HourUTC is the hour scheduled snapshots run at, resolved through the
	// global default; nil when nothing is scheduled (backups off, or enabled
	// without an hour — the case the scanner files an issue for).
	HourUTC *int `json:"hourUTC"`
	// NextAt is the next scheduled point, RFC3339; empty when unscheduled.
	NextAt string `json:"nextAt,omitempty"`
	// The newest recorded backup, when there is one.
	LastAt         string `json:"lastAt,omitempty"`
	LastKind       string `json:"lastKind,omitempty"`
	LastSizeBytes  int64  `json:"lastSizeBytes,omitempty"`
	LastFormat     string `json:"lastFormat,omitempty"`
	LastVerifiedAt string `json:"lastVerifiedAt,omitempty"`
	// Stale is the staleness alarm's own verdict: scheduled, and the newest
	// backup is missing or older than StaleAfter — but never before the
	// first run the app was ever expected to make has come and gone. False
	// for anything unscheduled: there is nothing to be late against.
	Stale bool `json:"stale"`
	// Offsite is the newest backup's state in the offsite targets: ""
	// (no target), "stored", "uploading", or "pending". The best state
	// across targets — one copy that landed is a copy that landed.
	Offsite string `json:"offsite,omitempty" ts:"\"\" | \"verified\" | \"stored\" | \"uploading\" | \"pending\""`
}

// FleetStatus reads every app's backup posture from the store. Pure over
// the store so the handler and its tests need no Manager.
func FleetStatus(st *store.Store, now time.Time) ([]AppBackupStatus, error) {
	list, err := st.ListApps()
	if err != nil {
		return nil, err
	}
	specs, err := apps.LatestSpecs(st)
	if err != nil {
		return nil, err
	}
	global, _ := st.GetSetting(SettingBackupHourUTC)

	// A failed read leaves every row's offsite state blank.
	offsite, _ := st.OffsiteStatesOf(store.OffsiteKindApp)
	// Likewise a failed read leaves every row's last backup blank.
	latest, _ := st.LatestBackups()
	out := make([]AppBackupStatus, 0, len(list))
	for _, a := range list {
		row := AppBackupStatus{App: a.Name, Mode: apps.BackupModeVolumes}
		spec := specs[a.ID]
		row.Enabled = spec.BackupsEnabled()
		if spec.DelegatedBackups() {
			row.Mode = apps.BackupModeDelegated
		}
		if b, ok := latest[a.ID]; ok {
			row.LastAt, row.LastKind, row.LastSizeBytes = b.CreatedAt, b.Kind, b.SizeBytes
			row.LastFormat, row.LastVerifiedAt = b.Format, b.VerifiedAt
			row.Offsite = offsite.Of(strconv.FormatInt(b.ID, 10))
		}
		if row.Enabled && spec != nil {
			if hour, ok := backupHour(spec, global); ok {
				h := hour
				row.HourUTC = &h
				next := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, time.UTC)
				if !next.After(now) {
					next = next.Add(24 * time.Hour)
				}
				row.NextAt = next.Format(time.RFC3339)
				row.Stale = isStale(st, a, hour, row.LastAt, now)
			}
		}
		out = append(out, row)
	}
	return out, nil
}

// firstGrace is how long past a scheduled run a backup may take to land
// before the app counts as overdue — the same slack StaleAfter carries over
// a day's cycle, applied to the very first run an app owes.
const firstGrace = StaleAfter - 24*time.Hour

// isStale answers "is this app late?" — the one rule the fleet table, the
// sidebar badge and the dead-man's-switch all read from.
//
// The newest backup aging past StaleAfter is the alarm proper. The second
// clause is what keeps it honest on a fresh app: an app that has never been
// expected to back up yet owes nothing. Installing an app at noon under a
// 03:00 schedule used to raise "no recent backup" on the spot, hours before
// fog itself intended to take one — an alarm the operator can only learn to
// ignore, which is precisely how a real overdue backup goes unnoticed.
func isStale(st *store.Store, a store.App, hour int, lastAt string, now time.Time) bool {
	if lastAt != "" {
		t, err := time.Parse(time.RFC3339, lastAt)
		if err != nil {
			return false // an unreadable stamp is not evidence of lateness
		}
		if now.Sub(t) <= StaleAfter {
			return false
		}
	}
	// The app owes backups from the later of its install and the moment
	// they were last switched on. A later start only pushes the first due
	// run later, so lateness is decided cheaply where it can be: not late
	// even counting from install means not late at all, and the revision
	// walk stops at the first switch-on stamp that is already late enough.
	late := func(since time.Time) bool { return now.After(firstDueAt(since, hour).Add(firstGrace)) }
	installed := installedAt(a)
	if !late(installed) {
		return false
	}
	since := installed
	if t, ok := backupsEnabledAt(st, a.ID, func(t time.Time) bool { return late(laterOf(installed, t)) }); ok {
		since = laterOf(installed, t)
	}
	return late(since)
}

func laterOf(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// firstDueAt is the first scheduled run strictly after since.
func firstDueAt(since time.Time, hour int) time.Time {
	due := time.Date(since.Year(), since.Month(), since.Day(), hour, 0, 0, 0, time.UTC)
	if !due.After(since) {
		due = due.Add(24 * time.Hour)
	}
	return due
}

// installedAt is when the app was installed. A zero time (unparseable
// stamp) grants no grace — the alarm's default has to be to fire.
func installedAt(a store.App) time.Time {
	if t, err := time.Parse(time.RFC3339, a.CreatedAt); err == nil {
		return t.UTC()
	}
	return time.Time{}
}

// enabledWalkFirst is how many revisions backupsEnabledAt reads before
// reaching for the rest: an app's newest few nearly always settle it.
const enabledWalkFirst = 10

// backupsEnabledAt walks the app's revisions newest-first and returns the
// creation time of the oldest one in the unbroken run that has backups
// enabled — when the operator last switched them on. A run reaching back
// past the first revision on record simply lands on install time, which is
// the same answer.
//
// enough, given the run's oldest stamp so far, may end the walk early: the
// caller's question is already answered by that stamp and an older one
// would not change it. The stamp returned is then the one enough accepted.
func backupsEnabledAt(st *store.Store, appID int64, enough func(time.Time) bool) (time.Time, bool) {
	var at time.Time
	found := false
	seen := 0
	for _, limit := range []int{enabledWalkFirst, 200} {
		revs, err := st.ListRevisionSpecs(appID, limit)
		if err != nil {
			return at, found
		}
		for _, r := range revs[min(seen, len(revs)):] {
			spec, err := apps.ParseSpec(r.Spec)
			if err != nil {
				return at, found
			}
			if !spec.BackupsEnabled() {
				return at, found
			}
			t, perr := time.Parse(time.RFC3339, r.CreatedAt)
			if perr != nil {
				return at, found
			}
			at, found = t.UTC(), true
			if enough != nil && enough(at) {
				return at, found
			}
		}
		if len(revs) < limit {
			break // that was every revision there is
		}
		seen = len(revs)
	}
	return at, found
}
