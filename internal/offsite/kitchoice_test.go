package offsite

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/scrypt"
	"golang.org/x/time/rate"

	"gitea.lan/matthew/fog/internal/fsx"
	"gitea.lan/matthew/fog/internal/objstore"
	"gitea.lan/matthew/fog/internal/objstore/fakes3"
	"gitea.lan/matthew/fog/internal/store"
)

const kitPass = "passphrase 12345"

func digestOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func putKit(t *testing.T, r *Reader, stamp string, sealed []byte) string {
	t.Helper()
	name := "fog-recovery-kit-" + stamp + ".enc"
	if _, err := r.Client.PutBytes(context.Background(), r.Layout.Kit(name), sealed, objstore.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	return name
}

// plantIndex seals ix into the locker as the index at stamp.
func plantIndex(t *testing.T, r *Reader, stamp string, ix Index) string {
	t.Helper()
	ix.Format = Format
	ix.LockerID = r.Layout.ID
	raw, _ := json.Marshal(ix)
	key := r.Layout.Index(stamp)
	plant(t, r, key, raw)
	return key
}

// legacyKit seals plain the way an older fog did (FOGKIT1, scrypt).
func legacyKit(t *testing.T, plain []byte, pass string) []byte {
	t.Helper()
	salt := make([]byte, 16)
	rand.Read(salt)
	key, err := scrypt.Key([]byte(pass), salt, 1<<15, 8, 1, 32)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	nonce := make([]byte, gcm.NonceSize())
	rand.Read(nonce)
	out := append([]byte("FOGKIT1\x00"), salt...)
	out = append(out, nonce...)
	return append(out, gcm.Seal(nil, nonce, plain, []byte("FOGKIT1\x00"))...)
}

// A bucket writer plants decoys to use up the recovery's kit attempts:
// names dated years ahead, objects too small to be kits, objects that do
// not start like one. None costs a decryption, so however many there are
// the real kit still opens. (Before, the first ten of anything sorting
// newest exhausted maxKitAttempts and recovery failed.)
func TestJunkKitsCannotExhaustAttempts(t *testing.T) {
	t.Parallel()

	r, _, _ := testBucket(t)
	ctx := context.Background()
	real := putKit(t, r, "20260827-010000", realKitBytes(t, "20260827-010000", kitPass))
	for i := 0; i < 30; i++ {
		putKit(t, r, fmt.Sprintf("2099%04d-000000", 1000+i), []byte("FOGKIT2\x00"+strings.Repeat("x", 100)))
		putKit(t, r, fmt.Sprintf("20260828-%06d", i), []byte("not a kit, but long enough to be fetched and looked at"))
		putKit(t, r, fmt.Sprintf("20260829-%06d", i), []byte("tiny"))
	}
	kit, err := OpenNewestKit(ctx, r, kitPass)
	if err != nil {
		t.Fatalf("planted junk blocked recovery: %v", err)
	}
	if kit.Name != real || !kit.FellBack {
		t.Fatalf("chose %s (fellBack %v), want %s and a fall-back flagged", kit.Name, kit.FellBack, real)
	}
	if len(kit.Skipped) != 90 {
		t.Fatalf("every decoy must be reported: %d", len(kit.Skipped))
	}
}

// Decoys that do look like kits — the right magic, a plausible size, dated
// in the past and newer than the real one — would each cost a decryption.
// The locker's sealed index records the kits fog uploaded, with their
// digests, and those are tried first: the decoys are never decrypted, and
// a recorded kit whose bytes were swapped is refused.
func TestRecordedKitsComeFirst(t *testing.T) {
	r, _, _ := testBucket(t)
	ctx := context.Background()
	sealed := realKitBytes(t, "20260827-010000", kitPass)
	real := putKit(t, r, "20260827-010000", sealed)
	for i := 0; i < 12; i++ {
		junk := make([]byte, 4096)
		rand.Read(junk)
		putKit(t, r, fmt.Sprintf("20260901-%06d", i), append([]byte("FOGKIT2\x00"), junk...))
	}
	rec := IndexKit{Name: real, Stamp: "20260827-010000", SHA256: digestOf(sealed)}
	plantIndex(t, r, "20260827-020000", Index{Kits: []IndexKit{rec}, LatestKit: &rec})

	start := time.Now()
	kit, err := OpenNewestKit(ctx, r, kitPass)
	if err != nil {
		t.Fatalf("kit-shaped decoys blocked recovery: %v", err)
	}
	if kit.Name != real || !kit.Recorded {
		t.Fatalf("chose %s (recorded %v)", kit.Name, kit.Recorded)
	}
	if d := time.Since(start); d > 20*time.Second {
		t.Fatalf("decoys were decrypted before the recorded kit: %s", d)
	}

	// The recorded kit's bytes replaced by another (valid) kit: refused.
	putKit(t, r, "20260827-010000", realKitBytes(t, "20260827-010000", kitPass))
	if _, err := OpenNewestKit(ctx, r, kitPass); err == nil || !strings.Contains(err.Error(), "recorded") {
		t.Fatalf("a recorded kit with other bytes must be refused: %v", err)
	}
}

// An old legacy kit (which cannot seal its date) renamed to sort newest
// used to be what recovery chose, hiding every current kit. Every current
// kit is now tried before any legacy one.
func TestRenamedLegacyKitCannotHideCurrentKits(t *testing.T) {
	t.Parallel()

	r, _, _ := testBucket(t)
	ctx := context.Background()
	current := putKit(t, r, "20260827-010000", realKitBytes(t, "20260827-010000", kitPass))
	putKit(t, r, "20260930-010000", legacyKit(t, []byte("an old kit's tar.gz"), kitPass))
	kit, err := OpenNewestKit(ctx, r, kitPass)
	if err != nil {
		t.Fatal(err)
	}
	if kit.Name != current || kit.Check.Legacy || !kit.FellBack {
		t.Fatalf("chose %s (legacy %v, fellBack %v), want the current kit", kit.Name, kit.Check.Legacy, kit.FellBack)
	}
	// With no current kit that opens, the legacy one is still offered.
	r2, _, _ := testBucket(t)
	putKit(t, r2, "20260930-010000", legacyKit(t, []byte("an old kit's tar.gz"), kitPass))
	if kit, err := OpenNewestKit(ctx, r2, kitPass); err != nil || !kit.Check.Legacy {
		t.Fatalf("a lone legacy kit: %+v %v", kit, err)
	}
}

// A restore opens exactly the kit the preview showed: the name and digest.
// Swapping that kit's bytes after the preview, or adding a newer one, does
// not change what is restored.
func TestPinnedKitOpensOnlyThatKit(t *testing.T) {
	t.Parallel()

	r, _, _ := testBucket(t)
	ctx := context.Background()
	sealed := realKitBytes(t, "20260827-010000", kitPass)
	name := putKit(t, r, "20260827-010000", sealed)
	pin := &KitPin{Name: name, SHA256: digestOf(sealed)}
	putKit(t, r, "20260828-010000", realKitBytes(t, "20260828-010000", kitPass)) // newer, added after the preview
	kit, err := OpenKit(ctx, r, kitPass, pin, time.Now())
	if err != nil || kit.Name != name {
		t.Fatalf("pinned: %v %v", kit, err)
	}
	putKit(t, r, "20260827-010000", realKitBytes(t, "20260827-010000", kitPass))
	if _, err := OpenKit(ctx, r, kitPass, pin, time.Now()); err == nil {
		t.Fatal("a pinned kit whose bytes changed after the preview was restored")
	}
}

// The index fog wrote last is read first; a newer junk index planted in
// the bucket does not displace it, and deleting it is noticed (FellBack).
// Objects too small to be sealed cost nothing; real tries are capped.
func TestIndexPrefersRecordedAndCapsTries(t *testing.T) {
	t.Parallel()

	r, client, _ := testBucket(t)
	ctx := context.Background()
	older := plantIndex(t, r, "20260826-010000", Index{WrittenAt: "older"})
	mine := plantIndex(t, r, "20260827-010000", Index{WrittenAt: "mine"})
	for i := 0; i < 20; i++ {
		client.PutBytes(ctx, r.Layout.Index(fmt.Sprintf("20260828-%06d", i)), []byte(strings.Repeat("j", 64)), objstore.PutOptions{})
		client.PutBytes(ctx, r.Layout.Index(fmt.Sprintf("20260829-%06d", i)), []byte("j"), objstore.PutOptions{})
	}
	r.LatestIndex = mine
	ix, choice, err := r.IndexInfo(ctx)
	if err != nil || ix.WrittenAt != "mine" || choice.FellBack {
		t.Fatalf("the recorded index must win: %+v %+v %v", ix, choice, err)
	}
	client.Delete(ctx, mine)
	ix, choice, err = r.IndexInfo(ctx)
	if err == nil {
		t.Fatalf("20 junk indexes newer than the survivor must exhaust the capped tries, got %s", ix.WrittenAt)
	}
	// With fewer junk objects than the cap, the older survivor is found and
	// the fall-back flagged.
	for i := 0; i < 20; i++ {
		client.Delete(ctx, r.Layout.Index(fmt.Sprintf("20260828-%06d", i)))
	}
	ix, choice, err = r.IndexInfo(ctx)
	if err != nil || ix.WrittenAt != "older" || !choice.FellBack || choice.Key != older {
		t.Fatalf("fall back to the older index, flagged: %+v %+v %v", ix, choice, err)
	}
}

// Verify opens the kit a recovery would choose and compares it with the
// newest kit fog recorded uploading. Deleting that kit from the bucket —
// so a recovery would quietly restore an older one — raises an alert.
func TestVerifyCatchesKitRollback(t *testing.T) {
	t.Parallel()

	h := newHarness(t, fakes3.New("fog-test"))
	ctx := context.Background()
	target, _, err := h.m.AddTarget(ctx, h.spec("cloud"), "key-1", "secret")
	if err != nil {
		t.Fatal(err)
	}
	r, err := h.m.Reader(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	pass, _ := h.m.passphrase()
	for _, stamp := range []string{"20260826-010000", "20260827-010000"} {
		sealed := realKitBytes(t, stamp, pass)
		name := putKit(t, r, stamp, sealed)
		h.st.UpsertOffsiteCopy(store.OffsiteCopy{TargetID: target.ID, Kind: store.OffsiteKindKit, Subject: name, Key: r.Layout.Kit(name), State: store.OffsiteStored})
		h.m.recordLatestKit(target, name, digestOf(sealed))
	}
	if err := h.m.recheckKits(ctx, target); err != nil {
		t.Fatalf("intact kits must pass: %v", err)
	}
	r.Client.Delete(ctx, r.Layout.Kit("fog-recovery-kit-20260827-010000.enc"))
	err = h.m.recheckKits(ctx, target)
	if err == nil || !strings.Contains(err.Error(), "roll this machine back") {
		t.Fatalf("a recovery that would restore an older kit must be flagged: %v", err)
	}
}

func init() {
	// Tests decrypt many kits; pacing is for the recovery door, not them.
	kitDecrypts = rate.NewLimiter(rate.Inf, 1)
}

// Junk planted under index/ — names that sort after fog's, well-formed or
// not — used to push fog's own indexes out of the few it keeps, so every
// index fog wrote was deleted as it wrote the next and recovery found none.
// Fog now deletes only its own older indexes, keeps the one it just wrote,
// and says the junk is there.
func TestJunkIndexObjectsCannotMakeFogDeleteItsIndexes(t *testing.T) {
	t.Parallel()

	h := newHarness(t, fakes3.New("fog-test"))
	ctx := context.Background()
	target, _, err := h.m.AddTarget(ctx, h.spec("cloud"), "key-1", "secret")
	if err != nil {
		t.Fatal(err)
	}
	r, err := h.m.Reader(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	for _, junk := range []string{"zz1", "zz2", "zz3", "20991231-000001.json", "20991231-000002.json", "20991231-000003.json"} {
		r.Client.PutBytes(ctx, r.Layout.IndexPrefix()+junk, []byte(strings.Repeat("j", 100)), objstore.PutOptions{})
	}
	clock := time.Date(2026, 8, 27, 1, 0, 0, 0, time.UTC)
	h.m.Now = func() time.Time { return clock }
	var keys []string
	for i := 0; i < 5; i++ {
		clock = clock.Add(time.Hour)
		if err := h.m.writeIndex(ctx, target, r, nil); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, r.Layout.Index(fsx.Stamp(clock)))
	}
	r2, _ := h.m.Reader(ctx, target)
	ix, choice, err := r2.IndexInfo(ctx)
	if err != nil || choice.Key != keys[4] {
		t.Fatalf("fog's newest index must survive the junk: %v %+v", err, choice)
	}
	var left []string
	r.Client.List(ctx, r.Layout.IndexPrefix(), func(o objstore.Object) error { left = append(left, o.Key); return nil })
	for _, k := range keys[2:] {
		if !slices.Contains(left, k) {
			t.Fatalf("fog deleted its own recent index %s: %v", k, left)
		}
	}
	_ = ix
	if activeNoticeKind(t, h, NoticeIndexJunk) == "" {
		t.Fatal("junk under index/ must raise a notice")
	}

	// With every index gone, recovery reads the listing instead.
	for _, k := range left {
		r.Client.Delete(ctx, k)
	}
	if _, err := r.IndexFromListing(ctx); err != nil {
		t.Fatalf("listing fallback: %v", err)
	}
}

func activeNoticeKind(t *testing.T, h *harness, kind string) string {
	t.Helper()
	ns, err := h.st.ActiveNotices()
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range ns {
		if n.Kind == kind {
			return n.Body
		}
	}
	return ""
}

// The index fog recorded writing last is a floor, not a ceiling: on a
// machine restored from a kit the record is the night before the kit, and
// a newer valid index (the night after) is preferred, without a fall-back
// reported. Older than the record is a fall-back.
func TestRecordedIndexIsAFloor(t *testing.T) {
	t.Parallel()

	r, _, _ := testBucket(t)
	ctx := context.Background()
	recorded := plantIndex(t, r, "20260827-010000", Index{WrittenAt: "recorded"})
	plantIndex(t, r, "20260828-010000", Index{WrittenAt: "newer"})
	r.LatestIndex = recorded
	ix, choice, err := r.IndexInfo(ctx)
	if err != nil || ix.WrittenAt != "newer" || choice.FellBack {
		t.Fatalf("a newer valid index must be preferred: %+v %+v %v", ix, choice, err)
	}
}

// Kit-shaped decoys planted newer than the real kit, with no index to
// vouch for it: the oldest unrecorded kit is tried within the first
// decryptions, so the real one is reached however many decoys there are.
func TestOldestUnrecordedKitIsReached(t *testing.T) {
	t.Parallel()

	r, _, _ := testBucket(t)
	ctx := context.Background()
	real := putKit(t, r, "20260801-010000", realKitBytes(t, "20260801-010000", kitPass))
	for i := 0; i < 15; i++ {
		junk := make([]byte, 4096)
		rand.Read(junk)
		putKit(t, r, fmt.Sprintf("20260901-%06d", i), append([]byte("FOGKIT2\x00"), junk...))
	}
	kit, err := OpenNewestKit(ctx, r, kitPass)
	if err != nil || kit.Name != real || !kit.FellBack {
		t.Fatalf("the oldest kit must still be reached: %+v %v", kit, err)
	}
}

// A fresh machine whose clock is far off judges kit names against when the
// bucket received each object, not its own clock: a real kit opens with
// the clock years behind, and a decoy named for the future is refused with
// the clock years ahead.
func TestKitDatesJudgedByTheBucketsClock(t *testing.T) {
	t.Parallel()

	r, _, _ := testBucket(t)
	ctx := context.Background()
	stamp := fsx.Stamp(time.Now().Add(-time.Hour))
	real := putKit(t, r, stamp, realKitBytes(t, stamp, kitPass))
	putKit(t, r, "21000101-000000", realKitBytes(t, "21000101-000000", kitPass))
	if kit, err := OpenKit(ctx, r, kitPass, nil, time.Now().AddDate(-5, 0, 0)); err != nil || kit.Name != real {
		t.Fatalf("clock behind: %+v %v", kit, err)
	}
	kit, err := OpenKit(ctx, r, kitPass, nil, time.Now().AddDate(100, 0, 0))
	if err != nil || kit.Name != real || len(kit.Skipped) != 1 || !strings.Contains(kit.Skipped[0].Reason, "after the bucket received it") {
		t.Fatalf("clock ahead: %+v %v", kit, err)
	}
}

// Ten recovery requests at once each fetched a kit and ran a 256 MiB key
// derivation side by side — gigabytes, before setup even exists. Opening a
// kit (fetching it included) now takes the process's one heavy slot, so at
// most one kit is ever being opened.
func TestConcurrentRecoveriesOpenOneKitAtATime(t *testing.T) {
	r, _, _ := testBucket(t)
	ctx := context.Background()
	putKit(t, r, "20260827-010000", realKitBytes(t, "20260827-010000", kitPass))
	var mu sync.Mutex
	active, peak := 0, 0
	heavyHook = func(d int) {
		mu.Lock()
		active += d
		peak = max(peak, active)
		mu.Unlock()
	}
	t.Cleanup(func() { heavyHook = nil })
	var wg sync.WaitGroup
	errs := make(chan error, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := OpenNewestKit(ctx, r, kitPass)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if peak != 1 {
		t.Fatalf("%d kits were being opened at once", peak)
	}
}

// When the index fog recorded writing is gone, recovering from the bucket's
// listing is what a tampered bucket would steer toward: it is refused until
// the operator says go ahead. And the listing read is bounded however many
// rows a bucket writer plants.
func TestRecoveryFromListingNeedsConsentAndIsBounded(t *testing.T) {
	t.Parallel()

	h := newHarness(t, fakes3.New("fog-test"))
	ctx := context.Background()
	target, _, err := h.m.AddTarget(ctx, h.spec("cloud"), "key-1", "secret")
	if err != nil {
		t.Fatal(err)
	}
	r, _ := h.m.Reader(ctx, target)
	if err := h.m.writeIndex(ctx, target, r, nil); err != nil {
		t.Fatal(err)
	}
	var keys []string
	r.Client.List(ctx, r.Layout.IndexPrefix(), func(o objstore.Object) error { keys = append(keys, o.Key); return nil })
	for _, k := range keys {
		r.Client.Delete(ctx, k)
	}
	if _, err := h.m.StartRecover("cloud", false); !errors.Is(err, ErrIndexMissing) {
		t.Fatalf("recovery without the recorded index must ask first: %v", err)
	}

	for i := 0; i < 100; i++ {
		stamp := fmt.Sprintf("20260801-%06d", i)
		raw, _ := json.Marshal(AppRow{App: "hub", Stamp: stamp})
		plant(t, r, r.Layout.AppRow("hub", stamp), raw)
	}
	ix, err := r.IndexFromListing(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(ix.Apps["hub"]); n == 0 || n > listingRowsPerApp {
		t.Fatalf("rows read from the listing: %d", n)
	}
}

// A bucket that trickles a kit used to hold the process's one heavy slot
// while it did, so a pre-setup stranger could keep every recovery queued.
// The download now happens outside the slot, and is given up once it stops
// moving.
func TestSlowKitDoesNotHoldTheSlot(t *testing.T) {
	old := kitReadStall
	kitReadStall = 400 * time.Millisecond
	t.Cleanup(func() { kitReadStall = old })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100000")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("FOGKIT2\x00"))
		w.(http.Flusher).Flush()
		<-r.Context().Done() // and then nothing
	}))
	t.Cleanup(srv.Close)
	client, err := objstore.New(objstore.Config{Endpoint: srv.URL, Region: "x", Bucket: "b", AccessKey: "k", SecretKey: "s", PathStyle: true, HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	r := &Reader{Client: client, Layout: Layout{Prefix: "p", ID: "x"}}
	held := make(chan error, 1)
	go func() {
		_, err := r.FetchKit(context.Background(), "fog-recovery-kit-20260827-010000.enc", io.Discard)
		held <- err
	}()
	time.Sleep(100 * time.Millisecond)
	release, err := takeHeavy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	release()
	select {
	case err := <-held:
		if err == nil {
			t.Fatal("a stalled kit download must fail")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a stalled kit download was not given up")
	}
}
