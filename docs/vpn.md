# Away access: reaching fog from anywhere without opening a port

[Remote access](remote-access.md) describes the `wan` target: point
`*.<domain>` at the house's public address and forward TCP 443. It is the
right answer when the router cooperates. When it doesn't — no NAT-PMP, no
UPnP, an ISP that puts the house behind carrier-grade NAT, or simply no
appetite for an open port — this is the other one.

Fog joins a Tailscale network of the operator's own and advertises a single
route: **its own LAN address**. That's the whole design, and the reason it's
worth preferring over the alternatives.

## Why a route, and not a second hostname

The obvious way to build this is to give fog a tailnet address and point DNS
at it. That breaks the moment a device at home isn't on the tailnet: the
name now resolves to an address only tailnet members can reach.

So nothing about DNS changes. `dns_target` stays `lan`, `*.<domain>` keeps
holding `192.168.1.200`, and the wildcard certificate keeps matching,
because the name and the address are the same ones as before. A phone on
cellular resolves `photos.<domain>` to the LAN address exactly as it does on
the sofa — and the tailnet carries the packets there.

One name, one record, one certificate, two paths to the same place.

## Why a /32

Fog advertises `<LAN IP>/32`, not `192.168.1.0/24`.

A /24 collides with every café, hotel, airport and parents' house that
numbers its network the same way, and on a travelling laptop the local
network wins — so the one setup that has to work away from home fails in
precisely the places you are away from home. A single host route is the
smallest thing that makes the domain work, and the least likely address on
the LAN to collide with anything.

It has a second benefit: a host route to fog's own address is delivered
locally rather than forwarded, so it needs no `net.ipv4.ip_forward`.

Someone who wants the whole LAN can still have it — "Advertise a different
route" under Settings → Network → Away access takes any set of CIDRs. Fog then
checks IP forwarding and says so if it's off, because a wider route genuinely
is routed through this box.

## The cost of the route: fog's connector becomes the only way in

Once a device accepts fog's `/32`, that route beats its own on-link LAN
route, so the device reaches fog **through the tailnet even at home**. The
tunnel still takes a direct path across the LAN, so this costs microseconds
— but it means the connector is that device's only path to fog, and a dead
connector takes fog off the LAN for it too.

This is not theoretical. It is exactly what happened the first time the
route was approved here: the daemon was stopped for a test, and the laptop
sitting on the same switch could no longer reach `192.168.1.200` at all,
while every other address on the LAN answered normally.

Two things follow, and both are built:

- **fogd supervises the daemon**, with the restart ladder in
  `internal/supervise`. A crash is recovered in seconds without anyone
  noticing.
- **Stopping withdraws the route first.** `Manager.Stop` runs `tailscale set
  --advertise-routes=` and waits ~1.5s before killing the daemon, so peers
  learn the withdrawal from the control plane while this node is still
  online and fall back to the LAN path they had all along. Killing first
  would leave them pointing into a tunnel that no longer exists until the
  control plane notices the node is gone. `Manager.Shutdown` does the same
  when fogd itself stops cleanly, so a restart or self-update strands nobody.

The residual case is fogd itself dying, or the box losing power, while a
device holds the route — then that device waits for Tailscale to withdraw
the route for an offline node. Nothing fog can do from the inside covers
that; switching the Tailscale profile off on the client is the immediate
way back.

## What fogd does (`internal/vpn`)

The same shape as the Cloudflare Tunnel connector, and for the same reason:
a downloaded binary supervised as a child of fogd, restarted on a backoff
ladder shared with it (`internal/supervise`).

- **Install.** Reads Tailscale's release index
  (`pkgs.tailscale.com/stable/?mode=json`), downloads the current stable
  tarball for this architecture, and verifies it against the **published
  sha256** beside it. Both binaries are then extracted, each checked to be
  an ELF executable for this machine, and only moved into place once both
  have passed — a new CLI against a missing daemon is worse than no install
  at all. Names inside the archive choose nothing but which file is which:
  paths in the tarball are ignored.
- **Supervise.** `tailscaled` runs with its state under
  `<data>/tailscale/`, restarted with the same ladder cloudflared uses (2s
  doubling to a minute, reset after five healthy minutes).
- **Join.** Once the daemon's socket appears, fogd runs `tailscale up
  --advertise-routes=<route> --accept-routes=false --accept-dns=false
  --reset`. Refusing the tailnet's routes and DNS is deliberate: fog is a
  door into the house, not the way out of wherever the laptop is, and
  joining must not change how the server itself reaches the internet.
- **The auth key** lives in the vault, and reaches `tailscale up` as
  `--authkey=file:<path>` — a key on a command line is readable by every
  process on the box. It's spent only when the node has no key of its own,
  so a fogd restart doesn't burn a single-use key.
- **Moving networks.** The advertised route is keyed on the LAN address it
  was made for, exactly like the port mapping in `internal/dnssync`.
  `internal/netwatch` kicks the manager on an address change and it
  re-advertises. The same kick recovers an autostart that lost the race with
  DHCP at boot.

## What fog cannot do for you

Two steps belong to the operator's Tailscale account, and no amount of code
here changes that:

1. **Approving the route.** Admin console → Machines → this machine → Edit
   route settings. Until that is ticked, everything else can be perfect and
   nothing will connect. An `autoApprovers` entry in the tailnet's access
   controls makes it automatic for next time — worth adding, because a new
   LAN address means a new route to approve.
2. **Installing Tailscale on the away device** and signing it into the same
   tailnet. Nothing needs configuring there; the route arrives on its own.

So the page's real job is saying which of the two is outstanding. It reads
`tailscale status --json` and judges the advertised route against
`Self.PrimaryRoutes` — what the coordination server says this node is
actually serving, not what fog asked for. Approval means *every* advertised
route is there; a partial set still leaves some name that won't connect.

And it distinguishes "we haven't asked the daemon yet" from "we asked and
the route isn't approved" (`statusKnown`). Without that the page would send
the operator to the admin console to fix something that may already be fine.

## One tailnet at a time

A device can only be on one tailnet at once. Someone who uses Tailscale for
work switches profiles in the client's menu to reach fog and switches back
afterwards — two clicks, a couple of seconds.

The alternatives are worth knowing but were not built:

- **Node sharing** into the work tailnet is the obvious escape, but shared
  nodes don't carry their subnet routes, so the route-based design above
  doesn't survive it — you'd be reaching fog at a raw tailnet address, and
  the domain would need a split-DNS entry in a tailnet you probably don't
  administer.
- **Raw WireGuard** coexists with Tailscale on macOS and Linux (both are
  route-based interfaces) but not on iOS or Android, which allow one tunnel
  system-wide. It also needs the inbound UDP port this feature exists to
  avoid.
- **The Cloudflare Tunnel** needs no client at all and so is orthogonal to
  whatever VPN is running — the fallback for a device you can't install
  anything on, at the cost of routing traffic through Cloudflare.

## Settings and API

| Key | Meaning |
|---|---|
| `tailscale_auth_key` (secret) | vault-held auth key; never returned by any API |
| `vpn_enabled` | `1` while away access is on; drives autostart at boot |
| `vpn_routes` | override for the advertised routes; empty means `<LAN IP>/32` |

| Call | Does |
|---|---|
| `GET /api/vpn` | install/run state, the advertised route, and the daemon's own reading when one was taken |
| `POST /api/vpn/start {reauth?}` | install if needed, supervise, join; `reauth` logs out first to move tailnets |
| `POST /api/vpn/stop` | stop the daemon and don't restart it; the node stays in the tailnet |
| `PUT /api/vpn/routes {routes}` | validate and save a route override, then re-advertise |

Stopping deliberately leaves the machine in the tailnet: removing it is a
decision for the admin console, not a side effect of a toggle.

## Verified on the box

`internal/vpn/live_test.go` is opt-in the same way `internal/wan`'s router
probes are, and runs against a temp `BinDir` and `DataDir` so it touches
neither `/usr/local/bin` nor the running fogd:

    GOOS=linux GOARCH=amd64 go test -c -o /tmp/vpn.test ./internal/vpn
    scp /tmp/vpn.test fog.lan:/tmp/ && ssh fog.lan
    cd /tmp && sudo FOG_VPN_LIVE=1 ./vpn.test -test.run TestLive -test.v

Root is needed because tailscaled creates a TUN device. As of 2026-09-09 on
fog (Ubuntu 24.04, amd64) this passes: the release index, published
checksum, download, extraction and ELF check produce a working tailscale
1.102.3 in about a second; tailscaled comes up under supervision, opens its
socket, and answers `status --json` in the shape `parseStatus` reads
(`BackendState`, `HaveNodeKey`, a null `Self` before login); the derived
route is `192.168.1.200/32`; and a bad auth key surfaces as
`upError: backend error: invalid key: unable to validate API key` rather
than only reaching a log.

Two more live tests need a real tailnet. `TestLiveJoin` joins it, and only
runs with `FOG_VPN_LIVE_KEY_FILE` naming a file that holds an auth key;
`TestLiveRoute` re-reads an already-joined node and needs no key.
`FOG_VPN_LIVE_BINDIR` and `FOG_VPN_LIVE_DATADIR` point them at real paths, so
a join lands the node key where fogd will look for it.

## Not done

- **Tailscale Serve / Funnel**: fog does not use them, and Funnel would be a
  public exposure decision of its own.
- **Exit-node mode**: not offered. It's a different feature wearing the same
  clothes, and nothing about the domain needs it.
- **Removing the node on stop**, for the reason above.
- **Headscale or NetBird** as the coordination plane. A self-hosted control
  server needs a public endpoint of its own, which is the problem this
  feature exists to avoid.
