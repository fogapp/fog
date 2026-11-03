package server

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"gitea.lan/matthew/fog/internal/auth"
	"gitea.lan/matthew/fog/internal/signin"
	"gitea.lan/matthew/fog/internal/store"
)

// directoryFixture: an app that signs in with fog, a member, a guest, and a
// paired household advertising one person with an address and one without.
func directoryFixture(t *testing.T, contracts []string, email bool) (*idpFixture, string) {
	t.Helper()
	m := signin.Manifest{Contract: signin.Contract, Redirect: "/auth/oidc/callback", Email: email}
	f := newIdP(t, m)
	addMember(t, f.srv, "kim")
	if _, err := f.srv.store.CreatePerson("visitor", "Visitor", store.PersonRoleGuest); err != nil {
		t.Fatal(err)
	}
	kim, _ := f.srv.store.PersonByHandle("kim")
	f.srv.store.SetPersonEmail(kim.ID, "kim@example.com")
	f.srv.store.SetAppContracts(f.app.ID, contracts)
	token := "stream-app-token"
	f.srv.store.SetAppAPITokenDigest(f.app.ID, auth.DigestToken(token))
	peer, err := f.srv.store.CreatePairedPeer("DAVEHOUSE", "dave", "https://fog.dave.test", "")
	if err != nil {
		t.Fatal(err)
	}
	people := `[{"address":"person:DAVEHOUSE/01DAVE","handle":"dave","displayName":"Dave","role":"member","devices":[]},
		{"handle":"old","displayName":"Old peer","role":"member","devices":[]},
		{"address":"person:SOMEONEELSE/01X","handle":"impostor","displayName":"Not Dave's","role":"member","devices":[]}]`
	if err := f.srv.store.UpdateFedPeerHeartbeat(peer.ID, "v", "[]", "[]", people); err != nil {
		t.Fatal(err)
	}
	return f, token
}

func appGet(t *testing.T, f *idpFixture, token, path string) (int, []byte) {
	t.Helper()
	w := do(t, f.srv.Handler(), "GET", path, "", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) })
	return w.Code, w.Body.Bytes()
}

func TestAppDirectoryIsTheHouseholdByAddress(t *testing.T) {
	f, token := directoryFixture(t, nil, false)
	code, body := appGet(t, f, token, "/app/v1/people")
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	var out struct{ People []directoryEntry }
	json.Unmarshal(body, &out)
	handles := []string{}
	for _, e := range out.People {
		handles = append(handles, e.Handle)
		if !strings.HasPrefix(e.Address, "person:") || e.Chat == "" || !e.Home.Ours {
			t.Errorf("entry %+v", e)
		}
		if e.Email != "" {
			t.Errorf("email released to an app not registered for it: %+v", e)
		}
	}
	if strings.Join(handles, ",") != "admin,kim" {
		t.Fatalf("people = %v; guests and peers must not appear for a non-federating app", handles)
	}
}

func TestAppDirectoryEmailOnlyWithTheScope(t *testing.T) {
	f, token := directoryFixture(t, nil, true)
	_, body := appGet(t, f, token, "/app/v1/people")
	if !strings.Contains(string(body), "kim@example.com") {
		t.Fatalf("email scope did not release email: %s", body)
	}
}

func TestAppDirectoryPeersForFederatingAppsOnly(t *testing.T) {
	f, token := directoryFixture(t, []string{ContractConnectV1}, true)
	_, body := appGet(t, f, token, "/app/v1/people")
	var out struct{ People []directoryEntry }
	json.Unmarshal(body, &out)
	var dave *directoryEntry
	for i, e := range out.People {
		if e.Handle == "impostor" || e.Handle == "old" {
			t.Fatalf("listed %s: a peer speaks only for its own people, and only by address", e.Handle)
		}
		if e.Handle == "dave" {
			dave = &out.People[i]
		}
	}
	if dave == nil || dave.Home.Ours || dave.Home.Name != "dave" || dave.Email != "" {
		t.Fatalf("dave = %+v", dave)
	}
	code, _ := appGet(t, f, token, "/app/v1/people/"+dave.Address)
	if code != 200 {
		t.Fatalf("one person by address: %d", code)
	}
}

func TestAppDirectoryNeedsTheContract(t *testing.T) {
	f, _ := directoryFixture(t, nil, false)
	other, _ := f.srv.store.CreateApp("sab")
	f.srv.store.SetAppAPITokenDigest(other.ID, auth.DigestToken("sab-token"))
	if code, _ := appGet(t, f, "sab-token", "/app/v1/people"); code != http.StatusForbidden {
		t.Fatalf("an app without fog-signin/1 read the directory: %d", code)
	}
}

func TestPersonPagesStayOnTheAppsOwnHosts(t *testing.T) {
	f, token := directoryFixture(t, []string{ContractConnectV1}, false)
	spec := `{"source":{"type":"image","image":"x"},"routes":[{"port":80}]}`
	f.srv.store.CreateRevision(f.app.ID, spec, "", "t")
	put := func(body string) int {
		return do(t, f.srv.Handler(), "PUT", "/app/v1/person-pages", body, func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+token)
		}).Code
	}
	if c := put(`{"address":"person:DAVEHOUSE/01DAVE","url":"https://evil.test/dave"}`); c != http.StatusBadRequest {
		t.Fatalf("a page off the app's hosts: %d", c)
	}
	if c := put(`{"address":"person:NOBODY/01","url":"https://stream.home.test/p/1"}`); c != http.StatusBadRequest {
		t.Fatalf("a page for someone outside the directory: %d", c)
	}
	if c := put(`{"address":"person:DAVEHOUSE/01DAVE","url":"https://stream.home.test/people/dave","label":"Open in Stream"}`); c != 200 {
		t.Fatalf("a good page: %d", c)
	}
	w := do(t, f.srv.Handler(), "GET", "/api/people/lookup?address="+url.QueryEscape("person:DAVEHOUSE/01DAVE"), "", withCookie(f.operator))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"chatAddress":"fog:DAVEHOUSE/dave"`) || !strings.Contains(w.Body.String(), "Open in Stream") {
		t.Fatalf("lookup: %d %s", w.Code, w.Body)
	}

	// The directory carries the page on its person and an empty list on
	// everyone else, both listed and read one at a time.
	_, body := appGet(t, f, token, "/app/v1/people")
	var out struct{ People []directoryEntry }
	json.Unmarshal(body, &out)
	for _, e := range out.People {
		want := 0
		if e.Address == "person:DAVEHOUSE/01DAVE" {
			want = 1
		}
		if e.Pages == nil || len(e.Pages) != want {
			t.Errorf("%s pages = %+v, want %d", e.Handle, e.Pages, want)
		}
	}
	code, one := appGet(t, f, token, "/app/v1/people/person:DAVEHOUSE/01DAVE")
	var dave directoryEntry
	json.Unmarshal(one, &dave)
	if code != 200 || len(dave.Pages) != 1 || dave.Pages[0].Label != "Open in Stream" {
		t.Fatalf("one person: %d %s", code, one)
	}
	kim, _ := f.srv.store.PersonByHandle("kim")
	code, one = appGet(t, f, token, "/app/v1/people/person:"+mustInstanceID(t, f)+"/"+kim.UID)
	if code != 200 || !strings.Contains(string(one), `"pages":[]`) {
		t.Fatalf("one person without pages: %d %s", code, one)
	}
}

func mustInstanceID(t *testing.T, f *idpFixture) string {
	t.Helper()
	inst, err := f.srv.fed.Identity()
	if err != nil {
		t.Fatal(err)
	}
	return inst.InstanceID
}
