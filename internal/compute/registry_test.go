package compute

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/fsx"
	"gitea.lan/matthew/fog/internal/store"
)

func testRegistry(t *testing.T) (*Registry, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &Registry{Store: st}, st
}

// mac is an M1 as fog-node reports it.
func mac(name string) Inventory {
	return Inventory{Name: name, OS: "darwin", Arch: "arm64", Version: "v1", Video: &VideoCaps{
		Family: "videotoolbox", FFmpeg: "9.0.2", Encoders: []string{"h264_videotoolbox", "libx264"},
		Filters: []string{"scale", "scale_vt", "zscale", "tonemap"}, Slots: Slots{Interactive: 4, Batch: 2},
		Confinement: "sandbox-exec",
	}}
}

// testFogKey is fog's instance key as pairing hands it out, in tests.
var testFogKey = NewKeySigner().PublicKeyB64()

// joinReq is a machine's request to pair with code: a proof, never the
// code, and its own credential's digest, sealed.
func joinReq(r *Registry, code string, inv Inventory) JoinRequest {
	req, _ := joinReqWith(r, code, inv)
	return req
}

// joinReqWith pairs as fog-node does: an attempt under each live code's
// salt, since it can't tell which is its code's.
func joinReqWith(r *Registry, code string, inv Inventory) (JoinRequest, string) {
	nonce := NewNonce()
	token := TokenPrefix + NewNonce()
	salts, _ := r.Salts()
	req := JoinRequest{Nonce: nonce, Inventory: inv}
	for _, salt := range salts {
		key := JoinKey(code, salt)
		req.Attempts = append(req.Attempts, JoinAttempt{Salt: salt, Proof: JoinProof(key, nonce), Credential: SealCredential(key, nonce, TokenDigest(token))})
	}
	return req, token
}

// paired is a machine as a test pairs it: its id, and the credential it
// made.
type paired struct{ Node, Token string }

// authenticate finds the machine a credential is (as AuthenticateRequest
// would, from its MAC).
func authenticate(r *Registry, token string) *store.ComputeNode {
	n, err := r.Store.ComputeNodeByToken(TokenDigest(token))
	if err != nil {
		return nil
	}
	return n
}

func pair(t *testing.T, r *Registry, inv Inventory) *paired {
	t.Helper()
	code, err := r.NewCode("admin")
	if err != nil {
		t.Fatal(err)
	}
	req, token := joinReqWith(r, code, inv)
	ans, err := r.Join(req, testFogKey)
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	return &paired{Node: ans.Node, Token: token}
}

func TestCodePairsOneMachineOnce(t *testing.T) {
	r, _ := testRegistry(t)
	code, err := r.NewCode("admin")
	if err != nil {
		t.Fatal(err)
	}
	// Typed however: lower case, without the dash, with spaces.
	req, token := joinReqWith(r, " "+string([]rune(code)[:4])+" "+string([]rune(code)[5:]), mac("Mini"))
	ans, err := r.Join(req, testFogKey)
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	if n := authenticate(r, token); n == nil || n.ID != ans.Node || n.Name != "Mini" || n.CreatedBy != "admin" {
		t.Fatalf("credential resolves to %+v", n)
	}
	if _, err := r.Join(joinReq(r, code, mac("Other")), testFogKey); !errors.Is(err, ErrBadCode) {
		t.Fatalf("second use of a code: %v", err)
	}
	if authenticate(r, "fognode_nonsense") != nil || authenticate(r, ans.Node) != nil {
		t.Fatal("a token fog never minted authenticated")
	}
}

func TestCodeRacesPairOnlyOne(t *testing.T) {
	r, _ := testRegistry(t)
	code, _ := r.NewCode("admin")
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := r.Join(joinReq(r, code, mac("Racer")), testFogKey); err == nil {
				mu.Lock()
				won++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if won != 1 {
		t.Fatalf("%d machines paired with one code", won)
	}
}

func TestJoinNeedsAName(t *testing.T) {
	r, _ := testRegistry(t)
	code, _ := r.NewCode("admin")
	if _, err := r.Join(joinReq(r, code, Inventory{}), testFogKey); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nameless machine: %v", err)
	}
}

func TestPoolsGroupMachinesByFamily(t *testing.T) {
	r, st := testRegistry(t)
	at := time.Unix(1_800_000_000, 0)
	r.now = func() time.Time { return at }
	mini := pair(t, r, mac("Mini"))
	studio := mac("Studio")
	studio.Video.Slots = Slots{Interactive: 16, Batch: 4}
	studio.Video.Filters = []string{"scale", "scale_vt"}
	pair(t, r, studio)
	cpu := mac("Old laptop")
	cpu.Video.Family, cpu.Video.Encoders = "software", []string{"libx264"}
	pair(t, r, cpu)
	nofilter := mac("Old ffmpeg")
	nofilter.Video.Filters = []string{"scale"}
	pair(t, r, nofilter)

	machines, err := r.Machines()
	if err != nil {
		t.Fatal(err)
	}
	why := map[string]string{}
	for _, m := range machines {
		why[m.Name] = m.Why
	}
	if why["Old laptop"] != "no hardware encoder (software)" || why["Old ffmpeg"] != "its ffmpeg has no scale_vt filter" {
		t.Fatalf("why not pooled: %v", why)
	}

	pools := Pools(machines)
	if len(pools) != 1 {
		t.Fatalf("pools: %+v", pools)
	}
	p := pools[0]
	if p.ID != "video/videotoolbox" || p.Name != "fog · VideoToolbox" || len(p.Machines) != 2 || p.Online != 2 {
		t.Fatalf("pool: %+v", p)
	}
	if p.Capacity != (Slots{Interactive: 20, Batch: 6}) {
		t.Fatalf("capacity: %+v", p.Capacity)
	}
	// Only the filters every machine has: an app's argv runs on any.
	if p.Hello.Encoder != "videotoolbox" || len(p.Hello.Filters) != 2 {
		t.Fatalf("hello: %+v", p.Hello)
	}

	// A machine unheard from leaves its pool's room, and the pool stays.
	at = at.Add(Online)
	n, _ := st.ComputeNode(mini.Node)
	if err := r.Heard(n, nil); err != nil {
		t.Fatal(err)
	}
	machines, _ = r.Machines()
	p = Pools(machines)[0]
	if p.Online != 1 || p.Capacity != (Slots{Interactive: 4, Batch: 2}) {
		t.Fatalf("after Studio went quiet: %+v", p)
	}
	at = at.Add(Online)
	machines, _ = r.Machines()
	if p = Pools(machines)[0]; p.Online != 0 || p.Capacity != (Slots{}) || len(p.Machines) != 2 {
		t.Fatalf("all offline: %+v", p)
	}
}

func TestBudgetPausedAndBusyShapeTheRoom(t *testing.T) {
	r, st := testRegistry(t)
	ans := pair(t, r, mac("Mini"))
	room := func() Slots {
		m, err := r.Machine(ans.Node)
		if err != nil {
			t.Fatal(err)
		}
		return m.Slots
	}
	if got := room(); got != (Slots{Interactive: 4, Batch: 2}) {
		t.Fatalf("its own: %+v", got)
	}
	if err := st.SetComputeNodeSettings(ans.Node, "Mini", `{"interactive":1,"batch":0}`, false); err != nil {
		t.Fatal(err)
	}
	if got := room(); got != (Slots{Interactive: 1}) {
		t.Fatalf("budget: %+v", got)
	}
	n, _ := st.ComputeNode(ans.Node)
	busy := mac("Mini")
	busy.Busy = true
	if err := r.Heard(n, &busy); err != nil {
		t.Fatal(err)
	}
	st.SetComputeNodeSettings(ans.Node, "Mini", "", false)
	if got := room(); got != (Slots{Interactive: 4}) {
		t.Fatalf("busy: %+v", got)
	}
	st.SetComputeNodeSettings(ans.Node, "Mini", "", true)
	if got := room(); got != (Slots{}) {
		t.Fatalf("paused: %+v", got)
	}
}

func TestHeardWritesInventoryAndThrottlesTouches(t *testing.T) {
	r, st := testRegistry(t)
	at := time.Unix(1_800_000_000, 0)
	r.now = func() time.Time { return at }
	ans := pair(t, r, mac("Mini"))
	n, _ := st.ComputeNode(ans.Node)
	first := n.LastSeenAt

	at = at.Add(5 * time.Second)
	r.Heard(n, nil)
	if n2, _ := st.ComputeNode(ans.Node); n2.LastSeenAt != first {
		t.Fatalf("touched within the throttle: %s → %s", first, n2.LastSeenAt)
	}
	at = at.Add(touchEvery)
	r.Heard(n, nil)
	if n2, _ := st.ComputeNode(ans.Node); n2.LastSeenAt == first {
		t.Fatal("not touched after the throttle")
	}
	changed := mac("Mini")
	changed.Video.FFmpeg = "9.1"
	if err := r.Heard(n, &changed); err != nil {
		t.Fatal(err)
	}
	m, _ := r.Machine(ans.Node)
	if m.Inventory.Video.FFmpeg != "9.1" {
		t.Fatalf("inventory not recorded: %+v", m.Inventory.Video)
	}
}

func TestPollHoldsUntilWorkArrives(t *testing.T) {
	r, st := testRegistry(t)
	s := &Service{Registry: r}
	ans := pair(t, r, mac("Mini"))
	n, _ := st.ComputeNode(ans.Node)

	done := make(chan *NodeAnswer)
	go func() {
		a, err := s.Poll(context.Background(), n, NodeReport{Running: []string{}, Wait: true}, "")
		if err != nil {
			t.Error(err)
		}
		done <- a
	}()
	time.Sleep(50 * time.Millisecond)
	s.deliver(ans.Node, []NodeRun{{ID: "r1"}}, []string{"r0"})
	select {
	case a := <-done:
		if len(a.Start) != 1 || a.Start[0].ID != "r1" || len(a.Stop) != 1 || a.Stop[0] != "r0" {
			t.Fatalf("answer: %+v", a)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the held poll didn't wake for work")
	}

	// Without Wait the poll answers at once, with nothing.
	start := time.Now()
	a, _ := s.Poll(context.Background(), n, NodeReport{Running: []string{}}, "")
	if time.Since(start) > time.Second || len(a.Start) != 0 {
		t.Fatalf("unheld poll: %+v after %v", a, time.Since(start))
	}
	// A poll given up by the node answers when it goes.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := s.Poll(ctx, n, NodeReport{Running: []string{}, Wait: true}, ""); err != nil {
		t.Fatal(err)
	}
}

// releasedBinaries serves dir's builds with a release manifest naming them,
// as fog's update source signs one (the signature is the machine's to
// check; "sig" here).
func releasedBinaries(dir, version string) *Binaries {
	b := &Binaries{Dir: dir, Version: version}
	b.Manifest = func(context.Context) ([]byte, []byte, error) {
		arts := map[string]map[string]string{}
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			sum, _ := fsx.FileSHA256(filepath.Join(dir, e.Name()))
			arts[e.Name()] = map[string]string{"url": e.Name(), "sha256": sum}
		}
		body, _ := json.Marshal(map[string]any{"version": b.Version, "artifacts": arts})
		return body, []byte("sig"), nil
	}
	return b
}

// fog offers an update only with a release manifest that names the build:
// without one there is nothing a machine would install, so nothing to
// offer, and the page says why.
func TestUpdateIsOfferedOnlyWithASignedManifest(t *testing.T) {
	r, st := testRegistry(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "fog-node-darwin-arm64"), "binary")
	b := releasedBinaries(dir, "v2")
	s := &Service{Registry: r, Version: "v2", Binaries: b}
	ans := pair(t, r, mac("Mini"))
	n, _ := st.ComputeNode(ans.Node)
	a, _ := s.Poll(context.Background(), n, NodeReport{Running: []string{}}, "")
	if a.Update == nil || !strings.Contains(string(a.Update.Manifest), a.Update.SHA256) || a.Update.ManifestSig != "sig" {
		t.Fatalf("update: %+v", a.Update)
	}
	for name, spoil := range map[string]func(){
		"no manifest": func() { b.Manifest = nil },
		"another build": func() {
			writeFile(t, filepath.Join(dir, "fog-node-darwin-arm64"), "changed")
			b.sums = nil
			b.Manifest = releasedBinaries(t.TempDir(), "v2").Manifest
		},
	} {
		spoil()
		s.RetryUpdate(ans.Node)
		if a, _ := s.Poll(context.Background(), n, NodeReport{Running: []string{}}, ""); a.Update != nil {
			t.Errorf("%s: offered %+v", name, a.Update)
		}
		if m, _ := s.Machine(ans.Node); m.Behind == nil || m.Behind.Error == "" {
			t.Errorf("%s: the page isn't told why: %+v", name, m.Behind)
		}
	}
}

func TestUpdateOffersThisReleasesBuild(t *testing.T) {
	r, st := testRegistry(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "fog-node-darwin-arm64"), "binary")
	s := &Service{Registry: r, Version: "v2", Binaries: releasedBinaries(dir, "v2")}
	ans := pair(t, r, mac("Mini"))
	n, _ := st.ComputeNode(ans.Node)
	a, err := s.Poll(context.Background(), n, NodeReport{Running: []string{}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if a.Update == nil || a.Update.Version != "v2" || a.Update.URL != "/compute/v1/fog-node/darwin-arm64" || len(a.Update.SHA256) != 64 {
		t.Fatalf("update: %+v", a.Update)
	}
	current := mac("Mini")
	current.Version = "v2"
	a, _ = s.Poll(context.Background(), n, NodeReport{Running: []string{}, Inventory: &current}, "")
	if a.Update != nil {
		t.Fatalf("offered an update to a node on this release: %+v", a.Update)
	}
	s.Version, s.Binaries.Version = "dev", "dev"

	n, _ = st.ComputeNode(ans.Node)
	stale := mac("Mini")
	a, _ = s.Poll(context.Background(), n, NodeReport{Running: []string{}, Inventory: &stale}, "")
	if a.Update != nil {
		t.Fatal("a development fog offered an update")
	}
}

func TestAMachineBehindIsOfferedOnceThenStuck(t *testing.T) {
	r, st := testRegistry(t)
	at := time.Now()
	r.now = func() time.Time { return at }
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "fog-node-darwin-arm64"), "binary")
	s := &Service{Registry: r, Version: "v2", Binaries: releasedBinaries(dir, "v2")}
	ans := pair(t, r, mac("Mini"))
	n, _ := st.ComputeNode(ans.Node)
	poll := func(rep NodeReport) *NodeAnswer {
		t.Helper()
		rep.Running = []string{}
		a, err := s.Poll(context.Background(), n, rep, "")
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	if a := poll(NodeReport{}); a.Update == nil || a.Update.Retry {
		t.Fatalf("first poll: %+v", a.Update)
	}
	// Offered once: a node that didn't take it isn't asked every poll.
	if a := poll(NodeReport{}); a.Update != nil {
		t.Fatal("offered again at once")
	}
	m, _ := s.Machine(ans.Node)
	if m.Behind == nil || m.Behind.Want != "v2" || m.Behind.Stuck {
		t.Fatalf("behind: %+v", m.Behind)
	}
	if len(s.Stuck()) != 0 {
		t.Fatal("stuck within the grace")
	}

	// The node says why it couldn't: stuck at once, with its reason.
	failed := mac("Mini")
	failed.UpdateFailed = &UpdateFailed{Version: "v2", Error: "permission denied"}
	poll(NodeReport{Inventory: &failed})
	if m, _ := s.Machine(ans.Node); !m.Behind.Stuck || m.Behind.Error != "permission denied" {
		t.Fatalf("failed: %+v", m.Behind)
	}
	if stuck := s.Stuck(); len(stuck) != 1 || stuck[0].ID != ans.Node {
		t.Fatalf("stuck: %+v", stuck)
	}

	// Update now wakes a held poll with the offer, marked to retry.
	got := make(chan *NodeAnswer)
	go func() { got <- poll(NodeReport{Wait: true}) }()
	time.Sleep(50 * time.Millisecond)
	s.RetryUpdate(ans.Node)
	select {
	case a := <-got:
		if a.Update == nil || !a.Update.Retry {
			t.Fatalf("retry: %+v", a.Update)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Update now didn't wake the held poll")
	}

	// Silent past the grace (a fog-node too old to say why) is stuck too.
	silent := mac("Mini")
	poll(NodeReport{Inventory: &silent})
	at = at.Add(UpdateGrace + time.Minute)
	if m, _ := s.Machine(ans.Node); !m.Behind.Stuck || m.Behind.Error != "" {
		t.Fatalf("silent: %+v", m.Behind)
	}

	current := mac("Mini")
	current.Version = "v2"
	poll(NodeReport{Inventory: &current})
	if m, _ := s.Machine(ans.Node); m.Behind != nil || len(s.Stuck()) != 0 {
		t.Fatalf("still behind on fog's release: %+v", m.Behind)
	}
}

func TestRemoveRevokes(t *testing.T) {
	r, _ := testRegistry(t)
	s := &Service{Registry: r}
	ans := pair(t, r, mac("Mini"))
	if err := s.Remove(ans.Node); err != nil {
		t.Fatal(err)
	}
	if authenticate(r, ans.Token) != nil {
		t.Fatal("a removed machine's credential still works")
	}
	if err := s.Remove(ans.Node); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("removing twice: %v", err)
	}
}

// Pairing isn't connecting: only the service's first poll is.
func TestFirstPollMarksTheMachineConnected(t *testing.T) {
	r, st := testRegistry(t)
	ans := pair(t, r, mac("Mini"))
	n, _ := st.ComputeNode(ans.Node)
	if n.ConnectedAt != "" {
		t.Fatalf("connected at pairing: %q", n.ConnectedAt)
	}
	if err := r.Heard(n, nil); err != nil {
		t.Fatal(err)
	}
	n, _ = st.ComputeNode(ans.Node)
	first := n.ConnectedAt
	if first == "" {
		t.Fatal("a poll didn't mark it connected")
	}
	r.Heard(n, nil)
	if n, _ = st.ComputeNode(ans.Node); n.ConnectedAt != first {
		t.Fatal("connected_at moved")
	}
}
