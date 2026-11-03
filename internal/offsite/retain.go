package offsite

import (
	"sort"
	"strconv"
	"time"

	"gitea.lan/matthew/fog/internal/fsx"
	"gitea.lan/matthew/fog/internal/store"
)

// Retention in the locker is tiered and counted in backups, not days, so
// the same numbers scale with whatever cadence the schedule runs at:
// nightly, 30 recent is a month; weekly, it is seven months.
//
//   - recent:  the newest N of a series
//   - monthly: beyond those, the first backup of each of the last M months
//   - yearly:  beyond those, the first backup of each of the last Y years
//
// The newest is always kept; a series of one is never touched.

const (
	SettingKeepRecent  = "offsite_keep_recent"
	SettingKeepMonthly = "offsite_keep_monthly"
	SettingKeepYearly  = "offsite_keep_yearly"
)

// Policy is the three tiers.
type Policy struct {
	Recent  int `json:"recent"`
	Monthly int `json:"monthly"`
	Yearly  int `json:"yearly"`
}

// DefaultPolicy is 30 / 6 / 3.
var DefaultPolicy = Policy{Recent: 30, Monthly: 6, Yearly: 3}

// ReadPolicy loads the settings, defaulting anything unset or unparsable.
func ReadPolicy(st *store.Store) Policy {
	p := DefaultPolicy
	read := func(key string, dst *int, min int) {
		v, _ := st.GetSetting(key)
		if v == "" {
			return
		}
		if n, err := strconv.Atoi(v); err == nil && n >= min {
			*dst = n
		}
	}
	read(SettingKeepRecent, &p.Recent, 1)
	read(SettingKeepMonthly, &p.Monthly, 0)
	read(SettingKeepYearly, &p.Yearly, 0)
	return p
}

// ValidPolicySetting is the settings-table validator for the three keys.
func ValidPolicySetting(key string) func(string) bool {
	min, max := 0, 200
	if key == SettingKeepRecent {
		min, max = 1, 1000
	}
	return func(v string) bool {
		if v == "" {
			return true
		}
		n, err := strconv.Atoi(v)
		return err == nil && n >= min && n <= max
	}
}

// Item is one member of a series: something with a time and a name.
type Item struct {
	Name string
	At   time.Time
}

// Retain picks which items a series keeps. Items may arrive in any order;
// the result is the set of Names to keep.
func (p Policy) Retain(items []Item, now time.Time) map[string]bool {
	keep := map[string]bool{}
	if len(items) == 0 {
		return keep
	}
	sorted := append([]Item(nil), items...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].At.After(sorted[j].At) })
	recent := p.Recent
	if recent < 1 {
		recent = 1
	}
	for i, it := range sorted {
		if i < recent {
			keep[it.Name] = true
		}
	}
	// First (oldest) item of each calendar month, for the last M months
	// counted back from now.
	if p.Monthly > 0 {
		firstOf := map[string]Item{}
		for _, it := range sorted {
			k := it.At.UTC().Format("2006-01")
			if cur, ok := firstOf[k]; !ok || it.At.Before(cur.At) {
				firstOf[k] = it
			}
		}
		y, m := now.UTC().Year(), now.UTC().Month()
		for i := 0; i < p.Monthly; i++ {
			k := time.Date(y, m, 1, 0, 0, 0, 0, time.UTC).AddDate(0, -i, 0).Format("2006-01")
			if it, ok := firstOf[k]; ok {
				keep[it.Name] = true
			}
		}
	}
	if p.Yearly > 0 {
		firstOf := map[string]Item{}
		for _, it := range sorted {
			k := it.At.UTC().Format("2006")
			if cur, ok := firstOf[k]; !ok || it.At.Before(cur.At) {
				firstOf[k] = it
			}
		}
		for i := 0; i < p.Yearly; i++ {
			k := strconv.Itoa(now.UTC().Year() - i)
			if it, ok := firstOf[k]; ok {
				keep[it.Name] = true
			}
		}
	}
	return keep
}

// LockDays is how long grade-A objects are locked on write: the recent
// tier's span at a nightly cadence, plus a week. Longer tiers are kept
// locked by the nightly extension pass.
func (p Policy) LockDays() int {
	r := p.Recent
	if r < 1 {
		r = 1
	}
	return r + 7
}

// StampTime parses a locker stamp (YYYYMMDD-HHMMSS, UTC, perhaps with a
// "-N" same-second suffix).
func StampTime(stamp string) (time.Time, bool) { return fsx.ParseStamp(stamp) }
