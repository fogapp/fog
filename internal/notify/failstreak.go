// FailStreak is the shared shape of "a recurring check keeps failing":
// count consecutive failures per key, tell the caller to raise its
// persistent notice when the streak crosses the threshold (and, unless Once,
// again at every further multiple), and clear the notice on the success that
// ends a raised streak. The app update checker, selfupdate, the DNS keeper
// and fed's dial health all follow this pattern; the notice text stays with
// each caller — every site says what is failing in its own words.
package notify

import (
	"sync"
	"time"

	"gitea.lan/matthew/fog/internal/store"
)

type FailStreak struct {
	Store     *store.Store
	Kind      string // notice kind; CreateNotice replaces per (kind, key), so raises never stack
	Threshold int    // consecutive failures before the notice is raised
	// Window, when set, also requires the streak to have lasted this long
	// before it raises: a check that fails at the cadence of its callers,
	// not its own, would otherwise reach the count in a burst.
	Window time.Duration
	// Once raises a streak's notice a single time instead of again at every
	// further multiple of Threshold — for checks whose failures don't arrive
	// on a fixed tick, where a multiple marks no particular stretch of time.
	Once bool
	// ClearLeftover makes the first success for a key in this process
	// dismiss its notice even when no streak raised it here: the notice
	// outlives the process that raised it, and a restart forgets the streak.
	ClearLeftover bool
	// Now is time.Now unless a test says otherwise.
	Now func() time.Time

	mu      sync.Mutex
	streaks map[string]*Streak
	seen    map[string]bool // keys that have had a success in this process
}

// Streak is one key's run of consecutive failures.
type Streak struct {
	Count   int
	Since   time.Time // the first failure of the run
	LastErr string    // the latest failure's message, when FailErr gave one
	Raised  bool      // the notice has been raised for this run
}

func (f *FailStreak) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

// Fail records one consecutive failure for key (the notice's app scope; ""
// for instance-wide notices) and reports the new streak plus whether the
// caller should raise its notice now. Raised at the threshold and — unless
// Once — re-raised at every further multiple: a notice dismissed while the
// source is still down comes back instead of staying silent until the
// streak happens to reset.
func (f *FailStreak) Fail(key string) (streak int, raise bool) {
	return f.FailErr(key, nil)
}

// FailErr is Fail that also keeps err's message for State.
func (f *FailStreak) FailErr(key string, err error) (streak int, raise bool) {
	now := f.now()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.streaks == nil {
		f.streaks = map[string]*Streak{}
	}
	s := f.streaks[key]
	if s == nil {
		s = &Streak{Since: now}
		f.streaks[key] = s
	}
	s.Count++
	if err != nil {
		s.LastErr = err.Error()
	}
	if s.Count < f.Threshold || now.Sub(s.Since) < f.Window {
		return s.Count, false
	}
	switch {
	case !s.Raised:
		raise = true
	case !f.Once:
		raise = s.Count%f.Threshold == 0
	}
	s.Raised = s.Raised || raise
	return s.Count, raise
}

// Success clears key's streak — and its notice, if the streak had raised
// one (or, with ClearLeftover, if this is the key's first success since
// the process started). It reports whether a raised streak just ended, so
// the caller can say the source recovered.
func (f *FailStreak) Success(key string) (recovered bool) {
	f.mu.Lock()
	s := f.streaks[key]
	delete(f.streaks, key)
	first := !f.seen[key]
	if f.ClearLeftover {
		if f.seen == nil {
			f.seen = map[string]bool{}
		}
		f.seen[key] = true
	}
	f.mu.Unlock()
	recovered = s != nil && s.Raised
	if recovered || (f.ClearLeftover && first) {
		f.Store.DismissNoticeByKind(f.Kind, key)
	}
	return recovered
}

// Streak reports key's current consecutive-failure count.
func (f *FailStreak) Streak(key string) int {
	return f.State(key).Count
}

// State reports key's current run of failures; the zero Streak when the
// last attempt succeeded (or there has been none).
func (f *FailStreak) State(key string) Streak {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s := f.streaks[key]; s != nil {
		return *s
	}
	return Streak{}
}
