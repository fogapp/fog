package auth

import (
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPasswordRoundTrip(t *testing.T) {
	h, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyPassword("correct horse battery", h) {
		t.Fatal("valid password rejected")
	}
	if VerifyPassword("wrong", h) {
		t.Fatal("invalid password accepted")
	}
	if VerifyPassword("correct horse battery", "garbage") {
		t.Fatal("garbage hash accepted")
	}
}

func TestTokens(t *testing.T) {
	tok, digest := NewToken()
	if tok == "" || digest == "" || tok == digest {
		t.Fatal("bad token/digest")
	}
	if DigestToken(tok) != digest {
		t.Fatal("digest mismatch")
	}
}

func TestRateLimiter(t *testing.T) {
	l := NewRateLimiter(3, time.Minute)
	for i := 0; i < 3; i++ {
		if !l.Allow("ip") {
			t.Fatalf("attempt %d should be allowed", i)
		}
	}
	if l.Allow("ip") {
		t.Fatal("4th attempt should be blocked")
	}
	if !l.Allow("other-ip") {
		t.Fatal("other key should be unaffected")
	}
	l.Reset("ip")
	if !l.Allow("ip") {
		t.Fatal("reset should clear attempts")
	}
}

func TestRateLimiterWindowExpiry(t *testing.T) {
	l := NewRateLimiter(2, 50*time.Millisecond)
	l.Allow("ip")
	l.Allow("ip")
	if l.Allow("ip") {
		t.Fatal("3rd attempt inside the window should be blocked")
	}
	time.Sleep(60 * time.Millisecond)
	if !l.Allow("ip") {
		t.Fatal("attempts outside the window should not count")
	}
}

// Only Reset removes a key, and Reset fires solely on successful login — so
// without the sweep a tunnel-exposed fogd keeps one map entry per attacker
// IP for the life of the process.
func TestRateLimiterSweepsAbandonedKeys(t *testing.T) {
	l := NewRateLimiter(3, 50*time.Millisecond)
	l.Allow("attacker-1")
	l.Allow("attacker-2")
	l.mu.Lock()
	n := len(l.buckets)
	l.mu.Unlock()
	if n != 2 {
		t.Fatalf("got %d tracked keys, want 2", n)
	}

	time.Sleep(60 * time.Millisecond)
	// The next Allow (any key) sweeps everything whose window has passed.
	if !l.Allow("visitor") {
		t.Fatal("fresh key should be allowed")
	}
	l.mu.Lock()
	_, a1 := l.buckets["attacker-1"]
	_, a2 := l.buckets["attacker-2"]
	n = len(l.buckets)
	l.mu.Unlock()
	if a1 || a2 {
		t.Errorf("abandoned keys survived the sweep (attacker-1=%v attacker-2=%v)", a1, a2)
	}
	if n != 1 {
		t.Errorf("got %d tracked keys after sweep, want 1 (just visitor)", n)
	}
}

// Refused attempts don't count: a caller hammering through a refusal is
// let back in when the window that refused it ends, not pushed further out.
func TestRateLimiterRefusalsDontExtendTheWindow(t *testing.T) {
	l := NewRateLimiter(1, 50*time.Millisecond)
	if !l.Allow("ip") {
		t.Fatal("first attempt refused")
	}
	for range 100 {
		if l.Allow("ip") {
			t.Fatal("attempt inside the window allowed")
		}
	}
	time.Sleep(60 * time.Millisecond)
	if !l.Allow("ip") {
		t.Fatal("refusals extended the window")
	}
}

func TestExhaustedSpendsNothing(t *testing.T) {
	l := NewRateLimiter(2, time.Minute)
	if l.Exhausted("k") {
		t.Fatal("a fresh key is exhausted")
	}
	l.Allow("k")
	if l.Exhausted("k") {
		t.Fatal("exhausted after one of two")
	}
	l.Allow("k")
	if !l.Exhausted("k") {
		t.Fatal("not exhausted after two of two")
	}
	l.Reset("k")
	if l.Exhausted("k") || !l.Allow("k") {
		t.Fatal("a reset key is still exhausted")
	}
}

// TestHostAllowedRefusesARebindingName: a page whose DNS name was rebound to
// this box's address still sends its own name as Host.
func TestHostAllowedRefusesARebindingName(t *testing.T) {
	for _, h := range []string{"localhost:8080", "127.0.0.1", "[::1]:8080", "fog.home.test", "sab.home.test:443"} {
		if !HostAllowed(h, "fog.home.test", "*.home.test") {
			t.Errorf("%s refused", h)
		}
	}
	for _, h := range []string{"evil.example:8080", "home.test.evil.example", "", "8.8.8.8", "fog.home.test.evil.example"} {
		if HostAllowed(h, "fog.home.test", "*.home.test") {
			t.Errorf("%s allowed", h)
		}
	}
}

// TestReserveIsOneStep: parallel attempts can't all see room before any is
// charged — exactly max succeed, however many race.
func TestReserveIsOneStep(t *testing.T) {
	l := NewRateLimiter(10, time.Minute)
	var wg sync.WaitGroup
	var got atomic.Int32
	for range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if l.Reserve("k") {
				got.Add(1)
			}
		}()
	}
	wg.Wait()
	if got.Load() != 10 {
		t.Fatalf("%d reservations against a budget of 10", got.Load())
	}
	l.Refund("k")
	if !l.Reserve("k") || l.Reserve("k") {
		t.Fatal("a refund didn't give back exactly one attempt")
	}
}

// TestLimiterKeysAreBounded: however many keys arrive, the limiter holds at
// most maxKeys.
func TestLimiterKeysAreBounded(t *testing.T) {
	l := NewRateLimiter(1, time.Hour)
	for i := range maxKeys + 500 {
		l.Allow(fmt.Sprintf("k%d", i))
	}
	if n := l.Len(); n > maxKeys {
		t.Fatalf("%d keys held, cap %d", n, maxKeys)
	}
}

// The attack: a victim's bucket is spent, then a flood of new keys fills
// the limiter, hoping the victim's is forgotten to make room and its budget
// starts over. A live bucket is never evicted; a full limiter refuses the
// new key instead, and a refusal makes no key at all.
func TestAFloodNeverResetsALiveBucket(t *testing.T) {
	l := NewRateLimiter(3, time.Hour)
	l.maxKeys = 50
	for range 3 {
		l.Allow("victim")
	}
	if l.Allow("victim") {
		t.Fatal("the victim's budget is not spent")
	}
	refused := 0
	for i := range 500 {
		if !l.Allow(fmt.Sprintf("made-up-%d", i)) {
			refused++
		}
	}
	if l.Allow("victim") || !l.Exhausted("victim") {
		t.Fatal("the flood gave the victim's bucket a fresh budget")
	}
	if n := l.Len(); n > 50 {
		t.Fatalf("%d keys held, cap 50", n)
	}
	if refused != 500-49 {
		t.Fatalf("%d new keys refused, want %d (fail closed once full)", refused, 500-49)
	}
	// A refusal leaves nothing behind.
	full := NewRateLimiter(0, time.Hour)
	full.Allow("x")
	if full.Len() != 0 {
		t.Fatal("a refused attempt made a key")
	}
}

// Keys whose window has ended are the only ones swept to make room.
func TestAFullLimiterMakesRoomFromEndedWindows(t *testing.T) {
	l := NewRateLimiter(1, 20*time.Millisecond)
	l.maxKeys = 10
	for i := range 10 {
		l.Allow(fmt.Sprintf("old-%d", i))
	}
	if l.Allow("new") {
		t.Fatal("a full limiter of live keys took a new one")
	}
	time.Sleep(30 * time.Millisecond)
	if !l.Allow("new") {
		t.Fatal("ended windows were not swept to make room")
	}
}

// TestPasswordChecksAreCapped: at most cap(hashSlots) argon2 runs at once,
// and past the line CheckPassword refuses instead of piling on.
func TestPasswordChecksAreCapped(t *testing.T) {
	h, _ := HashPassword("correct horse battery")
	held := fillLine(t, lineCap(), func(i int) Caller { return Caller{Known: true} })
	_, err := CheckPassword("x", h)
	held()
	if err != ErrBusy {
		t.Fatalf("a check past the line: %v, want ErrBusy", err)
	}
	if ok, err := CheckPassword("correct horse battery", h); !ok || err != nil {
		t.Fatalf("a check with room: %v %v", ok, err)
	}
}

// fillLine takes n places in line as caller(i) would, and returns their
// release.
func fillLine(t *testing.T, n int, caller func(int) Caller) func() {
	t.Helper()
	var took []Caller
	for i := range n {
		c := caller(i)
		if !join(c) {
			t.Fatalf("place %d of %d refused", i, n)
		}
		took = append(took, c)
	}
	return func() {
		for _, c := range took {
			leave(c)
		}
	}
}

// The attack: one address, or a crowd guessing made-up handles, holds the
// line full so the household's own sign-ins answer "busy". One address
// holds only its share; made-up handles hold at most half the line; a
// check against a real account from another address still gets in.
func TestTheHashingLineIsFair(t *testing.T) {
	h, _ := HashPassword("correct horse battery")
	attacker := Caller{Key: "203.0.113.9", Known: true}
	for range DefaultShare {
		if !join(attacker) {
			t.Fatal("the attacker's own share refused")
		}
	}
	if join(attacker) {
		t.Fatal("one address took more than its share")
	}
	for range DefaultShare {
		leave(attacker)
	}
	release := fillLine(t, lineCap()/2, func(i int) Caller {
		return Caller{Key: "2001:db8:" + strconv.Itoa(i) + "::/64"}
	})
	defer release()
	if join(Caller{Key: "2001:db8:ffff::/64"}) {
		t.Fatal("made-up handles took more than half the line")
	}
	ok, err := CheckPasswordAs("correct horse battery", h, Caller{Key: "192.168.1.20", Known: true})
	if !ok || err != nil {
		t.Fatalf("the household's sign-in behind a full stranger half: %v %v", ok, err)
	}
}

// The attack (fifth review, 2): strangers' addresses filled a limiter that
// apps and the household shared, and every new key — an app's, a member's
// — was refused. A StrangerLimiter holds addresses only; past its room
// strangers share one bucket, and a household address gets its own.
func TestStrangersFillOnlyStrangersRoom(t *testing.T) {
	l := NewStrangerLimiter(2, time.Hour)
	l.SetRoom(10)
	home := AddrKey("192.168.1.20")
	l.Known = func(k string) bool { return k == home }
	for i := range 10 {
		if !l.Allow(AddrKey(fmt.Sprintf("203.0.113.%d", i))) {
			t.Fatal("room refused a stranger")
		}
	}
	// Full: the next strangers share one bucket, then are refused ...
	shared := 0
	for i := range 100 {
		if l.Allow(AddrKey(fmt.Sprintf("2001:db8:%x::1", i))) {
			shared++
		}
	}
	if shared != 2*SharedScale {
		t.Fatalf("%d strangers through the shared bucket, want %d", shared, 2*SharedScale)
	}
	// ... while the household's address still has its own budget, and an
	// address already counted keeps its own.
	if !l.Allow(home) || !l.Allow(home) || l.Allow(home) {
		t.Fatal("the household's address did not keep a budget of its own")
	}
	if !l.Allow(AddrKey("203.0.113.0")) || l.Allow(AddrKey("203.0.113.0")) {
		t.Fatal("an address already counted lost its bucket")
	}
	// And a validated key's limiter is another limiter altogether.
	apps := NewRateLimiter(5, time.Hour)
	if !apps.Allow("app:lucid") {
		t.Fatal("an app was refused by strangers")
	}
}

// An IPv6 address is its /64 in every limiter.
func TestAddrKeyIsThe64(t *testing.T) {
	if AddrKey("2001:db8:1:2:aaaa::1") != AddrKey("[2001:db8:1:2:bbbb::9%eth0]") || AddrKey("::ffff:192.0.2.1") != "192.0.2.1" || AddrKey("nope") != "" {
		t.Fatal("AddrKey")
	}
}

// Refusals from a full limiter cost O(1): expiry is a queue, not a walk.
func TestAFullLimiterRefusesCheaply(t *testing.T) {
	l := NewRateLimiter(1, time.Hour)
	for i := range maxKeys {
		l.Allow(fmt.Sprintf("k%d", i))
	}
	start := time.Now()
	for i := range 200_000 {
		l.Allow(fmt.Sprintf("new%d", i))
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("200k refusals of a full limiter took %v", d)
	}
}

// The attack (fifth review, 3): password-protected share links verified
// outside the fair line, so guesses at a share held the hashing slots the
// household's sign-ins wait for. Now they queue in the same line, in a pool
// capped at a quarter of it.
func TestShareChecksCantCrowdOutSignIn(t *testing.T) {
	h, _ := HashPassword("correct horse battery")
	release, ok := HoldLine(Caller{Pool: PoolShare}, lineCap()/4)
	defer release()
	if !ok {
		t.Fatal("the share pool's own quarter refused")
	}
	if _, err := CheckPasswordAs("x", h, Caller{Key: "198.51.100.1", Pool: PoolShare}); err != ErrBusy {
		t.Fatalf("a share check past its pool: %v", err)
	}
	if ok, err := CheckPasswordAs("correct horse battery", h, Caller{Key: "192.168.1.20", Known: true}); !ok || err != nil {
		t.Fatalf("sign-in behind a full share pool: %v %v", ok, err)
	}
	if !VerifyPassword("correct horse battery", h) {
		t.Fatal("VerifyPassword with room in the line")
	}
}

// The attack (sixth review, 1): 36 workers looping one guest's correct
// password held the whole line, and the operator's sign-in was refused.
// One account holds one place, however many addresses ask.
func TestOneAccountHoldsOnePlace(t *testing.T) {
	h, _ := HashPassword("correct horse battery")
	release, ok := HoldLine(Caller{Key: "203.0.113.1", Known: true, Account: "guest"}, 1)
	defer release()
	if !ok {
		t.Fatal("the guest's one place")
	}
	if _, err := CheckPasswordAs("correct horse battery", h, Caller{Key: "203.0.113.2", Known: true, Account: "guest"}); err != ErrBusy {
		t.Fatalf("a second check for the same account from another address: %v", err)
	}
	if ok, err := CheckPasswordAs("correct horse battery", h, Caller{Key: "192.168.1.20", Known: true, Account: "admin"}); !ok || err != nil {
		t.Fatalf("the operator beside a looping guest: %v %v", ok, err)
	}
}

// A check that may wait (a share link's parallel downloads) gets its place
// once one frees, instead of an immediate ErrBusy.
func TestACheckMayWaitBriefly(t *testing.T) {
	h, _ := HashPassword("correct horse battery")
	release, _ := HoldLine(Caller{Pool: PoolShare}, lineCap()/4)
	go func() { time.Sleep(100 * time.Millisecond); release() }()
	if ok, err := CheckPasswordAs("correct horse battery", h, Caller{Key: "192.0.2.9", Pool: PoolShare, Wait: 2 * time.Second}); !ok || err != nil {
		t.Fatalf("a waiting check: %v %v", ok, err)
	}
}

// The host's own address stands for everyone without an address of their
// own: every StrangerLimiter gives it ten times an address's budget.
func TestTheHostsAddressGetsTheSharedScale(t *testing.T) {
	l := NewStrangerLimiter(3, time.Hour)
	n := 0
	for l.Allow(HostShared) {
		n++
	}
	if n != 3*SharedScale {
		t.Fatalf("the host's address got %d, want %d", n, 3*SharedScale)
	}
	if !l.Exhausted(HostShared) || l.Exhausted("192.0.2.1") {
		t.Fatal("Exhausted")
	}
}
