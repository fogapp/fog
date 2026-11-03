package supervise

import (
	"testing"
	"time"
)

// The ladder must climb on short runs, cap at a minute, and — crucially —
// drop back to the bottom after a healthy run: before the run's duration
// fed the delay, the healthy-run check compared against a timestamp taken
// AFTER the process exited, so it could never fire and one crash after a
// week of uptime cost the full crash-loop ceiling.
func TestRestartDelay(t *testing.T) {
	cases := []struct {
		name    string
		crashes int
		ran     time.Duration
		delay   time.Duration
		next    int
	}{
		{"first crash sleeps the base delay", 0, 10 * time.Second, BaseDelay, 1},
		{"short runs keep climbing", 3, 3 * time.Second, 16 * time.Second, 4},
		// The old ladder doubled 32s past its own ceiling to 64s.
		{"the ceiling is a minute, not 64s", 5, 3 * time.Second, time.Minute, 6},
		{"the ceiling holds", 40, 3 * time.Second, time.Minute, 41},
		{"healthy run resets from the ceiling", 40, 7 * 24 * time.Hour, BaseDelay, 1},
		{"healthy run resets mid-ladder", 3, 6 * time.Minute, BaseDelay, 1},
		{"a run just under the threshold does not reset", 2, HealthyRun - time.Second, 8 * time.Second, 3},
	}
	for _, c := range cases {
		delay, next := restartDelay(c.crashes, c.ran)
		if delay != c.delay || next != c.next {
			t.Errorf("%s: restartDelay(%d, %v) = (%v, %d), want (%v, %d)",
				c.name, c.crashes, c.ran, delay, next, c.delay, c.next)
		}
	}
}
