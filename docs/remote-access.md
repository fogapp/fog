# Reaching fog from outside the house

The domain wizard (Settings → Network → Domain & HTTPS) gives every app a
real hostname with a real certificate. By default those names only work
at home: `*.<domain>` is an unproxied Cloudflare A record holding the
server's **LAN** address, so nothing is exposed and there is nothing to
forward. Step 4 of the wizard is the switch that makes the same names work
from anywhere. This page is what that switch does, what it needs from the
router, and how fog tells you whether it worked.

## The two targets

| `dns_target` | `*.<domain>` holds | Works from | Needs |
|---|---|---|---|
| `lan` (default) | the server's LAN address | home | nothing (rebind allowlist on some routers) |
| `wan` | the house's public IPv4 address | anywhere | TCP 443 (and 80) forwarded to the server |

The record is never proxied through Cloudflare in either mode: LAN
traffic stays on the LAN, uploads and streams don't go through
Cloudflare's limits, and Caddy's DNS-01 certificate is the one the
browser sees.

Toggling the switch saves the target and runs one reconcile immediately;
the response the page shows is the state that reconcile produced.

## The keeper (`internal/dnssync`)

One loop keeps the record honest for both targets. Every five minutes it
learns the address that matters — the LAN address (free) or the public
one (one tiny HTTPS request to Cloudflare's trace endpoint, ipify as a
fallback, IPv4 only) — and only when that differs from the address it
last confirmed, at boot, or on a daily patrol does it touch the Cloudflare
API. So a quiet house costs one API round-trip a day; a new ISP lease or a
new LAN is corrected within minutes and announced as a notice.

"Refresh DNS now" on the page forces a full pass: re-learn the address,
rewrite the record if it drifted, renew the router mapping.

Three failures in a row against Cloudflare raise one persistent notice
(revoked token, moved zone); the next success takes it down.

## Getting through the router

With the `wan` target, fog asks the router to send TCP 443 and 80 here
using whichever of two protocols it speaks (`internal/wan`):

1. **NAT-PMP** — one UDP round-trip to the gateway on port 5351.
2. **UPnP IGD** — SSDP discovery, then a SOAP `AddPortMapping` on the
   WANIPConnection service. Routers that only keep permanent leases say so
   (error 725) and fog asks again for one.

Leases are one hour and renewed fifteen minutes early; permanent ones are
re-asserted on the daily patrol so a rebooted router relearns them.
Switching back to `lan`, or turning the "ask the router" switch off,
releases the mapping. Port 80 is optional (it only serves the http→https
redirect; certificates are DNS-01) so a router that refuses 80 but grants
443 counts as a success.

When neither protocol answers, the page and a notice say so, and give the
manual recipe with the real numbers in it: forward TCP 443 (and 80) to
`<LAN IP>`, router admin usually at `http://<gateway>`.

### Carrier-grade NAT

If the router's own WAN address (as reported by NAT-PMP/UPnP) is not the
address the internet sees — typically it's in 100.64.0.0/10 — the ISP is
NATing too, and no port forward can reach the house. Fog reports `cgnat`
and points at the Cloudflare Tunnel (Settings → Network → Cloudflare Tunnel),
which needs no open ports.

## When the router won't cooperate at all

A router that speaks neither NAT-PMP nor UPnP, or an ISP behind CGNAT,
leaves the `wan` target unreachable however patient you are. The answer that
needs nothing from the router is [away access](vpn.md): fog joins a
Tailscale network of yours and advertises a route to its own LAN address, so
`*.<domain>` keeps its `lan` target and resolves — and connects — the same
from the road as from the sofa.

## Moving house, renumbering, and the ISP's whims

Everything above is state that the network can invalidate under fog's
feet, so the keeper treats each piece as tied to the network it was
established on:

- **Noticing the move.** `internal/netwatch` subscribes to the kernel's
  route/address broadcast (netlink on Linux; a 15-second poll elsewhere)
  and kicks the keeper a few seconds after a burst of changes settles.
  A new DHCP lease or a cable moved to another router is reconciled
  within seconds, not on the next five-minute tick. While the network is
  still coming up (address but no internet yet) the loop retries at 15s,
  30s, 1m, 2m, 4m instead of waiting five minutes.
- **The port mapping** is keyed on the LAN address and gateway it was
  made on. A different gateway or a new address on the same router makes
  it void: it is requested again on the new network, and never
  "released" against a router that can no longer be reached. UPnP
  routers that still hold `443 → old address` refuse the new mapping
  with a conflict (718); fog deletes that entry and retries — it was
  fog's own. NAT-PMP has no such call; its stale entry simply expires
  with the one-hour lease, which is why fog never asks for a permanent
  lease unless the router insists.
- **Proof from outside** (the beacon, below) records the public address
  it was proof for. Under a different address it reads as stale on the
  page and no longer suppresses the port-forwarding notice.
- **Notices** raised by a previous process are taken down by the first
  success after a restart, not only by a flag that died with the process.
- **The record's TTL** is 60s with the `wan` target (300s for `lan`), so
  a phone on the road recovers within a minute of an ISP address change.

## The from-inside self-test

With the `wan` target every device at home reaches `fog.<domain>` at the
public address and relies on the router looping that traffic back in
(NAT loopback / hairpin). The keeper does exactly that itself: a TLS
handshake with `<public IP>:443` under the name `fog.<domain>`. Passing
proves two things at once — the forward works (loopback applies the same
rules) and home devices will work — so it also counts as reach
confirmation. Failing while the beacon has confirmed the outside means
the router lacks loopback; that raises its own notice, with the two
fixes: enable loopback, or give `<domain>` a local DNS override to the
LAN address in the router / Pi-hole. The test reruns every tick while
failing and on the daily patrol once passing.

## Proof from outside: the beacon

Only the outside can prove the outside: a router without NAT loopback
makes a working forward look broken from inside. So step 4 shows a link:

    https://fog.<domain>/api/reach/beacon/<token>

Open it on a phone with Wi-Fi off. The page says whether the request
arrived from outside, and Domain & HTTPS records the visit (time and
address). A visit from a public address is the fact that clears the
port-forwarding notice; a visit from the LAN is recorded but proves
nothing. The token is minted once per server and the endpoint answers
only to it (404 otherwise). Switching targets forgets past proof, and a
changed public address makes old proof stale.

## Settings and API

| Setting | Values |
|---|---|
| `dns_target` | `lan` (default when empty), `wan` |
| `port_map` | empty (ask the router, default), `off` |
| `reach_beacon_token` | minted on first use |
| `reach_beacon_seen` | JSON of the last beacon visit, with the public address it was proof for |

| Call | Does |
|---|---|
| `GET /api/domain/reach` | the keeper's snapshot + beacon URL (no network calls) |
| `PUT /api/domain/reach {target, portMap?}` | save and reconcile now; returns the result |
| `POST /api/domain/refresh` | force a reconcile; returns the result |
| `GET /api/reach/beacon/{token}` | unauthenticated; records the visit, answers a page |
| `GET /api/domain/status` | now carries `dnsTarget` and `desiredIp`; `wildcardRecord` is judged against the desired address |

## Not done

- IPv6 / AAAA: the record is IPv4 only. The ambitious next step for a
  house behind CGNAT with working IPv6 is an AAAA record holding the
  box's global address — but the router's IPv6 firewall blocks inbound
  by default and nothing standard opens it (UPnP's IPv6 pinhole service
  is rare), so fog would be publishing a name that may not answer. Do it
  only with a self-test that proves it, the way the hairpin test does
  for v4.
- Releasing mappings on shutdown: deliberately not — Caddy keeps serving
  through a fogd restart, and releasing would cut outside access on every
  update. A mapping left at the old house expires with its one-hour lease.
- PCP (RFC 6887), the NAT-PMP successor — no router seen yet that speaks
  PCP but not NAT-PMP.
- Proxying the record through Cloudflare (orange cloud) to hide the home
  address: deliberately not, see "The two targets".
