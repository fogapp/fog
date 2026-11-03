package dnssync

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/cloudflare"
	"gitea.lan/matthew/fog/internal/proxy"
	"gitea.lan/matthew/fog/internal/store"
	"gitea.lan/matthew/fog/internal/vault"
	"gitea.lan/matthew/fog/internal/wan"
)

func activeKind(t *testing.T, st *store.Store, kind string) *store.Notice {
	t.Helper()
	notices, err := st.ActiveNotices()
	if err != nil {
		t.Fatal(err)
	}
	for i := range notices {
		if notices[i].Kind == kind {
			return &notices[i]
		}
	}
	return nil
}

// Repeated reconcile failures must surface as ONE persistent notice — not a
// Warn line every five minutes forever, and not a fresh notice per tick —
// and the next success must take it back down.
func TestFailureStreakRaisesOneNoticeAndRecoveryClearsIt(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	k := &Keeper{Store: st, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	boom := errors.New("cloudflare: authentication error (10000)")

	// Below the threshold: no notice yet.
	for i := 0; i < failNoticeThreshold-1; i++ {
		k.noteFailure("home.example.com", boom)
	}
	if n := activeKind(t, st, noticeKind); n != nil {
		t.Fatalf("notice raised after only %d failures: %+v", failNoticeThreshold-1, n)
	}

	// Crossing the threshold raises it; further ticks must not stack more.
	k.noteFailure("home.example.com", boom)
	k.noteFailure("home.example.com", boom)
	k.noteFailure("home.example.com", boom)
	notices, err := st.ActiveNotices()
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, n := range notices {
		if n.Kind == noticeKind {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("want exactly 1 active %s notice, got %d", noticeKind, count)
	}

	// Recovery clears the notice and resets the streak.
	k.noteSuccess("home.example.com")
	if n := activeKind(t, st, noticeKind); n != nil {
		t.Fatalf("notice still active after recovery: %+v", n)
	}
	if n := k.fails.Streak(""); n != 0 || k.Status().Failing {
		t.Fatalf("streak not reset: %d", n)
	}

	// A fresh blip after recovery starts counting from zero again.
	k.noteFailure("home.example.com", boom)
	if n := activeKind(t, st, noticeKind); n != nil {
		t.Fatal("single post-recovery failure re-raised the notice")
	}
}

// ---- reconcile against fakes ----

// fakeCF is one zone with one record list, served the way the real API
// serves it, so the keeper's convergence is asserted on state.
type fakeCF struct {
	mu      sync.Mutex
	records []cloudflare.Record
	calls   int
}

func newFakeCF(t *testing.T) (*fakeCF, string) {
	t.Helper()
	f := &fakeCF{}
	env := func(w http.ResponseWriter, result any) {
		raw, _ := json.Marshal(result)
		json.NewEncoder(w).Encode(map[string]any{"success": true, "result": json.RawMessage(raw)})
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /zones", func(w http.ResponseWriter, r *http.Request) {
		env(w, []cloudflare.Zone{{ID: "z1", Name: r.URL.Query().Get("name"), Status: "active"}})
	})
	mux.HandleFunc("GET /zones/z1/dns_records", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls++
		typ, name := r.URL.Query().Get("type"), r.URL.Query().Get("name")
		out := []cloudflare.Record{}
		for _, rec := range f.records {
			if (typ == "" || rec.Type == typ) && (name == "" || rec.Name == name) {
				out = append(out, rec)
			}
		}
		env(w, out)
	})
	mux.HandleFunc("POST /zones/z1/dns_records", func(w http.ResponseWriter, r *http.Request) {
		var rec cloudflare.Record
		json.NewDecoder(r.Body).Decode(&rec)
		f.mu.Lock()
		defer f.mu.Unlock()
		rec.ID = "rec-1"
		f.records = append(f.records, rec)
		env(w, rec)
	})
	mux.HandleFunc("PUT /zones/z1/dns_records/{id}", func(w http.ResponseWriter, r *http.Request) {
		var rec cloudflare.Record
		json.NewDecoder(r.Body).Decode(&rec)
		rec.ID = r.PathValue("id")
		f.mu.Lock()
		defer f.mu.Unlock()
		for i := range f.records {
			if f.records[i].ID == rec.ID {
				f.records[i] = rec
			}
		}
		env(w, rec)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return f, srv.URL
}

func (f *fakeCF) wildcard() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.records {
		if r.Name == "*.home.example.com" && r.Type == "A" {
			return r.Content
		}
	}
	return ""
}

// fakeRouter is a PortMapper that either opens everything or refuses.
type fakeRouter struct {
	mu       sync.Mutex
	refuse   bool
	external string
	maps     int
	unmaps   int
}

func (r *fakeRouter) Map(ctx context.Context, lanIP string, ports []int, lease time.Duration) (*wan.Mapping, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.maps++
	if r.refuse {
		return nil, errors.New("NAT-PMP: refused; UPnP: no UPnP gateway answered")
	}
	return &wan.Mapping{Method: "natpmp", Gateway: "192.168.1.1", ExternalIP: r.external, Opened: ports, Lease: lease}, nil
}

func (r *fakeRouter) Unmap(ctx context.Context, method string, ports []int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.unmaps++
	return nil
}

type harness struct {
	k       *Keeper
	cf      *fakeCF
	router  *fakeRouter
	public  string
	pubErr  error
	hairpin error // what the from-inside self-test returns
	gateway string
	mu      sync.Mutex
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	vlt, err := vault.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	cf, base := newFakeCF(t)
	h := &harness{cf: cf, router: &fakeRouter{external: "203.0.113.9"}, public: "203.0.113.9", gateway: "192.168.1.1"}
	ct, _ := vlt.Encrypt("tok")
	st.SetSecret(0, proxy.SettingDNSToken, ct)
	st.SetSetting(store.SettingBaseDomain, "home.example.com")
	h.k = h.newKeeper(st, vlt, base)
	return h
}

// newKeeper builds a keeper over the harness's fakes — a second one over
// the same store is "fogd restarted".
func (h *harness) newKeeper(st *store.Store, vlt *vault.Vault, base string) *Keeper {
	return &Keeper{
		Store: st, Vault: vlt, Log: slog.New(slog.NewTextHandler(os.Stderr, nil)),
		CFBase: base, Mapper: h.router,
		Gateway: func() string {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.gateway
		},
		PublicIP: func(ctx context.Context) (string, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.public, h.pubErr
		},
		HairpinDial: func(ctx context.Context, publicIP, host string) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.hairpin
		},
	}
}

func (h *harness) set(f func(h *harness)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	f(h)
}

func (h *harness) setPublic(ip string, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.public, h.pubErr = ip, err
}

func TestLANTargetWritesLANAddress(t *testing.T) {
	h := newHarness(t)
	lan := wan.LANIP()
	if lan == "" {
		t.Skip("no LAN address on this machine")
	}
	st := h.k.Reconcile(context.Background(), false)
	if st.Target != TargetLAN || st.RecordIP != lan || st.LastAction != "created" {
		t.Fatalf("status: %+v", st)
	}
	if got := h.cf.wildcard(); got != lan {
		t.Fatalf("record holds %q, want LAN %q", got, lan)
	}
	if h.router.maps != 0 {
		t.Fatal("lan target must never touch the router")
	}
	// A second, unchanged pass is free: no record listing at all.
	before := h.cf.calls
	h.k.Reconcile(context.Background(), false)
	if h.cf.calls != before {
		t.Fatal("unchanged address must not consult the API")
	}
}

func TestWANTargetFollowsPublicAddressAndOpensPorts(t *testing.T) {
	h := newHarness(t)
	if wan.LANIP() == "" {
		t.Skip("no LAN address on this machine")
	}
	h.k.Reconcile(context.Background(), false) // lan first, like a real box
	h.k.Store.SetSetting(SettingTarget, "wan")

	st := h.k.Reconcile(context.Background(), true)
	if st.Target != TargetWAN || st.PublicIP != "203.0.113.9" || st.RecordIP != "203.0.113.9" {
		t.Fatalf("status after switch: %+v", st)
	}
	if got := h.cf.wildcard(); got != "203.0.113.9" {
		t.Fatalf("record holds %q after switching to wan", got)
	}
	if st.NAT != "nat" || st.PortMap.Method != "natpmp" || len(st.PortMap.Opened) != 2 || st.PortMap.ExpiresAt.IsZero() {
		t.Fatalf("port map status: %+v nat=%s", st.PortMap, st.NAT)
	}
	if n := activeKind(t, h.k.Store, portNoticeKind); n != nil {
		t.Fatalf("no port notice when the router opened the ports: %+v", n)
	}

	// The ISP hands out a new address: the next quiet tick catches it and
	// announces the change; the router lease, still fresh, is left alone.
	h.setPublic("198.51.100.7", nil)
	maps := h.router.maps
	st = h.k.Reconcile(context.Background(), false)
	if st.RecordIP != "198.51.100.7" || h.cf.wildcard() != "198.51.100.7" || st.LastAction != "updated" {
		t.Fatalf("record not re-pointed at the new public address: %+v", st)
	}
	if n := activeKind(t, h.k.Store, "dns-reconciled"); n == nil {
		t.Fatal("an unattended address change must raise the reconciled notice")
	}
	if h.router.maps != maps {
		t.Fatal("a fresh lease must not be renewed on every tick")
	}

	// The public-address sources go dark: the record is left alone, the
	// status says why, and the last-known address is still displayed.
	h.setPublic("", errors.New("couldn't learn the public address: timeout"))
	st = h.k.Reconcile(context.Background(), false)
	if st.PublicIPError == "" || st.PublicIP != "198.51.100.7" || h.cf.wildcard() != "198.51.100.7" {
		t.Fatalf("status with sources down: %+v", st)
	}

	// Back to lan: the record follows and the mapping is released.
	h.setPublic("198.51.100.7", nil)
	h.k.Store.SetSetting(SettingTarget, "lan")
	st = h.k.Reconcile(context.Background(), true)
	if st.Target != TargetLAN || h.cf.wildcard() != wan.LANIP() || h.router.unmaps != 1 || st.PortMap.Method != "" {
		t.Fatalf("switch back to lan: %+v unmaps=%d", st, h.router.unmaps)
	}
}

func TestPortForwardNoticeRaisedOnceAndClearedByBeacon(t *testing.T) {
	h := newHarness(t)
	if wan.LANIP() == "" {
		t.Skip("no LAN address on this machine")
	}
	h.router.refuse = true
	// Nothing gets in: the router won't map, and the from-inside test
	// fails too (no forward, no loopback — it can't tell which yet).
	h.set(func(h *harness) { h.hairpin = errors.New("i/o timeout") })
	h.k.Store.SetSetting(SettingTarget, "wan")
	for i := 0; i < 3; i++ {
		h.k.Reconcile(context.Background(), true)
	}
	st := h.k.Status()
	if st.PortMap.Error == "" || st.NAT != "nat" || st.Gateway != "192.168.1.1" {
		t.Fatalf("status: %+v", st)
	}
	notices, _ := h.k.Store.ActiveNotices()
	count := 0
	for _, n := range notices {
		if n.Kind == portNoticeKind {
			count++
			if !strings.Contains(n.Body, "192.168.1.1") || !strings.Contains(n.Body, "443") {
				t.Fatalf("notice must name the gateway and the port: %s", n.Body)
			}
		}
	}
	if count != 1 {
		t.Fatalf("want exactly one port notice, got %d", count)
	}

	// A hit from the LAN proves nothing; a hit from outside settles it.
	h.k.RecordBeacon("192.168.1.55")
	if activeKind(t, h.k.Store, portNoticeKind) == nil {
		t.Fatal("a LAN visitor must not clear the port notice")
	}
	seen := h.k.RecordBeacon("203.0.113.200")
	if !seen.Public || activeKind(t, h.k.Store, portNoticeKind) != nil {
		t.Fatal("an outside visitor must clear the port notice")
	}
	// Still refused next tick — but confirmed, so no nagging.
	h.k.Reconcile(context.Background(), true)
	if activeKind(t, h.k.Store, portNoticeKind) != nil {
		t.Fatal("port notice re-raised after the beacon confirmed reach")
	}
	if h.k.Status().Beacon == nil || h.k.Status().Beacon.IP != "203.0.113.200" {
		t.Fatal("beacon hit not kept in status")
	}

	// CGNAT: the router's WAN side isn't the address the world sees.
	h.router.refuse = false
	h.router.external = "100.70.1.2"
	h.k.ClearBeacon()
	st = h.k.Reconcile(context.Background(), true)
	if st.NAT != "cgnat" {
		t.Fatalf("router WAN 100.70.1.2 vs public 203.0.113.9 must read as cgnat, got %s", st.NAT)
	}
}

func TestBeaconTokenIsStableAndChecked(t *testing.T) {
	h := newHarness(t)
	url1, err := h.k.BeaconURL()
	if err != nil {
		t.Fatal(err)
	}
	url2, _ := h.k.BeaconURL()
	if url1 != url2 || !strings.HasPrefix(url1, "https://fog.home.example.com/api/reach/beacon/") {
		t.Fatalf("beacon url: %s / %s", url1, url2)
	}
	tok := strings.TrimPrefix(url1, "https://fog.home.example.com/api/reach/beacon/")
	if !h.k.CheckBeacon(tok) || h.k.CheckBeacon("nope") || h.k.CheckBeacon("") {
		t.Fatal("token check")
	}
}

// Moving house without a reboot (or the same router handing out a new
// address): the public address may or may not change, but the mapping
// the old network granted is void either way and must be asked for
// again — on the new gateway, for the new address.
func TestMappingFollowsTheNetwork(t *testing.T) {
	h := newHarness(t)
	if wan.LANIP() == "" {
		t.Skip("no LAN address on this machine")
	}
	h.k.Store.SetSetting(SettingTarget, "wan")
	h.k.Reconcile(context.Background(), true)
	if h.router.maps != 1 {
		t.Fatalf("maps = %d", h.router.maps)
	}
	// Same address, same router, fresh lease: nothing to do.
	h.k.Reconcile(context.Background(), false)
	if h.router.maps != 1 {
		t.Fatalf("fresh lease renewed needlessly: maps = %d", h.router.maps)
	}
	// New router, same public address (a second router at the same
	// house, or a test bench): the mapping is re-requested, and the old
	// one is NOT released against a router we can't reach.
	h.set(func(h *harness) { h.gateway = "10.0.0.1" })
	h.k.Reconcile(context.Background(), false)
	if h.router.maps != 2 || h.router.unmaps != 0 {
		t.Fatalf("after a gateway change: maps=%d unmaps=%d", h.router.maps, h.router.unmaps)
	}
	if st := h.k.Status(); st.PortMap.Method != "natpmp" {
		t.Fatalf("mapping not re-established: %+v", st.PortMap)
	}
}

// Proof of outside reach is proof for one public address. When the ISP
// (or a move) changes it, the old proof must stop vouching: the
// port-forwarding notice comes back if the new router won't open ports.
func TestBeaconProofExpiresWithThePublicAddress(t *testing.T) {
	h := newHarness(t)
	if wan.LANIP() == "" {
		t.Skip("no LAN address on this machine")
	}
	h.router.refuse = true
	h.set(func(h *harness) { h.hairpin = errors.New("i/o timeout") })
	h.k.Store.SetSetting(SettingTarget, "wan")
	h.k.Reconcile(context.Background(), true)
	if activeKind(t, h.k.Store, portNoticeKind) == nil {
		t.Fatal("refused mapping must raise the port notice")
	}
	seen := h.k.RecordBeacon("203.0.113.200")
	if seen.PublicIP != "203.0.113.9" || activeKind(t, h.k.Store, portNoticeKind) != nil {
		t.Fatalf("beacon must bind to the public address and clear the notice: %+v", seen)
	}
	// Moved: new public address, router still refusing.
	h.set(func(h *harness) { h.public = "198.51.100.7" })
	st := h.k.Reconcile(context.Background(), false)
	if !st.BeaconStale {
		t.Fatalf("proof from the old address must read as stale: %+v", st)
	}
	if activeKind(t, h.k.Store, portNoticeKind) == nil {
		t.Fatal("stale proof must not suppress the port notice at the new house")
	}
	// Fresh proof at the new house settles it again.
	h.k.RecordBeacon("203.0.113.201")
	if h.k.Status().BeaconStale || activeKind(t, h.k.Store, portNoticeKind) != nil {
		t.Fatal("fresh proof must clear the notice")
	}
}

// The from-inside self-test: passing proves the forward AND loopback (so
// the port notice comes down even when the router refused automatic
// mapping); failing while the outside is confirmed means the router has
// no NAT loopback, which is a notice of its own.
func TestHairpinSelfTest(t *testing.T) {
	h := newHarness(t)
	if wan.LANIP() == "" {
		t.Skip("no LAN address on this machine")
	}
	h.router.refuse = true
	h.k.Store.SetSetting(SettingTarget, "wan")
	// Router refuses to map, but an operator forwarded the port by hand
	// and the router loops back: the self-test passes.
	h.k.Reconcile(context.Background(), true)
	st := h.k.Status()
	if st.Hairpin != "ok" || activeKind(t, h.k.Store, portNoticeKind) != nil {
		t.Fatalf("a passing self-test must clear the port notice: hairpin=%s", st.Hairpin)
	}
	// New router without loopback; outside confirmed.
	h.set(func(h *harness) { h.hairpin = errors.New("connection refused"); h.gateway = "10.0.0.1" })
	h.k.RecordBeacon("203.0.113.200")
	h.k.Reconcile(context.Background(), true)
	st = h.k.Status()
	if st.Hairpin != "failed" || st.HairpinError == "" {
		t.Fatalf("hairpin status: %+v", st)
	}
	n := activeKind(t, h.k.Store, hairpinNoticeKind)
	if n == nil || !strings.Contains(n.Body, "NAT loopback") || !strings.Contains(n.Body, st.LanIP) {
		t.Fatalf("loopback notice missing or vague: %+v", n)
	}
	// The router's own admin page holds 443: the handshake reaches a
	// certificate for the router, not fog. Loopback works, so the
	// loopback notice is wrong and must come down; the answerer is named.
	h.set(func(h *harness) {
		h.hairpin = x509.HostnameError{Host: "fog.example.com", Certificate: &x509.Certificate{
			DNSNames: []string{"unifi.local", "localhost", "::1"}}}
	})
	h.k.Reconcile(context.Background(), false)
	st = h.k.Status()
	if st.Hairpin != "failed" || st.HairpinAnswerer != "unifi.local" {
		t.Fatalf("router-held port must name the answerer: %+v", st)
	}
	if activeKind(t, h.k.Store, hairpinNoticeKind) != nil {
		t.Fatal("a certificate mismatch proves loopback; the loopback notice must clear")
	}
	// It heals: loopback enabled on the router.
	h.set(func(h *harness) { h.hairpin = nil })
	h.k.Reconcile(context.Background(), false)
	st = h.k.Status()
	if activeKind(t, h.k.Store, hairpinNoticeKind) != nil || st.HairpinAnswerer != "" {
		t.Fatalf("loopback notice and answerer must clear when the self-test passes: %+v", st)
	}
	// Back to lan: the self-test no longer applies.
	h.k.Store.SetSetting(SettingTarget, "lan")
	if st := h.k.Reconcile(context.Background(), true); st.Hairpin != "" {
		t.Fatalf("hairpin must be cleared on the lan target: %+v", st)
	}
}

// A notice raised by the previous process must not outlive the problem
// just because the flag that tracked it died with that process.
func TestRestartClearsLeftoverNotices(t *testing.T) {
	h := newHarness(t)
	if wan.LANIP() == "" {
		t.Skip("no LAN address on this machine")
	}
	st := h.k.Store
	st.CreateNotice(noticeKind, "", "DNS for *.home.example.com can no longer be verified", "left over")
	st.CreateNotice(portNoticeKind, "", "Port forwarding needed for outside access", "left over")
	st.CreateNotice(hairpinNoticeKind, "", "Apps work from outside but not from home on this router", "left over")
	st.SetSetting(SettingTarget, "wan")
	// "Restart": a fresh keeper over the same store, everything working.
	fresh := h.newKeeper(st, h.k.Vault, h.k.CFBase)
	fresh.Reconcile(context.Background(), false)
	if activeKind(t, st, noticeKind) != nil {
		t.Fatal("first successful reconcile after a restart must clear the failing notice")
	}
	if activeKind(t, st, portNoticeKind) != nil {
		t.Fatal("first successful mapping after a restart must clear the port notice")
	}
	if activeKind(t, st, hairpinNoticeKind) != nil {
		t.Fatal("first passing self-test after a restart must clear the loopback notice")
	}
}

// The stored beacon is read once and then kept in step by the keeper's own
// writes, so Status (asked on every page load) stays off the database.
func TestStatusKeepsTheBeaconInStep(t *testing.T) {
	h := newHarness(t)
	st := h.k.Store
	raw, _ := json.Marshal(BeaconSeen{IP: "198.51.100.7", Public: true})
	st.SetSetting(SettingBeaconSeen, string(raw))
	if b := h.k.Status().Beacon; b == nil || b.IP != "198.51.100.7" {
		t.Fatalf("stored beacon not loaded: %+v", b)
	}
	h.k.RecordBeacon("198.51.100.8")
	if b := h.k.Status().Beacon; b == nil || b.IP != "198.51.100.8" {
		t.Fatalf("recorded beacon not reflected: %+v", b)
	}
	h.k.ClearBeacon()
	if b := h.k.Status().Beacon; b != nil {
		t.Fatalf("cleared beacon still reported: %+v", b)
	}
	// A fresh keeper finds the cleared state, not the first hit.
	if b := h.newKeeper(st, h.k.Vault, h.k.CFBase).Status().Beacon; b != nil {
		t.Fatalf("cleared beacon came back after a restart: %+v", b)
	}
}

// While the network is still coming up (no address, or no internet yet)
// the loop must retry quickly, then settle back to the regular cadence.
func TestBackoffWhileNetworkIsComingUp(t *testing.T) {
	h := newHarness(t)
	if wan.LANIP() == "" {
		t.Skip("no LAN address on this machine")
	}
	h.k.Store.SetSetting(SettingTarget, "wan")
	h.set(func(h *harness) { h.pubErr = errors.New("no route to host") })
	regular := 5 * time.Minute
	h.k.Reconcile(context.Background(), false)
	if w := h.k.nextWait(regular); w != 15*time.Second {
		t.Fatalf("first retry wait = %s, want 15s", w)
	}
	h.k.Reconcile(context.Background(), false)
	h.k.Reconcile(context.Background(), false)
	if w := h.k.nextWait(regular); w != time.Minute {
		t.Fatalf("third retry wait = %s, want 1m", w)
	}
	for i := 0; i < 10; i++ {
		h.k.Reconcile(context.Background(), false)
	}
	if w := h.k.nextWait(regular); w != 4*time.Minute {
		t.Fatalf("capped retry wait = %s, want 4m", w)
	}
	h.set(func(h *harness) { h.pubErr = nil })
	h.k.Reconcile(context.Background(), false)
	if w := h.k.nextWait(regular); w != regular {
		t.Fatalf("after success wait = %s, want %s", w, regular)
	}
}
