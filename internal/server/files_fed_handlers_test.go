package server

import (
	"context"
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
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/fed"
	"gitea.lan/matthew/fog/internal/filesfed"
	"gitea.lan/matthew/fog/internal/store"
)

// twoFogs pairs A (origin, reachable over HTTP) with B (the receiver) and
// returns both plus B's view of A as a peer and A's view of B.
type fogPair struct {
	a, b     *Server
	aH, bH   http.Handler
	aCookie  *http.Cookie
	bCookie  *http.Cookie
	aHTTP    *httptest.Server
	aPeerOnB *store.FedPeer // B's row for A
	bPeerOnA *store.FedPeer // A's row for B
}

func twoFogs(t *testing.T, grants string) *fogPair {
	t.Helper()
	p := &fogPair{}
	p.a, p.aH = newTestServer(t)
	p.aCookie = setup(t, p.aH)
	p.aHTTP = httptest.NewServer(p.aH)
	t.Cleanup(p.aHTTP.Close)
	if w := do(t, p.aH, "PUT", "/api/federation/identity", fmt.Sprintf(`{"url":"%s"}`, p.aHTTP.URL), withCookie(p.aCookie)); w.Code != http.StatusOK {
		t.Fatalf("set url: %d %s", w.Code, w.Body)
	}
	code := mintInvite(t, p.aH, p.aCookie, grants)
	p.b, p.bH = newTestServer(t)
	p.bCookie = setup(t, p.bH)
	if w := do(t, p.bH, "POST", "/api/federation/join", fmt.Sprintf(`{"code":%q}`, code), withCookie(p.bCookie)); w.Code != http.StatusOK {
		t.Fatalf("join: %d %s", w.Code, w.Body)
	}
	aID, _ := p.a.fed.Identity()
	bID, _ := p.b.fed.Identity()
	p.aPeerOnB, _ = p.b.store.FedPeerByInstanceID(aID.InstanceID)
	p.bPeerOnA, _ = p.a.store.FedPeerByInstanceID(bID.InstanceID)
	return p
}

func TestFilesFederationOriginSide(t *testing.T) {
	t.Parallel()

	p := twoFogs(t, `[{"capability":"files.share"}]`)
	root := p.a.files.Root()
	os.MkdirAll(filepath.Join(root, "Photos", "2026", "raw"), 0o755)
	os.MkdirAll(filepath.Join(root, "Photos", "private"), 0o755)
	os.WriteFile(filepath.Join(root, "Photos", "2026", "a.jpg"), []byte("aaaa"), 0o644)
	os.WriteFile(filepath.Join(root, "Photos", "2026", "notes.txt"), []byte("nn"), 0o644)
	os.WriteFile(filepath.Join(root, "Photos", "2026", "raw", "x.dng"), []byte("rrrrrr"), 0o644)
	os.WriteFile(filepath.Join(root, "Photos", "private", "secret.jpg"), []byte("s"), 0o644)
	os.WriteFile(filepath.Join(root, "Other.txt"), []byte("o"), 0o644)

	// Nothing crosses before a share exists: B sees an empty catalog.
	ctx := context.Background()
	var cat struct {
		Shares []filesfed.CatalogEntry `json:"shares"`
	}
	if _, err := p.b.fed.SignedGetJSON(ctx, p.aPeerOnB, "/fed/v1/files/catalog", "", &cat); err != nil {
		t.Fatalf("catalog: %v", err)
	}
	if len(cat.Shares) != 0 {
		t.Fatalf("catalog before sharing: %+v", cat.Shares)
	}

	// Keep the private folder on this box, then share Photos with B.
	if w := do(t, p.aH, "POST", "/api/files/keep", `{"path":"Photos/private","keep":true}`, withCookie(p.aCookie)); w.Code != http.StatusOK {
		t.Fatalf("keep: %d %s", w.Code, w.Body)
	}
	body := fmt.Sprintf(`{"peerId":%d,"path":"Photos","title":"Holiday"}`, p.bPeerOnA.ID)
	w := do(t, p.aH, "POST", "/api/files/peer-shares", body, withCookie(p.aCookie))
	if w.Code != http.StatusCreated {
		t.Fatalf("create share: %d %s", w.Code, w.Body)
	}
	var sh filesfed.ShareView
	json.Unmarshal(w.Body.Bytes(), &sh)
	if sh.Files != 3 || !strings.Contains(sh.Impact, "3 files in Holiday as full copies") || !strings.Contains(sh.Impact, "1 entry marked keep on this box") {
		t.Fatalf("share view = %+v", sh)
	}
	// Sharing the same folder twice with the same peer is a conflict.
	if w := do(t, p.aH, "POST", "/api/files/peer-shares", body, withCookie(p.aCookie)); w.Code != http.StatusConflict {
		t.Errorf("duplicate share: %d", w.Code)
	}

	// B's catalog now lists it — title and counts, never the path.
	if _, err := p.b.fed.SignedGetJSON(ctx, p.aPeerOnB, "/fed/v1/files/catalog", "", &cat); err != nil {
		t.Fatal(err)
	}
	if len(cat.Shares) != 1 || cat.Shares[0].Title != "Holiday" || cat.Shares[0].Files != 3 {
		t.Fatalf("catalog = %+v", cat.Shares)
	}
	raw, _ := json.Marshal(cat)
	if strings.Contains(string(raw), "Photos") || strings.Contains(string(raw), root) {
		t.Errorf("catalog leaks the origin path: %s", raw)
	}

	// The manifest applies the floor: private/ is absent, everything else in.
	var m filesfed.Manifest
	etag, err := p.b.fed.SignedGetJSON(ctx, p.aPeerOnB, fmt.Sprintf("/fed/v1/files/shares/%d/manifest", sh.ID), "", &m)
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]string{}
	for _, e := range m.Entries {
		paths[e.Path] = e.Kind
	}
	if paths["2026/a.jpg"] != "file" || paths["2026/raw/x.dng"] != "file" || paths["2026"] != "dir" || paths["private"] != "" || paths["private/secret.jpg"] != "" {
		t.Fatalf("manifest = %v", paths)
	}
	// An unchanged share answers 304 to its own ETag.
	if _, err := p.b.fed.SignedGetJSON(ctx, p.aPeerOnB, fmt.Sprintf("/fed/v1/files/shares/%d/manifest", sh.ID), etag, &m); err != fed.ErrNotModified {
		t.Errorf("conditional manifest: %v", err)
	}

	// A blob streams, signed over its declared digest; a floored file 404s.
	resp, err := p.b.fed.SignedDo(ctx, p.aPeerOnB, "GET", fmt.Sprintf("/fed/v1/files/shares/%d/blob/2026/a.jpg", sh.ID), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	sum := sha256.Sum256(got)
	if digest := fed.DeclaredDigest(resp); digest != hex.EncodeToString(sum[:]) || string(got) != "aaaa" {
		t.Fatalf("blob: %q digest=%s", got, digest)
	}
	if _, err := p.b.fed.SignedDo(ctx, p.aPeerOnB, "GET", fmt.Sprintf("/fed/v1/files/shares/%d/blob/private/secret.jpg", sh.ID), "", nil); err == nil {
		t.Error("floored file was served")
	}
	// A path that climbs out of the share folder never yields the file:
	// the mux cleans it away to something that is not a blob route.
	if esc, err := p.b.fed.SignedDo(ctx, p.aPeerOnB, "GET", fmt.Sprintf("/fed/v1/files/shares/%d/blob/../Other.txt", sh.ID), "", nil); err == nil {
		b, _ := io.ReadAll(esc.Body)
		esc.Body.Close()
		if string(b) == "o" || esc.Header.Get(fed.HeaderContentSHA256) != "" {
			t.Error("escape from the share folder was served")
		}
	}

	// Narrowing the lens bumps the revision and the manifest follows.
	w = do(t, p.aH, "PUT", fmt.Sprintf("/api/files/peer-shares/%d", sh.ID), `{"title":"Holiday","scope":"filtered","filters":["glob:*.jpg"]}`, withCookie(p.aCookie))
	if w.Code != http.StatusOK {
		t.Fatalf("narrow: %d %s", w.Code, w.Body)
	}
	json.Unmarshal(w.Body.Bytes(), &sh)
	if sh.Revision != 2 || sh.Files != 1 {
		t.Fatalf("after narrowing: rev %d files %d", sh.Revision, sh.Files)
	}
	if _, err := p.b.fed.SignedGetJSON(ctx, p.aPeerOnB, fmt.Sprintf("/fed/v1/files/shares/%d/manifest", sh.ID), "", &m); err != nil {
		t.Fatal(err)
	}
	// 2026/ and 2026/a.jpg: 2026/raw/ holds only what the filter keeps
	// back, so its name doesn't cross either.
	if m.Revision != 2 || len(m.Entries) != 2 {
		t.Fatalf("narrowed manifest = %+v", m.Entries)
	}

	// Without the grant the plane is closed, share or no share.
	p.a.store.RemoveFedCapability(p.bPeerOnA.ID, filesfed.CapFilesShare, store.DirGrant)
	if _, err := p.b.fed.SignedGetJSON(ctx, p.aPeerOnB, "/fed/v1/files/catalog", "", &cat); err == nil {
		t.Error("catalog served without the grant")
	}
	p.a.store.SetFedCapability(p.bPeerOnA.ID, filesfed.CapFilesShare, store.DirGrant, "{}")

	// Stop sharing: gone from the catalog; the listing shows the badge before and not after.
	w = do(t, p.aH, "GET", "/api/files/ls?path=Photos", "", withCookie(p.aCookie))
	if !strings.Contains(w.Body.String(), `"sharedWith"`) || !strings.Contains(w.Body.String(), `"kept":true`) {
		t.Errorf("listing lacks federation badges: %s", w.Body)
	}
	if w := do(t, p.aH, "DELETE", fmt.Sprintf("/api/files/peer-shares/%d", sh.ID), "", withCookie(p.aCookie)); w.Code != http.StatusOK {
		t.Fatalf("revoke: %d %s", w.Code, w.Body)
	}
	if _, err := p.b.fed.SignedGetJSON(ctx, p.aPeerOnB, "/fed/v1/files/catalog", "", &cat); err != nil || len(cat.Shares) != 0 {
		t.Errorf("catalog after revoke: %+v %v", cat.Shares, err)
	}
}

func TestFilesFederationReceivingSide(t *testing.T) {
	t.Parallel()

	p := twoFogs(t, `[{"capability":"files.share"}]`)
	rootA := p.a.files.Root()
	os.MkdirAll(filepath.Join(rootA, "Album", "raw"), 0o755)
	os.WriteFile(filepath.Join(rootA, "Album", "one.jpg"), []byte("one-bytes"), 0o644)
	os.WriteFile(filepath.Join(rootA, "Album", "raw", "two.dng"), []byte("two-bytes!"), 0o644)
	w := do(t, p.aH, "POST", "/api/files/peer-shares", fmt.Sprintf(`{"peerId":%d,"path":"Album","title":"Album"}`, p.bPeerOnA.ID), withCookie(p.aCookie))
	if w.Code != http.StatusCreated {
		t.Fatalf("share: %d %s", w.Code, w.Body)
	}
	var sh filesfed.ShareView
	json.Unmarshal(w.Body.Bytes(), &sh)

	// B reads A's catalog through the session API and subscribes.
	w = do(t, p.bH, "GET", fmt.Sprintf("/api/federation/peers/%d/files", p.aPeerOnB.ID), "", withCookie(p.bCookie))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"title":"Album"`) {
		t.Fatalf("catalog via api: %d %s", w.Code, w.Body)
	}
	w = do(t, p.bH, "POST", "/api/files/subscriptions", fmt.Sprintf(`{"peerId":%d,"remoteShareId":%d}`, p.aPeerOnB.ID, sh.ID), withCookie(p.bCookie))
	if w.Code != http.StatusCreated {
		t.Fatalf("subscribe: %d %s", w.Code, w.Body)
	}
	var sub store.FileFedSubscription
	json.Unmarshal(w.Body.Bytes(), &sub)
	if sub.Folder != "Peers/fog/Album" || sub.AcceptedImpact == "" {
		t.Fatalf("subscription = %+v", sub)
	}
	// The first pull runs as a job; do one synchronously to be sure.
	if err := p.b.filesFed.Sync(context.Background(), sub.ID, nil); err != nil && err != filesfed.ErrSyncBusy {
		t.Fatalf("sync: %v", err)
	}
	// Files transfer concurrently, so wait for both rather than assume one
	// landing means the other has.
	waitFor(t, 10*time.Second, func() bool {
		b, err := os.ReadFile(filepath.Join(p.b.files.Root(), "Peers", "fog", "Album", "raw", "two.dng"))
		return err == nil && string(b) == "two-bytes!"
	})
	waitFor(t, 10*time.Second, func() bool {
		b, err := os.ReadFile(filepath.Join(p.b.files.Root(), "Peers", "fog", "Album", "one.jpg"))
		return err == nil && string(b) == "one-bytes"
	})

	// The copy is read-only through every verb and never re-shareable.
	if w := do(t, p.bH, "POST", "/api/files/mkdir", `{"path":"Peers/fog/Album/mine"}`, withCookie(p.bCookie)); w.Code != http.StatusForbidden {
		t.Errorf("mkdir in replica: %d %s", w.Code, w.Body)
	}
	if w := do(t, p.bH, "POST", "/api/files/trash", `{"paths":["Peers/fog/Album/one.jpg"]}`, withCookie(p.bCookie)); w.Code != http.StatusForbidden {
		t.Errorf("trash in replica: %d %s", w.Code, w.Body)
	}
	w = do(t, p.bH, "POST", "/api/files/peer-shares", fmt.Sprintf(`{"peerId":%d,"path":"Peers/fog/Album"}`, p.aPeerOnB.ID), withCookie(p.bCookie))
	if w.Code != http.StatusConflict {
		t.Errorf("re-share of a replica: %d %s", w.Code, w.Body)
	}
	w = do(t, p.bH, "GET", "/api/files/ls?path=Peers/fog", "", withCookie(p.bCookie))
	if !strings.Contains(w.Body.String(), `"kind":"replica"`) || !strings.Contains(w.Body.String(), `"readOnly":true`) {
		t.Errorf("replica listing lacks the peer mark: %s", w.Body)
	}

	// Origin changes: a new file, a deleted one; the nudge plus a pull
	// converge B's copy.
	os.WriteFile(filepath.Join(rootA, "Album", "three.txt"), []byte("3"), 0o644)
	os.Remove(filepath.Join(rootA, "Album", "one.jpg"))
	do(t, p.aH, "GET", "/api/files/ls?path=Album", "", withCookie(p.aCookie)) // A notices
	p.a.filesFed.FlushNudges(context.Background())
	if err := p.b.filesFed.Sync(context.Background(), sub.ID, nil); err != nil && err != filesfed.ErrSyncBusy {
		t.Fatalf("second sync: %v", err)
	}
	waitFor(t, 10*time.Second, func() bool {
		_, e1 := os.Stat(filepath.Join(p.b.files.Root(), "Peers", "fog", "Album", "three.txt"))
		_, e2 := os.Stat(filepath.Join(p.b.files.Root(), "Peers", "fog", "Album", "one.jpg"))
		return e1 == nil && os.IsNotExist(e2)
	})

	// Widening (read → contribute) halts the pull until acknowledged.
	w = do(t, p.aH, "PUT", fmt.Sprintf("/api/files/peer-shares/%d", sh.ID), `{"title":"Album","mode":"contribute"}`, withCookie(p.aCookie))
	if w.Code != http.StatusOK {
		t.Fatalf("widen: %d %s", w.Code, w.Body)
	}
	if err := p.b.filesFed.Sync(context.Background(), sub.ID, nil); err != filesfed.ErrOfferChanged {
		t.Fatalf("sync after widening: %v", err)
	}
	got, _ := p.b.store.FileFedSubscriptionByID(sub.ID)
	if got.Status != filesfed.StatusOfferChanged {
		t.Fatalf("status = %s", got.Status)
	}
	if w := do(t, p.bH, "POST", fmt.Sprintf("/api/files/subscriptions/%d/acknowledge", sub.ID), "", withCookie(p.bCookie)); w.Code != http.StatusOK {
		t.Fatalf("acknowledge: %d %s", w.Code, w.Body)
	}
	if err := p.b.filesFed.Sync(context.Background(), sub.ID, nil); err != nil && err != filesfed.ErrSyncBusy {
		t.Fatalf("sync after ack: %v", err)
	}

	// Revoke at the origin: B's subscription flips; the copy stays; adopt
	// turns it into B's own files.
	if w := do(t, p.aH, "DELETE", fmt.Sprintf("/api/files/peer-shares/%d", sh.ID), "", withCookie(p.aCookie)); w.Code != http.StatusOK {
		t.Fatalf("revoke: %d", w.Code)
	}
	// B is not reachable in this harness, so the nudge cannot land; the
	// next pull finds the manifest gone and draws the same conclusion.
	p.b.filesFed.Sync(context.Background(), sub.ID, nil)
	waitFor(t, 10*time.Second, func() bool {
		got, _ := p.b.store.FileFedSubscriptionByID(sub.ID)
		return got != nil && got.Status == filesfed.StatusRevoked
	})
	if w := do(t, p.bH, "DELETE", fmt.Sprintf("/api/files/subscriptions/%d?mode=adopt", sub.ID), "", withCookie(p.bCookie)); w.Code != http.StatusOK {
		t.Fatalf("adopt: %d %s", w.Code, w.Body)
	}
	if _, err := os.Stat(filepath.Join(p.b.files.Root(), "Peers", "fog", "Album", "three.txt")); err != nil {
		t.Error("adopted files vanished")
	}
	if w := do(t, p.bH, "POST", "/api/files/mkdir", `{"path":"Peers/fog/Album/mine"}`, withCookie(p.bCookie)); w.Code != http.StatusCreated {
		t.Errorf("adopted folder still read-only: %d %s", w.Code, w.Body)
	}

	// Unpair removes every copy from that peer.
	w = do(t, p.aH, "POST", "/api/files/peer-shares", fmt.Sprintf(`{"peerId":%d,"path":"Album","title":"Again"}`, p.bPeerOnA.ID), withCookie(p.aCookie))
	json.Unmarshal(w.Body.Bytes(), &sh)
	w = do(t, p.bH, "POST", "/api/files/subscriptions", fmt.Sprintf(`{"peerId":%d,"remoteShareId":%d}`, p.aPeerOnB.ID, sh.ID), withCookie(p.bCookie))
	json.Unmarshal(w.Body.Bytes(), &sub)
	p.b.filesFed.Sync(context.Background(), sub.ID, nil)
	waitFor(t, 10*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(p.b.files.Root(), "Peers", "fog", "Again", "three.txt"))
		return err == nil
	})
	if w := do(t, p.bH, "DELETE", fmt.Sprintf("/api/federation/peers/%d", p.aPeerOnB.ID), "", withCookie(p.bCookie)); w.Code != http.StatusOK {
		t.Fatalf("unpair: %d %s", w.Code, w.Body)
	}
	if _, err := os.Stat(filepath.Join(p.b.files.Root(), "Peers", "fog", "Again")); err == nil {
		t.Error("unpair left the copy behind")
	}
	if subs, _ := p.b.store.ListFileFedSubscriptions(0); len(subs) != 0 {
		t.Errorf("subscriptions after unpair: %+v", subs)
	}
}

func waitFor(t *testing.T, d time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// TestFilesFederationEventsRequireGrant proves the files nudge is gated on
// the files.share grant, not merely a paired peer: a peer without the grant
// cannot make this box do sync work by sending events.
func TestFilesFederationEventsRequireGrant(t *testing.T) {
	t.Parallel()

	p := twoFogs(t, `[{"capability":"files.share"}]`)
	ctx := context.Background()
	body := []byte(`{"shareIds":[1],"event":"changed"}`)

	// With the grant, the nudge is accepted (a signed {"ok":true}).
	resp, err := p.b.fed.SignedDo(ctx, p.aPeerOnB, "POST", "/fed/v1/files/events", "application/json", body)
	if err != nil {
		t.Fatalf("events with grant: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("events with grant: status %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Remove the grant; the same nudge must now be refused.
	if err := p.a.store.RemoveFedCapability(p.bPeerOnA.ID, filesfed.CapFilesShare, store.DirGrant); err != nil {
		t.Fatalf("remove grant: %v", err)
	}
	resp2, err := p.b.fed.SignedDo(ctx, p.aPeerOnB, "POST", "/fed/v1/files/events", "application/json", body)
	if err == nil && resp2 != nil && resp2.StatusCode == http.StatusOK {
		resp2.Body.Close()
		t.Fatal("events without the files.share grant should be refused, got 200")
	}
	if resp2 != nil {
		resp2.Body.Close()
	}
}

// TestFilesFederationCreateShareGrantsAccess proves a share created through
// the API grants the peer files.share server-side, so the share is reachable
// even when it was not created through the settings UI that used to do it.
func TestFilesFederationCreateShareGrantsAccess(t *testing.T) {
	t.Parallel()

	p := twoFogs(t, `[]`) // paired, but no capabilities granted
	if _, granted, _ := p.a.store.FedCapabilityConfig(p.bPeerOnA.ID, filesfed.CapFilesShare, store.DirGrant); granted {
		t.Fatal("precondition: peer should not hold files.share yet")
	}
	root := p.a.files.Root()
	os.MkdirAll(filepath.Join(root, "Photos"), 0o755)
	os.WriteFile(filepath.Join(root, "Photos", "a.jpg"), []byte("aaaa"), 0o644)

	body := fmt.Sprintf(`{"peerId":%d,"path":"Photos","title":"Holiday"}`, p.bPeerOnA.ID)
	if w := do(t, p.aH, "POST", "/api/files/peer-shares", body, withCookie(p.aCookie)); w.Code != http.StatusCreated {
		t.Fatalf("create share: %d %s", w.Code, w.Body)
	}
	if _, granted, _ := p.a.store.FedCapabilityConfig(p.bPeerOnA.ID, filesfed.CapFilesShare, store.DirGrant); !granted {
		t.Fatal("creating a share should have granted files.share to the peer")
	}
}
