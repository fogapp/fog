// Package ring is a fixed-size, concurrency-safe buffer of the newest
// values: the in-memory history fogd keeps of host samples, health probes,
// and its own log lines. Adding never allocates once the buffer is full —
// the oldest value is overwritten in place.
package ring

import "sync"

// Ring holds the newest values added to it, up to the size it was made with.
type Ring[T any] struct {
	mu    sync.Mutex
	buf   []T
	next  int // where the next value goes
	count int
}

// New makes a ring that keeps the newest size values (at least one).
func New[T any](size int) *Ring[T] {
	if size < 1 {
		size = 1
	}
	return &Ring[T]{buf: make([]T, size)}
}

// Add stores v, evicting the oldest value once the ring is full.
func (r *Ring[T]) Add(v T) {
	r.mu.Lock()
	r.buf[r.next] = v
	r.next = (r.next + 1) % len(r.buf)
	if r.count < len(r.buf) {
		r.count++
	}
	r.mu.Unlock()
}

// Recent returns up to n of the newest values, oldest first; n <= 0 means
// all of them. The slice is a copy the caller may keep.
func (r *Ring[T]) Recent(n int) []T {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n <= 0 || n > r.count {
		n = r.count
	}
	out := make([]T, 0, n)
	start := r.next - n + len(r.buf)
	for i := 0; i < n; i++ {
		out = append(out, r.buf[(start+i)%len(r.buf)])
	}
	return out
}

// All returns every retained value, oldest first.
func (r *Ring[T]) All() []T { return r.Recent(0) }

// Latest returns the newest value, if any has been added.
func (r *Ring[T]) Latest() (T, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.count == 0 {
		var zero T
		return zero, false
	}
	return r.buf[(r.next-1+len(r.buf))%len(r.buf)], true
}
