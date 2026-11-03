package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"gitea.lan/matthew/fog/internal/fed"
	"gitea.lan/matthew/fog/internal/filesfed"
	"gitea.lan/matthew/fog/internal/store"
)

// wiretap is a TCP relay that records every byte crossing it, both ways.
type wiretap struct {
	ln  net.Listener
	mu  sync.Mutex
	buf bytes.Buffer
}

func newWiretap(t *testing.T, target string) *wiretap {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	w := &wiretap{ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				up, err := net.Dial("tcp", target)
				if err != nil {
					c.Close()
					return
				}
				go func() { io.Copy(io.MultiWriter(up, w), c); up.Close() }()
				io.Copy(io.MultiWriter(c, w), up)
				c.Close()
			}()
		}
	}()
	return w
}

func (w *wiretap) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *wiretap) seen() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte{}, w.buf.Bytes()...)
}

func (w *wiretap) url() string { return "http://" + w.ln.Addr().String() }

// A passive observer on the wire between two fogs — plain http, as on a
// bare LAN address — learns that they talk, the caller's instance ID and
// public keys, sizes and timing. Nothing else: not the invite secret, a pair
// request's identity, a heartbeat's directory or people, an error report, a
// share's title, a folder's or file's name or bytes, a person's handle, a
// room's ID or an app's name — in a body, a path, a query or a header. Every
// exchange is the same POST to the same path, answered 200.
func TestAPassiveObserverSeesNoBody(t *testing.T) {
	a, b := newFogNode(t), newFogNode(t)
	tap := newWiretap(t, strings.TrimPrefix(a.http.URL, "http://"))
	if w := do(t, a.h, "PUT", "/api/federation/identity", fmt.Sprintf(`{"url":%q}`, tap.url()), withCookie(a.cookie)); w.Code != http.StatusOK {
		t.Fatalf("advertise the tap: %d", w.Code)
	}
	code := mintInvite(t, a.h, a.cookie, `[{"capability":"files.share"},{"capability":"flare.ingest"}]`)
	inv, _ := fed.ParseInvite(code)
	if w := do(t, b.h, "POST", "/api/federation/join", fmt.Sprintf(`{"code":%q}`, code), withCookie(b.cookie)); w.Code != http.StatusOK {
		t.Fatalf("join through the tap: %d %s", w.Code, w.Body)
	}
	bOnA, _ := a.s.store.FedPeerByInstanceID(mustIdentity(t, b.s).InstanceID)
	aOnB, _ := b.s.store.FedPeerByInstanceID(mustIdentity(t, a.s).InstanceID)
	if aOnB.URL != tap.url() {
		t.Fatalf("B dials %s, not the tap", aOnB.URL)
	}
	ctx := context.Background()

	// A person on B with a handle to look for; B's heartbeat publishes its
	// people to A.
	if _, err := b.s.store.CreatePerson("wiretaphandle", "WIRETAP Person", store.PersonRoleMember); err != nil {
		t.Fatal(err)
	}
	// A shares a folder; B reads its catalog, manifest and a file in a
	// subfolder.
	const fileName, fileBytes = "WIRETAP-subdir/Taxes-2026-WIRETAP-name.pdf", "WIRETAP-file-contents-c0ffee"
	os.MkdirAll(filepath.Join(a.s.files.Root(), "WIRETAP-folder", "WIRETAP-subdir"), 0o755)
	os.WriteFile(filepath.Join(a.s.files.Root(), "WIRETAP-folder", filepath.FromSlash(fileName)), []byte(fileBytes), 0o644)
	sh, err := a.s.filesFed.CreateShare(&store.FileFedShare{PeerID: bOnA.ID, Path: "WIRETAP-folder", Title: "WIRETAP-share-title",
		Mode: filesfed.ModeRead, Scope: filesfed.ScopeAll, Detail: filesfed.DetailFull, Apps: filesfed.AppsDenied})
	if err != nil {
		t.Fatal(err)
	}
	var cat struct{ Shares []filesfed.CatalogEntry }
	if _, err := b.s.fed.SignedGetJSON(ctx, aOnB, "/fed/v1/files/catalog", "", &cat); err != nil || len(cat.Shares) != 1 {
		t.Fatalf("catalog through the tap: %v %+v", err, cat)
	}
	var m filesfed.Manifest
	for range 50 { // the manifest may still be hashing
		if _, err := b.s.fed.SignedGetJSON(ctx, aOnB, fmt.Sprintf("/fed/v1/files/shares/%d/manifest", sh.ID), "", &m); err != nil {
			t.Fatal(err)
		}
		if len(m.Entries) == 1 {
			break
		}
	}
	resp, err := b.s.fed.SignedDo(ctx, aOnB, "GET", fmt.Sprintf("/fed/v1/files/shares/%d/blob/%s", sh.ID, fileName), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != fileBytes {
		t.Fatalf("the file through the tap: %q", got)
	}
	// B reports an error to A, and heartbeats.
	const report = "WIRETAP-error-message-deadbeef"
	envelope := `{}` + "\n" + `{"type":"event"}` + "\n" + `{"event_id":"0123456789abcdef0123456789abcdef","message":"` + report + `","level":"error","platform":"go"}` + "\n"
	if _, err := b.s.fed.SignedPost(ctx, aOnB, "/fed/v1/flare/envelope", "application/x-sentry-envelope", []byte(envelope)); err != nil {
		t.Fatalf("flare through the tap: %v", err)
	}
	if err := b.s.fed.HeartbeatPeer(ctx, aOnB); err != nil {
		t.Fatalf("heartbeat through the tap: %v", err)
	}
	// Requests whose targets name a person, a room and a contribution's
	// path, and whose body names an app — refused or not, what they ask
	// for is the observer's to see only if it leaks.
	b.s.fed.SignedGetJSON(ctx, aOnB, "/fed/v1/chat/people/wiretaphandle/keys?ciphersuite=1", "", nil)
	b.s.fed.SignedGetJSON(ctx, aOnB, "/fed/v1/chat/rooms/WIRETAP-room-0042/events?after=0&limit=5", "", nil)
	b.s.fed.SignedDo(ctx, aOnB, "DELETE", fmt.Sprintf("/fed/v1/files/shares/%d/contributions?path=WIRETAP-contribution.txt", sh.ID), "", nil)
	b.s.fed.SignedPost(ctx, aOnB, "/fed/v1/connect", "application/json", []byte(`{"vouch":"x","payload":{"app":"wiretapapp"}}`))

	wire := tap.seen()
	// The observer did see the exchanges: every one the same POST.
	if n := bytes.Count(wire, []byte("POST "+fed.SealedPath+"?to=")); n < 8 {
		t.Fatalf("the tap saw %d envelopes: the test isn't observing", n)
	}
	for _, path := range []string{"/fed/v1/pair", "/fed/v1/heartbeat", "/fed/v1/files", "/fed/v1/flare", "/fed/v1/chat", "/fed/v1/connect", "/fed/v1/hello"} {
		if bytes.Contains(wire, []byte(path)) {
			t.Errorf("an observer can see which endpoint was called: %s", path)
		}
	}
	for _, status := range []string{"HTTP/1.1 403", "HTTP/1.1 404", "HTTP/1.1 304"} {
		if bytes.Contains(wire, []byte(status)) {
			t.Errorf("an observer can see an answer's status: %s", status)
		}
	}
	bDoc, _ := b.s.fed.InstanceDoc()
	for name, secret := range map[string]string{
		"the invite secret":      inv.Secret,
		"the joiner's name":      `"name":"` + bDoc.Name + `"`,
		"a heartbeat's payload":  `"fogVersion"`,
		"a heartbeat's grants":   `"grants"`,
		"any name at all":        "WIRETAP",
		"a person's handle":      "wiretaphandle",
		"an app's name":          "wiretapapp",
		"the file's bytes":       fileBytes,
		"the error report":       report,
		"a JSON body's opening":  `{"error"`,
		"the catalog's contract": `"contract"`,
	} {
		if i := bytes.Index(wire, []byte(secret)); i >= 0 {
			t.Errorf("an observer on the wire can read %s: …%q…", name, wire[max(0, i-200):min(len(wire), i+80)])
		}
	}
}

// A sealed body moved into another request doesn't open: C, a peer of A in
// its own right, can't send B's captured heartbeat as its own.
func TestASealedBodyCannotBeMovedBetweenPeers(t *testing.T) {
	a, b, c := newFogNode(t), newFogNode(t), newFogNode(t)
	pairNodes(t, a, b, `[]`)
	pairNodes(t, a, c, `[]`)
	body := []byte(`{"fogVersion":"x","grants":[],"directory":[],"url":"https://moved.example"}`)
	req, _ := http.NewRequest("POST", a.http.URL+"/fed/v1/heartbeat", nil)
	sealFor(t, mustIdentity(t, b.s), a.s, req, body)
	captured, _ := io.ReadAll(req.Body)

	// C signs the captured sealed bytes as its own request.
	cID := mustIdentity(t, c.s)
	moved, _ := http.NewRequest("POST", a.http.URL+fed.SealedTarget(mustIdentity(t, a.s).InstanceID), bytes.NewReader(captured))
	cID.SignRequest(moved, captured)
	status, raw := sendFed(t, moved, cID, a.s)
	var out struct{ Code string }
	json.Unmarshal(raw, &out)
	if status != http.StatusForbidden || out.Code != fed.CodeUnsealed {
		t.Fatalf("B's sealed body under C's signature: %d %q", status, out.Code)
	}
	if cOnA, _ := a.s.store.FedPeerByInstanceID(cID.InstanceID); cOnA.URL == "https://moved.example" {
		t.Fatal("the moved body was applied")
	}
}

// Downgrade, at the receiving end: a request that presents no encryption
// key, or whose body isn't sealed, is refused; so is a pair request that
// isn't sealed to the invite's key. Nothing of it is applied.
func TestUnsealedRequestsAreRefused(t *testing.T) {
	p := twoFogs(t, `[]`)
	bID := mustIdentity(t, p.b)
	body := []byte(`{"fogVersion":"x","grants":[],"directory":[],"url":"https://downgraded.example"}`)
	send := func(req *http.Request) (int, string) {
		status, raw := sendFed(t, req, bID, p.a)
		var out struct{ Code string }
		json.Unmarshal(raw, &out)
		return status, out.Code
	}

	// Sealed and signed, but without the key headers an older fog lacks.
	req, _ := http.NewRequest("POST", p.aHTTP.URL+"/fed/v1/heartbeat", nil)
	sealFor(t, bID, p.a, req, body)
	req.Header.Del(fed.HeaderEncKey)
	req.Header.Del(fed.HeaderEncBinding)
	if status, code := send(req); status != http.StatusForbidden || code != fed.CodeEncryptionRequired {
		t.Fatalf("no encryption key presented: %d %q", status, code)
	}
	// Signed as before the envelope: a plain body, to the endpoint itself.
	req, _ = http.NewRequest("POST", p.aHTTP.URL+"/fed/v1/heartbeat", bytes.NewReader(body))
	bID.SignRequest(req, body)
	if status, code := send(req); status != http.StatusForbidden || code != fed.CodeEncryptionRequired {
		t.Fatalf("an old-style request: %d %q", status, code)
	}
	// A plain body in the envelope's place.
	req, _ = http.NewRequest("POST", p.aHTTP.URL+fed.SealedTarget(mustIdentity(t, p.a).InstanceID), bytes.NewReader(body))
	bID.SignRequest(req, body)
	if status, code := send(req); status != http.StatusForbidden || code != fed.CodeUnsealed {
		t.Fatalf("an unsealed body: %d %q", status, code)
	}
	if got, _ := p.a.store.FedPeerByID(p.bPeerOnA.ID); got.URL == "https://downgraded.example" {
		t.Fatal("an unsealed request was applied")
	}

	// A pair request that isn't sealed to the invite's key.
	code := mintInvite(t, p.aH, p.aCookie, `[]`)
	inv, _ := fed.ParseInvite(code)
	c := mustServer(t)
	doc, _ := c.fed.InstanceDoc()
	pr, _ := json.Marshal(fed.PairRequest{Secret: inv.Secret, Instance: *doc})
	req, _ = http.NewRequest("POST", p.aHTTP.URL+"/fed/v1/pair", bytes.NewReader(pr))
	mustIdentity(t, c).SignRequest(req, pr)
	if status, _ := sendFed(t, req, nil, p.a); status != http.StatusForbidden {
		t.Fatalf("an unsealed pair request: %d", status)
	}
	if _, err := p.a.store.FedPeerByInstanceID(doc.InstanceID); err == nil {
		t.Fatal("an unsealed pair request paired")
	}
}

// A household re-pairing while both sides still hold each other — a fresh
// code pasted to refresh an address — reaches the pair handler like any
// pairing (the envelope tells a pair request by how it opens) and refreshes
// the rows it has.
func TestRepairWhileStillPaired(t *testing.T) {
	p := twoFogs(t, `[]`)
	peer, err := p.b.fed.Join(context.Background(), mintInvite(t, p.aH, p.aCookie, `[]`), "")
	if err != nil {
		t.Fatalf("re-pair while paired: %v", err)
	}
	if peer.ID != p.aPeerOnB.ID {
		t.Fatalf("re-pair made a new row %d, not %d", peer.ID, p.aPeerOnB.ID)
	}
	if again, _ := p.a.store.FedPeerByInstanceID(mustIdentity(t, p.b).InstanceID); again.ID != p.bPeerOnA.ID {
		t.Fatal("the inviter made a new row")
	}
}
