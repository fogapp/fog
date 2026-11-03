package server

import (
	"context"
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

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/files"
	"gitea.lan/matthew/fog/internal/filesfed"
	"gitea.lan/matthew/fog/internal/store"
)

// filesStub is an app serving the fog-files/1 callback.
type filesStub struct {
	server *httptest.Server
	got    chan map[string]string
}

func newFilesStub(t *testing.T) *filesStub {
	t.Helper()
	st := &filesStub{got: make(chan map[string]string, 16)}
	st.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != AppFilesCallbackPath || r.Method != "POST" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer hub-fog-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var n map[string]string
		json.Unmarshal(body, &n)
		st.got <- n
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(st.server.Close)
	return st
}

func (st *filesStub) await(t *testing.T, what string) map[string]string {
	t.Helper()
	select {
	case n := <-st.got:
		return n
	case <-time.After(8 * time.Second):
		t.Fatalf("no files notice for %s", what)
		return nil
	}
}

// An app mounting a peer's copy: the mount policy rules at deploy time,
// the app learns where its folder comes from, and a sync landing under it
// is one coalesced notice.
func TestAppFilesMountsAndNotices(t *testing.T) {
	p := twoFogs(t, `[{"capability":"files.share"}]`)
	rootA, rootB := p.a.files.Root(), p.b.files.Root()
	os.MkdirAll(filepath.Join(rootA, "Album"), 0o755)
	os.WriteFile(filepath.Join(rootA, "Album", "a.txt"), []byte("A"), 0o644)
	shareWith := func(apps string) filesfed.ShareView {
		t.Helper()
		w := do(t, p.aH, "POST", "/api/files/peer-shares", fmt.Sprintf(`{"peerId":%d,"path":"Album","title":"Album","apps":%q}`, p.bPeerOnA.ID, apps), withCookie(p.aCookie))
		if w.Code != http.StatusCreated {
			t.Fatalf("share: %d %s", w.Code, w.Body)
		}
		var sh filesfed.ShareView
		json.Unmarshal(w.Body.Bytes(), &sh)
		return sh
	}
	sh := shareWith("allowed")
	w := do(t, p.bH, "POST", "/api/files/subscriptions", fmt.Sprintf(`{"peerId":%d,"remoteShareId":%d,"detail":"previews"}`, p.aPeerOnB.ID, sh.ID), withCookie(p.bCookie))
	if w.Code != http.StatusCreated {
		t.Fatalf("subscribe: %d %s", w.Code, w.Body)
	}
	var sub store.FileFedSubscription
	json.Unmarshal(w.Body.Bytes(), &sub)
	waitFor(t, 10*time.Second, func() bool { return p.b.filesFed.Sync(context.Background(), sub.ID, nil) == nil })
	folder := filepath.Join(rootB, "Peers", "fog", "Album")

	// Policy: previews cannot mount; a full copy mounts read-only.
	if _, err := p.b.filesFed.MountPolicy(folder); err == nil || !strings.Contains(err.Error(), "previews") {
		t.Errorf("previews policy: %v", err)
	}
	do(t, p.bH, "PUT", fmt.Sprintf("/api/files/subscriptions/%d", sub.ID), `{"detail":"full"}`, withCookie(p.bCookie))
	waitFor(t, 10*time.Second, func() bool { _, err := os.Stat(filepath.Join(folder, "a.txt")); return err == nil })
	ro, err := p.b.filesFed.MountPolicy(folder)
	if err != nil || !ro {
		t.Errorf("full-copy policy: ro=%v err=%v", ro, err)
	}
	if ro, err := p.b.filesFed.MountPolicy(filepath.Join(rootB, "Own")); err != nil || ro {
		t.Errorf("local folder policy: ro=%v err=%v", ro, err)
	}
	// The materializer honours the policy: the compose mount ends in :ro.
	// FilesRoot as fogd wires it. A peer's copy is another household's
	// folder in Drive: it binds once acknowledged (and approved).
	mat := &apps.Materializer{DataDir: t.TempDir(), MountPolicy: p.b.filesFed.MountPolicy, FilesRoot: func() string { return rootB }}
	spec := &apps.AppSpec{Volumes: []apps.Volume{{Name: "album", Mount: "/album", Host: folder}}}
	if err := mat.Render("immich", spec, map[string]string{}, "img", "", io.Discard); err == nil {
		t.Fatal("a peer's folder bound unacknowledged")
	}
	spec.AcknowledgeHostAccess = []string{folder}
	if err := mat.Render("immich", spec, map[string]string{}, "img", "", io.Discard); err != nil {
		t.Fatalf("render: %v", err)
	}
	compose, _ := os.ReadFile(filepath.Join(mat.AppDir("immich"), "compose.yaml"))
	if !strings.Contains(string(compose), folder+":/album:ro") {
		t.Errorf("compose lacks the read-only mount: %s", compose)
	}
	// An apps-denied lens refuses the mount outright.
	do(t, p.aH, "PUT", fmt.Sprintf("/api/files/peer-shares/%d", sh.ID), `{"apps":"denied"}`, withCookie(p.aCookie))
	waitFor(t, 10*time.Second, func() bool {
		p.b.filesFed.Sync(context.Background(), sub.ID, nil)
		s2, _ := p.b.store.FileFedSubscriptionByID(sub.ID)
		return s2 != nil && !s2.AppsAllowed
	})
	if _, err := p.b.filesFed.MountPolicy(folder); err == nil || !strings.Contains(err.Error(), "not for apps") {
		t.Errorf("apps-denied policy: %v", err)
	}
	do(t, p.aH, "PUT", fmt.Sprintf("/api/files/peer-shares/%d", sh.ID), `{"apps":"allowed"}`, withCookie(p.aCookie))
	waitFor(t, 10*time.Second, func() bool {
		p.b.filesFed.Sync(context.Background(), sub.ID, nil)
		s2, _ := p.b.store.FileFedSubscriptionByID(sub.ID)
		return s2 != nil && s2.AppsAllowed && s2.Status == filesfed.StatusOfferChanged
	})
	do(t, p.bH, "POST", fmt.Sprintf("/api/files/subscriptions/%d/acknowledge", sub.ID), "", withCookie(p.bCookie))

	// The app: a spec mounting the copy, the contract, the callback.
	appID, token := appWithToken(t, p.b, "immich")
	// Stored beside the peer mount: a volume that names a Files folder,
	// which the mounts view resolves against the root and lists as local.
	stored := *spec
	stored.Volumes = append(stored.Volumes, apps.Volume{Name: "books", Mount: "/books", Files: "Apps/Books"})
	specJSON, _ := json.Marshal(stored)
	if _, err := p.b.store.CreateRevision(appID, string(specJSON), "mount", "test"); err != nil {
		t.Fatal(err)
	}
	p.b.store.SetAppContracts(appID, []string{ContractFilesV1})
	// main wires the app volumes from every spec; here the files side
	// knows the one mount above.
	p.b.files.MountedPaths = func() []files.MountedPath {
		return []files.MountedPath{{Path: folder, App: "immich", Mount: "/album"}}
	}
	stub := newFilesStub(t)
	p.b.appGrantEndpointFn = func(ctx context.Context, app *store.App) (string, string, error) {
		return stub.server.URL, "hub-fog-token", nil
	}
	w = do(t, p.bH, "GET", "/app/v1/files/mounts", "", withBearer(token))
	if w.Code != http.StatusOK {
		t.Fatalf("mounts: %d %s", w.Code, w.Body)
	}
	var mounts struct {
		Mounts []appFilesMount `json:"mounts"`
	}
	json.Unmarshal(w.Body.Bytes(), &mounts)
	if len(mounts.Mounts) != 2 || mounts.Mounts[0].Kind != "peer" || mounts.Mounts[0].Peer != "fog" || !mounts.Mounts[0].ReadOnly || !mounts.Mounts[0].Hydrated || mounts.Mounts[0].Share != "Album" {
		t.Fatalf("mounts view: %s", w.Body)
	}
	if m := mounts.Mounts[1]; m.Kind != "local" || m.Mount != "/books" || m.HostPath != filepath.Join(rootB, "Apps", "Books") || m.ReadOnly {
		t.Fatalf("Files volume in mounts view: %+v", m)
	}
	// The subscription view names the app.
	w = do(t, p.bH, "GET", "/api/files/subscriptions", "", withCookie(p.bCookie))
	if !strings.Contains(w.Body.String(), `"mountedBy":["immich"]`) {
		t.Errorf("mountedBy: %s", w.Body)
	}
	// Removing the copy refuses while mounted, unless forced.
	if w := do(t, p.bH, "DELETE", fmt.Sprintf("/api/files/subscriptions/%d", sub.ID), "", withCookie(p.bCookie)); w.Code != http.StatusConflict {
		t.Errorf("unsubscribe while mounted: %d %s", w.Code, w.Body)
	}

	// A sync landing under the mount is one notice, reason synced.
	os.WriteFile(filepath.Join(rootA, "Album", "b.txt"), []byte("B"), 0o644)
	os.WriteFile(filepath.Join(rootA, "Album", "c.txt"), []byte("C"), 0o644)
	do(t, p.aH, "GET", "/api/files/ls?path=Album", "", withCookie(p.aCookie))
	waitFor(t, 10*time.Second, func() bool {
		p.b.filesFed.Sync(context.Background(), sub.ID, nil)
		_, err := os.Stat(filepath.Join(folder, "c.txt"))
		return err == nil
	})
	n := stub.await(t, "sync")
	if n["mount"] != "/album" || n["reason"] != "synced" || n["changedAt"] == "" {
		t.Fatalf("notice: %v", n)
	}
	select {
	case extra := <-stub.got:
		t.Fatalf("a burst should coalesce into one notice, got another: %v", extra)
	case <-time.After(500 * time.Millisecond):
	}

	// Revoking reaches the app too.
	do(t, p.aH, "DELETE", fmt.Sprintf("/api/files/peer-shares/%d", sh.ID), "", withCookie(p.aCookie))
	p.b.filesFed.Sync(context.Background(), sub.ID, nil)
	if n := stub.await(t, "revoke"); n["reason"] != "revoked" {
		t.Fatalf("revoke notice: %v", n)
	}
}
