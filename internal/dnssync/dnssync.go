// Package dnssync keeps the public wildcard record honest. Fog's domain
// design writes *.<domain> as an unproxied A record. What it holds depends
// on where the operator wants the house reached from:
//
//   - "lan" (the default): this server's private address. Names work at
//     home, nothing is exposed, and the record goes stale the moment the
//     machine gets a new address (a new house, a new router, a DHCP
//     renumbering).
//   - "wan": the house's public address, so the same names work from a
//     phone on the road. That address is the ISP's to change, so this is
//     also a dynamic-DNS client — and since traffic then has to get through
//     the router, the keeper asks it (NAT-PMP, then UPnP) to send 443 and
//     80 here, and keeps that lease renewed.
//
// The wizard used to be the only writer, so any change silently killed
// every hostname until an operator re-ran it. This loop is the missing
// writer: it watches the address that matters and re-applies the record
// whenever the two disagree.
package dnssync

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"gitea.lan/matthew/fog/internal/backoff"
	"gitea.lan/matthew/fog/internal/cloudflare"
	"gitea.lan/matthew/fog/internal/loop"
	"gitea.lan/matthew/fog/internal/notify"
	"gitea.lan/matthew/fog/internal/proxy"
	"gitea.lan/matthew/fog/internal/randx"
	"gitea.lan/matthew/fog/internal/store"
	"gitea.lan/matthew/fog/internal/vault"
	"gitea.lan/matthew/fog/internal/wan"
)

// Settings this package owns.
const (
	// SettingTarget is "lan" (default when empty) or "wan".
	SettingTarget = "dns_target"
	// SettingPortMap is "" (ask the router automatically, the default) or
	// "off" for operators who forward ports by hand or distrust UPnP.
	SettingPortMap = "port_map"
	// SettingBeaconToken is the secret in the "confirm from outside" URL.
	SettingBeaconToken = "reach_beacon_token"
	// SettingBeaconSeen is the JSON of the last beacon hit, so a confirmed
	// setup stays confirmed across restarts.
	SettingBeaconSeen = "reach_beacon_seen"
)

type Target string

const (
	TargetLAN Target = "lan"
	TargetWAN Target = "wan"
)

// ParseTarget normalizes the stored setting; anything but "wan" is LAN.
func ParseTarget(s string) Target {
	if s == string(TargetWAN) {
		return TargetWAN
	}
	return TargetLAN
}

// Ports the wan target needs forwarded: 443 for everything, 80 only so
// http:// links redirect (certificates are DNS-01 and never need it).
var wanPorts = []int{443, 80}

// PortMapper is what the keeper asks to open ports; wan.Mapper in
// production, a fake in tests.
type PortMapper interface {
	Map(ctx context.Context, lanIP string, ports []int, lease time.Duration) (*wan.Mapping, error)
	Unmap(ctx context.Context, method string, ports []int) error
}

// PortMapStatus is the router's side of the wan target.
type PortMapStatus struct {
	Enabled bool `json:"enabled"`
	// Method/Gateway/ExternalIP/Opened/Failed/Permanent mirror the last
	// successful mapping; Error is the last failure (cleared on success).
	Method     string         `json:"method,omitempty"`
	Gateway    string         `json:"gateway,omitempty"`
	ExternalIP string         `json:"externalIp,omitempty"`
	Opened     []int          `json:"opened,omitempty"`
	Failed     map[int]string `json:"failed,omitempty"`
	Permanent  bool           `json:"permanent,omitempty"`
	ExpiresAt  time.Time      `json:"expiresAt,omitzero"`
	CheckedAt  time.Time      `json:"checkedAt,omitzero"`
	Error      string         `json:"error,omitempty"`
}

// BeaconSeen is one hit on the confirm-from-outside URL.
type BeaconSeen struct {
	At     time.Time `json:"at"`
	IP     string    `json:"ip"`
	Public bool      `json:"public"` // the visitor's address is not a LAN one
	// PublicIP is the house's address the visit was proof for. Proof
	// obtained at the old house says nothing about the new one.
	PublicIP string `json:"publicIp,omitempty"`
}

// Status is a snapshot of what the keeper knows. Nothing here reaches
// for the network — it is what the last reconcile established.
type Status struct {
	Target Target `json:"target"`
	Domain string `json:"domain,omitempty"`
	LanIP  string `json:"lanIp,omitempty"`
	// PublicIP is the last address the internet reported for this house
	// (wan target only); PublicIPError says why it is unknown.
	PublicIP      string `json:"publicIp,omitempty"`
	PublicIPError string `json:"publicIpError,omitempty"`
	// DesiredIP is what *.domain should hold right now; RecordIP is what
	// it was last confirmed to hold. They differ only while something is
	// broken.
	DesiredIP   string    `json:"desiredIp,omitempty"`
	RecordIP    string    `json:"recordIp,omitempty"`
	CheckedAt   time.Time `json:"checkedAt,omitzero"`
	ConfirmedAt time.Time `json:"confirmedAt,omitzero"`
	LastAction  string    `json:"lastAction,omitempty"` // created / updated / ok
	LastError   string    `json:"lastError,omitempty"`
	Failing     bool      `json:"failing"` // the streak has crossed the notice threshold
	// NAT is "none" (this box holds the public address), "nat" (one
	// router between us and the internet), "cgnat" (the ISP NATs too —
	// forwarding cannot work), or "unknown".
	NAT     string        `json:"nat,omitempty"`
	Gateway string        `json:"gateway,omitempty"`
	PortMap PortMapStatus `json:"portMap"`
	Beacon  *BeaconSeen   `json:"beacon,omitempty"`
	// BeaconStale is set when the proof on record was obtained under a
	// different public address than the current one.
	BeaconStale bool `json:"beaconStale,omitempty"`
	// Hairpin is the self-test from inside: a TLS handshake with
	// fog.<domain> at the public address, the way every device at home
	// now reaches it. "ok" proves both the forward and the router's NAT
	// loopback; "failed" with the beacon confirmed means loopback is
	// missing; "" means not tested yet.
	Hairpin          string    `json:"hairpin,omitempty"`
	HairpinError     string    `json:"hairpinError,omitempty"`
	HairpinCheckedAt time.Time `json:"hairpinCheckedAt,omitzero"`
	// HairpinAnswerer is set when the self-test's handshake reached
	// something on 443 whose certificate is not for fog's name: the
	// names the certificate carries. Loopback works then; the port is
	// simply held by another device — nearly always the router's own
	// admin page, which is also why UPnP/NAT-PMP refused to map it.
	HairpinAnswerer string `json:"hairpinAnswerer,omitempty"`
}

type Keeper struct {
	Store  *store.Store
	Vault  *vault.Vault
	Log    *slog.Logger
	Notify func(title, msg string, ok bool) // nil-checked

	// PublicIP and Mapper are the outside-world hooks; nil gets the real
	// ones from package wan.
	PublicIP func(ctx context.Context) (string, error)
	Mapper   PortMapper
	// Gateway names the router (for the guidance when mapping fails, and
	// to tell one router from the next); nil gets wan.Gateway.
	Gateway func() string
	// HairpinDial is the from-inside self-test; nil gets a real TLS
	// handshake with publicIP:443 under the given server name.
	HairpinDial func(ctx context.Context, publicIP, host string) error
	// CFBase overrides the Cloudflare API base (tests).
	CFBase string
	// Now is time.Now unless a test says otherwise.
	Now func() time.Time

	// runMu serializes reconciles (the loop and "refresh now" must not
	// interleave two EnsureWildcard calls); mu guards the snapshot.
	runMu sync.Mutex
	mu    sync.Mutex
	st    Status
	// beaconOnce loads the stored beacon into st the first time anyone
	// asks; RecordBeacon and ClearBeacon keep st in step after that.
	beaconOnce sync.Once

	// fails counts consecutive reconcile attempts that reached for the API
	// and failed; portNotice and hairpinNotice are the router notices. All
	// three are built on first use (noticesOnce), so a Keeper stays a plain
	// struct literal.
	noticesOnce   sync.Once
	fails         *notify.FailStreak
	portNotice    *notify.Latch
	hairpinNotice *notify.Latch
	// confirmed is the address the record was last known to hold, and for
	// which target — a target switch invalidates it so the next tick
	// rewrites rather than trusting a stale match.
	confirmed       string
	confirmedTarget Target
	lastPatrol      time.Time
	// mapped remembers a live mapping so a switch back to lan releases
	// it, together with the network it was made on: a mapping is only as
	// good as the router that holds it and the address it points at, so a
	// new gateway or a new LAN address makes it void.
	mapped   *wan.Mapping
	mappedAt time.Time
	mappedOn netKey
	// wanMapper is the real mapper when Mapper is nil: one for the
	// Keeper's life, so renewals reuse the router it already found.
	wanMapper wan.Mapper
	// retries counts consecutive passes that could not finish because the
	// network wasn't ready (no address, public address unknown); the loop
	// shortens its wait while that lasts.
	retries int

	kick chan struct{}
}

// failNoticeThreshold is how many consecutive failed reconciles it takes to
// raise a notice: one blip on a 5-minute tick is noise, three in a row is a
// revoked token or a moved zone.
const failNoticeThreshold = 3

// Notice kinds; one active of each at a time.
const (
	noticeKind        = "dns-sync-failing"
	portNoticeKind    = "reach-port-forward"
	hairpinNoticeKind = "reach-no-loopback"
)

// netKey identifies the network a mapping was made on.
type netKey struct{ lanIP, gateway string }

func (k *Keeper) now() time.Time {
	if k.Now != nil {
		return k.Now()
	}
	return time.Now()
}

func (k *Keeper) publicIP(ctx context.Context) (string, error) {
	if k.PublicIP != nil {
		return k.PublicIP(ctx)
	}
	return wan.PublicIP(ctx)
}

func (k *Keeper) gateway() string {
	if k.Gateway != nil {
		return k.Gateway()
	}
	if g, err := wan.Gateway(); err == nil {
		return g.String()
	}
	return ""
}

// notices builds the failure streak and the two router notices. The
// streak raises once per run, and the first success after a restart takes
// down whatever notice the previous process left behind; the latches do
// the same for theirs on their first Clear.
func (k *Keeper) notices() {
	k.noticesOnce.Do(func() {
		k.fails = &notify.FailStreak{Store: k.Store, Kind: noticeKind, Threshold: failNoticeThreshold, Once: true, ClearLeftover: true}
		k.portNotice = &notify.Latch{Store: k.Store, Kind: portNoticeKind}
		k.hairpinNotice = &notify.Latch{Store: k.Store, Kind: hairpinNoticeKind}
	})
}

func (k *Keeper) secrets() vault.Secrets { return vault.Secrets{Store: k.Store, Vault: k.Vault} }

var errNoToken = errors.New("no Cloudflare token saved yet")

// CloudflareClient is the API client over the saved DNS token.
func (k *Keeper) CloudflareClient() (*cloudflare.Client, error) {
	tok, err := k.secrets().Get(0, proxy.SettingDNSToken)
	if errors.Is(err, store.ErrNotFound) {
		return nil, errNoToken
	}
	if err != nil {
		return nil, err
	}
	cf := cloudflare.New(tok)
	cf.Base = k.CFBase
	return cf, nil
}

// Target is the address family the operator chose for the record.
func (k *Keeper) Target() Target {
	v, _ := k.Store.GetSetting(SettingTarget)
	return ParseTarget(v)
}

// DesiredIP is the address *.<domain> should hold right now: the LAN
// address, or — with the wan target — the public one the last reconcile
// learned ("" while that is still unknown). Unlike Status().DesiredIP it
// answers before the first reconcile, from the live LAN address, so the
// domain wizard never compares against nothing.
func (k *Keeper) DesiredIP() (string, Target) {
	target := k.Target()
	if target == TargetWAN {
		k.mu.Lock()
		defer k.mu.Unlock()
		return k.st.PublicIP, target
	}
	return wan.LANIP(), target
}

func (k *Keeper) hairpinDial(ctx context.Context, publicIP, host string) error {
	if k.HairpinDial != nil {
		return k.HairpinDial(ctx, publicIP, host)
	}
	d := &tls.Dialer{NetDialer: &net.Dialer{Timeout: 5 * time.Second}, Config: &tls.Config{ServerName: host}}
	conn, err := d.DialContext(ctx, "tcp4", net.JoinHostPort(publicIP, "443"))
	if err != nil {
		return err
	}
	return conn.Close()
}

func (k *Keeper) mapper() PortMapper {
	if k.Mapper != nil {
		return k.Mapper
	}
	return &k.wanMapper
}

// Status returns the current snapshot.
func (k *Keeper) Status() Status {
	k.beaconOnce.Do(func() {
		seen := k.loadBeacon()
		k.update(func(st *Status) {
			if st.Beacon == nil {
				st.Beacon = seen
			}
		})
	})
	k.mu.Lock()
	st := k.st
	k.mu.Unlock()
	if st.Target == "" {
		// Before the first reconcile the snapshot is empty; the target is
		// still a fact worth answering from the setting.
		st.Target = k.Target()
	}
	st.BeaconStale = st.Beacon != nil && st.Beacon.PublicIP != "" && st.PublicIP != "" && st.Beacon.PublicIP != st.PublicIP
	return st
}

func (k *Keeper) update(f func(st *Status)) {
	k.mu.Lock()
	defer k.mu.Unlock()
	f(&k.st)
}

// Kick asks the loop to reconcile soon (after a settings change).
func (k *Keeper) Kick() {
	k.mu.Lock()
	kick := k.kick
	k.mu.Unlock()
	if kick == nil {
		return
	}
	select {
	case kick <- struct{}{}:
	default:
	}
}

// Loop reconciles forever. The local address check is free and runs every
// few minutes; Cloudflare is only consulted when the address differs from
// the last one confirmed, at boot, and on a daily patrol — so a quiet
// server costs one API round-trip a day, while a moved server heals within
// minutes of getting its new lease. With the wan target the address check
// is one small HTTPS request to learn the public address, so a changed ISP
// lease is caught on the same cadence.
func (k *Keeper) Loop(ctx context.Context) {
	const localEvery = 5 * time.Minute
	k.mu.Lock()
	if k.kick == nil {
		k.kick = make(chan struct{}, 1)
	}
	kick := k.kick
	k.mu.Unlock()
	// Boot pass after a minute: a reboot after a move is when the record is
	// most likely wrong. A kick (netwatch saw the network change, or a
	// setting did) doesn't wait out the tick.
	loop.Run(ctx, time.Minute, kick, func(ctx context.Context) time.Duration {
		k.Reconcile(ctx, false)
		return k.nextWait(localEvery)
	})
}

// nextWait is the tick length: the regular cadence when the last pass
// finished, a short backoff while the network is still coming up at a
// new house (DHCP done, WAN not yet) so the record heals within a minute
// of the internet returning rather than up to five after.
func (k *Keeper) nextWait(regular time.Duration) time.Duration {
	k.mu.Lock()
	n := k.retries
	k.mu.Unlock()
	if n == 0 {
		return regular
	}
	// 15s, 30s, 1m, 2m, 4m, never past the regular cadence.
	return min(backoff.Exp(15*time.Second, 4*time.Minute, n-1), regular)
}

func (k *Keeper) setRetrying(on bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if on {
		k.retries++
	} else {
		k.retries = 0
	}
}

// Reconcile checks and, if needed, rewrites the record and renews the
// router mapping. force skips the "nothing changed locally" shortcut —
// the operator's Refresh button and every settings change use it.
// Returns the resulting snapshot.
func (k *Keeper) Reconcile(ctx context.Context, force bool) Status {
	k.runMu.Lock()
	defer k.runMu.Unlock()
	const patrolEvery = 24 * time.Hour
	patrolDue := k.now().Sub(k.lastPatrol) >= patrolEvery
	if ip := k.reconcile(ctx, force, patrolDue); ip != "" {
		k.confirmed = ip
		k.lastPatrol = k.now()
	}
	return k.Status()
}

// reconcile does one pass. force is the operator asking (no shortcut, no
// "changed on its own" notice); patrol is the daily re-check (no shortcut
// either, but an unexpected difference is still news). Returns the IP the
// record is now confirmed to hold, or "" when nothing could be confirmed
// (not configured, offline, API error) — the caller retries next tick.
func (k *Keeper) reconcile(ctx context.Context, force, patrol bool) string {
	thorough := force || patrol
	domain, _ := k.Store.GetSetting(store.SettingBaseDomain)
	target := k.Target()
	portMapSetting, _ := k.Store.GetSetting(SettingPortMap)
	portMap := target == TargetWAN && portMapSetting != "off"
	lanIP := wan.LANIP()
	now := k.now()

	k.update(func(st *Status) {
		st.Target = target
		st.Domain = domain
		st.LanIP = lanIP
		st.CheckedAt = now
		st.PortMap.Enabled = portMap
		if target == TargetLAN {
			st.PublicIP, st.PublicIPError, st.NAT = "", "", ""
		}
	})
	if target != k.confirmedTarget {
		// A switch: the record's last known content is for the other
		// address family, so it can't be trusted to match.
		k.confirmed = ""
		k.confirmedTarget = target
	}
	if domain == "" {
		return ""
	}
	cf, err := k.CloudflareClient()
	if err != nil {
		return ""
	}
	if lanIP == "" {
		k.setRetrying(true)
		return "" // offline; nothing to assert yet
	}

	// Which address should the record hold?
	desired := lanIP
	ttl := cloudflare.TTLLAN
	pubChanged := false
	if target == TargetWAN {
		ttl = cloudflare.TTLWAN
		pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		pub, perr := k.publicIP(pctx)
		cancel()
		if perr != nil {
			k.Log.Warn("dns reconcile: public address unknown", "err", perr)
			k.update(func(st *Status) { st.PublicIPError = perr.Error() })
			// Don't touch the record on a guess. The mapping can still be
			// renewed — the router is reachable even when the internet isn't.
			if portMap {
				k.ensureMapping(ctx, lanIP, thorough)
			}
			k.setRetrying(true)
			return ""
		}
		desired = pub
		k.update(func(st *Status) {
			pubChanged = st.PublicIP != pub
			st.PublicIP, st.PublicIPError = pub, ""
		})
	}
	k.setRetrying(false)
	k.update(func(st *Status) { st.DesiredIP = desired })

	if desired == k.confirmed && !thorough {
		if target == TargetWAN {
			if portMap {
				k.ensureMapping(ctx, lanIP, false)
			}
			k.checkHairpin(ctx, domain, desired, false)
		}
		return desired // nothing changed locally; skip the API round-trip
	}

	cctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	zone, err := cf.FindZone(cctx, domain)
	if err != nil {
		k.Log.Warn("dns reconcile: finding zone", "domain", domain, "err", err)
		k.noteFailure(domain, err)
		return ""
	}
	action, err := cf.EnsureWildcard(cctx, zone.ID, domain, desired, ttl)
	if err != nil {
		k.Log.Warn("dns reconcile: ensuring wildcard", "domain", domain, "err", err)
		k.noteFailure(domain, err)
		return ""
	}
	k.noteSuccess(domain)
	k.update(func(st *Status) {
		st.RecordIP = desired
		st.ConfirmedAt = k.now()
		st.LastAction = action
	})
	if action != "ok" {
		k.Log.Info("dns reconcile: wildcard re-pointed", "record", "*."+domain, "ip", desired, "target", target, "action", action)
		k.Store.Audit("system", "domain.dns.reconcile", "*."+domain+" → "+desired, string(target))
		// The operator's own click is not news; an unattended change is.
		if !force {
			k.announceChange(domain, target, desired)
		}
	}
	if target == TargetWAN {
		if portMap {
			k.ensureMapping(ctx, lanIP, thorough)
		} else {
			k.releaseMapping(ctx)
			k.update(func(st *Status) { st.NAT = natKind(lanIP, desired, "") })
		}
		k.checkHairpin(ctx, domain, desired, thorough || pubChanged)
	} else {
		k.releaseMapping(ctx)
		k.notices()
		k.hairpinNotice.Clear()
		k.update(func(st *Status) {
			st.Hairpin, st.HairpinError, st.HairpinCheckedAt, st.HairpinAnswerer = "", "", time.Time{}, ""
		})
	}
	return desired
}

// checkHairpin runs the from-inside self-test. A failed test is retried
// every tick (cheap, and the proxy may just be starting); a passing one
// only on a thorough pass or when the public address changed. With the
// outside confirmed and the inside failing, the router lacks NAT loopback
// and every device at home has just lost the names — that is a notice.
func (k *Keeper) checkHairpin(ctx context.Context, domain, publicIP string, again bool) {
	st := k.Status()
	if st.Hairpin == "ok" && !again {
		return
	}
	hctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	err := k.hairpinDial(hctx, publicIP, "fog."+domain)
	cancel()
	now := k.now()
	k.notices()
	if err == nil {
		k.update(func(st *Status) {
			st.Hairpin, st.HairpinError, st.HairpinCheckedAt, st.HairpinAnswerer = "ok", "", now, ""
		})
		k.hairpinNotice.Clear()
		// Loopback applies the forward rules, so a passing test is also
		// the forward working — take the port notice down with it.
		k.portNotice.Clear()
		return
	}
	answerer := certAnswerer(err)
	k.update(func(st *Status) {
		st.Hairpin, st.HairpinError, st.HairpinCheckedAt, st.HairpinAnswerer = "failed", err.Error(), now, answerer
	})
	if answerer != "" {
		// The handshake got through and something else presented its
		// certificate: loopback works, the port is just taken. That is
		// the forward's problem (the port notice covers it), not loopback's.
		k.hairpinNotice.Clear()
		return
	}
	if !k.beaconConfirmed() {
		return // can't tell loopback from a missing forward yet
	}
	if !k.hairpinNotice.Raise(
		"Apps work from outside but not from home on this router",
		"*."+domain+" now points at your public address, and this router doesn't loop traffic to that address back inside (\"NAT loopback\" / \"hairpin NAT\"), so devices at home can't reach the names. "+
			"Either enable NAT loopback in the router, or give "+domain+" a local DNS override to "+st.LanIP+" in the router or Pi-hole. "+
			"Switching DNS back to the LAN address in Settings → Network → Domain & HTTPS also fixes home, at the cost of outside access.") {
		return
	}
	k.Log.Warn("router has no NAT loopback: names fail from inside", "domain", domain, "err", err)
	if k.Notify != nil {
		k.Notify("Names fail at home", "router has no NAT loopback for "+domain, false)
	}
}

// certAnswerer reads a hostname-mismatch failure from the self-test and
// returns the names on the certificate that answered instead, comma
// separated; "" for any other failure. Reaching a certificate at all
// means the router looped the connection back and something on 443
// took it — a router's own admin page presents e.g. "unifi.local".
func certAnswerer(err error) string {
	var he x509.HostnameError
	if !errors.As(err, &he) || he.Certificate == nil {
		return ""
	}
	names := he.Certificate.DNSNames
	if len(names) == 0 && he.Certificate.Subject.CommonName != "" {
		names = []string{he.Certificate.Subject.CommonName}
	}
	// Drop loopback filler so the name that identifies the device leads.
	var out []string
	for _, n := range names {
		if n == "localhost" || n == "::1" || n == "127.0.0.1" {
			continue
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		out = names
	}
	if len(out) == 0 {
		return "an unnamed certificate"
	}
	return strings.Join(out, ", ")
}

func (k *Keeper) announceChange(domain string, target Target, ip string) {
	switch target {
	case TargetWAN:
		k.Store.CreateNotice("dns-reconciled", "",
			"Your home's public address changed — DNS was updated automatically",
			"*."+domain+" now points at "+ip+". Nothing to do: apps stay reachable from outside as long as the router keeps forwarding port 443 here.")
	default:
		k.Store.CreateNotice("dns-reconciled", "",
			"This server's address changed — DNS was updated automatically",
			"*."+domain+" now points at "+ip+". If names still don't resolve on this network after a few minutes, "+
				"the router's DNS-rebind protection may be dropping answers with private addresses — allowlist "+domain+" there.")
	}
	if k.Notify != nil {
		k.Notify("DNS updated", "*."+domain+" → "+ip, true)
	}
}

// natKind classifies the path to the internet from what is known: the
// box's LAN address, the address the internet sees, and (when a router
// answered) the router's own WAN address.
func natKind(lanIP, publicIP, routerWAN string) string {
	if publicIP == "" {
		return "unknown"
	}
	if lanIP == publicIP {
		return "none"
	}
	if routerWAN != "" && routerWAN != publicIP {
		// The router's WAN side is not the address the world sees: there
		// is another NAT upstream — the ISP's, or a second router.
		return "cgnat"
	}
	if routerWAN != "" && wan.Classify(routerWAN) != wan.KindPublic {
		return "cgnat"
	}
	return "nat"
}

// mapLease is what we ask for; renewal happens well before it runs out.
const (
	mapLease      = time.Hour
	mapRenewAhead = 15 * time.Minute
)

// ensureMapping keeps 443/80 forwarded here. A live lease is left alone
// until it nears expiry; a permanent one is re-asserted on the daily
// patrol (force) so a rebooted router that forgot it learns again.
func (k *Keeper) ensureMapping(ctx context.Context, lanIP string, force bool) {
	now := k.now()
	here := netKey{lanIP: lanIP, gateway: k.gateway()}
	if k.mapped != nil && k.mappedOn != here {
		// A different router, or a different address on the same one: the
		// old mapping either can't be reached or points at the wrong host.
		// Nothing to release — drop it and ask afresh.
		k.Log.Info("network changed; port mapping is void", "was", k.mappedOn, "now", here)
		k.mapped = nil
	}
	if k.mapped != nil && !force {
		if k.mapped.Permanent {
			return
		}
		if now.Before(k.mappedAt.Add(k.mapped.Lease - mapRenewAhead)) {
			return
		}
	}
	mctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	mp, err := k.mapper().Map(mctx, lanIP, wanPorts, mapLease)
	if err != nil {
		gw := k.gateway()
		k.Log.Warn("port mapping failed", "err", err)
		k.mapped = nil
		k.update(func(st *Status) {
			st.Gateway = gw
			st.PortMap = PortMapStatus{Enabled: true, Gateway: gw, CheckedAt: now, Error: err.Error()}
			st.NAT = natKind(lanIP, st.PublicIP, "")
		})
		k.notePortMapping(false)
		return
	}
	k.mapped = mp
	k.mappedAt = now
	k.mappedOn = here
	k.Log.Info("port mapping ok", "method", mp.Method, "opened", mp.Opened, "external", mp.ExternalIP, "permanent", mp.Permanent)
	k.update(func(st *Status) {
		st.Gateway = mp.Gateway
		st.PortMap = PortMapStatus{
			Enabled: true, Method: mp.Method, Gateway: mp.Gateway, ExternalIP: mp.ExternalIP,
			Opened: mp.Opened, Failed: mp.Failed, Permanent: mp.Permanent, CheckedAt: now,
		}
		if !mp.Permanent {
			st.PortMap.ExpiresAt = now.Add(mp.Lease)
		}
		st.NAT = natKind(lanIP, st.PublicIP, mp.ExternalIP)
	})
	k.notePortMapping(true)
}

func (k *Keeper) releaseMapping(ctx context.Context) {
	if k.mapped == nil {
		return
	}
	if k.mappedOn.gateway == k.gateway() {
		mctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		if err := k.mapper().Unmap(mctx, k.mapped.Method, k.mapped.Opened); err != nil {
			k.Log.Warn("releasing port mapping", "err", err)
		}
	}
	k.mapped = nil
	k.update(func(st *Status) { st.PortMap = PortMapStatus{Enabled: st.PortMap.Enabled} })
	k.notices()
	k.portNotice.Clear()
}

// notePortMapping raises one notice when the router won't open the port
// on its own and nothing has confirmed the house reachable, and takes it
// down as soon as either happens. Raised on the crossing only, so the
// 5-minute tick cannot stack duplicates.
func (k *Keeper) notePortMapping(ok bool) {
	k.notices()
	if ok || k.beaconConfirmed() || k.Status().Hairpin == "ok" {
		k.portNotice.Clear()
		return
	}
	st := k.Status()
	body := "DNS points at your public address, but the router didn't accept an automatic request (UPnP / NAT-PMP) to forward port 443 to this server. " +
		"Until it does, the names won't work from outside. Forward TCP 443 (and 80) to " + st.LanIP + " in the router's settings"
	if st.Gateway != "" {
		body += " — usually at http://" + st.Gateway
	}
	body += ". Settings → Network → Domain & HTTPS has the details and a link to confirm from outside."
	if st.NAT == "cgnat" {
		body = "DNS points at your public address, but your ISP puts this house behind its own NAT (carrier-grade NAT), so no port forward can reach it. " +
			"Use Settings → Network → Cloudflare Tunnel instead — it needs no open ports."
	}
	if !k.portNotice.Raise("Port forwarding needed for outside access", body) {
		return
	}
	if k.Notify != nil {
		k.Notify("Port forwarding needed", "the router didn't open port 443 for "+st.Domain, false)
	}
}

// noteFailure escalates a failing streak. Success is loudly noticed above;
// failure must be too — a revoked token or moved zone otherwise leaves the
// wildcard silently stale, the exact outcome this package exists to
// prevent, with only a Warn line every five minutes as witness. Crossing
// the threshold raises ONE persistent notice (raising exactly on the
// crossing, like fed's noteDialResult, so a notice dismissed mid-outage
// stays dismissed); the next success takes it back down.
func (k *Keeper) noteFailure(domain string, err error) {
	k.notices()
	streak, raise := k.fails.Fail("")
	k.update(func(st *Status) {
		st.LastError = err.Error()
		st.Failing = streak >= failNoticeThreshold
	})
	if !raise {
		return
	}
	k.Log.Warn("dns reconcile failing repeatedly", "domain", domain, "streak", streak, "err", err)
	k.Store.CreateNotice(noticeKind, "",
		"DNS for *."+domain+" can no longer be verified",
		"Checking the wildcard record against Cloudflare has failed "+strconv.Itoa(streak)+" times in a row: "+err.Error()+"\n\n"+
			"Until this works again, a change of this server's address will NOT be fixed in DNS automatically. "+
			"If the API token was revoked or the zone moved accounts, save a fresh token in the domain settings.")
	if k.Notify != nil {
		k.Notify("DNS sync failing", "can't verify *."+domain+" against Cloudflare: "+err.Error(), false)
	}
}

// noteSuccess resets the streak and retracts the notice if one was raised
// (or left behind by the previous process).
func (k *Keeper) noteSuccess(domain string) {
	k.notices()
	if k.fails.Success("") {
		k.Log.Info("dns reconcile healthy again", "domain", domain)
	}
	k.update(func(st *Status) { st.LastError, st.Failing = "", false })
}

// ---- the beacon: proof from outside ----

// BeaconToken returns the secret for the confirm-from-outside URL,
// minting one on first use.
func (k *Keeper) BeaconToken() (string, error) {
	if tok, _ := k.Store.GetSetting(SettingBeaconToken); tok != "" {
		return tok, nil
	}
	tok := randx.Hex(16)
	if err := k.Store.SetSetting(SettingBeaconToken, tok); err != nil {
		return "", err
	}
	return tok, nil
}

// CheckBeacon says whether a presented token is the real one.
func (k *Keeper) CheckBeacon(presented string) bool {
	tok, _ := k.Store.GetSetting(SettingBeaconToken)
	return tok != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(presented)) == 1
}

// RecordBeacon notes a hit on the beacon URL from ip. A hit from a
// non-LAN address is the proof the wan target was waiting for: it takes
// the port-forwarding notice down.
func (k *Keeper) RecordBeacon(ip string) BeaconSeen {
	seen := BeaconSeen{At: k.now(), IP: ip, Public: wan.Classify(ip) == wan.KindPublic, PublicIP: k.Status().PublicIP}
	if raw, err := json.Marshal(seen); err == nil {
		k.Store.SetSetting(SettingBeaconSeen, string(raw))
	}
	k.update(func(st *Status) { st.Beacon = &seen })
	k.Store.Audit("system", "domain.reach.beacon", ip, "")
	if seen.Public {
		k.notices()
		k.portNotice.Clear()
	}
	return seen
}

func (k *Keeper) loadBeacon() *BeaconSeen {
	raw, _ := k.Store.GetSetting(SettingBeaconSeen)
	if raw == "" {
		return nil
	}
	var seen BeaconSeen
	if err := json.Unmarshal([]byte(raw), &seen); err != nil {
		return nil
	}
	return &seen
}

// beaconConfirmed is whether an outside visitor has reached the beacon
// while the current public address was in force. Proof from the old
// house does not carry to the new one.
func (k *Keeper) beaconConfirmed() bool {
	st := k.Status()
	return st.Beacon != nil && st.Beacon.Public && !st.BeaconStale
}

// ClearBeacon forgets past proof — used when the target is switched, so
// a confirmation from the last configuration cannot vouch for this one.
func (k *Keeper) ClearBeacon() {
	k.Store.DeleteSetting(SettingBeaconSeen)
	k.update(func(st *Status) { st.Beacon = nil })
}

var errNoDomain = errors.New("no domain configured")

// BeaconURL is the link to open from outside the house.
func (k *Keeper) BeaconURL() (string, error) {
	domain, _ := k.Store.GetSetting(store.SettingBaseDomain)
	if domain == "" {
		return "", errNoDomain
	}
	tok, err := k.BeaconToken()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("https://fog.%s/api/reach/beacon/%s", domain, tok), nil
}
