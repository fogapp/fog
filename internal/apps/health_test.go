package apps

import (
	"testing"
	"time"
)

// History holds a day of probes at the monitor's cadence, newest last, and
// once full keeps sliding forward rather than growing.
func TestHealthHistoryKeepsADay(t *testing.T) {
	e := &Engine{}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	n := healthRingSize()
	for i := 0; i < n+10; i++ {
		e.recordHealth("app", i%2 == 0, start.Add(time.Duration(i)*HealthInterval))
	}
	got := e.probeHistory("app")
	if len(got) > n {
		t.Fatalf("history grew past a day: %d probes, ring of %d", len(got), n)
	}
	last := start.Add(time.Duration(n+9) * HealthInterval)
	if !got[len(got)-1].At.Equal(last) {
		t.Fatalf("newest = %v, want %v", got[len(got)-1].At, last)
	}
	if got[0].At.Before(last.Add(-healthKeep)) {
		t.Fatalf("oldest %v is more than a day before the newest", got[0].At)
	}
	for i := 1; i < len(got); i++ {
		if !got[i].At.After(got[i-1].At) {
			t.Fatal("history out of order")
		}
	}
}

// The ring counts probes, not time: after a pause in probing, what came
// before the gap is more than a day old and must not be shown.
func TestHealthHistoryDropsProbesBeforeAGap(t *testing.T) {
	e := &Engine{}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	e.recordHealth("app", false, start)
	e.recordHealth("app", false, start.Add(HealthInterval))
	later := start.Add(3 * 24 * time.Hour)
	e.recordHealth("app", true, later)
	got := e.probeHistory("app")
	if len(got) != 1 || !got[0].At.Equal(later) || !got[0].OK {
		t.Fatalf("history after a gap = %+v", got)
	}
	if e.probeHistory("other") != nil {
		t.Fatal("an unprobed app has history")
	}
}
