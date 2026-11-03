// Package randx mints the random identifiers and secrets fog stores and
// hands out: job ids, tokens, generated passwords.
//
// crypto/rand.Read cannot fail on the Go versions fog builds with (it
// crashes the program rather than return short), so neither helper returns
// an error and callers don't carry a branch that can never run.
package randx

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"time"
)

// Hex returns n random bytes as 2n lowercase hex characters.
func Hex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Token returns a 256-bit secret as 43 characters of unpadded base64url —
// the shape of session tokens, safe in a URL or a cookie.
func Token() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// crockford is the ULID alphabet: base32 without I, L, O, U.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// ULID returns a 26-character ULID: 48 bits of millisecond timestamp
// followed by 80 bits of crypto randomness, Crockford-base32 encoded.
// Sortable by creation time, no coordination, no dependency.
func ULID() string { return ulidAt(time.Now()) }

func ulidAt(t time.Time) string {
	var b [16]byte
	ms := uint64(t.UnixMilli())
	for i := 0; i < 6; i++ {
		b[i] = byte(ms >> (40 - 8*i))
	}
	rand.Read(b[6:])
	// 128 bits → 26 chars of 5 bits, reading from the top with 2 bits of
	// leading padding (the standard ULID layout).
	out := make([]byte, 26)
	for i := range out {
		start := i*5 - 2
		var v byte
		for j := 0; j < 5; j++ {
			bit := start + j
			v <<= 1
			if bit >= 0 && b[bit/8]&(1<<(7-bit%8)) != 0 {
				v |= 1
			}
		}
		out[i] = crockford[v]
	}
	return string(out)
}
