package filesfed

import (
	"sync"
	"time"

	"gitea.lan/matthew/fog/internal/store"
)

// Subscriptions are read on hot paths — the nightly snapshot asks
// BackupSkip about every entry it walks, every write in Files asks Guard,
// every change asks onChange — and they change rarely. So those paths read
// an in-memory snapshot. Every write to the table goes through this
// package's wrappers below, which drop the snapshot; the TTL only bounds how
// long a row changed underneath (a cascade from its peer's deletion) can
// go unseen.
const subsTTL = 30 * time.Second

type subsSnapshot struct {
	mu   sync.Mutex
	gen  uint64 // bumped on every invalidation
	subs []store.FileFedSubscription
	at   time.Time
}

// subscriptions is every subscription, from the snapshot when it is fresh.
// The slice is shared: callers read it and never write to it.
func (s *Service) subscriptions() ([]store.FileFedSubscription, error) {
	c := &s.subsSnap
	c.mu.Lock()
	if c.subs != nil && time.Since(c.at) < subsTTL {
		subs := c.subs
		c.mu.Unlock()
		return subs, nil
	}
	gen := c.gen
	c.mu.Unlock()
	subs, err := s.Store.ListFileFedSubscriptions(0)
	if err != nil {
		return nil, err
	}
	if subs == nil {
		subs = []store.FileFedSubscription{}
	}
	c.mu.Lock()
	// A write that landed while we read may not be in what we read; keep
	// it only if nothing was invalidated meanwhile.
	if c.gen == gen {
		c.subs, c.at = subs, time.Now()
	}
	c.mu.Unlock()
	return subs, nil
}

func (s *Service) invalidateSubscriptions() {
	c := &s.subsSnap
	c.mu.Lock()
	c.gen++
	c.subs = nil
	c.mu.Unlock()
}

func (s *Service) createSubscription(sub *store.FileFedSubscription) (*store.FileFedSubscription, error) {
	defer s.invalidateSubscriptions()
	return s.Store.CreateFileFedSubscription(sub)
}

func (s *Service) updateSubscription(sub *store.FileFedSubscription) error {
	defer s.invalidateSubscriptions()
	return s.Store.UpdateFileFedSubscription(sub)
}

// addSubscriptionFetched leaves the snapshot alone: it runs once per file a
// sync brings in, and nothing that reads the snapshot looks at the fetched
// counters — dropping it here sent every Guard and BackupSkip back to the
// database for the length of a sync.
func (s *Service) addSubscriptionFetched(id, n int64) error {
	return s.Store.AddFileFedSubscriptionFetched(id, n)
}

func (s *Service) deleteSubscription(id int64) error {
	defer s.invalidateSubscriptions()
	return s.Store.DeleteFileFedSubscription(id)
}
