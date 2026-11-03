package server

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/fed"
	"gitea.lan/matthew/fog/internal/store"
)

// countingReader is an endless body that counts what the server took.
type countingReader struct{ n atomic.Int64 }

func (c *countingReader) Read(b []byte) (int, error) {
	for i := range b {
		b[i] = 'x'
	}
	c.n.Add(int64(len(b)))
	return len(b), nil
}

// A caller this fog has never paired with may send a pairing's worth and
// no more: a large body is cut off where a pair request would end, not
// read into memory. And only a few such callers are read at once — the
// rest are turned away before anything of theirs is read.
func TestUnauthenticatedFedRequestsAreBounded(t *testing.T) {
	p := twoFogs(t, `[]`)
	stranger := mustIdentity(t, mustServer(t))

	body := &countingReader{}
	req, _ := http.NewRequest("POST", p.aHTTP.URL+fed.SealedTarget(mustIdentity(t, p.a).InstanceID), io.LimitReader(body, 256<<20))
	req.Header.Set(fed.HeaderInstance, stranger.InstanceID)
	req.ContentLength = 256 << 20
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Fatalf("a stranger's 256 MiB body: %d", resp.StatusCode)
		}
	}
	if n := body.n.Load(); n > 32<<20 {
		t.Fatalf("the server read %d bytes from a caller it doesn't know", n)
	}

	// Slow callers hold unauthenticated slots — headers sent, the body
	// trickling — but no more than a few from one address, and none past
	// the phase's absolute deadline however they trickle. A peer, whose
	// headers verify, never waits on them.
	capy := p.a.fedCap()
	capy.mu.Lock()
	capy.grace = time.Second
	capy.mu.Unlock()
	addr := strings.TrimPrefix(p.aHTTP.URL, "http://")
	var conns []net.Conn
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	for i := 0; i < fedUnauthPerAddr; i++ {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
		fmt.Fprintf(c, "POST %s HTTP/1.1\r\nHost: a\r\n%s: %s\r\nContent-Length: 1000\r\n\r\nxx", fed.SealedTarget(mustIdentity(t, p.a).InstanceID), fed.HeaderInstance, stranger.InstanceID)
	}
	sem := p.a.fedUnauthSlots()
	waitFor(t, 10*time.Second, func() bool { return len(sem) == fedUnauthPerAddr })
	req, _ = http.NewRequest("POST", p.aHTTP.URL+fed.SealedTarget(mustIdentity(t, p.a).InstanceID), strings.NewReader("x"))
	req.Header.Set(fed.HeaderInstance, stranger.InstanceID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("a fifth slow caller from one address: %d, want 503", resp.StatusCode)
	}
	if err := p.b.fed.HeartbeatPeer(t.Context(), p.aPeerOnB); err != nil {
		t.Fatalf("a peer while strangers hold slots: %v", err)
	}
	// Trickling a byte at a time keeps none of them past the deadline.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(200 * time.Millisecond):
				for _, c := range conns {
					c.Write([]byte("x"))
				}
			}
		}
	}()
	waitFor(t, 15*time.Second, func() bool { return len(sem) == 0 })
}

// The unauthenticated pool has room for fedUnauthSlots in all, and no
// address more than fedUnauthPerAddr of them.
func TestUnauthenticatedPoolBounds(t *testing.T) {
	s := mustServer(t)
	c := s.fedCap()
	var held []func()
	for i := 0; i < fedUnauthSlots; i++ {
		rel, ok := c.takeUnauth(fmt.Sprintf("10.0.0.%d", i))
		if !ok {
			t.Fatalf("slot %d refused", i)
		}
		held = append(held, rel)
	}
	if _, ok := c.takeUnauth("10.0.1.1"); ok {
		t.Fatal("a slot past the pool")
	}
	for _, rel := range held {
		rel()
	}
	for i := 0; i < fedUnauthPerAddr; i++ {
		if _, ok := c.takeUnauth("10.9.9.9"); !ok {
			t.Fatal("an address refused under its cap")
		}
	}
	if _, ok := c.takeUnauth("10.9.9.9"); ok {
		t.Fatal("one address took more than its cap")
	}
}

// badPair sends one sealed pair request with a wrong secret, as a stranger
// guessing at invite codes, and returns the status.
func badPair(t *testing.T, p *fogPair, invEncKey string) int {
	t.Helper()
	guesser := mustIdentity(t, mustServer(t))
	body, _ := json.Marshal(fed.PairRequest{Secret: "not-the-secret", Instance: fed.InstanceDoc{
		InstanceID: guesser.InstanceID, Name: "g", URL: "https://g.example", PublicKey: guesser.PublicKeyB64(),
		EncryptionKey: guesser.EncPublicB64(), EncryptionKeySig: guesser.SignTranscript("fogfed1-enckey", guesser.InstanceID, guesser.EncPublic),
	}})
	req, _ := http.NewRequest("POST", p.aHTTP.URL+"/fed/v1/pair", nil)
	req.Header.Set("Content-Type", "application/json")
	k, _ := fed.ParseEncKey(invEncKey)
	if err := fed.SealRequest(guesser, req, fed.PairRecipient(invEncKey), k, body); err != nil {
		t.Fatal(err)
	}
	status, _ := sendFedAs(t, req, guesser, mustIdentity(t, p.a).Public, fed.PairRecipient(invEncKey))
	return status
}

// Behind fogd's proxy every caller shares one address. Somebody guessing
// at invite codes uses up only the guessers' allowance: the household
// holding a real code still pairs.
func TestPairGuessingDoesNotLockOutTheInvited(t *testing.T) {
	p := twoFogs(t, `[]`)
	code := mintInvite(t, p.aH, p.aCookie, `[]`)
	inv, err := fed.ParseInvite(code)
	if err != nil {
		t.Fatal(err)
	}
	limited := false
	for i := 0; i < 20 && !limited; i++ {
		limited = badPair(t, p, inv.EncKey) == http.StatusTooManyRequests
	}
	if !limited {
		t.Fatal("guessing at invite secrets was never rate limited")
	}
	c, cH := newTestServer(t)
	_ = c
	cCookie := setup(t, cH)
	if w := do(t, cH, "POST", "/api/federation/join", fmt.Sprintf(`{"code":%q}`, code), withCookie(cCookie)); w.Code != http.StatusOK {
		t.Fatalf("the invited household, after the guessing: %d %s", w.Code, w.Body)
	}
}

// The encryption-key header is outside the signature. A key swapped into
// it on the way is not the peer changing keys: the request is refused, the
// pinned key stays, and nobody is alarmed.
func TestUnboundEncryptionKeyRaisesNoAlarm(t *testing.T) {
	p := twoFogs(t, `[]`)
	bID := mustIdentity(t, p.b)
	other := mustIdentity(t, mustServer(t))
	before, _ := p.a.store.CountActiveNotices()
	pinned, _ := p.a.store.FedPeerByID(p.bPeerOnA.ID)

	req, _ := http.NewRequest("POST", p.aHTTP.URL+"/fed/v1/heartbeat", nil)
	sealFor(t, bID, p.a, req, []byte(`{"fogVersion":"x","grants":[],"directory":[]}`))
	req.Header.Set(fed.HeaderEncKey, other.EncPublicB64())
	status, raw := sendFed(t, req, bID, p.a)
	var out struct {
		Code string `json:"code"`
	}
	json.Unmarshal(raw, &out)
	if status != http.StatusForbidden || out.Code != fed.CodeEncryptionRequired {
		t.Fatalf("a swapped encryption key: %d %s", status, raw)
	}
	if after, _ := p.a.store.CountActiveNotices(); after != before {
		t.Fatalf("an unbound key raised %d notices", after-before)
	}
	if got, _ := p.a.store.FedPeerByID(p.bPeerOnA.ID); got.EncPublicKey != pinned.EncPublicKey {
		t.Fatal("an unbound key changed the pin")
	}
	if err := p.b.fed.HeartbeatPeer(t.Context(), p.aPeerOnB); err != nil {
		t.Fatalf("B afterwards: %v", err)
	}
}

// The envelope carries /fed/v1 requests only. A peer's inner request for
// any other route — the API, the UI, or a fed path that climbs out — is
// not dispatched.
func TestInnerRequestsReachOnlyFedRoutes(t *testing.T) {
	p := twoFogs(t, `[]`)
	bID := mustIdentity(t, p.b)
	for _, target := range []string{
		"/api/federation/identity",
		"/api/setup/status",
		"/",
		"/fed/v1/../api/federation/identity",
		"/fed/v1//heartbeat",
		fed.SealedPath,
	} {
		req, _ := http.NewRequest("GET", p.aHTTP.URL+target, nil)
		sealFor(t, bID, p.a, req, nil)
		status, raw := sendFed(t, req, bID, p.a)
		if status != http.StatusNotFound || !strings.Contains(string(raw), "no such fed route") {
			t.Errorf("inner %s: %d %s", target, status, raw)
		}
	}
}

// A peer already paired here that gets hold of another invite — one
// leaked, or minted for someone else — and re-pairs with it keeps the
// grants it has: the invite's grants are not applied, and the operator is
// told.
func TestLeakedInviteDoesNotWidenAnExistingPeer(t *testing.T) {
	p := twoFogs(t, `[]`)
	code := mintInvite(t, p.aH, p.aCookie, `[{"capability":"flare.ingest"}]`)
	before, _ := p.a.store.ListFedCapabilities(p.bPeerOnA.ID)
	if w := do(t, p.bH, "POST", "/api/federation/join", fmt.Sprintf(`{"code":%q}`, code), withCookie(p.bCookie)); w.Code != http.StatusOK {
		t.Fatalf("re-pair: %d %s", w.Code, w.Body)
	}
	if _, ok, _ := p.a.store.FedCapabilityConfig(p.bPeerOnA.ID, store.CapFlareIngest, store.DirGrant); ok {
		t.Fatal("re-pairing with another invite widened the existing peer's grants")
	}
	after, _ := p.a.store.ListFedCapabilities(p.bPeerOnA.ID)
	if len(after) != len(before) {
		t.Fatalf("grants changed on re-pair: %v -> %v", before, after)
	}
	found := false
	notices, _ := p.a.store.ActiveNotices()
	for _, n := range notices {
		found = found || n.Kind == "fed-pair-invite-unused"
	}
	if !found {
		t.Fatal("the operator was not told the invite's grants went unused")
	}
}

// A peer asking for a preview of a small file that declares a vast image
// gets no thumbnail, and the origin decodes nothing: the federation path
// goes through the same pixel limit as the household's own browsing.
func TestFedThumbnailOfADecompressionBomb(t *testing.T) {
	p := twoFogs(t, `[{"capability":"files.share"}]`)
	root := p.a.files.Root()
	os.MkdirAll(filepath.Join(root, "Photos"), 0o755)
	var small bytes.Buffer
	png.Encode(&small, image.NewGray(image.Rect(0, 0, 1, 1)))
	bomb := small.Bytes()
	// IHDR: 8-byte signature, 4 length, "IHDR", width, height, ..., CRC.
	binary.BigEndian.PutUint32(bomb[16:], 60000)
	binary.BigEndian.PutUint32(bomb[20:], 60000)
	binary.BigEndian.PutUint32(bomb[29:], crc32.ChecksumIEEE(bomb[12:29]))
	os.WriteFile(filepath.Join(root, "Photos", "bomb.png"), bomb, 0o644)

	w := do(t, p.aH, "POST", "/api/files/peer-shares", fmt.Sprintf(`{"peerId":%d,"path":"Photos","title":"P","detail":"previews"}`, p.bPeerOnA.ID), withCookie(p.aCookie))
	if w.Code != http.StatusCreated {
		t.Fatalf("share: %d %s", w.Code, w.Body)
	}
	var sh struct {
		ID int64 `json:"id"`
	}
	json.Unmarshal(w.Body.Bytes(), &sh)
	start := time.Now()
	resp, err := p.b.fed.SignedDo(t.Context(), p.aPeerOnB, "GET", fmt.Sprintf("/fed/v1/files/shares/%d/thumb/bomb.png?w=1024", sh.ID), "", nil)
	if err == nil {
		resp.Body.Close()
		t.Fatalf("a thumbnail of a 60000×60000 image: %d", resp.StatusCode)
	}
	var re *fed.RemoteError
	if !errors.As(err, &re) || re.Status != http.StatusNotFound {
		t.Fatalf("the bomb's thumbnail: %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("refusing took %s: it was decoded", d)
	}
}

// "Unpaired" is the peer's own word — this side stamps it from the peer's
// unknown_peer answers — so a peer can bring it about by forgetting us,
// then re-pair with a leaked invite. It is still the peer this side holds:
// the invite's grants don't land.
func TestPeerStampedUnpairedCannotWidenWithALeakedInvite(t *testing.T) {
	p := twoFogs(t, `[]`)
	for i := 0; i < 3; i++ {
		if err := p.a.store.RecordFedPeerRejection(p.bPeerOnA.ID, 3); err != nil {
			t.Fatal(err)
		}
	}
	if row, _ := p.a.store.FedPeerByID(p.bPeerOnA.ID); row.UnpairedAt == "" {
		t.Fatal("the fixture did not stamp the row unpaired")
	}
	code := mintInvite(t, p.aH, p.aCookie, `[{"capability":"flare.ingest"}]`)
	if w := do(t, p.bH, "POST", "/api/federation/join", fmt.Sprintf(`{"code":%q}`, code), withCookie(p.bCookie)); w.Code != http.StatusOK {
		t.Fatalf("re-pair: %d %s", w.Code, w.Body)
	}
	if _, ok, _ := p.a.store.FedCapabilityConfig(p.bPeerOnA.ID, store.CapFlareIngest, store.DirGrant); ok {
		t.Fatal("a peer that stamped itself unpaired widened its grants with a leaked invite")
	}
}

// A paired peer with no grants at all can't make this fog read or hold a
// large envelope: the envelope is held to what its inner route takes —
// learned from its first record — while it is still arriving, and the
// body is opened only as the route's handler reads it.
func TestGrantlessPeerCannotSendHugeEnvelopes(t *testing.T) {
	p := twoFogs(t, `[]`)
	bID, aID := mustIdentity(t, p.b), mustIdentity(t, p.a)
	huge := bytes.Repeat([]byte("x"), 60<<20)
	req, _ := http.NewRequest("POST", p.aHTTP.URL+"/fed/v1/heartbeat", nil)
	sealFor(t, bID, p.a, req, huge)
	wire, _ := io.ReadAll(req.Body)
	// The body gives its first 4 MiB and then holds the rest back until the
	// answer is in: a server that waited for all 60 MiB would never answer.
	gate := make(chan struct{})
	req.Body = io.NopCloser(&gatedBody{b: wire, free: 4 << 20, gate: gate})
	req.ContentLength = int64(len(wire))
	type result struct {
		resp *http.Response
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		done <- result{resp, err}
	}()
	select {
	case r := <-done:
		close(gate)
		if r.err != nil {
			t.Fatalf("no answer: %v", r.err)
		}
		// Refused before the body could verify: in the clear, unsigned —
		// nothing that could be carried to B as A's answer.
		r.resp.Body.Close()
		if r.resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("a 60 MiB heartbeat: %d, want a plain 413", r.resp.StatusCode)
		}
		_ = aID
	case <-time.After(20 * time.Second):
		close(gate)
		t.Fatal("the server waited for more than a heartbeat's worth of body")
	}
	// A route's own size still goes through.
	if err := p.b.fed.HeartbeatPeer(t.Context(), p.aPeerOnB); err != nil {
		t.Fatal(err)
	}
}

// gatedBody serves its first free bytes, then blocks until gate closes.
type gatedBody struct {
	b    []byte
	off  int
	free int
	gate chan struct{}
}

func (g *gatedBody) Read(p []byte) (int, error) {
	if g.off >= len(g.b) {
		return 0, io.EOF
	}
	if g.off >= g.free {
		<-g.gate
		return 0, io.ErrUnexpectedEOF
	}
	end := min(len(g.b), g.free, g.off+len(p))
	k := copy(p, g.b[g.off:end])
	g.off += k
	return k, nil
}

// A peer's slots: every one held by a request of its own that verified,
// and the next is turned away; held by bodies not yet verified — headers
// anyone could have copied — and the next request takes the oldest.
func TestPeerConcurrencyIsCapped(t *testing.T) {
	s := mustServer(t)
	c := s.fedCap()
	for i := 0; i < fedPeerSlots; i++ {
		if _, _, ok := c.takePeer(7, true, func() {}); !ok {
			t.Fatal("refused under the cap")
		}
	}
	if _, _, ok := c.takePeer(7, true, func() {}); ok {
		t.Fatal("a peer took more than its slots")
	}
	if _, _, ok := c.takePeer(8, false, func() {}); !ok {
		t.Fatal("one peer's slots held another back")
	}
	// Unverified holders: one stalling, the rest arriving at a fair rate.
	evicted := 0
	var holds []*peerHold
	for i := 0; i < fedPeerSlots; i++ {
		h, _, _ := c.takePeer(9, false, func() { evicted++ })
		holds = append(holds, h)
	}
	c.mu.Lock()
	for i, h := range holds {
		h.start = time.Now().Add(-10 * time.Second)
		if i != 3 {
			h.read.Store(10 * 4 * fedUnauthMinRate)
		}
	}
	c.mu.Unlock()
	if _, _, ok := c.takePeer(9, true, func() {}); !ok || evicted != 1 {
		t.Fatalf("a stalled unverified holder kept its slot from the peer: ok=%v evicted=%d", ok, evicted)
	}
	// Every holder making progress: the newcomer waits its turn.
	if _, _, ok := c.takePeer(9, true, func() {}); ok || evicted != 1 {
		t.Fatalf("a holder making progress was evicted: ok=%v evicted=%d", ok, evicted)
	}
}

// Headers B signed are good only at the fog B made them for. Sent on to
// another — by the fog they were meant for, an eavesdropper, a squatter at
// an old address — they are refused before anything is held; replayed at
// the right fog with a trickling body they hold nothing B needs: B's own
// next request takes the slot, the replay record isn't spent until a body
// verifies, and a trickle is cut off at its deadline.
func TestRelayedHeadersCannotLockAPeerOut(t *testing.T) {
	p := twoFogs(t, `[]`)
	bID := mustIdentity(t, p.b)
	hb := []byte(`{"fogVersion":"x","grants":[],"directory":[]}`)

	// Signed for another fog, sent here.
	other := mustServer(t)
	req, _ := http.NewRequest("POST", p.aHTTP.URL+"/fed/v1/heartbeat", nil)
	sealFor(t, bID, other, req, hb)
	req.URL, _ = url.Parse(p.aHTTP.URL + req.URL.RequestURI())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(raw), fed.CodeWrongRecipient) {
		t.Fatalf("headers signed for another fog: %d %s", resp.StatusCode, raw)
	}

	// Signed for this fog, copied and replayed with bodies that trickle:
	// the first copy reserves the request, the rest are refused at once,
	// and B's own requests go through meanwhile.
	capy := p.a.fedCap()
	capy.mu.Lock()
	capy.grace = 2 * time.Second
	capy.mu.Unlock()
	req, _ = http.NewRequest("POST", p.aHTTP.URL+"/fed/v1/heartbeat", nil)
	sealFor(t, bID, p.a, req, hb)
	wire, _ := io.ReadAll(req.Body)
	conns := trickle(t, p.aHTTP.URL, req, wire, fedPeerSlots)
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	held := func() int {
		capy.mu.Lock()
		defer capy.mu.Unlock()
		return len(capy.byPeer[p.bPeerOnA.ID])
	}
	waitFor(t, 10*time.Second, func() bool { return held() == 1 })
	if err := p.b.fed.HeartbeatPeer(t.Context(), p.aPeerOnB); err != nil {
		t.Fatalf("B, with its copied headers trickling at A: %v", err)
	}
	// The trickle ends at its deadline, whatever it sends, and gives its
	// reservation back: the real request, delivered whole, is taken once.
	waitFor(t, 15*time.Second, func() bool { return held() == 0 })
	send := func() int {
		r2, _ := http.NewRequest("POST", req.URL.String(), bytes.NewReader(wire))
		r2.Header = req.Header.Clone()
		status, _ := sendFed(t, r2, bID, p.a)
		return status
	}
	if status := send(); status != http.StatusOK {
		t.Fatalf("the genuine request after the replays: %d", status)
	}
	if status := send(); status != http.StatusForbidden {
		t.Fatalf("the genuine request, again: %d", status)
	}
}

// trickle opens n connections that each send req's headers and the first
// bytes of wire, and no more.
func trickle(t *testing.T, base string, req *http.Request, wire []byte, n int) []net.Conn {
	t.Helper()
	addr := strings.TrimPrefix(base, "http://")
	var conns []net.Conn
	for i := 0; i < n; i++ {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
		var hdr strings.Builder
		fmt.Fprintf(&hdr, "POST %s HTTP/1.1\r\nHost: a\r\nContent-Length: %d\r\n", req.URL.RequestURI(), len(wire))
		for k, vs := range req.Header {
			for _, v := range vs {
				fmt.Fprintf(&hdr, "%s: %s\r\n", k, v)
			}
		}
		hdr.WriteString("\r\n")
		c.Write([]byte(hdr.String()))
		c.Write(wire[:8])
	}
	return conns
}

// A captured request whose body is held back can't be accepted twice: its
// headers are reserved in the replay record the moment they arrive — so
// the genuine request is refused while the copy is pending — and the
// record outlives the longest the copy's body could take. A copy whose
// body fails gives the reservation back.
func TestWithheldBodyCannotOutliveTheReplayRecord(t *testing.T) {
	p := twoFogs(t, `[]`)
	bID := mustIdentity(t, p.b)
	req, _ := http.NewRequest("POST", p.aHTTP.URL+"/fed/v1/heartbeat", nil)
	sealFor(t, bID, p.a, req, []byte(`{"fogVersion":"x","grants":[],"directory":[]}`))
	wire, _ := io.ReadAll(req.Body)
	digest, _ := fed.RequestDigest(fed.BoundTo(req.Header))
	c := trickle(t, p.aHTTP.URL, req, wire, 1)[0]
	defer c.Close()
	waitFor(t, 10*time.Second, func() bool { return p.a.store.FedRequestHeld(digest) })
	ts, _ := time.Parse(time.RFC3339, req.Header.Get(fed.HeaderTimestamp))
	if exp, ok := p.a.store.FedRequestExpiry(digest); !ok || exp.Before(ts.Add(fed.MaxSkew+10*time.Minute)) {
		t.Fatalf("the record lets the request go at %v, before a withheld body could still complete", exp)
	}
	// The genuine request, meanwhile: refused, the copy holds it.
	r2, _ := http.NewRequest("POST", req.URL.String(), bytes.NewReader(wire))
	r2.Header = req.Header.Clone()
	if status, _ := sendFed(t, r2, bID, p.a); status != http.StatusForbidden {
		t.Fatalf("a request already pending was taken again: %d", status)
	}
	// The copy completes: taken once.
	c.Write(wire[8:])
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("the completed copy: %v %v", resp, err)
	}

	// A copy whose body isn't the one signed gives its reservation back.
	req, _ = http.NewRequest("POST", p.aHTTP.URL+"/fed/v1/heartbeat", nil)
	sealFor(t, bID, p.a, req, []byte(`{"fogVersion":"x","grants":[],"directory":[]}`))
	wire, _ = io.ReadAll(req.Body)
	bad := bytes.Clone(wire)
	bad[len(bad)-1] ^= 1
	r3, _ := http.NewRequest("POST", req.URL.String(), bytes.NewReader(bad))
	r3.Header = req.Header.Clone()
	if resp, err := http.DefaultClient.Do(r3); err == nil {
		resp.Body.Close()
	}
	r4, _ := http.NewRequest("POST", req.URL.String(), bytes.NewReader(wire))
	r4.Header = req.Header.Clone()
	if status, _ := sendFed(t, r4, bID, p.a); status != http.StatusOK {
		t.Fatalf("the genuine request after a failed copy: %d", status)
	}
}

// Bodies past what is held in memory go to fog's own data directory —
// never /tmp, which is memory where fog runs — against one allowance for
// every request at once.
func TestSpoolLivesUnderTheDataDirAndIsCapped(t *testing.T) {
	s := mustServer(t)
	sp := s.fedSpoolFor()
	if !strings.HasPrefix(sp.dir, s.cfg.DataDir) || s.cfg.DataDir == "" {
		t.Fatalf("spool at %q, not under the data directory %q", sp.dir, s.cfg.DataDir)
	}
	big := bytes.Repeat([]byte("x"), 3<<20)
	r := httptest.NewRequest("POST", "/fed/v1/sealed", bytes.NewReader(big))
	b, err := readFedBody(httptest.NewRecorder(), r, 8<<20, time.Time{}, nil, sp, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b.file == nil || !strings.HasPrefix(b.file.Name(), sp.dir) {
		t.Fatalf("not spooled under the data directory: %v", b.file)
	}
	name := b.file.Name()
	b.close()
	if _, err := os.Stat(name); !os.IsNotExist(err) {
		t.Fatal("a spooled body outlived its request")
	}
	c := s.fedCap()
	c.mu.Lock()
	if c.spooled != 0 {
		t.Fatalf("spool allowance not given back: %d", c.spooled)
	}
	c.spooled = fedSpoolMax - 1<<20
	c.mu.Unlock()
	r = httptest.NewRequest("POST", "/fed/v1/sealed", bytes.NewReader(big))
	if _, err := readFedBody(httptest.NewRecorder(), r, 8<<20, time.Time{}, nil, sp, nil); !errors.Is(err, errFedSpoolFull) {
		t.Fatalf("past the spool's allowance: %v", err)
	}
}

// Strangers are counted by their network: an IPv6 caller by its /64, not
// one address of the many it holds.
func TestStrangerCapCountsANetwork(t *testing.T) {
	s := mustServer(t)
	key := func(remote string) string {
		r := httptest.NewRequest("POST", "/fed/v1/sealed", nil)
		r.RemoteAddr = remote
		k, _ := s.limitAddr(r)
		return k
	}
	if key("[2001:db8:1:2::1]:4000") != key("[2001:db8:1:2:ffff::9]:4001") {
		t.Fatal("two addresses of one /64 are counted apart")
	}
	if key("[2001:db8:1:2::1]:4000") == key("[2001:db8:1:3::1]:4000") {
		t.Fatal("two /64s are counted together")
	}
}

// A refusal to a request whose signature didn't verify goes out unsigned
// and unsealed: an on-path attacker who tampers with B's request can't
// carry A's refusal back to B as A's answer to the request B sent.
func TestRefusalOfATamperedRequestIsNotSigned(t *testing.T) {
	p := twoFogs(t, `[]`)
	bID, aID := mustIdentity(t, p.b), mustIdentity(t, p.a)
	req, _ := http.NewRequest("POST", p.aHTTP.URL+"/fed/v1/heartbeat", nil)
	sealFor(t, bID, p.a, req, []byte(`{"fogVersion":"x","grants":[],"directory":[]}`))
	wire, _ := io.ReadAll(req.Body)
	for _, strip := range []bool{false, true} {
		tampered := bytes.Clone(wire)
		tampered[len(tampered)-1] ^= 1
		r2, _ := http.NewRequest("POST", req.URL.String(), bytes.NewReader(tampered))
		r2.Header = req.Header.Clone()
		if strip {
			r2.Header.Del(fed.HeaderContentSHA256) // the slow lane
		}
		resp, err := http.DefaultClient.Do(r2)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Fatalf("a tampered request's refusal came in the envelope (strip=%v)", strip)
		}
		// Relayed to B as the answer to its own request, it proves nothing.
		resp.Request = req
		resp.StatusCode = http.StatusOK
		resp.Body = io.NopCloser(bytes.NewReader(raw))
		if _, _, err := fed.OpenAnswer(bID, aID.Public, aID.InstanceID, resp); err == nil {
			t.Fatal("A's refusal of a tampered request verified as A's answer to B's")
		}
	}
}
