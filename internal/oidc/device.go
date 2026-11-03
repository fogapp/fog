package oidc

import (
	"crypto/rand"
	"strings"
	"time"
)

// Device authorization (RFC 8628): sign in on a screen that has no keyboard
// worth typing on — Stream on the television — by approving a short code on
// a phone. The client is still the app server; the TV only shows the code.

const (
	DeviceGrantType  = "urn:ietf:params:oauth:grant-type:device_code"
	DeviceCodeTTL    = 10 * time.Minute
	DevicePollPeriod = 5 * time.Second
)

// userCodeAlphabet is RFC 8628 §6.1's: consonants only, no vowels (no words
// spelled by accident), no look-alikes.
const userCodeAlphabet = "BCDFGHJKLMNPQRSTVWXZ"

// NewUserCode is eight characters from the alphabet, shown as XXXX-XXXX:
// 20^8 ≈ 2.6e10 codes, against a ten-minute life and rate-limited entry.
func NewUserCode() string {
	b := make([]byte, 8)
	rand.Read(b)
	out := make([]byte, 0, 9)
	for i, c := range b {
		if i == 4 {
			out = append(out, '-')
		}
		out = append(out, userCodeAlphabet[int(c)%len(userCodeAlphabet)])
	}
	return string(out)
}

// NormalizeUserCode accepts what a person types: any case, with or without
// the dash or spaces. "" when it cannot be a code.
func NormalizeUserCode(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		if r == '-' || r == ' ' {
			continue
		}
		if !strings.ContainsRune(userCodeAlphabet, r) {
			return ""
		}
		b.WriteRune(r)
	}
	if b.Len() != 8 {
		return ""
	}
	v := b.String()
	return v[:4] + "-" + v[4:]
}

// Device-flow token errors (§3.5).
const (
	ErrAuthorizationPending Code = "authorization_pending"
	ErrSlowDown             Code = "slow_down"
	ErrExpiredToken         Code = "expired_token"
)
