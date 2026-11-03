package notify

import (
	"sync"
	"time"
)

// Throttle lets one thing through per key per interval: one push a day per
// alarm, one retry an hour per failing app. The persistent notice carries
// the ongoing state; the throttle only keeps a phone from buzzing on every
// tick of a loop that keeps seeing the same problem.
//
// The zero value is ready to use, so a throttle can sit in a struct that is
// built as a literal. It lives in memory: a restart lets each key through
// once more, the right side to err on.
type Throttle struct {
	mu   sync.Mutex
	last map[string]time.Time
}

// Allow reports whether key may go through at now, at most once per every,
// and if so starts its next interval. now is passed in so callers with a
// test clock keep it.
func (t *Throttle) Allow(key string, every time.Duration, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if prev, ok := t.last[key]; ok && now.Sub(prev) < every {
		return false
	}
	if t.last == nil {
		t.last = map[string]time.Time{}
	}
	t.last[key] = now
	return true
}
