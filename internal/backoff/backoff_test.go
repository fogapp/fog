package backoff

import (
	"math"
	"testing"
	"time"
)

func TestExpLadder(t *testing.T) {
	cases := []struct {
		n    int
		want time.Duration
	}{
		{-1, 15 * time.Second},
		{0, 15 * time.Second},
		{1, 30 * time.Second},
		{2, time.Minute},
		{4, 4 * time.Minute},
		{5, 5 * time.Minute}, // capped
		{math.MaxInt, 5 * time.Minute},
	}
	for _, c := range cases {
		if got := Exp(15*time.Second, 5*time.Minute, c.n); got != c.want {
			t.Errorf("Exp(n=%d) = %v, want %v", c.n, got, c.want)
		}
	}
}

func TestJitterBounds(t *testing.T) {
	for range 1000 {
		d := Jitter(time.Second)
		if d < time.Second || d >= time.Second+time.Second/4 {
			t.Fatalf("Jitter(1s) = %v, want [1s, 1.25s)", d)
		}
	}
	if Jitter(0) != 0 {
		t.Error("Jitter(0) != 0")
	}
}
