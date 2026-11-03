package notify

import (
	"testing"
	"time"
)

func TestThrottleOncePerIntervalPerKey(t *testing.T) {
	var th Throttle
	t0 := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	allow := func(key string, at time.Duration) bool { return th.Allow(key, time.Hour, t0.Add(at)) }
	if !allow("a", 0) {
		t.Fatal("first call refused")
	}
	if allow("a", 59*time.Minute) {
		t.Error("second call inside the hour let through")
	}
	if !allow("b", time.Minute) {
		t.Error("another key held back by the first")
	}
	if !allow("a", time.Hour) {
		t.Error("call after the hour refused")
	}
	// A refused call doesn't restart the interval.
	if allow("a", 90*time.Minute) || !allow("a", 2*time.Hour) {
		t.Error("interval measured from the wrong call")
	}
}
