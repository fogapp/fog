package auth

import (
	"net/netip"
	"strings"
	"sync"
	"time"
)

// RateLimiter is a small fixed-window limiter keyed by a caller-chosen string:
// an address, an app, a person, a token. Fog is one household's server, so
// anything beyond "slow down" is over-design.
//
// Each key holds one counter and the start of its window, so Allow costs
// the same at the 600th request a minute as at the first. Keys whose window
// has ended are dropped in the order their windows began (an expiry
// queue), so keeping the map small costs O(1) amortised per call — never a
// walk of the whole map under the lock.
type RateLimiter struct {
	mu      sync.Mutex
	window  time.Duration
	max     int
	buckets map[string]bucket
	// expiry lists each bucket's window start in the order they began —
	// which, every window being the same length, is the order they end.
	// An entry whose bucket has since been reset or restarted is skipped.
	expiry []expiryEntry
	head   int
	// maxKeys overrides the package's maxKeys; tests set it.
	maxKeys int
}

type bucket struct {
	start time.Time
	n     int
}

type expiryEntry struct {
	key   string
	start time.Time
}

func NewRateLimiter(max int, window time.Duration) *RateLimiter {
	return &RateLimiter{window: window, max: max, buckets: map[string]bucket{}}
}

// maxKeys bounds how many keys a limiter holds at once. Its keys are short
// and caller-built (an address, an id fog checked), so this is megabytes at
// most. A key whose window is still running is never forgotten to make room:
// forgetting it would hand whoever filled the limiter a fresh budget for
// it. Past maxKeys live keys a new key is refused instead — closed to a
// stranger, never reopened for someone already being counted. Keys anyone
// can mint (addresses) belong in a StrangerLimiter, never beside keys fog
// checked, so a stranger's flood fills only strangers' room.
const maxKeys = 100_000

// Allow records an attempt and reports whether it is permitted. A refused
// attempt changes nothing: no key is made for it, and no other key is
// touched.
func (l *RateLimiter) Allow(key string) bool {
	ok, _ := l.Admit(key)
	return ok
}

// Admit is Allow, also saying whether a refusal was for want of room for a
// new key (full) rather than the key's own budget.
func (l *RateLimiter) Admit(key string) (ok, full bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	l.expire(now)
	b, exists := l.buckets[key]
	if !exists || now.Sub(b.start) >= l.window {
		if !exists && len(l.buckets) >= l.cap() {
			return false, true
		}
		b = bucket{start: now}
		if l.max > 0 {
			l.expiry = append(l.expiry, expiryEntry{key, now})
		}
	}
	if b.n >= l.max {
		return false, false
	}
	b.n++
	l.buckets[key] = b
	return true, false
}

// Reserve is Allow for a check that might succeed: it takes an attempt now,
// in the same step as seeing there is one — so a burst of parallel guesses
// can't all see room before any is charged — and Refund gives it back if the
// check passes.
func (l *RateLimiter) Reserve(key string) bool { return l.Allow(key) }

// Refund returns an attempt Reserve took, for a check that passed.
func (l *RateLimiter) Refund(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if b, ok := l.buckets[key]; ok && b.n > 0 {
		b.n--
		l.buckets[key] = b
	}
}

// expire drops the buckets whose window has ended, oldest first, stopping
// at the first still running. Caller holds l.mu.
func (l *RateLimiter) expire(now time.Time) {
	for l.head < len(l.expiry) {
		e := l.expiry[l.head]
		if now.Sub(e.start) < l.window {
			break
		}
		if b, ok := l.buckets[e.key]; ok && b.start.Equal(e.start) {
			delete(l.buckets, e.key)
		}
		l.expiry[l.head] = expiryEntry{}
		l.head++
	}
	// Reclaim the consumed front once it is most of the slice.
	if l.head > 1024 && l.head*2 > len(l.expiry) {
		l.expiry = append(l.expiry[:0:0], l.expiry[l.head:]...)
		l.head = 0
	}
}

func (l *RateLimiter) cap() int {
	if l.maxKeys > 0 {
		return l.maxKeys
	}
	return maxKeys
}

func (l *RateLimiter) has(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.buckets[key]
	return ok
}

// Len is how many keys the limiter holds.
func (l *RateLimiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// Window is the length of a key's counting window, the longest a refused
// caller waits.
func (l *RateLimiter) Window() time.Duration { return l.window }

// Reset clears attempts for a key (called on successful login).
func (l *RateLimiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.buckets, key)
}

// Exhausted reports whether key has no attempts left in its window, without
// spending one: for limits charged only on failure — a wrong code — which
// must still refuse before the next check is made, or a guesser would learn
// the answer before being refused.
func (l *RateLimiter) Exhausted(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[key]
	return ok && time.Since(b.start) < l.window && b.n >= l.max
}

// AddrKey is an address as a limiter or pin key: an IPv4 address as itself,
// an IPv6 address as its /64 (which one household or one attacker holds
// whole), brackets and IPv4 mapping aside. A link-local IPv6 address is the
// exception: every device on a LAN is in fe80::/64, so it is keyed as the
// whole address with its zone (the interface). "" for what isn't an
// address.
func AddrKey(s string) string {
	a, err := netip.ParseAddr(strings.Trim(strings.TrimSpace(s), "[]"))
	if err != nil {
		return ""
	}
	a = a.Unmap()
	if a.Is4() {
		return a.String()
	}
	if a.IsLinkLocalUnicast() {
		return a.String()
	}
	a = a.WithZone("")
	p, _ := a.Prefix(64)
	return p.String()
}

// StrangerLimiter limits callers fog knows only by their address, apart
// from every limiter keyed by something fog checked: one bucket per address
// (AddrKey), and when strangers have filled its room, one bucket they all
// share, with SharedScale times the budget. A flood of addresses fills only
// strangers' room and then their shared bucket — never an app's, a person's
// or a household address's. Known, when set, names the addresses that are
// the household's (a live session signed in from there): past the room
// they get buckets of their own in a limiter strangers never reach.
type StrangerLimiter struct {
	per, shared, household, host *RateLimiter
	Known                        func(addrKey string) bool
}

// HostShared is the key for every caller fog sees from the host's own
// address — no address of their own to tell them apart. Its bucket is
// SharedScale times an address's, in every StrangerLimiter.
const HostShared = "host-shared"

// SharedScale is how much larger the strangers' shared bucket is than one
// address's.
const SharedScale = 10

// strangersShared is the shared bucket's key.
const strangersShared = "strangers"

func NewStrangerLimiter(max int, window time.Duration) *StrangerLimiter {
	return &StrangerLimiter{
		per:       NewRateLimiter(max, window),
		shared:    NewRateLimiter(max*SharedScale, window),
		household: NewRateLimiter(max, window),
		host:      NewRateLimiter(max*SharedScale, window),
	}
}

// Take takes an attempt for addrKey and reports whether it may proceed;
// refund gives it back (for a check that passed).
func (l *StrangerLimiter) Take(addrKey string) (refund func(), ok bool) {
	if addrKey == "" {
		addrKey = "unknown"
	}
	if addrKey == HostShared {
		if l.host.Allow(addrKey) {
			return func() { l.host.Refund(addrKey) }, true
		}
		return func() {}, false
	}
	if ok, full := l.per.Admit(addrKey); ok {
		return func() { l.per.Refund(addrKey) }, true
	} else if !full {
		return func() {}, false
	}
	if l.Known != nil && l.Known(addrKey) {
		if l.household.Allow(addrKey) {
			return func() { l.household.Refund(addrKey) }, true
		}
		return func() {}, false
	}
	if l.shared.Allow(strangersShared) {
		return func() { l.shared.Refund(strangersShared) }, true
	}
	return func() {}, false
}

// Allow takes an attempt for addrKey that is not given back.
func (l *StrangerLimiter) Allow(addrKey string) bool {
	_, ok := l.Take(addrKey)
	return ok
}

// Reserve is Allow, for a caller that gives the attempt back with Refund.
func (l *StrangerLimiter) Reserve(addrKey string) bool { return l.Allow(addrKey) }

// Refund gives back an attempt Reserve took for addrKey, from whichever
// bucket it came out of.
func (l *StrangerLimiter) Refund(addrKey string) {
	if addrKey == "" {
		addrKey = "unknown"
	}
	switch {
	case addrKey == HostShared:
		l.host.Refund(addrKey)
	case l.per.has(addrKey):
		l.per.Refund(addrKey)
	case l.Known != nil && l.Known(addrKey):
		l.household.Refund(addrKey)
	default:
		l.shared.Refund(strangersShared)
	}
}

// Exhausted reports whether addrKey's own bucket has nothing left.
func (l *StrangerLimiter) Exhausted(addrKey string) bool {
	if addrKey == HostShared {
		return l.host.Exhausted(addrKey)
	}
	return l.per.Exhausted(addrKey)
}

// Len is how many addresses have buckets of their own.
func (l *StrangerLimiter) Len() int { return l.per.Len() }

// Window is the counting window.
func (l *StrangerLimiter) Window() time.Duration { return l.per.window }

// SetRoom caps how many addresses get buckets of their own (tests).
func (l *StrangerLimiter) SetRoom(n int) { l.per.maxKeys = n }
