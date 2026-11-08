package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/auth"
	"gitea.lan/matthew/fog/internal/signin"
	"gitea.lan/matthew/fog/internal/store"
)

// TestSCIMDeprovisionsAPersonFogDeleted is the SCIM finding: deleting a
// person cascaded scim_state away, so the app never heard active: false,
// and a failure was only logged. Now the removal is kept until the app has
// heard it — through an outage and a restart — and an outage that lasts is
// in the Inbox until it heals.
func TestSCIMDeprovisionsAPersonFogDeleted(t *testing.T) {
	t.Parallel()

	f := newIdP(t, signin.Manifest{Contract: signin.Contract, Redirect: "/auth/oidc/callback", SCIM: "/scim/v2",
		Roles: []string{"admin", "member"}, Default: map[string]string{"operator": "admin", "member": "member"}})
	tok, err := f.srv.scim.Token(f.app)
	if err != nil {
		t.Fatal(err)
	}
	app := &fakeSCIM{token: tok, users: map[string]signin.SCIMUser{}}
	var down atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		app.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	client := &http.Client{Transport: hostRouter{addr: ts.Listener.Addr().String()}}
	f.srv.scim.HTTP = client
	addMember(t, f.srv, "kim")
	f.srv.store.PutSigninPolicy(store.SigninPolicy{AppID: f.app.ID, Audience: "household"})
	ctx := context.Background()
	f.srv.Provision(ctx)
	if kim, ok := app.byHandle("kim"); !ok || !kim.Active {
		t.Fatalf("kim was not provisioned: %+v", kim)
	}

	// The app is down when the operator removes Kim.
	down.Store(true)
	kim, _ := f.srv.store.PersonByHandle("kim")
	if w := do(t, f.srv.Handler(), "DELETE", fmt.Sprintf("/api/people/%d", kim.ID), "", withCookie(f.operator)); w.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	f.srv.bgWG.Wait()
	f.srv.Provision(ctx)

	// fog restarts: a new provisioner over the same database still owes the
	// app Kim's departure. An hour of failures later, the Inbox says so.
	restarted := &signin.Provisioner{Store: f.srv.store, Vault: f.srv.vault, Log: f.srv.log, Instance: f.srv.fed.RequireInstanceID,
		HTTP: client, Now: func() time.Time { return time.Now().Add(2 * time.Hour) }}
	restarted.Run(ctx)
	if !hasNotice(t, f.srv, "signin-scim", "stream") {
		t.Fatal("an hour of failed provisioning reached no one")
	}
	if u, _ := app.byHandle("kim"); !u.Active {
		t.Fatal("kim was switched off while the app was down?")
	}

	// It heals: Kim is switched off, the debt is paid, and the notice goes.
	down.Store(false)
	restarted.Run(ctx)
	if u, ok := app.byHandle("kim"); !ok || u.Active {
		t.Fatalf("after the app came back, kim = %+v (present %v); want active: false", u, ok)
	}
	if gone, _ := f.srv.store.SCIMDepartures("stream"); len(gone) != 0 {
		t.Fatalf("departures still owed: %+v", gone)
	}
	if hasNotice(t, f.srv, "signin-scim", "stream") {
		t.Fatal("the notice outlived the outage")
	}
	app.take()
	restarted.Run(ctx)
	if got := app.take(); len(got) != 0 {
		t.Fatalf("a settled departure was sent again: %v", got)
	}
}

func hasNotice(t *testing.T, srv *Server, kind, app string) bool {
	t.Helper()
	notices, err := srv.store.ActiveNotices()
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range notices {
		if n.Kind == kind && n.App == app {
			return true
		}
	}
	return false
}

// TestAnAppCantRedirectFogsDeliveries: a hostile app answering its
// back-channel logout or SCIM endpoint with a redirect could aim fogd at an
// internal address, or at a server that answers 200 for it and fakes the
// delivery. fog follows no redirect: the 3xx is a failed delivery, and the
// target is never reached.
func TestAnAppCantRedirectFogsDeliveries(t *testing.T) {
	t.Parallel()

	f := newIdP(t, signin.Manifest{Contract: signin.Contract, Redirect: "/auth/oidc/callback", Logout: "/auth/oidc/backchannel",
		SCIM: "/scim/v2", Roles: []string{"admin", "member"}, Default: map[string]string{"operator": "admin", "member": "member"}})
	var internalHits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal" {
			internalHits.Add(1)
			w.Header().Set("Content-Type", "application/scim+json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"id":"u1","Resources":[]}`))
			return
		}
		http.Redirect(w, r, "/internal", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(ts.Close)
	client := &http.Client{Transport: hostRouter{addr: ts.Listener.Addr().String()}}
	f.srv.logouts.HTTP = client
	f.srv.scim.HTTP = client

	// A logout owed to the app: refused by redirect, still owed.
	f.signInRP(t)
	f.srv.store.DeletePersonSessionByDigest(auth.DigestToken(f.operator.Value))
	f.srv.bgWG.Wait()
	f.srv.DeliverLogouts(context.Background())
	if owed, _ := f.srv.store.CountAppOutbox(); owed["stream"] == 0 {
		t.Fatal("a redirected logout counted as delivered")
	}
	// A reconcile: refused by redirect, recorded as failing.
	f.srv.Provision(context.Background())
	if _, noticed, err := f.srv.store.SCIMFailing("stream", "probe"); err != nil || noticed {
		t.Fatalf("scim health: %v %v", noticed, err)
	}
	if states, _ := f.srv.store.SCIMStates("stream"); len(states) != 0 {
		t.Fatalf("a redirected SCIM call was recorded as sent: %+v", states)
	}
	if n := internalHits.Load(); n != 0 {
		t.Fatalf("fogd followed the app's redirect %d times", n)
	}
}
