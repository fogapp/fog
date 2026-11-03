// Compute nodes: a machine pairs with a one-time code from Settings ›
// Compute, polls with the credential it got, and shows in the overview;
// removing it cuts the credential off at once.
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/compute"
)

func withComputeServer(t *testing.T) (*Server, http.Handler, *http.Cookie) {
	t.Helper()
	s, _ := newTestServer(t)
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "fog-node-darwin-arm64"), []byte("fog-node"), 0o755)
	s.compute = &compute.Service{Registry: &compute.Registry{Store: s.store}, Binaries: &compute.Binaries{Dir: bin, Version: "dev"}, Version: "dev",
		Signer: computeSigner}
	h := s.Handler()
	return s, h, setup(t, h)
}

// computeSigner is fog's instance identity in these tests.
var computeSigner = compute.NewKeySigner()

func withNodeBearer(token string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }
}

// asNodeAt is asNode with the machine's clock at at.
func asNodeAt(node, token, nonce, body string, at time.Time) func(*http.Request) {
	return func(r *http.Request) {
		ts := at.UTC().Format(time.RFC3339Nano)
		r.Header.Set(compute.HeaderNodeID, node)
		r.Header.Set(compute.HeaderNodeNonce, nonce)
		r.Header.Set(compute.HeaderNodeTime, ts)
		sum := compute.BodySHA256([]byte(body))
		r.Header.Set(compute.HeaderBodySHA256, sum)
		r.Header.Set(compute.HeaderNodeAuth, compute.RequestMAC(compute.TokenDigest(token), node, nonce, ts, "application/json", sum))
	}
}

func refusalCode(w *httptest.ResponseRecorder) string {
	var v struct{ Code string }
	json.Unmarshal(w.Body.Bytes(), &v)
	return v.Code
}

// asNode signs a poll as fog-node does: a MAC under the credential over
// the request, never the credential; nonce is the poll's.
func asNode(node, token, nonce, body string) func(*http.Request) {
	return func(r *http.Request) {
		at := time.Now().UTC().Format(time.RFC3339Nano)
		r.Header.Set(compute.HeaderNodeID, node)
		r.Header.Set(compute.HeaderNodeNonce, nonce)
		r.Header.Set(compute.HeaderNodeTime, at)
		sum := compute.BodySHA256([]byte(body))
		r.Header.Set(compute.HeaderBodySHA256, sum)
		r.Header.Set(compute.HeaderNodeAuth, compute.RequestMAC(compute.TokenDigest(token), node, nonce, at, "application/json", sum))
	}
}

func signedBy(w *httptest.ResponseRecorder, node, nonce string) bool {
	pub, _ := compute.ParseFogKey(computeSigner.PublicKeyB64())
	return compute.VerifyAnswer(pub, node, nonce, w.Body.Bytes(), w.Header().Get(compute.HeaderFogSignature))
}

func TestComputePairPollAndRemove(t *testing.T) {
	_, h, c := withComputeServer(t)

	w := do(t, h, "POST", "/api/compute/codes", "", withCookie(c), withOrigin("http://example.com"))
	if w.Code != http.StatusOK {
		t.Fatalf("code: %d %s", w.Code, w.Body)
	}
	var code struct{ Code, Command string }
	json.Unmarshal(w.Body.Bytes(), &code)
	if code.Command != "curl -fsSL http://example.com/compute/v1/install.sh | FOG_PAIR_CODE="+code.Code+" sh -s -- http://example.com" {
		t.Fatalf("command: %q", code.Command)
	}

	// The live codes' salts, then a pairing under the one there is.
	w = do(t, h, "POST", "/compute/v1/join", `{}`)
	var salts compute.JoinSalts
	json.Unmarshal(w.Body.Bytes(), &salts)
	if w.Code != http.StatusOK || len(salts.Salts) != 1 {
		t.Fatalf("salts: %d %s", w.Code, w.Body)
	}
	nonce := compute.NewNonce()
	key := compute.JoinKey(code.Code, salts.Salts[0])
	// The machine's own credential: fog only ever gets its digest, sealed.
	token := compute.TokenPrefix + compute.NewNonce()
	sealed := compute.SealCredential(key, nonce, compute.TokenDigest(token))
	join, _ := json.Marshal(compute.JoinRequest{Nonce: nonce, Attempts: []compute.JoinAttempt{{Salt: salts.Salts[0], Proof: compute.JoinProof(key, nonce), Credential: sealed}}, Inventory: compute.Inventory{Name: "Mini", OS: "darwin", Arch: "arm64",
		Video: &compute.VideoCaps{Family: "videotoolbox", Encoders: []string{"h264_videotoolbox"}, Filters: []string{"scale_vt"},
			Slots: compute.Slots{Interactive: 4, Batch: 2}, Confinement: "sandbox-exec"}}})
	w = do(t, h, "POST", "/compute/v1/join", string(join))
	if w.Code != http.StatusOK {
		t.Fatalf("join: %d %s", w.Code, w.Body)
	}
	var ans compute.JoinAnswer
	json.Unmarshal(w.Body.Bytes(), &ans)
	if _, err := compute.CheckJoinAnswer(key, nonce, sealed, ans); err != nil || ans.FogKey != computeSigner.PublicKeyB64() {
		t.Fatalf("join answer: %+v %v", ans, err)
	}
	if w = do(t, h, "POST", "/compute/v1/join", string(join)); w.Code != http.StatusForbidden {
		t.Fatalf("code reused: %d", w.Code)
	}

	if w = do(t, h, "POST", "/compute/v1/poll", `{"running":[]}`); w.Code != http.StatusUnauthorized || refusalCode(w) != compute.CodeUnknownMachine {
		t.Fatalf("poll naming no machine: %d", w.Code)
	}
	// There is no bearer way in, even with the machine's real credential.
	if w = do(t, h, "POST", "/compute/v1/poll", `{"running":[]}`, withNodeBearer(token)); w.Code == http.StatusOK {
		t.Fatalf("a bearer was taken: %d %s", w.Code, w.Body)
	}
	// As fog-node polls: proven, not sent; answered signed for it.
	poll := `{"running":[]}`
	n1 := compute.NewNonce()
	if w = do(t, h, "POST", "/compute/v1/poll", poll, asNode(ans.Node, token, n1, poll)); w.Code != http.StatusOK || !signedBy(w, ans.Node, n1) {
		t.Fatalf("a MACed poll: %d %s signed=%v", w.Code, w.Body, signedBy(w, ans.Node, n1))
	}
	// Someone in between who replays the poll, tampers with its time, or
	// sends it with the wrong credential gets a signed refusal that is not
	// "unknown machine": nothing that would unpair the machine.
	tampered := asNode(ans.Node, token, compute.NewNonce(), poll)
	for name, c := range map[string]struct {
		opt  func(*http.Request)
		code string
	}{
		"replayed":  {asNode(ans.Node, token, n1, poll), compute.CodeReplay},
		"wrong key": {asNode(ans.Node, "fognode_guess", compute.NewNonce(), poll), compute.CodeBadRequest},
		"time tampered": {func(r *http.Request) {
			tampered(r)
			r.Header.Set(compute.HeaderNodeTime, time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano))
		}, compute.CodeBadRequest},
	} {
		nonce := compute.NewNonce()
		w = do(t, h, "POST", "/compute/v1/poll", poll, c.opt, func(r *http.Request) { nonce = r.Header.Get(compute.HeaderNodeNonce) })
		if w.Code != http.StatusForbidden || refusalCode(w) != c.code || !signedBy(w, ans.Node, nonce) {
			t.Errorf("%s: %d %s signed=%v", name, w.Code, w.Body, signedBy(w, ans.Node, nonce))
		}
	}
	// Someone who swaps the body under a MACed poll's headers is refused
	// by its declared digest.
	if w = do(t, h, "POST", "/compute/v1/poll", `{"running":["evil"]}`, asNode(ans.Node, token, compute.NewNonce(), poll)); w.Code != http.StatusForbidden ||
		refusalCode(w) != compute.CodeBadRequest {
		t.Fatalf("a swapped body: %d %s", w.Code, w.Body)
	}
	// The machine itself with its clock an hour fast: refused for that, and
	// the page shows it.
	nc := compute.NewNonce()
	if w = do(t, h, "POST", "/compute/v1/poll", poll, asNodeAt(ans.Node, token, nc, poll, time.Now().Add(time.Hour))); w.Code != http.StatusForbidden ||
		refusalCode(w) != compute.CodeClock || !signedBy(w, ans.Node, nc) {
		t.Fatalf("clock: %d %s", w.Code, w.Body)
	}
	w = do(t, h, "GET", "/api/compute", "", withCookie(c))
	if !strings.Contains(w.Body.String(), "ahead of fog") {
		t.Fatalf("the page doesn't say the clock is off: %s", w.Body)
	}
	if w = do(t, h, "POST", "/compute/v1/poll", poll, asNode(ans.Node, token, compute.NewNonce(), poll)); w.Code != http.StatusOK {
		t.Fatalf("a poll after: %d", w.Code)
	}

	w = do(t, h, "GET", "/api/compute", "", withCookie(c))
	var ov computeOverview
	json.Unmarshal(w.Body.Bytes(), &ov)
	if len(ov.Machines) != 1 || !ov.Machines[0].Online || ov.Machines[0].Pool != "video/videotoolbox" {
		t.Fatalf("machines: %+v", ov.Machines)
	}
	if len(ov.Pools) != 1 || ov.Pools[0].Capacity != (compute.Slots{Interactive: 4, Batch: 2}) {
		t.Fatalf("pools: %+v", ov.Pools)
	}
	if w = do(t, h, "GET", "/api/compute", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("overview without a session: %d", w.Code)
	}

	w = do(t, h, "PATCH", "/api/compute/nodes/"+ans.Node, `{"name":"Mac mini","paused":true,"budget":{"interactive":2,"batch":0}}`,
		withCookie(c), withOrigin("http://example.com"))
	var m compute.Machine
	json.Unmarshal(w.Body.Bytes(), &m)
	if w.Code != http.StatusOK || m.Name != "Mac mini" || !m.Paused || m.Budget == nil || m.Budget.Interactive != 2 || m.Slots != (compute.Slots{}) {
		t.Fatalf("update: %d %+v", w.Code, m)
	}
	if w = do(t, h, "PATCH", "/api/compute/nodes/"+ans.Node, `{"budget":{"interactive":99,"batch":0}}`, withCookie(c)); w.Code != http.StatusBadRequest {
		t.Fatalf("budget out of range: %d", w.Code)
	}

	if w = do(t, h, "DELETE", "/api/compute/nodes/"+ans.Node, "", withCookie(c)); w.Code != http.StatusOK {
		t.Fatalf("remove: %d %s", w.Code, w.Body)
	}
	// Told signed that fog has no such machine, so it believes it and
	// forgets.
	n2 := compute.NewNonce()
	if w = do(t, h, "POST", "/compute/v1/poll", poll, asNode(ans.Node, token, n2, poll)); w.Code != http.StatusUnauthorized ||
		refusalCode(w) != compute.CodeUnknownMachine || !signedBy(w, ans.Node, n2) {
		t.Fatalf("removed: %d signed=%v", w.Code, signedBy(w, ans.Node, n2))
	}
}

func TestComputeInstallerAndBinaries(t *testing.T) {
	_, h, _ := withComputeServer(t)
	w := do(t, h, "GET", "/compute/v1/install.sh", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"$BIN" join --dir "$DIR" --fog "$FOG"`) {
		t.Fatalf("installer: %d", w.Code)
	}
	w = do(t, h, "GET", "/compute/v1/fog-node/darwin-arm64", "")
	if w.Code != http.StatusOK || w.Body.String() != "fog-node" || len(w.Header().Get("X-Fog-Node-Sha256")) != 64 {
		t.Fatalf("binary: %d %q", w.Code, w.Body)
	}
	w = do(t, h, "GET", "/compute/v1/fog-node/darwin-arm64.sha256", "")
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != w.Header().Get("X-Fog-Node-Sha256") && len(strings.TrimSpace(w.Body.String())) != 64 {
		t.Fatalf("digest: %d %q", w.Code, w.Body)
	}
	if w = do(t, h, "GET", "/compute/v1/fog-node/linux-arm64", ""); w.Code != http.StatusNotFound {
		t.Fatalf("missing build: %d", w.Code)
	}
}

func TestComputeUnavailableWithoutTheService(t *testing.T) {
	_, h := newTestServer(t)
	if w := do(t, h, "POST", "/compute/v1/poll", `{}`, withNodeBearer("fognode_x")); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("poll: %d", w.Code)
	}
	c := setup(t, h)
	if w := do(t, h, "GET", "/api/compute", "", withCookie(c)); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"machines":[]`) {
		t.Fatalf("overview: %d %s", w.Code, w.Body)
	}
}

func TestComputeGrantSwitchesAPoolForAnApp(t *testing.T) {
	s, h, c := withComputeServer(t)
	app, err := s.store.CreateApp("stream")
	if err != nil {
		t.Fatal(err)
	}
	if w := do(t, h, "PUT", "/api/compute/grants", `{"app":"stream","pool":"video/videotoolbox","on":false}`, withCookie(c)); w.Code != http.StatusOK {
		t.Fatalf("off: %d %s", w.Code, w.Body)
	}
	if off, _ := s.store.ComputeGrantOff(app.ID, "video/videotoolbox"); !off {
		t.Fatal("not switched off")
	}
	do(t, h, "PUT", "/api/compute/grants", `{"app":"stream","pool":"video/videotoolbox","on":true}`, withCookie(c))
	if off, _ := s.store.ComputeGrantOff(app.ID, "video/videotoolbox"); off {
		t.Fatal("not switched on again")
	}
	if w := do(t, h, "PUT", "/api/compute/grants", `{"app":"nope","pool":"video/videotoolbox","on":false}`, withCookie(c)); w.Code != http.StatusNotFound {
		t.Fatalf("unknown app: %d", w.Code)
	}
	if w := do(t, h, "PUT", "/api/compute/grants", `{"app":"stream","pool":"../x","on":false}`, withCookie(c)); w.Code != http.StatusBadRequest {
		t.Fatalf("bad pool: %d", w.Code)
	}
}
