// Package backoff holds the one doubling ladder fog's retries climb: a
// failed peer delivery, a federation sync, an object store request, a DNS
// record that couldn't be written yet.
package backoff

import (
	"math/rand/v2"
	"time"
)

// Exp is the wait before retry n (n=0 for the first retry): base doubled n
// times, never more than max. A large n saturates at max, never overflows.
func Exp(base, max time.Duration, n int) time.Duration {
	if n < 0 {
		n = 0
	}
	d := base
	for i := 0; i < n && d < max; i++ {
		d *= 2
	}
	return min(d, max)
}

// Jitter adds up to a quarter of d at random, so many clients that failed
// together don't all retry in the same instant.
func Jitter(d time.Duration) time.Duration {
	if d < 4 {
		return d
	}
	return d + rand.N(d/4)
}
