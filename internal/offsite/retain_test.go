package offsite

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
)

func nightly(from time.Time, days int) []Item {
	var items []Item
	for i := 0; i < days; i++ {
		at := from.AddDate(0, 0, -i)
		items = append(items, Item{Name: at.Format("20060102-150405"), At: at})
	}
	return items
}

func kept(keep map[string]bool) []string {
	var out []string
	for k := range keep {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Two years of nightly backups under 30/6/3: the last thirty nights, the
// first night of each of the last six months, the first night of this
// year and the previous two.
func TestRetainTiers(t *testing.T) {
	now := time.Date(2026, 8, 27, 1, 0, 0, 0, time.UTC)
	items := nightly(now, 2*365)
	keep := (Policy{Recent: 30, Monthly: 6, Yearly: 3}).Retain(items, now)
	got := kept(keep)
	// Recent: the newest thirty.
	for i := 0; i < 30; i++ {
		if !keep[items[i].Name] {
			t.Errorf("recent night %d not kept", i)
		}
	}
	if keep[items[30].Name] {
		t.Error("night 31 should not be in the recent tier")
	}
	// Monthly: first of Aug, Jul, Jun, May, Apr, Mar 2026.
	for _, m := range []string{"20260801", "20260701", "20260601", "20260501", "20260401", "20260301"} {
		if !keep[m+"-010000"] {
			t.Errorf("first of month %s not kept", m)
		}
	}
	if keep["20260201-010000"] {
		t.Error("February is past the monthly tier")
	}
	// Yearly: first backup of 2026, 2025 (2024 has none in the two-year set... the oldest is 2024-08-28).
	if !keep["20260101-010000"] || !keep["20250101-010000"] {
		t.Error("first of year not kept")
	}
	// 2024's first *available* backup is the oldest item.
	oldest := items[len(items)-1]
	if !strings.HasPrefix(oldest.Name, "2024") || !keep[oldest.Name] {
		t.Errorf("the oldest backup of 2024 (%s) should be the yearly keeper", oldest.Name)
	}
	// 30 recent + 5 monthly (August 1st is already in the recent tier) + 3 yearly.
	if len(got) != 30+5+3 {
		t.Errorf("kept %d, want 38: %v", len(got), got)
	}
}

// Counts, not days: the same policy on weekly backups keeps thirty weeks.
func TestRetainScalesWithCadence(t *testing.T) {
	now := time.Date(2026, 8, 27, 1, 0, 0, 0, time.UTC)
	var items []Item
	for i := 0; i < 60; i++ {
		at := now.AddDate(0, 0, -7*i)
		items = append(items, Item{Name: fmt.Sprintf("w%02d", i), At: at})
	}
	keep := (Policy{Recent: 30, Monthly: 0, Yearly: 0}).Retain(items, now)
	if len(keep) != 30 || !keep["w29"] || keep["w30"] {
		t.Fatalf("weekly: %v", kept(keep))
	}
}

func TestRetainNeverEmptiesASeries(t *testing.T) {
	now := time.Now()
	one := []Item{{Name: "only", At: now.AddDate(-5, 0, 0)}}
	if keep := (Policy{Recent: 0, Monthly: 0, Yearly: 0}).Retain(one, now); !keep["only"] {
		t.Fatal("a series of one must keep it")
	}
	if keep := (Policy{}).Retain(nil, now); len(keep) != 0 {
		t.Fatal("empty in, empty out")
	}
	// Order of input does not matter.
	items := nightly(now, 5)
	items[0], items[4] = items[4], items[0]
	keep := (Policy{Recent: 2}).Retain(items, now)
	// items[4] is the newest after the swap; items[1] the second newest.
	if len(keep) != 2 || !keep[items[4].Name] || !keep[items[1].Name] {
		t.Fatalf("shuffled: %v", kept(keep))
	}
}

func TestPolicyValidationAndLockDays(t *testing.T) {
	if !ValidPolicySetting(SettingKeepRecent)("") || !ValidPolicySetting(SettingKeepRecent)("30") || ValidPolicySetting(SettingKeepRecent)("0") || ValidPolicySetting(SettingKeepRecent)("x") {
		t.Fatal("recent validation")
	}
	if !ValidPolicySetting(SettingKeepMonthly)("0") || ValidPolicySetting(SettingKeepMonthly)("500") {
		t.Fatal("monthly validation")
	}
	if (Policy{Recent: 30}).LockDays() != 37 || (Policy{}).LockDays() != 8 {
		t.Fatal("lock days")
	}
	if _, ok := StampTime("20260827-010314"); !ok {
		t.Fatal("stamp time")
	}
	if !inWindow("", time.Now()) || !inWindow("01:00-07:00", time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC)) ||
		inWindow("01:00-07:00", time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)) ||
		!inWindow("22:00-06:00", time.Date(2026, 1, 1, 23, 30, 0, 0, time.UTC)) ||
		!inWindow("22:00-06:00", time.Date(2026, 1, 1, 2, 0, 0, 0, time.UTC)) {
		t.Fatal("window")
	}
}
