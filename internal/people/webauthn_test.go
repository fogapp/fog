package people

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
)

// TestCeremoniesCantBeEvictedOrReused: a ceremony lives in the id its
// browser holds, so a flood of other ceremonies can't displace it; it is good
// once, for its own purpose, until it expires, and an altered id is nothing.
func TestCeremoniesCantBeEvictedOrReused(t *testing.T) {
	c := NewCeremonies()
	victim := c.Put(CeremonyLogin, &webauthn.SessionData{Challenge: "victims-challenge"}, 0)
	for range 5000 {
		c.Put(CeremonyLogin, &webauthn.SessionData{Challenge: "flood"}, 0)
	}
	got, _, ok := c.Take(CeremonyLogin, victim)
	if !ok || got.Challenge != "victims-challenge" {
		t.Fatal("a flood of ceremonies displaced the victim's")
	}
	if _, _, ok := c.Take(CeremonyLogin, victim); ok {
		t.Fatal("a ceremony was finished twice")
	}

	reg := c.Put(CeremonyRegister, &webauthn.SessionData{Challenge: "r"}, 7)
	if _, _, ok := c.Take(CeremonyConfirm, reg); ok {
		t.Fatal("a registration ceremony finished a confirmation")
	}
	if _, who, ok := c.Take(CeremonyRegister, reg); !ok || who != 7 {
		t.Fatalf("the registration's own finish: %v %d", ok, who)
	}

	tampered := []byte(c.Put(CeremonyLogin, &webauthn.SessionData{Challenge: "t"}, 0))
	tampered[len(tampered)-3] ^= 1
	if _, _, ok := c.Take(CeremonyLogin, string(tampered)); ok {
		t.Fatal("an altered ceremony opened")
	}
	other := NewCeremonies() // another process's key
	if _, _, ok := other.Take(CeremonyLogin, c.Put(CeremonyLogin, &webauthn.SessionData{}, 0)); ok {
		t.Fatal("a ceremony opened under another key")
	}

	stale := c.Put(CeremonyLogin, &webauthn.SessionData{}, 0)
	c.now = func() time.Time { return time.Now().Add(ceremonyTTL + time.Second) }
	if _, _, ok := c.Take(CeremonyLogin, stale); ok {
		t.Fatal("an expired ceremony opened")
	}
}

// TestPasskeysAreSpentOnlyAtFogsOrigin: the RP ID is the household's domain,
// so a sibling app may get a browser to make an assertion for it. fog
// accepts an assertion only for its own origin, however the request names
// its host.
func TestPasskeysAreSpentOnlyAtFogsOrigin(t *testing.T) {
	req := func(host, fwd string) *http.Request {
		r := httptest.NewRequest("POST", "/api/people/login/passkey/finish", nil)
		r.Host = host
		r.Header.Set("X-Forwarded-Proto", "https")
		if fwd != "" {
			r.Header.Set("X-Forwarded-Host", fwd)
		}
		return r
	}
	rp, err := RelyingParty(req("fog.home.test", ""), "home.test", "Home")
	if err != nil {
		t.Fatal(err)
	}
	if rp.Config.RPID != "home.test" || len(rp.Config.RPOrigins) != 1 || rp.Config.RPOrigins[0] != "https://fog.home.test" {
		t.Fatalf("rp = %s %v", rp.Config.RPID, rp.Config.RPOrigins)
	}
	for _, r := range []*http.Request{req("stream.home.test", ""), req("fog.home.test", "stream.home.test"), req("home.test", "")} {
		if _, err := RelyingParty(r, "home.test", "Home"); err == nil {
			t.Errorf("a passkey ceremony was offered for %s / %s", r.Host, r.Header.Get("X-Forwarded-Host"))
		}
	}
	// Off the household's domain (a tunnel's name) the page's own origin
	// stands, as before.
	if rp, err := RelyingParty(req("myfog.example.net", ""), "home.test", "Home"); err != nil || rp.Config.RPOrigins[0] != "https://myfog.example.net" {
		t.Fatalf("off-domain: %v %v", err, rp)
	}
}
