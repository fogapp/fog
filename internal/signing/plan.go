// Package signing holds the keys fog signs with — gate assertions today, ID
// tokens and logout tokens with household sign-in (docs/signin-contract.md)
// — and the schedule that rotates them.
//
// The schedule is a pure planner over the key ring: Plan looks at the ring
// and the time and says what to create and what to drop, and Keyring does
// it. Everything about rotation is therefore testable at any instant
// without a clock or a database.
package signing

import "time"

const (
	// Lead is how long a key is published before it signs, so every
	// verifier that caches the key set has seen it by the time a token
	// signed with it arrives.
	Lead = 7 * 24 * time.Hour
	// Life is how long a key signs.
	Life = 90 * 24 * time.Hour
	// Tail is how long a key stays published after its last signature:
	// longer than any token fog signs lives (ID tokens 5 minutes,
	// assertions 60 seconds), with room for clock skew and caches.
	Tail = 24 * time.Hour
)

// Key is a key's schedule. Its material is the Keyring's business.
type Key struct {
	KID         string
	PublishAt   time.Time
	SignFrom    time.Time
	SignUntil   time.Time
	UnpublishAt time.Time
}

// Published reports whether verifiers should be offered the key at t.
func (k Key) Published(t time.Time) bool { return !t.Before(k.PublishAt) && t.Before(k.UnpublishAt) }

// Signs reports whether the key may sign at t.
func (k Key) Signs(t time.Time) bool { return !t.Before(k.SignFrom) && t.Before(k.SignUntil) }

// Op is what a planned action does.
type Op int

const (
	Create Op = iota + 1
	Drop
)

// Action is one step of a plan. A Create carries the schedule of the key
// to make (KID empty: the Keyring names it); a Drop names the key to delete.
type Action struct {
	Op  Op
	Key Key
}

// newKey schedules a key that signs from `from`, published from `publish`.
func newKey(publish, from time.Time) Key {
	return Key{PublishAt: publish, SignFrom: from, SignUntil: from.Add(Life), UnpublishAt: from.Add(Life).Add(Tail)}
}

// Plan returns what must change so that, at now, the ring has exactly one
// key that signs, the key after it is already published once the current
// one is within Lead of its end, and nothing past its unpublish time is
// kept.
//
// A ring with no signing key — a fresh box, or an emergency retirement —
// gets one that signs immediately: there is nobody yet holding a cached key
// set to protect, and a fog that cannot sign cannot let anyone in.
func Plan(ring []Key, now time.Time) []Action {
	var plan []Action
	var live []Key
	for _, k := range ring {
		if !now.Before(k.UnpublishAt) {
			plan = append(plan, Action{Op: Drop, Key: k})
			continue
		}
		live = append(live, k)
	}
	cur, ok := Current(live, now)
	if !ok {
		return append(plan, Action{Op: Create, Key: newKey(now, now)})
	}
	if now.Before(cur.SignUntil.Add(-Lead)) {
		return plan
	}
	for _, k := range live {
		if !k.SignFrom.Before(cur.SignUntil) {
			return plan // the successor exists
		}
	}
	return append(plan, Action{Op: Create, Key: newKey(now, cur.SignUntil)})
}

// Current is the key that signs at t: of those whose window holds t, the one
// that started last, so a successor takes over the moment its window opens.
func Current(ring []Key, t time.Time) (Key, bool) {
	var best Key
	found := false
	for _, k := range ring {
		if k.Signs(t) && (!found || k.SignFrom.After(best.SignFrom)) {
			best, found = k, true
		}
	}
	return best, found
}

// Retire ends a key now: it stops signing and stops being published, so
// every token it ever signed stops verifying. The emergency rotation —
// the next Plan replaces it with a key that signs at once.
func Retire(k Key, now time.Time) Key {
	k.SignUntil, k.UnpublishAt = now, now
	if k.SignFrom.After(now) {
		k.SignFrom = now
	}
	if k.PublishAt.After(now) {
		k.PublishAt = now
	}
	return k
}
