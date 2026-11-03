package compute

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/store"
)

// A machine that can't prove its ffmpeg is confined joins no pool, and the
// page says why; one whose owner turned confinement off does, and says so.
func TestAMachineMustConfineItsFFmpegToLendVideo(t *testing.T) {
	for _, c := range []struct {
		name               string
		confinement, error string
		unconfined         bool
		pool, why          string
	}{
		{"confined", "landlock", "", false, "video/videotoolbox", ""},
		{"can't", "", "this kernel has no Landlock", false, "", "it can't confine ffmpeg: this kernel has no Landlock"},
		{"too old to say", "", "", false, "", "its fog-node doesn't confine ffmpeg"},
		{"owner opted out", "", "", true, "video/videotoolbox", ""},
	} {
		inv := mac("Mini")
		inv.Video.Confinement, inv.Video.ConfinementError, inv.Video.Unconfined = c.confinement, c.error, c.unconfined
		pool, why := poolFor(inv.Video)
		if pool != c.pool || !strings.HasPrefix(why, c.why) {
			t.Errorf("%s: pool %q, why %q", c.name, pool, why)
		}
	}
}

// fog reaches a machine's inference listener at the address the machine
// polls from, never one the machine names: a node can't point fog at
// fogd itself, an app, or the internet.
func TestInferenceIsReachedWhereTheMachinePollsFrom(t *testing.T) {
	for _, c := range []struct{ from, lent, want string }{
		{"192.168.1.20", "http://192.168.1.20:7718", "http://192.168.1.20:7718"},
		{"192.168.1.20", "http://127.0.0.1:8080", "http://192.168.1.20:8080"},
		{"192.168.1.20", "http://169.254.169.254:80/latest", "http://192.168.1.20:80"},
		{"fd00::5", "http://[fd00::5]:7718", "http://[fd00::5]:7718"},
		{"192.168.1.20", "http://10.0.0.1", ""},
		{"192.168.1.20", "https://evil.example:443", ""},
		{"192.168.1.20", "http://x:99999", ""},
		{"not an ip", "http://192.168.1.20:7718", ""},
	} {
		if got := reachAt(c.from, c.lent); got != c.want {
			t.Errorf("reachAt(%q, %q) = %q, want %q", c.from, c.lent, got, c.want)
		}
	}

	r, _ := testRegistry(t)
	s := &Service{Registry: r}
	ans := pair(t, r, mac("Mini"))
	n := authenticate(r, ans.Token)
	inv := mac("Mini")
	inv.Inference = &InferenceCaps{API: "openai", URL: "http://127.0.0.1:8080", Models: []string{"qwen"}, Local: true}
	if _, err := s.Poll(context.Background(), n, NodeReport{Running: []string{}, Inventory: &inv}, "192.168.1.20"); err != nil {
		t.Fatal(err)
	}
	m, _ := r.Machine(ans.Node)
	if m.Inventory.Inference == nil || m.Inventory.Inference.URL != "http://192.168.1.20:8080" {
		t.Fatalf("inference: %+v", m.Inventory.Inference)
	}
}

// Removing a machine answers the poll it holds open at once, and every
// poll after, so it stops its runs and forgets the pairing promptly.
func TestRemovingAMachineAnswersItsHeldPoll(t *testing.T) {
	r, _ := testRegistry(t)
	s := &Service{Registry: r}
	ans := pair(t, r, mac("Mini"))
	n := authenticate(r, ans.Token)
	got := make(chan *NodeAnswer, 1)
	go func() {
		a, err := s.Poll(context.Background(), n, NodeReport{Running: []string{}, Wait: true}, "")
		if err != nil {
			t.Error(err)
		}
		got <- a
	}()
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	if err := s.Remove(ans.Node); err != nil {
		t.Fatal(err)
	}
	select {
	case a := <-got:
		if !a.Removed || len(a.Start) != 0 {
			t.Fatalf("held poll answered %+v", a)
		}
		if time.Since(start) > time.Second {
			t.Fatalf("answered only after %v", time.Since(start))
		}
	case <-time.After(HoldPoll + time.Second):
		t.Fatal("the held poll wasn't answered")
	}
	// One that authenticated just before the removal is told too.
	if a, _ := s.Poll(context.Background(), n, NodeReport{Running: []string{}}, ""); !a.Removed {
		t.Fatalf("a late poll: %+v", a)
	}
}

// A run's data route ends with the run:
// even a request already streaming is cut off then. Only reads and
// whole-file uploads go through, on clean paths.
func TestDataRouteEndsWithTheRun(t *testing.T) {
	rig := newRelayRig(t)
	streaming := make(chan struct{})
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("first"))
		w.(http.Flusher).Flush()
		close(streaming)
		<-r.Context().Done()
	}))
	defer app.Close()
	mini := pair(t, rig.svc.Registry, mac("Mini"))
	rig.node(t, mini.Token, NodeReport{})
	rn := &run{ID: "r1", AppID: rig.stream.ID, Pool: "video/videotoolbox", AppRun: "s1", Node: mini.Node, Data: "tok",
		Started: time.Now(), Confirmed: true, done: make(chan struct{})}
	rig.relay.mu.Lock()
	rig.relay.runs["r1"] = rn
	rig.relay.mu.Unlock()
	h := rig.svc.DataHandler(func(context.Context, *store.App) (string, error) { return app.URL, nil })
	srv := httptest.NewServer(h)
	defer srv.Close()

	for method, want := range map[string]int{"POST": 405, "DELETE": 405, "PATCH": 405} {
		req, _ := http.NewRequest(method, srv.URL+DataPath+"tok/fog/compute/runs/abc/source", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("%s: %d", method, resp.StatusCode)
		}
	}
	for _, bad := range []string{"tok/fog/compute/runs/./abc", "tok/fog/compute/runs/abc/", "tok/fog/computer/x", "tok/fog/compute/a\\..\\..\\api"} {
		req := httptest.NewRequest("GET", "/x", nil)
		req.URL.Path = DataPath + bad
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s: %d", bad, w.Code)
		}
	}

	resp, err := getAs(mini, srv.URL+DataPath+"tok/fog/compute/runs/abc/source")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	<-streaming
	ended := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(resp.Body)
		ended <- err
	}()
	// The machine goes away: the run is lost, and its transfer with it.
	rig.relay.loseNode(mini.Node, "removed")
	select {
	case <-ended:
	case <-time.After(3 * time.Second):
		t.Fatal("a request on a run that ended kept going")
	}
	if r2, _ := getAs(mini, srv.URL+DataPath+"tok/fog/compute/runs/abc/source"); r2.StatusCode != http.StatusNotFound {
		t.Fatalf("the token still works: %d", r2.StatusCode)
	}
}

// An app's run must upload where it reads from, and may only lower the
// machine's limits, which go to the machine with it.
func TestARunsUploadsAndLimitsAreChecked(t *testing.T) {
	base := AppRun{ID: "o1", Kind: "optimize", Argv: []string{"ffmpeg"}, Upload: "{server}/fog/compute/runs/y/files/{name}"}
	if _, _, err := shaped(base); err != nil {
		t.Fatal(err)
	}
	for _, upload := range []string{"http://evil.example/{name}", "{server}/api/v1/settings/{name}", "/fog/compute/x/{name}",
		"{server}/fog/compute/../../api/{name}", "{server}"} {
		bad := base
		bad.Upload = upload
		if _, _, err := shaped(bad); err == nil {
			t.Errorf("upload %q allowed", upload)
		}
	}
	neg := base
	neg.Limits = &RunLimits{Seconds: -1}
	if _, _, err := shaped(neg); err == nil {
		t.Error("a negative limit allowed")
	}

	rig := newRelayRig(t)
	mini := pair(t, rig.svc.Registry, mac("Mini"))
	rig.node(t, mini.Token, NodeReport{})
	waitFor(t, "a poll", func() bool { return rig.app.lastReport().Capacity.Interactive == 4 })
	limited := base
	limited.Limits = &RunLimits{Seconds: 60}
	rig.app.start(limited)
	var runs []NodeRun
	waitFor(t, "the run", func() bool {
		runs = append(runs, rig.node(t, mini.Token, NodeReport{}).Start...)
		return len(runs) == 1
	})
	if runs[0].Limits == nil || runs[0].Limits.Seconds != 60 {
		t.Fatalf("limits: %+v", runs[0].Limits)
	}
}

func TestLowerLimits(t *testing.T) {
	for _, c := range []struct{ a, b, want RunLimits }{
		{RunLimits{100, 1000}, RunLimits{}, RunLimits{100, 1000}},
		{RunLimits{100, 1000}, RunLimits{50, 0}, RunLimits{50, 1000}},
		{RunLimits{100, 1000}, RunLimits{500, 5000}, RunLimits{100, 1000}},
		{RunLimits{}, RunLimits{5, 6}, RunLimits{5, 6}},
		{RunLimits{100, 1000}, RunLimits{-5, -6}, RunLimits{100, 1000}},
	} {
		if got := c.a.Lower(c.b); got != c.want {
			t.Errorf("%+v.Lower(%+v) = %+v", c.a, c.b, got)
		}
	}
}

// signAs MACs a request on the data route as machine p: a read, or an
// upload of a body with digest sum.
func signAs(req *http.Request, p *paired, sum ...string) {
	nonce, at := NewNonce(), time.Now().UTC().Format(time.RFC3339Nano)
	body := ""
	if len(sum) > 0 {
		body = sum[0]
		req.Header.Set(HeaderBodySHA256, body)
	}
	req.Header.Set(HeaderNodeID, p.Node)
	req.Header.Set(HeaderNodeNonce, nonce)
	req.Header.Set(HeaderNodeTime, at)
	req.Header.Set(HeaderNodeAuth, DataMAC(TokenDigest(p.Token), p.Node, nonce, at, req.Method, req.URL.RequestURI(), body))
}

// An upload declares its body's digest under the machine's MAC, and fog
// checks the bytes against it as they stream to the app: a body swapped
// on the way is cut off, never delivered whole.
func TestDataRouteUploadsAreTheMachinesBytes(t *testing.T) {
	rig := newRelayRig(t)
	var got []string
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			return // cut off: the app never has the whole of it
		}
		got = append(got, string(b))
	}))
	defer app.Close()
	mini := pair(t, rig.svc.Registry, mac("Mini"))
	rig.node(t, mini.Token, NodeReport{})
	rig.relay.mu.Lock()
	rig.relay.runs["r1"] = &run{ID: "r1", AppID: rig.stream.ID, Pool: "video/videotoolbox", AppRun: "s1", Node: mini.Node, Data: "tok",
		Started: time.Now(), Confirmed: true, done: make(chan struct{})}
	rig.relay.mu.Unlock()
	srv := httptest.NewServer(rig.svc.DataHandler(func(context.Context, *store.App) (string, error) { return app.URL, nil }))
	defer srv.Close()
	put := func(body string, declared ...string) int {
		req, _ := http.NewRequest("PUT", srv.URL+DataPath+"tok/fog/compute/runs/abc/files/gop-1.m4s", strings.NewReader(body))
		signAs(req, mini, declared...)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := put("fragment", BodySHA256([]byte("fragment"))); c != 200 {
		t.Fatalf("the machine's upload: %d", c)
	}
	if c := put("swapped!", BodySHA256([]byte("fragment"))); c == 200 {
		t.Errorf("a swapped body went through: %d", c)
	}
	if c := put("fragment"); c != http.StatusBadRequest {
		t.Errorf("an upload declaring nothing: %d", c)
	}
	read := httptest.NewRequest("GET", DataPath+"tok/fog/compute/runs/abc/source", nil)
	signAs(read, mini, BodySHA256(nil))
	w := httptest.NewRecorder()
	rig.svc.DataHandler(func(context.Context, *store.App) (string, error) { return app.URL, nil }).ServeHTTP(w, read)
	if w.Code != http.StatusBadRequest {
		t.Errorf("a read declaring a body: %d", w.Code)
	}
	if len(got) != 1 || got[0] != "fragment" {
		t.Fatalf("the app received %q", got)
	}
}

func getAs(p *paired, url string) (*http.Response, error) {
	req, _ := http.NewRequest("GET", url, nil)
	signAs(req, p)
	return http.DefaultClient.Do(req)
}

// A run's data token only says which run: alone, or from another machine,
// or replayed, it opens nothing.
func TestDataRouteTakesOnlyTheRunsMachine(t *testing.T) {
	rig := newRelayRig(t)
	var hits int
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.Header.Get(HeaderNodeAuth) != "" || r.Header.Get(HeaderNodeID) != "" {
			t.Error("the machine's MAC reached the app")
		}
		w.Write([]byte("media"))
	}))
	defer app.Close()
	mini := pair(t, rig.svc.Registry, mac("Mini"))
	other := pair(t, rig.svc.Registry, mac("Other"))
	rig.node(t, mini.Token, NodeReport{})
	rig.relay.mu.Lock()
	rig.relay.runs["r1"] = &run{ID: "r1", AppID: rig.stream.ID, Pool: "video/videotoolbox", AppRun: "s1", Node: mini.Node, Data: "tok",
		Started: time.Now(), Confirmed: true, done: make(chan struct{})}
	rig.relay.mu.Unlock()
	h := rig.svc.DataHandler(func(context.Context, *store.App) (string, error) { return app.URL, nil })
	path := DataPath + "tok/fog/compute/runs/abc/source"
	serve := func(req *http.Request) int {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}
	if c := serve(httptest.NewRequest("GET", path, nil)); c != http.StatusUnauthorized {
		t.Errorf("the token alone: %d", c)
	}
	bearer := httptest.NewRequest("GET", path, nil)
	bearer.Header.Set("Authorization", "Bearer "+mini.Token)
	if c := serve(bearer); c != http.StatusUnauthorized {
		t.Errorf("the token and a bearer: %d", c)
	}
	fromOther := httptest.NewRequest("GET", path, nil)
	signAs(fromOther, other)
	if c := serve(fromOther); c != http.StatusNotFound {
		t.Errorf("another machine: %d", c)
	}
	good := httptest.NewRequest("GET", path, nil)
	signAs(good, mini)
	replay := good.Clone(context.Background())
	if c := serve(good); c != http.StatusOK {
		t.Fatalf("the run's machine: %d", c)
	}
	if c := serve(replay); c != http.StatusUnauthorized {
		t.Errorf("a replay: %d", c)
	}
	elsewhere := httptest.NewRequest("GET", DataPath+"tok/fog/compute/runs/other/source", nil)
	signAs(elsewhere, mini)
	elsewhere.URL.Path = DataPath + "tok/fog/compute/runs/abc/source"
	if c := serve(elsewhere); c != http.StatusUnauthorized {
		t.Errorf("a MAC for another path: %d", c)
	}
	if hits != 1 {
		t.Fatalf("the app was reached %d times", hits)
	}
}

// Pairing proves the code both ways without sending it, never carries the
// machine's credential (nor its digest) in the clear, and hands the
// machine fog's key, which only fog's answer can carry. Each code has a
// salt of its own.
func TestPairingProvesTheCodeBothWays(t *testing.T) {
	r, _ := testRegistry(t)
	code, _ := r.NewCode("admin")
	r.NewCode("admin")
	salts, _ := r.Salts()
	if len(salts) != 2 || salts[0] == salts[1] || len(salts[0]) != 32 {
		t.Fatalf("salts: %v", salts)
	}
	signer := NewKeySigner()
	if _, err := r.Join(joinReq(r, "WRNG-CODE", mac("Mini")), signer.PublicKeyB64()); !errors.Is(err, ErrBadCode) {
		t.Fatalf("a wrong code: %v", err)
	}
	req, token := joinReqWith(r, code, mac("Mini"))
	raw, _ := json.Marshal(req)
	for _, secret := range []string{code, NormalizeCode(code), token, TokenDigest(token), InferenceKey(TokenDigest(token))} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("the request carries %q", secret)
		}
	}
	// A credential sealed under another code, or tampered, isn't taken.
	tampered := req
	tampered.Attempts = append([]JoinAttempt(nil), req.Attempts...)
	for i := range tampered.Attempts {
		tampered.Attempts[i].Credential = SealCredential(JoinKey("ZZZZ-ZZZZ", tampered.Attempts[i].Salt), req.Nonce, TokenDigest(token))
	}
	if _, err := r.Join(tampered, signer.PublicKeyB64()); err == nil {
		t.Fatal("a credential sealed under another code was taken")
	}
	ans, err := r.Join(req, signer.PublicKeyB64())
	if err != nil {
		t.Fatal(err)
	}
	answer, _ := json.Marshal(ans)
	if strings.Contains(string(answer), TokenDigest(token)) || strings.Contains(string(answer), token) {
		t.Fatal("the answer carries the credential")
	}
	var ok *JoinAttempt
	for i, a := range req.Attempts {
		if _, err := CheckJoinAnswer(JoinKey(code, a.Salt), req.Nonce, a.Credential, *ans); err == nil {
			ok = &req.Attempts[i]
		}
	}
	if ok == nil || ans.FogKey != signer.PublicKeyB64() {
		t.Fatalf("answer: %+v", ans)
	}
	if n := authenticate(r, token); n == nil || n.ID != ans.Node {
		t.Fatal("fog didn't keep the machine's own credential")
	}
	key := JoinKey(code, ok.Salt)
	forged := *ans
	forged.FogKey = NewKeySigner().PublicKeyB64()
	if _, err := CheckJoinAnswer(key, req.Nonce, ok.Credential, forged); err == nil {
		t.Fatal("a swapped key passed")
	}
	if _, err := CheckJoinAnswer(key, req.Nonce, tampered.Attempts[0].Credential, *ans); err == nil {
		t.Fatal("an answer for another credential passed")
	}
	// The proof is good once: the code is spent.
	if _, err := r.Join(req, signer.PublicKeyB64()); !errors.Is(err, ErrBadCode) {
		t.Fatalf("a replayed pairing: %v", err)
	}
}

// pollMAC is a poll's headers as fog-node makes them.
func pollMAC(digest, node, nonce, at string, body []byte) (string, string) {
	sum := BodySHA256(body)
	return RequestMAC(digest, node, nonce, at, "application/json", sum), sum
}

// A machine's requests carry a MAC under its credential's digest, over its
// headers and declared body, and each is taken once, inside the skew
// window and not from before fog started. Each way a request fails says
// which: only a machine fog doesn't have is "unknown"; a bad MAC, a replay
// and a clock that's off are not, and the last shows on the page.
func TestMachineRequestsAreMACedAndTakenOnce(t *testing.T) {
	r, _ := testRegistry(t)
	ans := pair(t, r, mac("Mini"))
	other := pair(t, r, mac("Other"))
	body := []byte(`{"running":[]}`)
	digest := TokenDigest(ans.Token)
	at := time.Now().UTC().Format(time.RFC3339Nano)
	nonce := NewNonce()
	good, sum := pollMAC(digest, ans.Node, nonce, at, body)
	auth := func(node, nonce, at, mac, sum string) error {
		_, err := r.AuthenticateRequest(node, nonce, at, mac, "application/json", sum)
		return err
	}
	if err := auth(ans.Node, nonce, at, good, sum); err != nil {
		t.Fatalf("a good request refused: %v", err)
	}
	if err := auth(ans.Node, nonce, at, good, sum); !errors.Is(err, ErrReplayed) {
		t.Fatalf("a replayed request: %v", err)
	}
	n2 := NewNonce()
	long := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	g2, _ := pollMAC(digest, ans.Node, n2, at, body)
	for name, c := range map[string]struct {
		node, at, mac, sum string
		want               error
	}{
		"another body":     {ans.Node, at, g2, BodySHA256([]byte(`{"running":["x"]}`)), ErrBadRequestMAC},
		"as another node":  {other.Node, at, RequestMAC(digest, other.Node, n2, at, "application/json", sum), sum, ErrBadRequestMAC},
		"another key":      {ans.Node, at, RequestMAC(TokenDigest("fognode_guess"), ans.Node, n2, at, "application/json", sum), sum, ErrBadRequestMAC},
		"another type":     {ans.Node, at, RequestMAC(digest, ans.Node, n2, at, "text/plain", sum), sum, ErrBadRequestMAC},
		"a tweaked time":   {ans.Node, long, g2, sum, ErrBadRequestMAC},
		"the digest as it": {ans.Node, at, digest, sum, ErrBadRequestMAC},
		"no node":          {"", at, g2, sum, ErrUnknownMachine},
		"a node fog lacks": {"01NOSUCHMACHINE", at, g2, sum, ErrUnknownMachine},
	} {
		if err := auth(c.node, n2, c.at, c.mac, c.sum); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
	// Sent before this fog started: refused as a possible replay, though
	// well inside the skew window.
	r.epoch = time.Now()
	before := r.epoch.Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	n4 := NewNonce()
	m4, s4 := pollMAC(digest, ans.Node, n4, before, body)
	if err := auth(ans.Node, n4, before, m4, s4); !errors.Is(err, ErrReplayed) {
		t.Fatalf("from before fog started: %v", err)
	}
	r.epoch = time.Time{}
	// The machine itself, with its clock an hour slow: refused for that,
	// and the page says so until it's right.
	slow, _ := pollMAC(digest, ans.Node, n2, long, body)
	if err := auth(ans.Node, n2, long, slow, sum); !errors.Is(err, ErrClock) {
		t.Fatalf("an hour slow: %v", err)
	}
	if m, _ := r.Machine(ans.Node); !strings.Contains(m.ClockOff, "1h0m0s behind") {
		t.Fatalf("clock: %q", m.ClockOff)
	}
	n3 := NewNonce()
	m3, s3 := pollMAC(digest, ans.Node, n3, at, body)
	if err := auth(ans.Node, n3, at, m3, s3); err != nil {
		t.Fatal(err)
	}
	if m, _ := r.Machine(ans.Node); m.ClockOff != "" {
		t.Fatalf("still off: %q", m.ClockOff)
	}
}

// A machine rotates its credential right after pairing, by an exchange
// only it and fog share: the digest sealed at pairing (all a later crack
// of a recorded pairing could reveal) stops working once the machine is
// heard with the new one, and a rotation whose answer was lost can be
// asked again from the old.
func TestRotationRetiresThePairingsCredential(t *testing.T) {
	r, _ := testRegistry(t)
	s := &Service{Registry: r}
	ans := pair(t, r, mac("Mini"))
	old := TokenDigest(ans.Token)
	poll := func(digest string, rep NodeReport) (*NodeAnswer, error) {
		rep.Running = []string{}
		body, _ := json.Marshal(rep)
		nonce, at := NewNonce(), time.Now().UTC().Format(time.RFC3339Nano)
		m, sum := pollMAC(digest, ans.Node, nonce, at, body)
		n, err := r.AuthenticateRequest(ans.Node, nonce, at, m, "application/json", sum)
		if err != nil {
			return nil, err
		}
		return s.Poll(context.Background(), n, rep, "")
	}
	rotate := func(from string) string {
		t.Helper()
		priv, _ := ecdh.X25519().GenerateKey(rand.Reader)
		pub := base64.StdEncoding.EncodeToString(priv.PublicKey().Bytes())
		a, err := poll(from, NodeReport{Rotate: &RotateRequest{Pub: pub}})
		if err != nil || a.Rotated == nil {
			t.Fatalf("rotate: %+v %v", a, err)
		}
		raw, _ := base64.StdEncoding.DecodeString(a.Rotated.Pub)
		fp, _ := ecdh.X25519().NewPublicKey(raw)
		shared, _ := priv.ECDH(fp)
		return RotatedDigest(shared, ans.Node, from, pub, a.Rotated.Pub)
	}
	lost := rotate(old)
	// Its answer lost: the old credential still works, and rotates again.
	fresh := rotate(old)
	if fresh == lost {
		t.Fatal("two rotations agreed on the same credential")
	}
	if _, err := poll(lost, NodeReport{}); err == nil {
		t.Fatal("the lost rotation's credential works")
	}
	if _, err := poll(fresh, NodeReport{}); err != nil {
		t.Fatalf("the new credential: %v", err)
	}
	// Heard with the new one: the pairing's is done.
	if _, err := poll(old, NodeReport{}); !errors.Is(err, ErrBadRequestMAC) {
		t.Fatalf("the pairing's credential after: %v", err)
	}
	if m, _ := r.Machine(ans.Node); m.secret != InferenceKey(fresh) {
		t.Fatal("the inference key didn't follow")
	}
}
