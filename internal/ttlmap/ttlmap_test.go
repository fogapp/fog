package ttlmap

import (
	"testing"
	"time"
)

// clocked is a map on a hand-moved clock.
func clocked(ttl time.Duration, max int) (*Map[string, int], *time.Time) {
	m := New[string, int](ttl, max)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }
	return m, &now
}

func TestGetTakeAndExpiry(t *testing.T) {
	m, now := clocked(time.Minute, 0)
	m.Put("a", 1)
	if v, ok := m.Get("a"); !ok || v != 1 {
		t.Fatalf("Get = %v, %v", v, ok)
	}
	if v, ok := m.Take("a"); !ok || v != 1 {
		t.Fatalf("Take = %v, %v", v, ok)
	}
	if _, ok := m.Take("a"); ok {
		t.Fatal("a handle was good twice")
	}

	m.Put("b", 2)
	*now = now.Add(time.Minute)
	if _, ok := m.Get("b"); !ok {
		t.Fatal("expired at exactly its TTL")
	}
	*now = now.Add(time.Second)
	if _, ok := m.Get("b"); ok {
		t.Fatal("outlived its TTL")
	}
	if m.Len() != 0 {
		t.Fatalf("an expired Get left %d entries", m.Len())
	}
}

// Expired entries go on a later Put without anyone asking for them, and
// a re-Put restarts a key's TTL rather than inheriting its old expiry.
func TestPutDropsWhatExpired(t *testing.T) {
	m, now := clocked(time.Minute, 0)
	m.Put("old", 1)
	m.Put("kept", 1)
	*now = now.Add(50 * time.Second)
	m.Put("kept", 2)
	*now = now.Add(20 * time.Second)
	m.Put("new", 3)
	if m.Len() != 2 {
		t.Fatalf("Len = %d, want 2 (kept, new)", m.Len())
	}
	if v, ok := m.Get("kept"); !ok || v != 2 {
		t.Fatalf("re-put key = %v, %v", v, ok)
	}
}

// A full map makes room by evicting its oldest entry, even when every
// entry was put at the same instant, and the newest is always kept.
func TestCapEvictsTheOldest(t *testing.T) {
	m, _ := clocked(time.Minute, 3)
	for _, k := range []string{"a", "b", "c", "d", "e"} {
		m.Put(k, 0)
	}
	if m.Len() != 3 {
		t.Fatalf("Len = %d, cap 3", m.Len())
	}
	for k, want := range map[string]bool{"a": false, "b": false, "c": true, "d": true, "e": true} {
		if _, ok := m.Get(k); ok != want {
			t.Errorf("%s present = %v, want %v", k, ok, want)
		}
	}
	// Replacing a held key in a full map evicts nothing.
	m.Put("c", 1)
	if _, ok := m.Get("d"); !ok || m.Len() != 3 {
		t.Fatal("re-putting a held key evicted another")
	}
}

// Taken handles leave stale queue elements behind; they must not pile up
// over a long run of put-then-take.
func TestQueueStaysBounded(t *testing.T) {
	m, _ := clocked(time.Hour, 0)
	m.Put("long-lived", 0)
	for range 10000 {
		m.Put("h", 1)
		m.Take("h")
	}
	if n := len(m.queue); n > 200 {
		t.Fatalf("queue grew to %d for %d entries", n, m.Len())
	}
	if _, ok := m.Get("long-lived"); !ok {
		t.Fatal("compaction dropped a live entry")
	}
}
