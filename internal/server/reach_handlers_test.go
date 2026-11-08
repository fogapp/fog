package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"gitea.lan/matthew/fog/internal/dnssync"
	"gitea.lan/matthew/fog/internal/proxy"
	"gitea.lan/matthew/fog/internal/store"
)

// The beacon is the one unauthenticated door this feature opens. It must
// answer only to the minted token, and a visit must be recorded with the
// address the visit came from — through the proxy, the forwarded one.
func TestReachBeaconRequiresTokenAndRecordsVisitor(t *testing.T) {
	t.Parallel()

	srv, h := newTestServer(t)
	c := setup(t, h)
	srv.store.SetSetting(store.SettingBaseDomain, "home.example.com")
	secret := withProxy(t, srv)

	if w := do(t, h, "GET", "/api/reach/beacon/anything", ""); w.Code != http.StatusNotFound {
		t.Fatalf("no token minted yet: want 404, got %d", w.Code)
	}

	// Reading reach status mints the token via the beacon URL.
	w := do(t, h, "GET", "/api/domain/reach", "", withCookie(c))
	if w.Code != http.StatusOK {
		t.Fatalf("reach status: %d %s", w.Code, w.Body)
	}
	var res struct {
		BeaconURL string         `json:"beaconUrl"`
		Status    dnssync.Status `json:"status"`
	}
	json.Unmarshal(w.Body.Bytes(), &res)
	if !strings.HasPrefix(res.BeaconURL, "https://fog.home.example.com/api/reach/beacon/") {
		t.Fatalf("beacon url: %q", res.BeaconURL)
	}
	if res.Status.Target != dnssync.TargetLAN {
		t.Fatalf("default target must be lan, got %q", res.Status.Target)
	}
	path := strings.TrimPrefix(res.BeaconURL, "https://fog.home.example.com")

	if w := do(t, h, "GET", path+"x", ""); w.Code != http.StatusNotFound {
		t.Fatalf("wrong token: want 404, got %d", w.Code)
	}
	w = do(t, h, "GET", path, "", func(r *http.Request) {
		r.RemoteAddr = "172.18.0.5:41000" // Caddy's container
		r.Header.Set(proxy.HeaderClientIP, "203.0.113.200")
		r.Header.Set(proxy.HeaderSecret, secret)
	})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "from outside your home network") {
		t.Fatalf("beacon hit: %d %s", w.Code, w.Body)
	}
	w = do(t, h, "GET", "/api/domain/reach", "", withCookie(c))
	json.Unmarshal(w.Body.Bytes(), &res)
	if res.Status.Beacon == nil || res.Status.Beacon.IP != "203.0.113.200" || !res.Status.Beacon.Public {
		t.Fatalf("visit not recorded: %+v", res.Status.Beacon)
	}

	// A LAN visitor is recorded but doesn't count as proof.
	w = do(t, h, "GET", path, "", func(r *http.Request) { r.RemoteAddr = "192.168.1.55:5000" })
	if !strings.Contains(w.Body.String(), "inside your home network") {
		t.Fatalf("LAN visit page: %s", w.Body)
	}
}

func TestReachUpdateValidatesAndPersists(t *testing.T) {
	t.Parallel()

	srv, h := newTestServer(t)
	c := setup(t, h)
	if w := do(t, h, "PUT", "/api/domain/reach", `{"target":"moon"}`, withCookie(c)); w.Code != http.StatusBadRequest {
		t.Fatalf("bad target: want 400, got %d", w.Code)
	}
	if w := do(t, h, "PUT", "/api/domain/reach", `{"target":"wan"}`); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: want 401, got %d", w.Code)
	}
	// No domain/token yet: the setting is saved and the reconcile is a
	// no-op rather than an error — the wizard's later steps pick it up.
	w := do(t, h, "PUT", "/api/domain/reach", `{"target":"wan","portMap":false}`, withCookie(c))
	if w.Code != http.StatusOK {
		t.Fatalf("update: %d %s", w.Code, w.Body)
	}
	if v, _ := srv.store.GetSetting(dnssync.SettingTarget); v != "wan" {
		t.Fatalf("target not saved: %q", v)
	}
	if v, _ := srv.store.GetSetting(dnssync.SettingPortMap); v != "off" {
		t.Fatalf("port map not saved: %q", v)
	}
	var res struct {
		Status dnssync.Status `json:"status"`
	}
	json.Unmarshal(w.Body.Bytes(), &res)
	if res.Status.Target != dnssync.TargetWAN || res.Status.PortMap.Enabled {
		t.Fatalf("status after update: %+v", res.Status)
	}
}
