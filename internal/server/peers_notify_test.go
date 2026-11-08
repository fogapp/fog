package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/store"
)

// notices collects the change notices a stub app receives, so a test can
// wait for one instead of sleeping — the push is deliberately asynchronous.
type noticeStub struct {
	server *httptest.Server
	got    chan map[string]string
}

func newNoticeStub(t *testing.T) *noticeStub {
	t.Helper()
	stub := &noticeStub{got: make(chan map[string]string, 8)}
	stub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != AppDirectoryCallbackPath || r.Method != "POST" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer hub-fog-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var notice map[string]string
		json.Unmarshal(body, &notice)
		stub.got <- notice
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(stub.server.Close)
	return stub
}

func (n *noticeStub) await(t *testing.T, what string) map[string]string {
	t.Helper()
	select {
	case notice := <-n.got:
		return notice
	case <-time.After(3 * time.Second):
		t.Fatalf("no directory notice for %s", what)
		return nil
	}
}

func (n *noticeStub) awaitSilence(t *testing.T, what string) {
	t.Helper()
	select {
	case notice := <-n.got:
		t.Fatalf("unexpected directory notice after %s: %v", what, notice)
	case <-time.After(400 * time.Millisecond):
	}
}

// Pairing on one instance reaches the other instance's apps without anyone
// asking: the contracted app is told the directory moved, and re-reads it
// to find the new peer. The notice is a nudge, not a payload — what the app
// then sees comes from the GET.
func TestDirectoryChangeNotifiesContractedApps(t *testing.T) {
	t.Parallel()

	a, aHandler := newTestServer(t)
	aCookie := setup(t, aHandler)
	aHTTP := httptest.NewServer(aHandler)
	defer aHTTP.Close()
	do(t, aHandler, "PUT", "/api/federation/identity", fmt.Sprintf(`{"url":"%s"}`, aHTTP.URL), withCookie(aCookie))
	_ = a

	b, bHandler := newTestServer(t)
	bCookie := setup(t, bHandler)

	// hub@B declares the contract and is where notices land.
	_, appToken := appWithToken(t, b, "hub")
	hubApp, err := b.store.AppByName("hub")
	if err != nil {
		t.Fatal(err)
	}
	b.store.SetAppContracts(hubApp.ID, []string{ContractConnectV1})
	stub := newNoticeStub(t)
	b.appGrantEndpointFn = func(ctx context.Context, app *store.App) (string, string, error) {
		return stub.server.URL, "hub-fog-token", nil
	}

	code := mintInvite(t, aHandler, aCookie, `[{"capability":"apps.introduce"}]`)
	w := do(t, bHandler, "POST", "/api/federation/join", fmt.Sprintf(`{"code":%q}`, code), withCookie(bCookie))
	if w.Code != http.StatusOK {
		t.Fatalf("join: %d %s", w.Code, w.Body)
	}
	var joined struct {
		ID int64 `json:"id"`
	}
	json.Unmarshal(w.Body.Bytes(), &joined)
	// The first change after boot always pushes — fogd cannot know what an
	// app last saw, so a restart re-nudges once rather than staying quiet
	// about something missed while it was down.
	stub.await(t, "join")

	// Exposing the peer to apps is a change apps can see, so it pushes too.
	do(t, bHandler, "PUT", fmt.Sprintf("/api/federation/peers/%d", joined.ID),
		`{"capabilities":[{"capability":"apps.directory","direction":"use","enabled":true}]}`, withCookie(bCookie))
	notice := stub.await(t, "apps.directory toggle")
	if notice["reason"] != DirChangeUpdated || notice["changedAt"] == "" {
		t.Fatalf("notice shape: %v", notice)
	}

	// And the app now finds the peer by re-reading, which is the whole
	// point of a bodiless notice.
	w = do(t, bHandler, "GET", "/app/v1/connect/peers", "", withBearer(appToken))
	var dir struct {
		Peers []appPeerView `json:"peers"`
	}
	json.Unmarshal(w.Body.Bytes(), &dir)
	if len(dir.Peers) != 1 {
		t.Fatalf("app-visible directory: %s", w.Body)
	}

	// A write that moves nothing an app can see stays silent.
	do(t, bHandler, "PUT", fmt.Sprintf("/api/federation/peers/%d", joined.ID),
		`{"capabilities":[{"capability":"apps.directory","direction":"use","enabled":true}]}`, withCookie(bCookie))
	stub.awaitSilence(t, "a repeated no-op update")

	// Unpairing is a change: the app is told to re-read and finds nobody.
	do(t, bHandler, "DELETE", fmt.Sprintf("/api/federation/peers/%d", joined.ID), "", withCookie(bCookie))
	if notice := stub.await(t, "unpair"); notice["reason"] != DirChangeUnpaired {
		t.Fatalf("unpair reason: %v", notice)
	}
}

// An app that never declared fog-connect/1 is not called at all — the
// notice follows the same contract gate the grant calls do.
func TestDirectoryChangeSkipsUncontractedApps(t *testing.T) {
	t.Parallel()

	s, handler := newTestServer(t)
	setup(t, handler)
	appWithToken(t, s, "hub")
	stub := newNoticeStub(t)
	s.appGrantEndpointFn = func(ctx context.Context, app *store.App) (string, string, error) {
		return stub.server.URL, "hub-fog-token", nil
	}
	s.pushDirectoryChange(DirChangePaired)
	stub.awaitSilence(t, "a push with no contracted apps")
}
