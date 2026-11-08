// Package auth provides password hashing (argon2id), opaque session tokens
// and their lifetime, and the keyed rate limiter every endpoint that needs
// slowing down shares. Passkeys live in internal/people, which owns the
// people who hold these sessions.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"
)

// argon2id parameters per OWASP recommendation (19 MiB, t=2, p=1).
// Memory and time are variables only so a test binary can lower them
// (CheapPasswordHashForTests); each hash records its own, so lowering them
// changes only what new hashes cost.
var (
	argonMemory uint32 = owaspMemory
	argonTime   uint32 = owaspTime
)

const (
	owaspMemory = 19 * 1024
	owaspTime   = 2
)

const (
	argonThreads = 1
	argonKeyLen  = 32
	argonSaltLen = 16
)

// CheapPasswordHashForTests makes hashing a new password cost ~1ms instead
// of ~140ms under -race, for the rest of this test binary. It panics
// outside a test: a real fog's hashes must cost what OWASP says. Call it
// from a test file's init.
func CheapPasswordHashForTests() {
	if !testing.Testing() {
		panic("auth.CheapPasswordHashForTests called outside a test binary")
	}
	argonMemory, argonTime = 64, 1
}

// hashSlots caps how many argon2id computations run at once: each takes
// argonMemory (19 MiB), so an unbounded burst of sign-ins could run the box
// out of memory. Beyond the cap, callers wait their turn; CheckPassword
// refuses instead once its line is full (checkLine).
var hashSlots = make(chan struct{}, 4)

// maxHashWaiters is how many may wait for a slot before CheckPassword says
// ErrBusy: its line holds cap(hashSlots)+maxHashWaiters checks in all.
const maxHashWaiters = 32

// ErrBusy is a password check refused because too many are under way.
var ErrBusy = errors.New("too many password checks at once; try again shortly")

// Caller is who a password check is for, so one caller can't hold the line
// that everyone's sign-in waits in.
type Caller struct {
	// Key is the caller's address bucket (an IPv4 address, an IPv6 /64,
	// the shared stranger bucket); "" is not counted per caller.
	Key string
	// Share is how many places in line Key may hold at once; 0 is
	// DefaultShare.
	Share int
	// Known is a check against a real account's credential. Half the line
	// is kept for these: guesses at made-up handles — which no account's
	// budget bounds — can never fill it.
	Known bool
	// Pool, when set, is a class of check with a cap of its own in the
	// line (PoolShare), so it can't crowd out sign-in.
	Pool string
	// Account, when set, is the account the check is for: it holds at most
	// one place in line, from however many addresses — so one valid
	// password looped by many workers holds one place, not the line.
	Account string
	// Wait, when set, is how long the check may wait for a place before
	// ErrBusy: a short queue, for a caller that would rather wait than be
	// refused (a share link's parallel downloads).
	Wait time.Duration
}

// PoolShare is a password-protected share link's check: at most a quarter
// of the line.
const PoolShare = "share"

func poolCap(pool string) int {
	if pool == PoolShare {
		return lineCap() / 4
	}
	return lineCap()
}

// DefaultShare is how many places in line one address holds at most.
const DefaultShare = 2

// checkLine is CheckPassword's line: every check admitted and not yet
// finished, waiting or running.
var checkLine = struct {
	sync.Mutex
	total, strangers int
	by, pools        map[string]int
	accounts         map[string]bool
}{by: map[string]int{}, pools: map[string]int{}, accounts: map[string]bool{}}

func lineCap() int { return cap(hashSlots) + maxHashWaiters }

// join takes a place in line for c, or reports there is none for it.
func join(c Caller) bool {
	l := &checkLine
	l.Lock()
	defer l.Unlock()
	share := c.Share
	if share <= 0 {
		share = DefaultShare
	}
	switch {
	case l.total >= lineCap():
		return false
	case !c.Known && l.strangers >= lineCap()/2:
		return false
	case c.Key != "" && l.by[c.Key] >= share:
		return false
	case c.Pool != "" && l.pools[c.Pool] >= poolCap(c.Pool):
		return false
	case c.Account != "" && l.accounts[c.Account]:
		return false
	}
	if c.Account != "" {
		l.accounts[c.Account] = true
	}
	l.total++
	if c.Pool != "" {
		l.pools[c.Pool]++
	}
	if !c.Known {
		l.strangers++
	}
	if c.Key != "" {
		l.by[c.Key]++
	}
	return true
}

func leave(c Caller) {
	l := &checkLine
	l.Lock()
	defer l.Unlock()
	l.total--
	if c.Account != "" {
		delete(l.accounts, c.Account)
	}
	if c.Pool != "" {
		l.pools[c.Pool]--
	}
	if !c.Known {
		l.strangers--
	}
	if c.Key != "" {
		if l.by[c.Key]--; l.by[c.Key] <= 0 {
			delete(l.by, c.Key)
		}
	}
}

func withHashSlot(fn func()) {
	hashSlots <- struct{}{}
	defer func() { <-hashSlots }()
	fn()
}

func HashPassword(password string) (string, error) {
	var out string
	var err error
	withHashSlot(func() { out, err = hashPassword(password) })
	return out, err
}

func hashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s",
		argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword checks password against an encoded hash, in the same fair
// line as every other check (CheckPasswordAs); no place in it is a "no".
// Only for a check no stranger can cause; anything reachable from the
// network says who is asking with CheckPasswordAs.
func VerifyPassword(password, encoded string) bool {
	ok, _ := CheckPassword(password, encoded)
	return ok
}

// CheckPassword is VerifyPassword for a sign-in: it waits for a slot only
// while there is room in line for it, and otherwise answers ErrBusy, for a
// 429. The check counts as one against a real account, with no per-caller
// share; CheckPasswordAs says who is asking.
func CheckPassword(password, encoded string) (bool, error) {
	return CheckPasswordAs(password, encoded, Caller{Known: true})
}

// CheckPasswordAs is CheckPassword with a fair line: each caller holds at
// most its share of places, and made-up handles at most half of them.
func CheckPasswordAs(password, encoded string, c Caller) (bool, error) {
	deadline := time.Now().Add(c.Wait)
	for !join(c) {
		if !time.Now().Before(deadline) {
			return false, ErrBusy
		}
		time.Sleep(25 * time.Millisecond)
	}
	defer leave(c)
	var ok bool
	withHashSlot(func() { ok = verifyPassword(password, encoded) })
	return ok, nil
}

func verifyPassword(password, encoded string) bool {
	parts := strings.Split(encoded, "$")
	// ["", "argon2id", "v=19", "m=...,t=...,p=...", salt, key]
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var m uint32
	var t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

var ErrWeakPassword = errors.New("password must be at least 10 characters")

func ValidatePassword(password string) error {
	if len(password) < 10 {
		return ErrWeakPassword
	}
	return nil
}

// HoldLine takes n places in the hashing line as c would, and returns their
// release: for tests that need the line full.
func HoldLine(c Caller, n int) (release func(), ok bool) {
	took := 0
	release = func() {
		for range took {
			leave(c)
		}
	}
	for range n {
		if !join(c) {
			return release, false
		}
		took++
	}
	return release, true
}
