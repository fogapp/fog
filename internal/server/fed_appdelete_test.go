package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/build"
	"gitea.lan/matthew/fog/internal/dockerx"
	"gitea.lan/matthew/fog/internal/fed"
	"gitea.lan/matthew/fog/internal/store"
)

// Peers' grants name this household's apps. Deleting an app takes its name
// out of every grant and has it revoke, while it still runs, what it minted
// for peers through Connect: a new app taking the name inherits no peer's
// access — it is neither listed to them nor reachable by them.
func TestDeletedAppLeavesPeersNoAccessToItsName(t *testing.T) {
	cp := newConnectPair(t, `[{"capability":"apps.introduce","config":{"apps":["feed","hub"]}},{"capability":"apps.directory","config":{"apps":["feed"]}}]`, true)
	t.Setenv("DOCKER_SOCK", filepath.Join(t.TempDir(), "none.sock"))
	a := cp.a
	a.engine = &apps.Engine{Store: a.store, Vault: a.vault}
	a.engine.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	a.engine.Mat = &apps.Materializer{DataDir: a.cfg.DataDir}
	a.engine.Builder = &build.Builder{DataDir: a.cfg.DataDir}
	a.engine.Docker = dockerx.New()

	// B connects to feed: a credential minted for it.
	if status, body := cp.fedConnect(t, cp.vouchFromB(t, "n-1"), fed.SignVouch(mustIdentity(t, cp.b), cp.vouchFromB(t, "n-1")), ""); status != http.StatusOK {
		t.Fatalf("connect: %d %s", status, body)
	}
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
	a.appGrantEndpointFn = func(context.Context, *store.App) (string, string, error) { return revoker.URL, "t", nil }

	if w := do(t, cp.aH, "DELETE", "/api/apps/feed", "", withCookie(cp.aCookie)); w.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	mu.Lock()
	if len(revoked) != 1 || revoked[0] != mustIdentity(t, cp.b).InstanceID {
		t.Fatalf("feed's credentials for B were not revoked before it went: %v", revoked)
	}
	mu.Unlock()
	for c, want := range map[string]string{store.CapAppsIntroduce: `{"apps":["hub"]}`, store.CapAppsDirectory: `{"apps":[]}`} {
		if cfg, _, _ := a.store.FedCapabilityConfig(cp.bPeerOnA.ID, c, store.DirGrant); cfg != want {
			t.Errorf("%s still names the deleted app: %s", c, cfg)
		}
	}

	// A new app takes the name: B can't reach it, and it isn't listed to B.
	feed2, err := a.store.CreateApp("feed")
	if err != nil {
		t.Fatal(err)
	}
	a.store.SetAppContracts(feed2.ID, []string{ContractConnectV1})
	if status, body := cp.fedConnect(t, cp.vouchFromB(t, "n-2"), fed.SignVouch(mustIdentity(t, cp.b), cp.vouchFromB(t, "n-2")), ""); status != http.StatusForbidden {
		t.Fatalf("B reached the new app under the old name: %d %s", status, body)
	}
	raw, _, _ := a.store.FedCapabilityConfig(cp.bPeerOnA.ID, store.CapAppsDirectory, store.DirGrant)
	if scope, err := fed.ParseAppScope(raw); err != nil || scope.Covers("feed") {
		t.Fatal("the new app under the old name is listed to B")
	}
}

// The credentials an app minted for peers live in its own data. If it
// can't confirm revoking them, its data is not kept unless the operator
// says so: otherwise an app installed later under its name would accept
// them. Deleting the data too goes ahead.
func TestDeleteKeepsNoDataBehindUnrevokedCredentials(t *testing.T) {
	cp := newConnectPair(t, `[{"capability":"apps.introduce","config":{}}]`, true)
	t.Setenv("DOCKER_SOCK", filepath.Join(t.TempDir(), "none.sock"))
	a := cp.a
	a.engine = &apps.Engine{Store: a.store, Vault: a.vault}
	a.engine.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	a.engine.Mat = &apps.Materializer{DataDir: a.cfg.DataDir}
	a.engine.Builder = &build.Builder{DataDir: a.cfg.DataDir}
	a.engine.Docker = dockerx.New()
	// The app is down: nothing can be revoked.
	a.appGrantEndpointFn = func(context.Context, *store.App) (string, string, error) {
		return "", "", errors.New("not running")
	}
	w := do(t, cp.aH, "DELETE", "/api/apps/feed", "", withCookie(cp.aCookie))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "connect_revoke_failed") {
		t.Fatalf("deleting, keeping data, with revocation unconfirmed: %d %s", w.Code, w.Body)
	}
	if _, err := a.store.AppByName("feed"); err != nil {
		t.Fatal("the refused delete removed the app")
	}
	if w := do(t, cp.aH, "DELETE", "/api/apps/feed?keepUnrevoked=1", "", withCookie(cp.aCookie)); w.Code != http.StatusOK {
		t.Fatalf("with the operator's say-so: %d %s", w.Code, w.Body)
	}

	// Deleting its data too needs no confirmation: the credentials go with it.
	cp2 := newConnectPair(t, `[{"capability":"apps.introduce","config":{}}]`, true)
	b := cp2.a
	b.engine = &apps.Engine{Store: b.store, Vault: b.vault}
	b.engine.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	b.engine.Mat = &apps.Materializer{DataDir: b.cfg.DataDir}
	b.engine.Builder = &build.Builder{DataDir: b.cfg.DataDir}
	b.engine.Docker = dockerx.New()
	b.appGrantEndpointFn = func(context.Context, *store.App) (string, string, error) {
		return "", "", errors.New("not running")
	}
	if w := do(t, cp2.aH, "DELETE", "/api/apps/feed?data=1", "", withCookie(cp2.aCookie)); w.Code != http.StatusOK {
		t.Fatalf("deleting with its data: %d %s", w.Code, w.Body)
	}
}

// appEngine gives a test server an engine that can delete an app.
func appEngine(t *testing.T, s *Server) {
	t.Helper()
	t.Setenv("DOCKER_SOCK", filepath.Join(t.TempDir(), "none.sock"))
	s.engine = &apps.Engine{Store: s.store, Vault: s.vault}
	s.engine.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	s.engine.Mat = &apps.Materializer{DataDir: s.cfg.DataDir}
	s.engine.Builder = &build.Builder{DataDir: s.cfg.DataDir}
	s.engine.Docker = dockerx.New()
}

// A household unpaired while the app it holds a credential at was down is
// still owed that credential's revocation — kept, and retried, until the
// app answers. Until then the app's data isn't kept on deletion without the
// operator saying so, though the household is no longer paired; once the
// app answers, it is.
func TestRevocationOwedToAnUnpairedHouseholdIsKeptUntilPaid(t *testing.T) {
	cp := newConnectPair(t, `[{"capability":"apps.introduce","config":{}}]`, true)
	a := cp.a
	appEngine(t, a)
	bInst := mustIdentity(t, cp.b).InstanceID
	if status, body := cp.fedConnect(t, cp.vouchFromB(t, "n-1"), fed.SignVouch(mustIdentity(t, cp.b), cp.vouchFromB(t, "n-1")), ""); status != http.StatusOK {
		t.Fatalf("connect: %d %s", status, body)
	}
	// The app goes down; A unpairs B.
	a.appGrantEndpointFn = func(context.Context, *store.App) (string, string, error) { return "", "", errors.New("down") }
	if w := do(t, cp.aH, "DELETE", fmt.Sprintf("/api/federation/peers/%d", cp.bPeerOnA.ID), "", withCookie(cp.aCookie)); w.Code != http.StatusOK {
		t.Fatalf("unpair: %d %s", w.Code, w.Body)
	}
	a.bgWG.Wait()
	if owed := a.loadOwed(); len(owed) != 1 || owed[0].Instance != bInst {
		t.Fatalf("the revocation owed to the unpaired household was not kept: %+v", owed)
	}
	// Deleting the app, keeping its data: refused — B is no longer paired,
	// but its credential is still in the data.
	if w := do(t, cp.aH, "DELETE", "/api/apps/feed", "", withCookie(cp.aCookie)); w.Code != http.StatusConflict {
		t.Fatalf("keeping the data of an app owing a former peer's revocation: %d %s", w.Code, w.Body)
	}
	// The app comes back; the retry pays the debt.
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
	a.appGrantEndpointFn = func(context.Context, *store.App) (string, string, error) { return revoker.URL, "t", nil }
	a.payOwed(t.Context(), func(owedRevocation) bool { return true })
	mu.Lock()
	if !slices.Contains(revoked, bInst) {
		t.Fatalf("the owed revocation was not paid when the app came back: %v", revoked)
	}
	mu.Unlock()
	if owed := a.loadOwed(); len(owed) != 0 {
		t.Fatalf("a paid revocation is still owed: %+v", owed)
	}
	if w := do(t, cp.aH, "DELETE", "/api/apps/feed", "", withCookie(cp.aCookie)); w.Code != http.StatusOK {
		t.Fatalf("keeping the data once nothing is owed: %d %s", w.Code, w.Body)
	}
}

// Nothing is minted for an app while it is being removed: a mint already
// under way finishes before the revocations are worked out (and is among
// them), and one asked for meanwhile is refused.
func TestNoCredentialSlipsInWhileAnAppIsRemoved(t *testing.T) {
	cp := newConnectPair(t, `[{"capability":"apps.introduce","config":{}}]`, true)
	a := cp.a
	appEngine(t, a)
	bInst := mustIdentity(t, cp.b).InstanceID
	arrived, release := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	var events []string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == AppGrantsPath:
			close(arrived)
			<-release
			mu.Lock()
			events = append(events, "minted")
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"credential":{"apiKey":"k"}}`)
		case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, AppGrantsPath+"/"):
			mu.Lock()
			events = append(events, "revoked "+strings.TrimPrefix(r.URL.Path, AppGrantsPath+"/"))
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer stub.Close()
	a.appGrantEndpointFn = func(context.Context, *store.App) (string, string, error) { return stub.URL, "t", nil }

	minted := make(chan int, 1)
	go func() {
		status, _ := cp.fedConnect(t, cp.vouchFromB(t, "n-1"), fed.SignVouch(mustIdentity(t, cp.b), cp.vouchFromB(t, "n-1")), "")
		minted <- status
	}()
	<-arrived
	deleted := make(chan int, 1)
	go func() {
		deleted <- do(t, cp.aH, "DELETE", "/api/apps/feed", "", withCookie(cp.aCookie)).Code
	}()
	// The removal holds minting: a new connect is refused while it waits.
	waitFor(t, 10*time.Second, func() bool {
		l := a.connectLedger()
		l.mu.Lock()
		defer l.mu.Unlock()
		return l.frozen["feed"]
	})
	if status, body := cp.fedConnect(t, cp.vouchFromB(t, "n-2"), fed.SignVouch(mustIdentity(t, cp.b), cp.vouchFromB(t, "n-2")), ""); status != http.StatusServiceUnavailable {
		t.Fatalf("a connect while the app is being removed: %d %s", status, body)
	}
	close(release)
	if status := <-minted; status != http.StatusOK {
		t.Fatalf("the mint under way: %d", status)
	}
	if code := <-deleted; code != http.StatusOK {
		t.Fatalf("delete: %d", code)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) < 2 || events[0] != "minted" || !slices.Contains(events[1:], "revoked "+bInst) {
		t.Fatalf("the credential minted during removal was not revoked after it: %v", events)
	}
}
