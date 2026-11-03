// Limits on sign-in's credential checks (docs/signin-contract.md). Three
// rules, so an outsider spends their own budget and never the household's:
//
//   - A guess at an account is charged to that account (10 wrong in 15
//     minutes), wherever it comes from.
//   - A guess is first charged to the address it came from, with a far
//     larger budget (100 in 15 minutes). An IPv6 address counts as its /64,
//     which one household or one attacker holds whole. A visitor through
//     fog's Cloudflare Tunnel is the address Cloudflare names: Caddy's tunnel
//     listener, which only the host can reach, says so (internal/proxy,
//     TunnelPort). Anything else reaching Caddy from one of this host's own
//     addresses shares one stranger bucket, with ten times the budget: never
//     unlimited, and never the bucket anyone with an address of their own
//     uses.
//   - Then to the account it aimed at (10 in 15 minutes), or — for a handle
//     no one active holds — to one bucket all of those share, so made-up
//     handles never make keys of their own. An attempt refused at its
//     address never reaches the account limiter at all.
//   - Only failures are charged, and a bucket is checked before the next
//     check is made. Passkeys have a bucket of their own. A correct password
//     or a passkey never spends anything, so failures can't use up what
//     signed-in or passkey traffic needs.
package server

import (
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"gitea.lan/matthew/fog/internal/auth"
)

const (
	accountSuccesses = 20 // right passwords for one account, per window
	addressSuccesses = 60 // right passwords from one real address, per window
	sessionMints     = 30 // sessions minted for one person, per window
	// maxSessionsPerPerson is how many sessions one person holds; past it
	// the oldest end.
	maxSessionsPerPerson = 50
	// sessionInUse is how recently a session was seen to count as in use:
	// one is never ended to make room (last_seen_at moves every 5 minutes
	// of use). Past the cap, sessions mint at sessionMints a window.
	sessionInUse    = 15 * time.Minute
	accountFailures = 10  // wrong credentials for one account, per window
	addressFailures = 100 // wrong credentials from one real address, per window
	passkeyAttempts = 60  // passkey ceremonies begun from one real address
	// sharedScale is how much more the one stranger bucket — the host's own
	// address, no tunnel to say who is behind it — holds than an address's.
	sharedScale   = 10
	failureWindow = 15 * time.Minute
)

// sharedAddr is the stranger bucket's key.
const sharedAddr = auth.HostShared

// unknownAccount is the one account bucket every guess at a handle no one
// active holds is charged to. A made-up handle never gets a bucket of its
// own: a flood of them would otherwise be a flood of keys.
const unknownAccount = "login-unknown"

// signinLimiters are sign-in's buckets, built on first use.
type signinLimiters struct {
	once          sync.Once
	account       *auth.RateLimiter
	address       *auth.StrangerLimiter
	passkey       *auth.StrangerLimiter
	sharedAddress *auth.RateLimiter
	sharedPasskey *auth.RateLimiter
	// unknown holds unknownAccount, with sharedScale times an account's
	// budget: it is everyone's typo as well as the guesser's.
	unknown *auth.RateLimiter
	// A right password is charged too, to its account and its address:
	// a valid password looped is never free.
	accountSuccess *auth.RateLimiter
	addressSuccess *auth.StrangerLimiter
	sharedSuccess  *auth.RateLimiter
	// sessionMint bounds how fast interactive sign-ins mint sessions for
	// one person; deviceMint, how fast one browser session mints native
	// device tokens (signin_handlers_device.go). Neither spends the other.
	sessionMint *auth.RateLimiter
	deviceMint  *auth.RateLimiter
	// Share-link passwords (signin_handlers_share.go): wrong ones per link
	// and address, per address a day, per link a day; the operator's
	// once-a-day alarm; right ones' grants.
	shareFailures, shareStrikes *auth.StrangerLimiter
	shareAlarms, shareAlarmed   *auth.RateLimiter
	shareGrants                 shareGrants

	// hostAddrs caches this host's own addresses.
	addrMu  sync.Mutex
	addrs   map[string]bool
	addrsAt time.Time

	// bridges caches the Docker bridges' subnets (containerPrefixes);
	// containerNets replaces them in tests.
	bridges       []netip.Prefix
	bridgesAt     time.Time
	containerNets func() []netip.Prefix

	// tunnelSeen throttles noteTunnelPath (signin_handlers_tunnel.go).
	tunnelSeen tunnelSightings

	// strangers are the limiters for callers known only by address, one
	// per purpose (strangerBudgets), built on first use; homeAddrs caches
	// the household's addresses for them.
	strangerMu sync.Mutex
	strangers  map[string]*auth.StrangerLimiter
	homeAddrs  map[string]bool
	homeAt     time.Time
}

// strangerBudgets are the limits on what a caller known only by its address
// may do, per purpose. Every such limit is a StrangerLimiter, apart from the
// limiters keyed by something fog checked — an app, a person, a registered
// client — so strangers filling theirs never refuse those.
var strangerBudgets = map[string]struct {
	max    int
	window time.Duration
}{
	"setup-restore": {10, 15 * time.Minute},
	"setup-claim":   {10, 15 * time.Minute},
	"compute-join":  {10, 15 * time.Minute},
	"fed-pair":      {10, 15 * time.Minute},
	"oidc-client":   {120, time.Minute},
	"userinfo":      {120, time.Minute},
	"authorize":     {120, time.Minute},
	"visit":         {120, time.Minute},
	"chat-hook":     {120, time.Minute},
	"mcp":           {120, time.Minute},
	"share":         {120, time.Minute},
	"dav":           {600, time.Minute},
}

// stranger is the limiter for purpose.
func (s *Server) stranger(purpose string) *auth.StrangerLimiter {
	l := s.signinLimits()
	l.strangerMu.Lock()
	defer l.strangerMu.Unlock()
	if l.strangers == nil {
		l.strangers = map[string]*auth.StrangerLimiter{}
	}
	sl := l.strangers[purpose]
	if sl == nil {
		b, ok := strangerBudgets[purpose]
		if !ok {
			panic("no stranger budget for " + purpose)
		}
		sl = auth.NewStrangerLimiter(b.max, b.window)
		sl.Known = s.householdAddr
		l.strangers[purpose] = sl
	}
	return sl
}

// householdAddr says whether an address key is one a live session was
// signed in from, read at most once a minute.
func (s *Server) householdAddr(key string) bool {
	l := s.signinLimits()
	l.strangerMu.Lock()
	defer l.strangerMu.Unlock()
	if l.homeAddrs == nil || time.Since(l.homeAt) > time.Minute {
		l.homeAddrs = map[string]bool{}
		if list, err := s.store.LiveSessionAddrs(); err == nil {
			for _, a := range list {
				if k := auth.AddrKey(a); k != "" {
					l.homeAddrs[k] = true
				}
			}
		}
		l.homeAt = time.Now()
	}
	return l.homeAddrs[key]
}

// reserveStranger takes an attempt for purpose from the request's address
// (limitAddr: its own, its /64, a tunnel visitor's, or the host's shared
// one), answering 429 with msg and reporting false when there is none;
// refund gives it back for a request that proved to be someone fog knows.
func (s *Server) reserveStranger(w http.ResponseWriter, r *http.Request, purpose, msg string) (refund func(), ok bool) {
	sl := s.stranger(purpose)
	addr, _ := s.limitAddr(r)
	refund, ok = sl.Take(addr)
	if !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(sl.Window().Seconds())))
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": msg, "code": "rate_limited"})
	}
	return refund, ok
}

// limitStranger is reserveStranger for an attempt that is never given back.
func (s *Server) limitStranger(w http.ResponseWriter, r *http.Request, purpose, msg string) bool {
	_, ok := s.reserveStranger(w, r, purpose, msg)
	return ok
}

// strangerAllowed is limitStranger for a caller that answers a refusal
// itself.
func (s *Server) strangerAllowed(r *http.Request, purpose string) bool {
	addr, _ := s.limitAddr(r)
	return s.stranger(purpose).Allow(addr)
}

func (s *Server) signinLimits() *signinLimiters {
	l := &s.signinLimitsState
	l.once.Do(func() {
		l.account = auth.NewRateLimiter(accountFailures, failureWindow)
		// Addresses are anyone's to mint: they live among strangers, apart
		// from the account limiter's checked keys (auth.StrangerLimiter).
		l.address = auth.NewStrangerLimiter(addressFailures, failureWindow)
		l.address.Known = s.householdAddr
		l.passkey = auth.NewStrangerLimiter(passkeyAttempts, failureWindow)
		l.passkey.Known = s.householdAddr
		l.sharedAddress = auth.NewRateLimiter(addressFailures*sharedScale, failureWindow)
		l.sharedPasskey = auth.NewRateLimiter(passkeyAttempts*sharedScale, failureWindow)
		l.unknown = auth.NewRateLimiter(accountFailures*sharedScale, failureWindow)
		l.accountSuccess = auth.NewRateLimiter(accountSuccesses, failureWindow)
		l.addressSuccess = auth.NewStrangerLimiter(addressSuccesses, failureWindow)
		l.addressSuccess.Known = s.householdAddr
		l.sharedSuccess = auth.NewRateLimiter(addressSuccesses*sharedScale, failureWindow)
		l.sessionMint = auth.NewRateLimiter(sessionMints, failureWindow)
		l.deviceMint = auth.NewRateLimiter(deviceMints, failureWindow)
		l.shareFailures = auth.NewStrangerLimiter(shareFailures, failureWindow)
		l.shareStrikes = auth.NewStrangerLimiter(shareStrikes, shareDay)
		l.shareAlarms = auth.NewRateLimiter(shareAlarm, shareDay)
		l.shareAlarmed = auth.NewRateLimiter(1, shareDay)
	})
	return l
}

// isHostAddr says whether ip is one of this host's own addresses (loopback,
// a Docker bridge's gateway, any local interface): the peer Caddy sees for
// the tunnel and the userland proxy, never a client's own.
func (l *signinLimiters) isHostAddr(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsUnspecified() {
		return true
	}
	l.addrMu.Lock()
	defer l.addrMu.Unlock()
	if l.addrs == nil || time.Since(l.addrsAt) > time.Minute {
		l.addrs = map[string]bool{}
		if list, err := net.InterfaceAddrs(); err == nil {
			for _, a := range list {
				if n, ok := a.(*net.IPNet); ok {
					l.addrs[n.IP.String()] = true
				}
			}
		}
		l.addrsAt = time.Now()
	}
	return l.addrs[ip.String()]
}

// parseClientAddr reads an address as a request or a session row carries
// it: brackets and IPv4-in-IPv6 mapping are dropped, so one client is
// always one address. A zone is dropped too, except on a link-local
// address, where it says which link (addrKey).
func parseClientAddr(s string) (netip.Addr, bool) {
	a, err := netip.ParseAddr(strings.Trim(strings.TrimSpace(s), "[]"))
	if err != nil {
		return netip.Addr{}, false
	}
	a = a.Unmap()
	if !a.IsLinkLocalUnicast() {
		a = a.WithZone("")
	}
	return a, true
}

// addrKey is an address as a bucket or pin key (auth.AddrKey, the one
// rule): an IPv4 address as itself, an IPv6 address as its /64 — except a
// link-local one, which every device on the LAN shares the /64 of
// (fe80::/64), and so is keyed as the whole address and its interface.
func addrKey(a netip.Addr) string { return auth.AddrKey(a.String()) }

// fromContainerNet says whether a request's client is on an app container's
// network: a Docker bridge on this host (docker0, br-…, an app's own
// network). Through Caddy the client is the one Caddy names; directly, the
// TCP peer. No person signs in from there — only apps live there.
func (s *Server) fromContainerNet(r *http.Request) bool {
	a, ok := parseClientAddr(s.clientIP(r))
	if !ok {
		return false
	}
	for _, p := range s.signinLimits().containerPrefixes() {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// containerPrefixes are the subnets of this host's Docker bridges, read at
// most once a minute; tests set containerNets.
func (l *signinLimiters) containerPrefixes() []netip.Prefix {
	if l.containerNets != nil {
		return l.containerNets()
	}
	l.addrMu.Lock()
	defer l.addrMu.Unlock()
	if l.bridges == nil || time.Since(l.bridgesAt) > time.Minute {
		l.bridges = []netip.Prefix{}
		if ifs, err := net.Interfaces(); err == nil {
			for _, ifc := range ifs {
				if ifc.Name != "docker0" && !strings.HasPrefix(ifc.Name, "br-") {
					continue
				}
				addrs, _ := ifc.Addrs()
				for _, a := range addrs {
					if n, ok := a.(*net.IPNet); ok {
						if p, err := netip.ParsePrefix(n.String()); err == nil {
							l.bridges = append(l.bridges, p.Masked())
						}
					}
				}
			}
		}
		l.bridgesAt = time.Now()
	}
	return l.bridges
}

// limitAddr is the address bucket a request belongs to (see the package
// comment): the client's own address or /64 — a tunnel visitor's as
// Cloudflare names it, through Caddy's tunnel listener — or sharedAddr for
// one of this host's own. A Cf-Connecting-Ip a request carries itself is
// nobody's word. The bool is always true — every request has a bucket — and
// is kept for callers that branch on it.
func (s *Server) limitAddr(r *http.Request) (string, bool) {
	a, ok := parseClientAddr(s.clientIP(r))
	if !ok || s.signinLimits().isHostAddr(net.IP(a.AsSlice())) {
		return sharedAddr, true
	}
	return addrKey(a), true
}

// refuseSignInHere refuses to sign anyone in from an app container's
// network, where no session would ever be accepted (resolveSession), and
// reports whether it did. A request carrying Cf-Connecting-Ip from there is
// a Cloudflare Tunnel still pointed at :443, where every visitor arrives
// from the host's bridge address: the operator is told where it belongs.
func (s *Server) refuseSignInHere(w http.ResponseWriter, r *http.Request) bool {
	if !s.fromContainerNet(r) {
		return false
	}
	msg := "sign-in isn't available from an app's network"
	if r.Header.Get("Cf-Connecting-Ip") != "" {
		msg = "this Cloudflare Tunnel reaches fog's proxy on port 443, where fog can't tell its visitors apart; " +
			"set the tunnel's service to " + s.tunnelService() + " (Settings › Tunnel)"
	}
	writeJSON(w, http.StatusForbidden, map[string]string{"error": msg, "code": "signin_unavailable_here"})
	return true
}

// bucket is one limiter and one key in it. An address bucket (addr) is
// the asker's own: what it is charged stays charged when the attempt is
// refused further on, or found busy.
type bucket struct {
	l    limiter
	key  string
	addr bool
}

// limiter is what a bucket's limiter does: auth.RateLimiter for keys fog
// checked, auth.StrangerLimiter for addresses.
type limiter interface {
	Reserve(key string) bool
	Refund(key string)
	Window() time.Duration
}

// credentialBuckets are the buckets a credential check for account is
// charged to: the real address's first, then the account's — so an
// address out of budget never reaches the account limiter at all. account
// "" charges only the address; unknownAccount is the one shared bucket for
// handles no one holds.
func (s *Server) credentialBuckets(r *http.Request, account string) []bucket {
	l := s.signinLimits()
	out := []bucket{s.addressBucket(r, l.address, l.sharedAddress)}
	switch account {
	case "":
	case unknownAccount:
		out = append(out, bucket{l: l.unknown, key: account})
	default:
		out = append(out, bucket{l: l.account, key: account})
	}
	return out
}

// addressBucket is the request's real address in lim, or the stranger
// bucket in shared.
func (s *Server) addressBucket(r *http.Request, lim *auth.StrangerLimiter, shared *auth.RateLimiter) bucket {
	addr, _ := s.limitAddr(r)
	if addr == sharedAddr {
		return bucket{l: shared, key: addr, addr: true}
	}
	return bucket{l: lim, key: addr, addr: true}
}

// passkeyBuckets is the passkey ceremony bucket for the real address.
func (s *Server) passkeyBuckets(r *http.Request) []bucket {
	l := s.signinLimits()
	return []bucket{s.addressBucket(r, l.passkey, l.sharedPasskey)}
}

// hashCaller is who a password check is for, for its place in the hashing
// line (auth.CheckPasswordAs): the real address, with the stranger bucket
// given the stranger bucket's scale of places.
func (s *Server) hashCaller(r *http.Request, known bool) auth.Caller {
	addr, _ := s.limitAddr(r)
	c := auth.Caller{Key: addr, Known: known}
	if addr == sharedAddr {
		c.Share = auth.DefaultShare * sharedScale
	}
	return c
}

// shareCaller is a share link's password check in the hashing line: as the
// request's address, a stranger's, in the share links' capped pool.
func (s *Server) shareCaller(r *http.Request) auth.Caller {
	c := s.hashCaller(r, false)
	c.Pool = auth.PoolShare
	return c
}

// refuseLimited reserves one attempt in every bucket, in order, before the
// check is made, and answers 429 when any has none left. Reserving and
// checking for room are one step per bucket, so a burst of parallel
// guesses can't all find room before any is charged; and a bucket that
// refuses was never written to (auth.RateLimiter), so a refused attempt
// makes no key. An account's reservation is given back when a later bucket
// refuses; an address keeps its charge — a refused attempt is still an
// attempt from there. The attempt stays charged: the caller refunds it
// (refund) only when the check passes. Reports whether it refused.
func refuseLimited(w http.ResponseWriter, msg string, bs ...bucket) bool {
	for i, b := range bs {
		if !b.l.Reserve(b.key) {
			refundAccounts(bs[:i]...)
			w.Header().Set("Retry-After", strconv.Itoa(int(b.l.Window().Seconds())))
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": msg, "code": "rate_limited"})
			return true
		}
	}
	return false
}

// refundAccounts gives back what bs reserved except at an address.
func refundAccounts(bs ...bucket) {
	for _, b := range bs {
		if !b.addr {
			b.l.Refund(b.key)
		}
	}
}

// refund gives back the attempt refuseLimited reserved, for a check that
// passed: a right password or code spends nothing.
func refund(bs ...bucket) {
	for _, b := range bs {
		b.l.Refund(b.key)
	}
}

// chargeAttempt takes an attempt in every bucket and reports whether all
// had room: for begins, where every call holds state, so none is refunded.
func chargeAttempt(w http.ResponseWriter, msg string, bs ...bucket) bool {
	return !refuseLimited(w, msg, bs...)
}

// busyHashing answers 429 when too many password checks are under way
// (auth.ErrBusy), and reports whether it did. The account gets its attempt
// back — nobody's password was checked — but the address doesn't: holding
// the line full is not free for whoever does it.
func busyHashing(w http.ResponseWriter, err error, bs ...bucket) bool {
	if err == nil {
		return false
	}
	refundAccounts(bs...)
	w.Header().Set("Retry-After", "5")
	writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": err.Error(), "code": "rate_limited"})
	return true
}

// errSessionRate is a session refused because its person has had too many
// minted lately.
var errSessionRate = errors.New("too many sign-ins for this account; wait a few minutes")

// successesSpent answers 429 — keeping the address's charge, giving back
// the account's — when the account or the address has had all the right
// passwords it may in this window, before any hashing is done.
func (s *Server) successesSpent(w http.ResponseWriter, r *http.Request, account string, bs ...bucket) bool {
	l := s.signinLimits()
	if !l.accountSuccess.Exhausted(account) && !s.successBucket(r).Exhausted() {
		return false
	}
	refundAccounts(bs...)
	w.Header().Set("Retry-After", strconv.Itoa(int(failureWindow.Seconds())))
	writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many sign-ins; try again later", "code": "rate_limited"})
	return true
}

// chargeSuccess charges a right password to its account and address.
func (s *Server) chargeSuccess(r *http.Request, account string) {
	s.signinLimits().accountSuccess.Allow(account)
	s.successBucket(r).take()
}

// successBucket is the request's address's success budget: its own, or the
// stranger bucket's, ten times the size.
func (s *Server) successBucket(r *http.Request) successBudget {
	l := s.signinLimits()
	addr, _ := s.limitAddr(r)
	if addr == sharedAddr {
		return successBudget{shared: l.sharedSuccess, key: addr}
	}
	return successBudget{own: l.addressSuccess, key: addr}
}

type successBudget struct {
	own    *auth.StrangerLimiter
	shared *auth.RateLimiter
	key    string
}

func (b successBudget) Exhausted() bool {
	if b.shared != nil {
		return b.shared.Exhausted(b.key)
	}
	return b.own.Exhausted(b.key)
}

func (b successBudget) take() {
	if b.shared != nil {
		b.shared.Allow(b.key)
		return
	}
	b.own.Allow(b.key)
}

// clearAccount forgets an account's failures after it proves itself.
func (s *Server) clearAccount(account string) { s.signinLimits().account.Reset(account) }
