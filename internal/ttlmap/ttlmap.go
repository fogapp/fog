// Package ttlmap is the small in-memory map fogd keeps short-lived
// handshake state in: WebAuthn challenges, device nonces, replayed
// connect answers. Each entry lives a fixed TTL from when it was put, and a
// map can be capped so whoever can reach the endpoint that fills it cannot
// grow it without bound.
//
// There used to be three of these, each sweeping its whole map on every
// Put. Here every entry lives the same TTL, so the order entries were put
// in is the order they expire in: a queue in that order lets a Put drop
// what has expired, and a full map its oldest entry, without looking at
// anything else.
package ttlmap

import (
	"sync"
	"time"
)

// Map is a TTL map, safe for concurrent use. Make one with New.
type Map[K comparable, V any] struct {
	ttl time.Duration
	max int

	mu    sync.Mutex
	items map[K]entry[V]
	// queue is every Put in order, so its front is the oldest. A Take or
	// a re-Put leaves its old element behind; such stale elements are
	// skipped when they reach the front and dropped wholesale by compact.
	queue []queued[K]
	seq   uint64 // numbers each Put, tying an entry to its own element
	now   func() time.Time
}

type entry[V any] struct {
	v       V
	expires time.Time
	seq     uint64
}

type queued[K comparable] struct {
	k       K
	expires time.Time
	seq     uint64
}

// New makes a map whose entries expire ttl after they are put. max > 0
// caps how many it holds: a Put of a new key into a full map evicts the
// oldest entry, the one nearest its expiry anyway. max <= 0 is no cap.
func New[K comparable, V any](ttl time.Duration, max int) *Map[K, V] {
	return &Map[K, V]{ttl: ttl, max: max, items: map[K]entry[V]{}, now: time.Now}
}

// Put stores v under k for the map's TTL, replacing what k held.
func (m *Map[K, V]) Put(k K, v V) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	m.expire(now)
	if _, ok := m.items[k]; !ok && m.max > 0 {
		for len(m.items) >= m.max && len(m.queue) > 0 {
			m.popFront()
		}
	}
	exp := now.Add(m.ttl)
	m.seq++
	m.items[k] = entry[V]{v: v, expires: exp, seq: m.seq}
	m.queue = append(m.queue, queued[K]{k: k, expires: exp, seq: m.seq})
	if len(m.queue) > 2*len(m.items)+64 {
		m.compact()
	}
}

// Get returns k's value while it is unexpired, leaving it in place.
func (m *Map[K, V]) Get(k K) (V, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lookup(k, false)
}

// Take returns k's value while it is unexpired and removes it either way:
// a handle is good exactly once.
func (m *Map[K, V]) Take(k K) (V, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lookup(k, true)
}

// Len is how many entries are held, counting any expired one not yet
// dropped.
func (m *Map[K, V]) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.items)
}

func (m *Map[K, V]) lookup(k K, remove bool) (V, bool) {
	e, ok := m.items[k]
	expired := ok && m.now().After(e.expires)
	if remove || expired {
		delete(m.items, k)
	}
	if !ok || expired {
		var zero V
		return zero, false
	}
	return e.v, true
}

// expire drops entries past their expiry from the front of the queue.
func (m *Map[K, V]) expire(now time.Time) {
	for len(m.queue) > 0 && now.After(m.queue[0].expires) {
		m.popFront()
	}
}

// popFront removes the queue's front element, and its entry if the
// element is still that entry's (not left behind by a Take or a re-Put).
// The queue must not be empty.
func (m *Map[K, V]) popFront() {
	q := m.queue[0]
	m.queue[0] = queued[K]{}
	m.queue = m.queue[1:]
	if e, ok := m.items[q.k]; ok && e.seq == q.seq {
		delete(m.items, q.k)
	}
}

// compact rebuilds the queue from the live entries' own elements, keeping
// its order, so stale elements cannot pile up behind a long-lived front.
func (m *Map[K, V]) compact() {
	q := make([]queued[K], 0, len(m.items))
	for _, el := range m.queue {
		if e, ok := m.items[el.k]; ok && e.seq == el.seq {
			q = append(q, el)
		}
	}
	m.queue = q
}
