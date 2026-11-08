package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/fed"
	"gitea.lan/matthew/fog/internal/filesfed"
	"gitea.lan/matthew/fog/internal/store"
)

// Finding 1, the inviting side. B is paired with A and holds a grant there.
// Mallory gets hold of one of A's invite codes and pairs claiming B's
// instance ID under her own key. That used to replace B's key in A's row,
// grants and all: from then on Mallory was B to A. Now the pairing is
// refused, B's row is untouched, Mallory's key opens nothing, and A's
// operator is told.
func TestInviteCannotRekeyAPairedHousehold(t *testing.T) {
	t.Parallel()

	p := twoFogs(t, `[{"capability":"apps.introduce"}]`)
	bID := mustIdentity(t, p.b)
	mallory := mustIdentity(t, mustServer(t))
	code := mintInvite(t, p.aH, p.aCookie, `[{"capability":"flare.ingest"}]`)
	inv, err := fed.ParseInvite(code)
	if err != nil {
		t.Fatal(err)
	}

	// A complete, well-formed pair request: Mallory's own keys, her
	// encryption key bound to B's instance ID under her identity key,
	// sealed to the key A's invite carries.
	body, _ := json.Marshal(fed.PairRequest{Secret: inv.Secret, Instance: fed.InstanceDoc{
		InstanceID: bID.InstanceID, Name: "b", URL: "https://evil.example", PublicKey: mallory.PublicKeyB64(),
		EncryptionKey: mallory.EncPublicB64(), EncryptionKeySig: mallory.SignTranscript("fogfed1-enckey", bID.InstanceID, mallory.EncPublic),
	}})
	req, _ := http.NewRequest("POST", p.aHTTP.URL+"/fed/v1/pair", nil)
	req.Header.Set("Content-Type", "application/json")
	// The pair hop is verified against the key inside the request, under the
	// instance header it names — which Mallory controls.
	req.Header.Set(fed.HeaderInstance, bID.InstanceID)
	inviteKey, _ := fed.ParseEncKey(inv.EncKey)
	// SealRequest signs as Mallory's own instance; the pair hop takes the
	// instance from the header, so seal for B's.
	asB := *mallory
	asB.InstanceID = bID.InstanceID
	if err := fed.SealRequest(&asB, req, fed.PairRecipient(inv.EncKey), inviteKey, body); err != nil {
		t.Fatal(err)
	}
	aID := mustIdentity(t, p.a)
	// B's instance ID is derived from B's key, so Mallory's key can't carry
	// it at all: refused before the invite is even claimed.
	if status, body := sendFedAs(t, req, &asB, aID.Public, fed.PairRecipient(inv.EncKey)); status != http.StatusForbidden || !strings.Contains(string(body), "belongs to another key") {
		t.Fatalf("claiming B's derived instance ID: status %d: %s", status, body)
	}

	row, err := p.a.store.FedPeerByID(p.bPeerOnA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.PublicKey != bID.PublicKeyB64() || row.URL != p.bPeerOnA.URL {
		t.Fatalf("A's row for B was changed by the refused pairing: %+v", row)
	}
	if _, ok, _ := p.a.store.FedCapabilityConfig(row.ID, store.CapFlareIngest, store.DirGrant); ok {
		t.Fatal("the refused invite's grants landed on B's row")
	}

	// A household with a legacy (random) instance ID can't be checked that
	// way; claiming it under another key meets the no-rekey rule instead:
	// 409, the row untouched, the operator told.
	const legacy = "01JV1CT1M0000000000000000A"
	old := mustIdentity(t, mustServer(t))
	lrow, err := p.a.store.CreatePairedPeer(legacy, "legacy", "https://legacy.example", old.PublicKeyB64())
	if err != nil {
		t.Fatal(err)
	}
	body, _ = json.Marshal(fed.PairRequest{Secret: inv.Secret, Instance: fed.InstanceDoc{
		InstanceID: legacy, Name: "legacy", URL: "https://evil.example", PublicKey: mallory.PublicKeyB64(),
		EncryptionKey: mallory.EncPublicB64(), EncryptionKeySig: mallory.SignTranscript("fogfed1-enckey", legacy, mallory.EncPublic),
	}})
	req, _ = http.NewRequest("POST", p.aHTTP.URL+"/fed/v1/pair", nil)
	req.Header.Set("Content-Type", "application/json")
	asLegacy := *mallory
	asLegacy.InstanceID = legacy
	if err := fed.SealRequest(&asLegacy, req, fed.PairRecipient(inv.EncKey), inviteKey, body); err != nil {
		t.Fatal(err)
	}
	if status, body := sendFedAs(t, req, &asLegacy, aID.Public, fed.PairRecipient(inv.EncKey)); status != http.StatusConflict {
		t.Fatalf("re-key of a legacy ID by invite: status %d, want 409: %s", status, body)
	}
	if got, _ := p.a.store.FedPeerByID(lrow.ID); got.PublicKey != old.PublicKeyB64() || got.URL != "https://legacy.example" {
		t.Fatalf("the legacy row was changed: %+v", got)
	}
	if _, ok, _ := p.a.store.FedCapabilityConfig(lrow.ID, store.CapFlareIngest, store.DirGrant); ok {
		t.Fatal("the refused invite's grants landed on the legacy row")
	}
	n, _ := p.a.store.CountActiveNotices()
	if n == 0 {
		t.Fatal("A's operator was not told")
	}

	// Mallory, signing as "B", is refused; B still gets in.
	hb := []byte(`{"fogVersion":"x","grants":[],"directory":[]}`)
	req, _ = http.NewRequest("POST", p.aHTTP.URL+"/fed/v1/heartbeat", nil)
	sealFor(t, &asB, p.a, req, hb)
	if status, body := sendFed(t, req, &asB, p.a); status != http.StatusForbidden {
		t.Fatalf("Mallory's heartbeat as B: %d %s", status, body)
	}
	if err := p.b.fed.HeartbeatPeer(context.Background(), p.aPeerOnB); err != nil {
		t.Fatalf("B after the refused re-key: %v", err)
	}
}

// Finding 1, the joining side. Mallory hands B's operator an invite whose
// pair answer claims A's instance ID under her key. B refuses, keeps A as
// it was, and says why.
func TestJoinCannotRekeyAPairedHousehold(t *testing.T) {
	t.Parallel()

	p := twoFogs(t, `[]`)
	aID := mustIdentity(t, p.a)
	mallory := mustIdentity(t, mustServer(t))
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A complete, well-formed inviter: it opens the sealed request,
		// binds its own encryption key to A's instance ID under its own
		// identity key, and seals its answer back.
		self := fed.PairRecipient(mallory.EncPublicB64())
		raw, _ := io.ReadAll(r.Body)
		sr, _, err := fed.OpenSealedRequest(mallory, r, raw, self)
		if err != nil || sr.Hello {
			t.Errorf("evil inviter can't open the pair request: %v", err)
			return
		}
		plain, _ := io.ReadAll(sr.Inner.Body)
		var req fed.PairRequest
		json.Unmarshal(plain, &req)
		doc, _ := json.Marshal(fed.InstanceDoc{InstanceID: aID.InstanceID, Name: "a", URL: "https://evil.example",
			PublicKey: mallory.PublicKeyB64(), EncryptionKey: mallory.EncPublicB64(),
			EncryptionKeySig: mallory.SignTranscript("fogfed1-enckey", aID.InstanceID, mallory.EncPublic)})
		answer, _ := json.Marshal(fed.PairResponse{Instance: doc, Grants: []string{"chat"}, Sig: fed.PairSign(mallory, req.Secret, doc)})
		pk, _ := fed.ParseEncKey(req.Instance.EncryptionKey)
		sum := sha256.Sum256(answer)
		fed.WriteSealedAnswer(w, mallory, pk, sr.AnswerInfo(self), sr.Target, sr.Bound, http.StatusOK, nil, hex.EncodeToString(sum[:]), bytes.NewReader(answer))
	}))
	defer evil.Close()

	w := do(t, p.bH, "POST", "/api/federation/join", fmt.Sprintf(`{"code":%q}`, fed.InviteCode(evil.URL, "s3cret", "A", mallory.EncPublicB64())), withCookie(p.bCookie))
	// A's instance ID is derived from A's key: an answer carrying it under
	// another key is refused outright.
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "belongs to another key") {
		t.Fatalf("join re-keying A: %d %s", w.Code, w.Body)
	}
	row, _ := p.b.store.FedPeerByID(p.aPeerOnB.ID)
	if row.PublicKey != aID.PublicKeyB64() || row.URL != p.aPeerOnB.URL {
		t.Fatalf("B's row for A changed: %+v", row)
	}
}

// Finding 4, at the API edge: the two shapes the peer settings write are
// accepted; anything else is refused rather than stored for a reader to
// guess at. Invites meet the same rules.
func TestCapabilityConfigShapesAreChecked(t *testing.T) {
	t.Parallel()

	p := twoFogs(t, `[]`)
	put := func(c, dir, config string) int {
		body := fmt.Sprintf(`{"capabilities":[{"capability":%q,"direction":%q,"enabled":true,"config":%s}]}`, c, dir, config)
		return do(t, p.aH, "PUT", fmt.Sprintf("/api/federation/peers/%d", p.bPeerOnA.ID), body, withCookie(p.aCookie)).Code
	}
	for _, ok := range [][3]string{
		{"apps.directory", "grant", `{}`},
		{"apps.directory", "grant", `{"apps":["feed"]}`},
		{"apps.directory", "grant", `{"apps":[]}`},
		{"apps.introduce", "grant", `{"apps":["feed"]}`},
		{"people.directory", "grant", `{"people":"members"}`},
		{"people.visit", "grant", `{"people":["sam"]}`},
		{"chat", "grant", `{"acceptThirdHouseholds":false}`},
		{"flare.ingest", "use", `{"mode":"redacted"}`},
		{"files.share", "use", `{"bandwidthBps":1000000}`},
	} {
		if code := put(ok[0], ok[1], ok[2]); code != http.StatusOK {
			t.Errorf("%s %s %s refused: %d", ok[0], ok[1], ok[2], code)
		}
	}
	for _, bad := range [][3]string{
		{"apps.directory", "grant", `{"apps":"all"}`},
		{"apps.directory", "grant", `{"visible":true}`},
		{"apps.introduce", "grant", `{"apps":[""]}`},
		{"people.directory", "grant", `{"people":"everyone"}`},
		{"people.visit", "grant", `{"people":{"sam":true}}`},
		{"people.directory", "grant", `{}`},
		{"people.visit", "grant", `{}`},
		{"people.directory", "grant", `{"people":null}`},
		{"chat", "grant", `{"acceptThirdHouseholds":"no"}`},
		{"flare.ingest", "use", `{"mode":"loud"}`},
		{"flare.ingest", "use", `{"mode":"full","apps":[]}`},
	} {
		if code := put(bad[0], bad[1], bad[2]); code != http.StatusBadRequest {
			t.Errorf("%s %s %s accepted: %d", bad[0], bad[1], bad[2], code)
		}
	}
	if w := do(t, p.aH, "POST", "/api/federation/invites", `{"grants":[{"capability":"apps.directory","config":{"apps":"all"}}]}`, withCookie(p.aCookie)); w.Code != http.StatusBadRequest {
		t.Fatalf("an invite with an unreadable scope: %d", w.Code)
	}
	// A people grant with no people in it grants nobody; it is refused with
	// a reason, not stored.
	w := do(t, p.aH, "POST", "/api/federation/invites", `{"grants":[{"capability":"people.visit"}]}`, withCookie(p.aCookie))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "people scope") {
		t.Fatalf("an invite granting people.visit to nobody: %d %s", w.Code, w.Body)
	}
}

// Connect's allow-list is exactly its config: [] connects nothing (it used
// to mean every contracted app). And withdrawing or narrowing the grant
// reaches the credentials already minted, as unpairing does.
func TestConnectGrantIsExactAndWithdrawalRevokes(t *testing.T) {
	t.Parallel()

	cp := newConnectPair(t, `[{"capability":"apps.introduce","config":{"apps":[]}}]`, true)
	var mu sync.Mutex
	var revoked []string
	revoker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, AppGrantsPath+"/") {
			mu.Lock()
			revoked = append(revoked, strings.TrimPrefix(r.URL.Path, AppGrantsPath+"/"))
			mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer revoker.Close()

	status, body := cp.fedConnect(t, cp.vouchFromB(t, "n-1"), fed.SignVouch(mustIdentity(t, cp.b), cp.vouchFromB(t, "n-1")), "")
	if status != http.StatusForbidden || cp.grantCalls.Load() != 0 {
		t.Fatalf("an empty allow-list connected: %d %s", status, body)
	}

	cp.a.appGrantEndpointFn = func(context.Context, *store.App) (string, string, error) { return revoker.URL, "t", nil }
	put := func(enabled bool, config string) {
		t.Helper()
		b := fmt.Sprintf(`{"capabilities":[{"capability":"apps.introduce","direction":"grant","enabled":%v,"config":%s}]}`, enabled, config)
		if w := do(t, cp.aH, "PUT", fmt.Sprintf("/api/federation/peers/%d", cp.bPeerOnA.ID), b, withCookie(cp.aCookie)); w.Code != http.StatusOK {
			t.Fatalf("put: %d %s", w.Code, w.Body)
		}
		cp.a.bgWG.Wait()
	}
	put(true, `{}`)
	if len(revoked) != 0 {
		t.Fatalf("widening the grant revoked: %v", revoked)
	}
	put(true, `{"apps":["hub"]}`) // feed no longer covered
	put(false, `{}`)
	mu.Lock()
	defer mu.Unlock()
	bInst := mustIdentity(t, cp.b).InstanceID
	if len(revoked) != 2 || revoked[0] != bInst || revoked[1] != bInst {
		t.Fatalf("narrowing then withdrawing apps.introduce should each revoke feed's credentials for B: %v", revoked)
	}
}

// Withdrawing files.share stops every share to the peer — told "revoked",
// rows gone — so a later share can't silently bring the old ones back by
// re-writing the grant.
func TestWithdrawingFilesGrantStopsEveryShare(t *testing.T) {
	t.Parallel()

	p := twoFogs(t, `[{"capability":"files.share"}]`)
	root := p.a.files.Root()
	os.MkdirAll(filepath.Join(root, "Old"), 0o755)
	os.MkdirAll(filepath.Join(root, "New"), 0o755)
	if _, err := p.a.filesFed.CreateShare(&store.FileFedShare{PeerID: p.bPeerOnA.ID, Path: "Old", Title: "Old",
		Mode: filesfed.ModeRead, Scope: filesfed.ScopeAll, Detail: filesfed.DetailFull, Apps: filesfed.AppsDenied}); err != nil {
		t.Fatal(err)
	}
	b := `{"capabilities":[{"capability":"files.share","direction":"grant","enabled":false}]}`
	if w := do(t, p.aH, "PUT", fmt.Sprintf("/api/federation/peers/%d", p.bPeerOnA.ID), b, withCookie(p.aCookie)); w.Code != http.StatusOK {
		t.Fatalf("withdraw: %d %s", w.Code, w.Body)
	}
	if _, err := p.a.filesFed.CreateShare(&store.FileFedShare{PeerID: p.bPeerOnA.ID, Path: "New", Title: "New",
		Mode: filesfed.ModeRead, Scope: filesfed.ScopeAll, Detail: filesfed.DetailFull, Apps: filesfed.AppsDenied}); err != nil {
		t.Fatal(err)
	}
	var cat struct {
		Shares []filesfed.CatalogEntry `json:"shares"`
	}
	if _, err := p.b.fed.SignedGetJSON(context.Background(), p.aPeerOnB, "/fed/v1/files/catalog", "", &cat); err != nil {
		t.Fatal(err)
	}
	if len(cat.Shares) != 1 || cat.Shares[0].Title != "New" {
		t.Fatalf("after withdrawing the grant and sharing anew, B sees %+v", cat.Shares)
	}
}

// Every refusal on /fed/v1 is signed for the request it refuses, so the
// dialing side can tell the peer's "no" from anybody else's.
func TestFedRefusalsAreSignedForTheRequest(t *testing.T) {
	t.Parallel()

	p := twoFogs(t, `[]`)
	aID := mustIdentity(t, p.a)
	stranger := mustIdentity(t, mustServer(t))
	body := []byte(`{}`)
	req, _ := http.NewRequest("POST", p.aHTTP.URL+"/fed/v1/flare/envelope", nil)
	sealFor(t, mustIdentity(t, p.b), p.a, req, body)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	wire, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a refusal left the envelope: %d", resp.StatusCode)
	}
	// Opened as B, verified under A's key, for B's request: a 403.
	resp.Body = io.NopCloser(bytes.NewReader(wire))
	head, _, err := fed.OpenAnswer(mustIdentity(t, p.b), aID.Public, aID.InstanceID, resp)
	if err != nil || head.Status != http.StatusForbidden {
		t.Fatalf("A's refusal is not signed for B's request: %+v %v", head, err)
	}
	// The same bytes, taken as the answer to another caller's request,
	// don't verify (nor open).
	resp.Body = io.NopCloser(bytes.NewReader(wire))
	resp.Request.Header.Set(fed.HeaderInstance, stranger.InstanceID)
	if _, _, err := fed.OpenAnswer(mustIdentity(t, p.b), aID.Public, aID.InstanceID, resp); err == nil {
		t.Fatal("A's refusal verified for another caller")
	}
}

// A captured request, sent again inside its five-minute window, verifies —
// and is refused, because its signature is already in the replay record.
// The record lives in the database, so a restart doesn't forget it.
func TestReplayedRequestIsRefused(t *testing.T) {
	t.Parallel()

	p := twoFogs(t, `[]`)
	body := []byte(`{"fogVersion":"x","grants":[],"directory":[],"url":"https://moved.example"}`)
	req, _ := http.NewRequest("POST", p.aHTTP.URL+"/fed/v1/heartbeat", nil)
	sealFor(t, mustIdentity(t, p.b), p.a, req, body)
	wire, _ := io.ReadAll(req.Body)
	send := func() (int, string) {
		r2, _ := http.NewRequest("POST", req.URL.String(), bytes.NewReader(wire))
		r2.Header = req.Header.Clone()
		status, raw := sendFed(t, r2, mustIdentity(t, p.b), p.a)
		var out struct {
			Code string `json:"code"`
		}
		json.Unmarshal(raw, &out)
		return status, out.Code
	}
	if status, _ := send(); status != http.StatusOK {
		t.Fatalf("first delivery: %d", status)
	}
	p.a.store.UpdateFedPeerURL(p.bPeerOnA.ID, "https://fixed.example")
	if status, code := send(); status != http.StatusForbidden || code != fed.CodeReplayed {
		t.Fatalf("the same signed request again: %d %q", status, code)
	}
	// Every other spelling of the same signature — base64url's last
	// character has spare bits — is the same request, not a new one.
	sig := req.Header.Get(fed.HeaderSignature)
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	v := strings.IndexByte(alphabet, sig[len(sig)-1])
	for extra := 1; extra < 16; extra++ {
		r2, _ := http.NewRequest("POST", req.URL.String(), bytes.NewReader(wire))
		r2.Header = req.Header.Clone()
		r2.Header.Set(fed.HeaderSignature, sig[:len(sig)-1]+string(alphabet[v|extra]))
		if status, _ := sendFed(t, r2, mustIdentity(t, p.b), p.a); status != http.StatusForbidden {
			t.Fatalf("a respelled signature (spare bits %d) was accepted: %d", extra, status)
		}
	}
	if got, _ := p.a.store.FedPeerByID(p.bPeerOnA.ID); got.URL != "https://fixed.example" {
		t.Fatalf("the replay was applied: %s", got.URL)
	}
	// The record is in the store, not in this process.
	digest, _ := fed.RequestDigest(fed.BoundTo(req.Header))
	fresh, err := p.a.store.ClaimFedRequest(digest, time.Now().Add(time.Minute))
	if err != nil || fresh {
		t.Fatalf("the replay record forgot the request: fresh=%v err=%v", fresh, err)
	}
}

// sealFor seals body for a request from `from` to the fog to, sets it on
// req, and signs req: a hand-built fed request, as fogd's client sends one.
func sealFor(t *testing.T, from *fed.Identity, to *Server, req *http.Request, body []byte) {
	t.Helper()
	toID := mustIdentity(t, to)
	pk, err := fed.ParseEncKey(toID.EncPublicB64())
	if err != nil {
		t.Fatal(err)
	}
	if err := fed.SealRequest(from, req, toID.InstanceID, pk, body); err != nil {
		t.Fatal(err)
	}
}

// sendFed sends a hand-built envelope request to the fog at `to` and opens
// its answer as `as` (nil: don't open), returning the inner status and
// body. An answer outside the envelope (a refusal to a caller that
// presented no key, or to an old-style path) comes back as it is.
func sendFed(t *testing.T, req *http.Request, as *fed.Identity, to *Server) (int, []byte) {
	t.Helper()
	toID := mustIdentity(t, to)
	return sendFedAs(t, req, as, toID.Public, toID.InstanceID)
}

// sendFedAs is sendFed with the responder named outright: the pair hop
// answers as fed.PairRecipient of its invite's key.
func sendFedAs(t *testing.T, req *http.Request, as *fed.Identity, pub ed25519.PublicKey, responder string) (int, []byte) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || as == nil {
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, raw
	}
	head, body, err := fed.OpenAnswer(as, pub, responder, resp)
	if err != nil {
		t.Fatalf("answer does not open or verify: %v", err)
	}
	return head.Status, body
}
