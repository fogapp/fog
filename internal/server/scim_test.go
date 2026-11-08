package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"gitea.lan/matthew/fog/internal/signin"
	"gitea.lan/matthew/fog/internal/store"
)

// fakeSCIM is an app's SCIM endpoint (RFC 7644, the subset fog speaks),
// in memory, counting what it was asked.
type fakeSCIM struct {
	mu       sync.Mutex
	token    string
	users    map[string]signin.SCIMUser
	requests []string
	next     int
}

func (f *fakeSCIM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+f.token {
		w.WriteHeader(401)
		return
	}
	f.requests = append(f.requests, r.Method)
	w.Header().Set("Content-Type", "application/scim+json")
	switch {
	case r.Method == "GET":
		ext := strings.TrimSuffix(strings.TrimPrefix(r.URL.Query().Get("filter"), `externalId eq "`), `"`)
		res := []signin.SCIMUser{}
		for _, u := range f.users {
			if u.ExternalID == ext {
				res = append(res, u)
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"Resources": res, "totalResults": len(res)})
	case r.Method == "POST":
		var u signin.SCIMUser
		json.NewDecoder(r.Body).Decode(&u)
		f.next++
		u.ID = fmt.Sprintf("u%d", f.next)
		f.users[u.ID] = u
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(u)
	case r.Method == "PUT":
		id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		if _, ok := f.users[id]; !ok {
			w.WriteHeader(404)
			return
		}
		var u signin.SCIMUser
		json.NewDecoder(r.Body).Decode(&u)
		u.ID = id
		f.users[id] = u
		json.NewEncoder(w).Encode(u)
	}
}

func (f *fakeSCIM) byHandle(h string) (signin.SCIMUser, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, u := range f.users {
		if u.UserName == h {
			return u, true
		}
	}
	return signin.SCIMUser{}, false
}

func (f *fakeSCIM) take() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.requests
	f.requests = nil
	return r
}

func TestSCIMProvisioningReconciles(t *testing.T) {
	t.Parallel()

	f := newIdP(t, signin.Manifest{Contract: signin.Contract, Redirect: "/auth/oidc/callback", SCIM: "/scim/v2",
		Roles: []string{"admin", "member"}, Default: map[string]string{"operator": "admin", "member": "member"}})
	tok, err := f.srv.scim.Token(f.app)
	if err != nil {
		t.Fatal(err)
	}
	app := &fakeSCIM{token: tok, users: map[string]signin.SCIMUser{}}
	ts := httptest.NewServer(app)
	t.Cleanup(ts.Close)
	f.srv.scim.HTTP = &http.Client{Transport: hostRouter{addr: ts.Listener.Addr().String()}}
	addMember(t, f.srv, "kim")
	ctx := context.Background()

	// Only me, by default: the operator, as the app's admin.
	f.srv.Provision(ctx)
	if u, ok := app.byHandle("admin"); !ok || !u.Active || u.Roles[0].Value != "admin" || !strings.HasPrefix(u.ExternalID, "person:") {
		t.Fatalf("operator = %+v", u)
	}
	if _, ok := app.byHandle("kim"); ok {
		t.Fatal("kim provisioned into an operator-only app")
	}
	app.take()

	// Nothing changed: nothing sent.
	f.srv.Provision(ctx)
	if got := app.take(); len(got) != 0 {
		t.Fatalf("an idle reconcile sent %v", got)
	}

	// Shared with the household: Kim exists before she ever signs in.
	f.srv.store.PutSigninPolicy(store.SigninPolicy{AppID: f.app.ID, Audience: "household"})
	f.srv.Provision(ctx)
	kim, ok := app.byHandle("kim")
	if !ok || !kim.Active || kim.Roles[0].Value != "member" {
		t.Fatalf("kim = %+v", kim)
	}

	// Renamed: replaced in place.
	p, _ := f.srv.store.PersonByHandle("kim")
	f.srv.store.UpdatePerson(p.ID, "Kim Werner", p.Role, p.Status)
	app.take()
	f.srv.Provision(ctx)
	if got := app.take(); strings.Join(got, ",") != "PUT" {
		t.Fatalf("a rename sent %v", got)
	}
	if kim, _ := app.byHandle("kim"); kim.DisplayName != "Kim Werner" {
		t.Fatalf("after rename: %+v", kim)
	}

	// Suspended: switched off, never deleted.
	f.srv.store.UpdatePerson(p.ID, "Kim Werner", p.Role, store.PersonStatusSuspended)
	f.srv.Provision(ctx)
	if kim, ok := app.byHandle("kim"); !ok || kim.Active {
		t.Fatalf("after suspension: %+v (present %v)", kim, ok)
	}

	// fog forgets what it told the app (a restore): it finds Kim again by
	// externalId instead of creating her twice.
	f.srv.store.UpdatePerson(p.ID, "Kim Werner", p.Role, store.PersonStatusActive)
	f.srv.store.DeletePersonSessions(p.ID)
	if _, err := f.srv.store.SCIMStates("stream"); err != nil {
		t.Fatal(err)
	}
	execSQL(t, f.srv.store, `DELETE FROM scim_state`)
	before := len(app.users)
	f.srv.Provision(ctx)
	if len(app.users) != before {
		t.Fatalf("lost state created duplicates: %d → %d", before, len(app.users))
	}
	if kim, _ := app.byHandle("kim"); !kim.Active {
		t.Fatalf("reinstated kim = %+v", kim)
	}

	// The app folds Kim's provisioned account into an older one (a new id,
	// the same externalId): fog's next change finds her again by
	// externalId instead of PUTting into the void forever.
	app.mu.Lock()
	for id, u := range app.users {
		if u.UserName == "kim" {
			delete(app.users, id)
			u.ID = "folded"
			app.users["folded"] = u
		}
	}
	app.mu.Unlock()
	f.srv.store.UpdatePerson(p.ID, "Kim W.", p.Role, store.PersonStatusActive)
	f.srv.Provision(ctx)
	if kim, _ := app.byHandle("kim"); kim.ID != "folded" || kim.DisplayName != "Kim W." {
		t.Fatalf("after the app folded kim: %+v", kim)
	}
}

func execSQL(t *testing.T, st *store.Store, q string) {
	t.Helper()
	if _, err := st.DB().Exec(q); err != nil {
		t.Fatal(err)
	}
}
