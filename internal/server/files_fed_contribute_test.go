package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/filesfed"
	"gitea.lan/matthew/fog/internal/store"
)

// A contribute share: the peer's drop lands at the origin with its
// author recorded, is theirs alone to change there, supersedes on edit,
// withdraws on delete, is renamed on a name collision, and can be turned
// away by the origin's operator.
func TestFilesFederationContribute(t *testing.T) {
	p := twoFogs(t, `[{"capability":"files.share"}]`)
	rootA, rootB := p.a.files.Root(), p.b.files.Root()
	os.MkdirAll(filepath.Join(rootA, "Album"), 0o755)
	os.WriteFile(filepath.Join(rootA, "Album", "one.txt"), []byte("A's one"), 0o644)
	w := do(t, p.aH, "POST", "/api/files/peer-shares", fmt.Sprintf(`{"peerId":%d,"path":"Album","title":"Album","mode":"contribute"}`, p.bPeerOnA.ID), withCookie(p.aCookie))
	if w.Code != http.StatusCreated {
		t.Fatalf("share: %d %s", w.Code, w.Body)
	}
	var sh filesfed.ShareView
	json.Unmarshal(w.Body.Bytes(), &sh)
	if !strings.Contains(sh.Impact, "add files of their own") {
		t.Errorf("impact = %q", sh.Impact)
	}
	w = do(t, p.bH, "POST", "/api/files/subscriptions", fmt.Sprintf(`{"peerId":%d,"remoteShareId":%d}`, p.aPeerOnB.ID, sh.ID), withCookie(p.bCookie))
	if w.Code != http.StatusCreated {
		t.Fatalf("subscribe: %d %s", w.Code, w.Body)
	}
	var sub store.FileFedSubscription
	json.Unmarshal(w.Body.Bytes(), &sub)
	sync := func() {
		t.Helper()
		waitFor(t, 10*time.Second, func() bool { return p.b.filesFed.Sync(context.Background(), sub.ID, nil) == nil })
	}
	sync()
	folderB := filepath.Join(rootB, "Peers", "fog", "Album")
	bName := p.bPeerOnA.DisplayName()

	// A drop into the replica folder is allowed (the one write there) and
	// goes up on the next sync with the author recorded.
	w = do(t, p.bH, "POST", "/api/files/mkdir", `{"path":"Peers/fog/Album/mine"}`, withCookie(p.bCookie))
	if w.Code != http.StatusCreated && w.Code != http.StatusOK {
		t.Fatalf("mkdir in a drop box: %d %s", w.Code, w.Body)
	}
	os.WriteFile(filepath.Join(folderB, "mine", "photo.txt"), []byte("from B"), 0o644)
	do(t, p.bH, "GET", "/api/files/ls?path=Peers/fog/Album/mine", "", withCookie(p.bCookie))
	sync()
	if b, _ := os.ReadFile(filepath.Join(rootA, "Album", "mine", "photo.txt")); string(b) != "from B" {
		t.Fatalf("contribution did not land: %q", b)
	}
	w = do(t, p.aH, "GET", "/api/files/ls?path=Album/mine", "", withCookie(p.aCookie))
	if !strings.Contains(w.Body.String(), `"kind":"contribution"`) {
		t.Errorf("origin listing lacks the author: %s", w.Body)
	}
	w = do(t, p.bH, "GET", "/api/files/ls?path=Peers/fog/Album/mine", "", withCookie(p.bCookie))
	if !strings.Contains(w.Body.String(), `"mine":true`) || strings.Contains(w.Body.String(), `"readOnly":true`) {
		t.Errorf("our entry is not ours in the listing: %s", w.Body)
	}

	// Theirs alone to change at the origin: trash and rename refuse; a
	// copy is allowed.
	if w := do(t, p.aH, "POST", "/api/files/trash", `{"paths":["Album/mine/photo.txt"]}`, withCookie(p.aCookie)); w.Code != http.StatusForbidden {
		t.Errorf("trash a contribution: %d %s", w.Code, w.Body)
	}
	if w := do(t, p.aH, "POST", "/api/files/rename", `{"path":"Album/mine/photo.txt","name":"x.txt"}`, withCookie(p.aCookie)); w.Code != http.StatusForbidden {
		t.Errorf("rename a contribution: %d %s", w.Code, w.Body)
	}
	if w := do(t, p.aH, "POST", "/api/files/copy", `{"paths":["Album/mine/photo.txt"],"to":"Album"}`, withCookie(p.aCookie)); w.Code >= 300 {
		t.Errorf("copy a contribution: %d %s", w.Code, w.Body)
	}

	// The origin's own file is not B's to change, even from home.
	if w := do(t, p.bH, "POST", "/api/files/trash", `{"paths":["Peers/fog/Album/one.txt"]}`, withCookie(p.bCookie)); w.Code != http.StatusForbidden {
		t.Errorf("trash the origin's file in a drop box: %d %s", w.Code, w.Body)
	}

	// A name both sides create at once is the origin's to keep: B's
	// arrival is renamed on both sides.
	os.WriteFile(filepath.Join(rootA, "Album", "two.txt"), []byte("A's two"), 0o644)
	os.WriteFile(filepath.Join(folderB, "two.txt"), []byte("B's two"), 0o644)
	do(t, p.aH, "GET", "/api/files/ls?path=Album", "", withCookie(p.aCookie))
	do(t, p.bH, "GET", "/api/files/ls?path=Peers/fog/Album", "", withCookie(p.bCookie))
	sync()
	renamed := "two (from " + bName + ").txt"
	if b, _ := os.ReadFile(filepath.Join(rootA, "Album", renamed)); string(b) != "B's two" {
		t.Errorf("collision at the origin: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(rootA, "Album", "two.txt")); string(b) != "A's two" {
		t.Errorf("the origin's file was touched: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(folderB, renamed)); string(b) != "B's two" {
		t.Errorf("the rename was not followed locally: %q", b)
	}
	// The push reopened the file under its new name; that handle is closed
	// too, not only the one it opened first.
	if n := openFDsOn(t, filepath.Join(folderB, renamed)); n != 0 {
		t.Errorf("the push left %d handle(s) open on the renamed file", n)
	}
	if b, _ := os.ReadFile(filepath.Join(folderB, "two.txt")); string(b) != "A's two" {
		t.Errorf("the origin's two.txt should have arrived: %q", b)
	}

	// An edit supersedes; a delete withdraws.
	os.WriteFile(filepath.Join(folderB, "mine", "photo.txt"), []byte("from B, edited"), 0o644)
	later := time.Now().Add(2 * time.Second)
	os.Chtimes(filepath.Join(folderB, "mine", "photo.txt"), later, later)
	do(t, p.bH, "GET", "/api/files/ls?path=Peers/fog/Album/mine", "", withCookie(p.bCookie))
	sync()
	if b, _ := os.ReadFile(filepath.Join(rootA, "Album", "mine", "photo.txt")); string(b) != "from B, edited" {
		t.Errorf("supersede: %q", b)
	}
	if w := do(t, p.bH, "POST", "/api/files/trash", `{"paths":["Peers/fog/Album/mine/photo.txt"]}`, withCookie(p.bCookie)); w.Code != http.StatusOK {
		t.Fatalf("trash our own contribution: %d %s", w.Code, w.Body)
	}
	sync()
	if _, err := os.Stat(filepath.Join(rootA, "Album", "mine", "photo.txt")); err == nil {
		t.Error("withdrawal did not reach the origin")
	}
	if _, err := p.a.store.FileFedOriginByPath("Album/mine/photo.txt"); err == nil {
		t.Error("origin row survived the withdrawal")
	}

	// Reject: the origin turns B's other file away; B learns on its next
	// pull and keeps its own copy.
	w = do(t, p.aH, "POST", "/api/files/contributions/reject", fmt.Sprintf(`{"path":%q}`, "Album/"+renamed), withCookie(p.aCookie))
	if w.Code != http.StatusOK {
		t.Fatalf("reject: %d %s", w.Code, w.Body)
	}
	if _, err := os.Stat(filepath.Join(rootA, "Album", renamed)); err == nil {
		t.Error("rejected file still at the origin")
	}
	sync()
	w = do(t, p.bH, "GET", "/api/files/ls?path=Peers/fog/Album", "", withCookie(p.bCookie))
	if !strings.Contains(w.Body.String(), `"rejected":true`) {
		t.Errorf("rejection not shown: %s", w.Body)
	}
	if b, _ := os.ReadFile(filepath.Join(folderB, renamed)); string(b) != "B's two" {
		t.Errorf("our rejected file should stay ours: %q", b)
	}
	if !strings.Contains(noticeKinds(t, p.bH, p.bCookie), filesfed.NoticeRejected) {
		t.Error("no rejection notice")
	}
}

func noticeKinds(t *testing.T, h http.Handler, c *http.Cookie) string {
	t.Helper()
	w := do(t, h, "GET", "/api/notices", "", withCookie(c))
	return w.Body.String()
}

// fogTrio is three fogs: A the origin, B and C both subscribed to A.
type fogTrio struct {
	*fogPair
	c        *Server
	cH       http.Handler
	cCookie  *http.Cookie
	cHTTP    *httptest.Server
	cPeerOnA *store.FedPeer
	aPeerOnC *store.FedPeer
}

func threeFogs(t *testing.T) *fogTrio {
	t.Helper()
	tr := &fogTrio{fogPair: twoFogs(t, `[{"capability":"files.share"}]`)}
	tr.c, tr.cH = newTestServer(t)
	tr.cCookie = setup(t, tr.cH)
	tr.cHTTP = httptest.NewServer(tr.cH)
	t.Cleanup(tr.cHTTP.Close)
	if w := do(t, tr.cH, "PUT", "/api/federation/identity", fmt.Sprintf(`{"url":"%s"}`, tr.cHTTP.URL), withCookie(tr.cCookie)); w.Code != http.StatusOK {
		t.Fatalf("set url: %d %s", w.Code, w.Body)
	}
	if w := do(t, tr.cH, "PUT", "/api/settings", `{"server_name":"carol"}`, withCookie(tr.cCookie)); w.Code != http.StatusOK {
		t.Fatalf("name c: %d %s", w.Code, w.Body)
	}
	code := mintInvite(t, tr.aH, tr.aCookie, `[{"capability":"files.share"}]`)
	if w := do(t, tr.cH, "POST", "/api/federation/join", fmt.Sprintf(`{"code":%q}`, code), withCookie(tr.cCookie)); w.Code != http.StatusOK {
		t.Fatalf("c joins a: %d %s", w.Code, w.Body)
	}
	cID, _ := tr.c.fed.Identity()
	aID, _ := tr.a.fed.Identity()
	tr.cPeerOnA, _ = tr.a.store.FedPeerByInstanceID(cID.InstanceID)
	tr.aPeerOnC, _ = tr.c.store.FedPeerByInstanceID(aID.InstanceID)
	return tr
}

// A third party's contribution reaches a subscriber as a stub until the
// two are peered; then the bytes come from the author, never the origin.
// When the author unpairs from the origin, the entry becomes a stub for
// everyone.
func TestFilesFederationStubs(t *testing.T) {
	tr := threeFogs(t)
	rootA := tr.a.files.Root()
	os.MkdirAll(filepath.Join(rootA, "Album"), 0o755)
	os.WriteFile(filepath.Join(rootA, "Album", "a.txt"), []byte("A"), 0o644)
	share := func(peerID int64) filesfed.ShareView {
		t.Helper()
		w := do(t, tr.aH, "POST", "/api/files/peer-shares", fmt.Sprintf(`{"peerId":%d,"path":"Album","title":"Album","mode":"contribute"}`, peerID), withCookie(tr.aCookie))
		if w.Code != http.StatusCreated {
			t.Fatalf("share: %d %s", w.Code, w.Body)
		}
		var sh filesfed.ShareView
		json.Unmarshal(w.Body.Bytes(), &sh)
		return sh
	}
	shB, shC := share(tr.bPeerOnA.ID), share(tr.cPeerOnA.ID)
	subscribe := func(h http.Handler, c *http.Cookie, peerID, shareID int64) store.FileFedSubscription {
		t.Helper()
		w := do(t, h, "POST", "/api/files/subscriptions", fmt.Sprintf(`{"peerId":%d,"remoteShareId":%d}`, peerID, shareID), withCookie(c))
		if w.Code != http.StatusCreated {
			t.Fatalf("subscribe: %d %s", w.Code, w.Body)
		}
		var sub store.FileFedSubscription
		json.Unmarshal(w.Body.Bytes(), &sub)
		return sub
	}
	subC := subscribe(tr.cH, tr.cCookie, tr.aPeerOnC.ID, shC.ID)
	syncC := func() {
		t.Helper()
		waitFor(t, 10*time.Second, func() bool { return tr.c.filesFed.Sync(context.Background(), subC.ID, nil) == nil })
	}
	syncC()
	// C contributes.
	os.WriteFile(filepath.Join(tr.c.files.Root(), "Peers", "fog", "Album", "from-c.txt"), []byte("carol's"), 0o644)
	do(t, tr.cH, "GET", "/api/files/ls?path=Peers/fog/Album", "", withCookie(tr.cCookie))
	syncC()
	if b, _ := os.ReadFile(filepath.Join(rootA, "Album", "from-c.txt")); string(b) != "carol's" {
		t.Fatalf("contribution at A: %q", b)
	}

	// B, not peered with C, sees a stub and cannot open it.
	subB := subscribe(tr.bH, tr.bCookie, tr.aPeerOnB.ID, shB.ID)
	syncB := func() {
		t.Helper()
		waitFor(t, 10*time.Second, func() bool { return tr.b.filesFed.Sync(context.Background(), subB.ID, nil) == nil })
	}
	syncB()
	fromC := filepath.Join(tr.b.files.Root(), "Peers", "fog", "Album", "from-c.txt")
	w := do(t, tr.bH, "GET", "/api/files/ls?path=Peers/fog/Album", "", withCookie(tr.bCookie))
	if !strings.Contains(w.Body.String(), `"stub":true`) || !strings.Contains(w.Body.String(), `"author":"someone fog knows"`) {
		t.Fatalf("stub listing: %s", w.Body)
	}
	if _, err := os.Stat(fromC); err == nil {
		t.Fatal("a stub must carry no bytes")
	}
	if w := do(t, tr.bH, "GET", "/api/files/raw/Peers/fog/Album/from-c.txt", "", withCookie(tr.bCookie)); w.Code != http.StatusForbidden {
		t.Errorf("open a stub: %d %s", w.Code, w.Body)
	}
	// The origin never serves it either, even asked directly.
	if _, _, _, err := tr.a.filesFed.OpenBlob(tr.bPeerOnA, &store.FileFedShare{ID: shB.ID, Path: "Album", PeerID: tr.bPeerOnA.ID, Scope: "all", Detail: "full"}, "from-c.txt"); err == nil {
		t.Error("the origin served a third party's bytes")
	}

	// C pairs with B and grants files.share: the stub fills from C.
	code := mintInvite(t, tr.cH, tr.cCookie, `[{"capability":"files.share"}]`)
	if w := do(t, tr.bH, "POST", "/api/federation/join", fmt.Sprintf(`{"code":%q}`, code), withCookie(tr.bCookie)); w.Code != http.StatusOK {
		t.Fatalf("b joins c: %d %s", w.Code, w.Body)
	}
	w = do(t, tr.bH, "GET", "/api/files/raw/Peers/fog/Album/from-c.txt", "", withCookie(tr.bCookie))
	if w.Code != http.StatusOK || w.Body.String() != "carol's" {
		t.Fatalf("fill through own peering: %d %q", w.Code, w.Body)
	}
	w = do(t, tr.bH, "GET", "/api/files/ls?path=Peers/fog/Album", "", withCookie(tr.bCookie))
	if strings.Contains(w.Body.String(), `"stub":true`) || !strings.Contains(w.Body.String(), `"author":"carol"`) {
		t.Errorf("filled listing: %s", w.Body)
	}

	// C unpairs from A: its contribution becomes a stub at A and for B.
	if w := do(t, tr.cH, "DELETE", fmt.Sprintf("/api/federation/peers/%d", tr.aPeerOnC.ID), "", withCookie(tr.cCookie)); w.Code != http.StatusOK {
		t.Fatalf("unpair: %d %s", w.Code, w.Body)
	}
	waitFor(t, 10*time.Second, func() bool {
		o, err := tr.a.store.FileFedOriginByPath("Album/from-c.txt")
		return err == nil && o.Withdrawn
	})
	if _, err := os.Stat(filepath.Join(rootA, "Album", "from-c.txt")); err == nil {
		t.Error("withdrawn bytes still at the origin")
	}
	w = do(t, tr.aH, "GET", "/api/files/ls?path=Album", "", withCookie(tr.aCookie))
	if !strings.Contains(w.Body.String(), `"name":"from-c.txt"`) || !strings.Contains(w.Body.String(), `"dehydrated":true`) {
		t.Errorf("origin stub listing: %s", w.Body)
	}
	syncB()
	if _, err := os.Stat(fromC); err == nil {
		t.Error("B still holds bytes the author withdrew")
	}
	// The operator removes the stub.
	if w := do(t, tr.aH, "POST", "/api/files/contributions/reject", `{"path":"Album/from-c.txt"}`, withCookie(tr.aCookie)); w.Code != http.StatusOK {
		t.Errorf("remove stub: %d %s", w.Code, w.Body)
	}
	w = do(t, tr.aH, "GET", "/api/files/ls?path=Album", "", withCookie(tr.aCookie))
	if strings.Contains(w.Body.String(), `"name":"from-c.txt"`) {
		t.Errorf("stub survived removal: %s", w.Body)
	}
}

// TestFilesFederationContributeQuota proves the origin refuses a contribution
// that would push a contribute share past its byte quota, so a peer granted
// contribute cannot fill the origin's disk.
func TestFilesFederationContributeQuota(t *testing.T) {
	p := twoFogs(t, `[{"capability":"files.share"}]`)
	os.MkdirAll(filepath.Join(p.a.files.Root(), "Album"), 0o755)
	w := do(t, p.aH, "POST", "/api/files/peer-shares",
		fmt.Sprintf(`{"peerId":%d,"path":"Album","title":"Album","mode":"contribute","contributeQuotaBytes":5}`, p.bPeerOnA.ID),
		withCookie(p.aCookie))
	if w.Code != http.StatusCreated {
		t.Fatalf("share: %d %s", w.Code, w.Body)
	}
	var sh filesfed.ShareView
	json.Unmarshal(w.Body.Bytes(), &sh)

	body := []byte(`{"path":"big.txt","size":100,"sha256":"` + strings.Repeat("a", 64) + `","mtime":"2026-01-01T00:00:00Z"}`)
	_, err := p.b.fed.SignedDo(context.Background(), p.aPeerOnB, "POST",
		fmt.Sprintf("/fed/v1/files/shares/%d/contributions", sh.ID), "application/json", body)
	if err == nil {
		t.Fatal("a contribution over the share's quota should be refused")
	}
	if !strings.Contains(err.Error(), "413") || !strings.Contains(err.Error(), "quota") {
		t.Fatalf("expected a 413 quota refusal, got: %v", err)
	}
}

// The origin's contribute quota counts in-flight reservations, not just
// committed bytes — yet a resume of the same file supersedes its own
// reservation instead of double-charging it (code-review fixes).
func TestFilesFederationContributeQuotaInFlight(t *testing.T) {
	p := twoFogs(t, `[{"capability":"files.share"}]`)
	os.MkdirAll(filepath.Join(p.a.files.Root(), "Album"), 0o755)
	w := do(t, p.aH, "POST", "/api/files/peer-shares",
		fmt.Sprintf(`{"peerId":%d,"path":"Album","title":"Album","mode":"contribute","contributeQuotaBytes":1000}`, p.bPeerOnA.ID),
		withCookie(p.aCookie))
	if w.Code != http.StatusCreated {
		t.Fatalf("share: %d %s", w.Code, w.Body)
	}
	var sh filesfed.ShareView
	json.Unmarshal(w.Body.Bytes(), &sh)
	share := &store.FileFedShare{ID: sh.ID, PeerID: p.bPeerOnA.ID, Path: "Album", Mode: "contribute", ContributeQuotaBytes: 1000}

	// An 800-byte contribution is opened (never completed): it reserves in-flight.
	if _, err := p.a.filesFed.BeginContribution(p.bPeerOnA, share, "one.bin", 800, "", "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("first begin: %v", err)
	}
	// A second, different 800-byte file would exceed 1000 counting the in-flight
	// reservation — refused (the bug this closed let both past the cap).
	if _, err := p.a.filesFed.BeginContribution(p.bPeerOnA, share, "two.bin", 800, "", "2026-01-01T00:00:00Z"); err != filesfed.ErrQuotaExceeded {
		t.Fatalf("in-flight reservation should block a second contribution, got: %v", err)
	}
	// Resuming the SAME file supersedes its own reservation, so it isn't
	// double-charged and still fits.
	if _, err := p.a.filesFed.BeginContribution(p.bPeerOnA, share, "one.bin", 800, "", "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("resume of the same file must not be double-charged: %v", err)
	}
}
