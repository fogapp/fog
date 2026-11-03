package chat

import (
	"strconv"
	"sync"
	"time"
)

// KeyBudget bounds how many one-use KeyPackages an asker consumes. Each
// fetch of a person's key material takes one package per device; without a
// bound, anyone allowed to fetch — a paired household, a member here — could
// drain a person's devices and keep them out of rooms. Past its budget an
// asker is served the devices' last-resort packages instead, which are never
// consumed: adds still work, they just reuse a package.
type KeyBudget struct {
	mu   sync.Mutex
	seen map[string][]time.Time
}

// KeyRule is one budget: at most Limit takes per Window under Key.
type KeyRule struct {
	Key    string
	Limit  int
	Window time.Duration
}

// The budgets fog applies.
const (
	keyWindow = time.Hour
	// KeysPerPeer is what one paired household may consume an hour.
	KeysPerPeer = 120
	// KeysPerAskerPerson is what one asker (a household, or a person here)
	// may consume an hour of one person's packages.
	KeysPerAskerPerson = 12
	// KeysPerPerson is what everyone together may consume an hour of one
	// person's packages.
	KeysPerPerson = 120
)

// PeerRules are the budgets for a paired household fetching handle's keys.
func PeerRules(peerID int64, handle string) []KeyRule {
	p := "peer:" + strconv.FormatInt(peerID, 10)
	return []KeyRule{
		{Key: p, Limit: KeysPerPeer, Window: keyWindow},
		{Key: p + ":" + handle, Limit: KeysPerAskerPerson, Window: keyWindow},
		{Key: "person:" + handle, Limit: KeysPerPerson, Window: keyWindow},
	}
}

// LocalRules are the budgets for a person here fetching handle's keys.
func LocalRules(askerID int64, handle string) []KeyRule {
	return []KeyRule{
		{Key: "local:" + strconv.FormatInt(askerID, 10) + ":" + handle, Limit: KeysPerAskerPerson, Window: keyWindow},
		{Key: "person:" + handle, Limit: KeysPerPerson, Window: keyWindow},
	}
}

// Take spends one from every rule if each has one left, and reports whether
// it did; a refused take spends nothing.
func (b *KeyBudget) Take(now time.Time, rules ...KeyRule) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.seen == nil {
		b.seen = map[string][]time.Time{}
	}
	for _, r := range rules {
		kept := b.seen[r.Key][:0]
		for _, t := range b.seen[r.Key] {
			if now.Sub(t) < r.Window {
				kept = append(kept, t)
			}
		}
		b.seen[r.Key] = kept
		if len(kept) >= r.Limit {
			return false
		}
	}
	for _, r := range rules {
		b.seen[r.Key] = append(b.seen[r.Key], now)
	}
	return true
}
