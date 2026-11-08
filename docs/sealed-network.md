# Sealed apps and LAN-only routes

Two spec fields give an app a boundary that can be checked rather than
promised: `network: sealed` takes away its route out of the machine, and
`lanOnly` on a route keeps the internet from reaching it through the front
door. Fit is the app they exist for, and neither knows anything about it.

```yaml
name: fit
spec:
  network: sealed
  routes:
    - port: 80
      lanOnly: true
---
name: ntfy
spec:
  sealedPeer: true
```

**The code and its unit tests are done. None of this has run on a real
Docker daemon or a real Caddy**: the machine it was written on has neither.
Everything either does in this document is reasoned from how it is documented
to behave, and [Verifying on a real server](#verifying-on-a-real-server)
lists exactly what to run before relying on it.

## Isolation: every app on a network of its own

No two apps share a network. An app's main and worker containers join its
own network, `fog-app-<app>` (`fog-sealed-<app>`, internal, for a sealed
app), and nothing of another app's is on it: only fog's own containers —
the proxy, fog's Postgres when the app needs a database, and `fog-wire`.
So an app can't reach any other app's container, and can't go around the
proxy to a gated app and send it `Remote-User: <anyone>`.

fog's proxies reach an app as `<app>.fog-app`, a name only fog sets. A
name with a dot is one no compose service, container or app can have, so no
app can answer in another's place (app `foo`'s worker `bar` is `foo-bar` on
the network, which used to capture app `foo-bar`'s traffic). An app's
process may not make a service name that is another app's, either.

### Wires

An app is **wired** to another, and reaches it by name, only with the
operator's approval for that pair:

- the other app is in its `reach` list — set in the spec editor, by the
  store for a companion (Office reaches Gotenberg), or by connecting an
  integration on the Integrations tab — or in its `uses` list;
- or it holds the `home` grant (the operator's) and the other app serves
  widgets (`fog-widgets/1`), whose pictures it fetches on port 80
  ([widgets](widgets-contract.md)).

It reaches an approved app on the ports its `http://` (or `ws://`) env URLs
name — `http://glass`, `http://gotenberg:3000`, an integration's `{{url}}`
— else on the app's connection port, else its route ports. An env URL
naming any other app wires nothing: an app installed after another
mentions its name is never wired to it on its own. The issues sweep asks
instead (`wire-unapproved`), and the import review shows every URL a
manifest's settings name, and the reach it asks for. Approval is read from
the app's newest revision, so it takes effect without a redeploy.

Every wire runs through **`fog-wire`**, a Caddy of fog's own with no
published port, in its own compose project under `<data>/wire`. On the
consumer's network it answers to the provider's name, so `http://glass`
resolves to it; for each port a consumer reaches a provider on, it serves
that port and forwards a request only when its Host is the provider (its
name, or a public name it is routed at) and its peer is on one of that
provider's consumers' networks. It deletes every spelling of the identity
headers first, and forwards to `<provider>.fog-app`. Anything else is 403.

So a wire runs **one way**. The provider has no path into the consumer's
network, and nothing it sends reaches the consumer at all, let alone with a
forged identity. A contract a repo declares (`fog-widgets/1`) only ever
makes that app reachable by the apps the operator granted `home`; it never
gives the app reach.

What it costs: a wire carries HTTP (and websockets) only. A `uses` or env
URL of another scheme (`https://app`, `postgres://app`) isn't wired, and
the app can't reach that app; the issues sweep says so (`wire-unsupported`).
Every connection the catalog and fog's own apps make is HTTP: integration
offers (`{{url}}`), Glass's ingest (`POST /ingest/v1/batch`, gzip),
ntfy, Gotenberg, Immich's API. An app configured inside itself to call
another by name (a Sonarr pointed at `http://sabnzbd:8080`) must have that
app in its `uses` list.

### What it rests on

- **`ReconcileNetworks`** keeps running containers in line with the
  deployed specs: after each deploy, after each proxy apply (which may
  recreate Caddy or fog-wire), at boot and every minute (which heals
  Postgres after its own lifecycle recreates it). It is what moved every app
  off the old shared `fog` and `fog-sealed` networks, and off the per-wire
  `fog-link-*` networks fog briefly made, without a redeploy. Caddy,
  Postgres and fog-wire stay on `fog`.
- **An app's sidecars** share a network with its main and workers alone,
  `fog-aux-<app>` (`fog-auxsealed-<app>` for a sealed app). fog makes it too,
  rather than compose, and moves running sidecars onto it at the first
  deploy that needs it, so the new replica and the release command find
  them; the network compose used to make is removed once empty.
- **fog picks each network's subnet**, a /27 out of 172.30.0.0/16,
  172.31.0.0/16 or 10.213.0.0/16, skipping anything a Docker network or a
  host interface already uses: Docker's default pools hold about thirty
  networks. fog-wire's rules name these subnets.
- **Docker's internal-network flag can't change** on a network that exists.
  An app that is sealed or unsealed gets the other name, so the old network
  is simply left behind.
- Verified against a real Docker daemon (Colima): `TestLiveNetworkIsolation`
  (an app on the old shared bridge impersonates the operator to a gated app;
  after, no app reaches it, and the proxy does by its fog-only name) and
  `TestLiveWires` (a consumer reaches its provider through fog-wire with
  forged identity headers deleted; the provider can't reach back; an unwired
  app reaches neither; the proxy's name for `foo-bar` isn't `foo`'s worker).

## What `network: sealed` does

A sealed app's containers join `fog-sealed-<app>`, its own network, which
fog creates with `internal: true`. Docker gives a container on an internal
network no default route, so nothing it sends can leave.

- **Its workers** join the same network.
- **Its sidecars** stay on the app's sidecar network, `fog-auxsealed-<app>`,
  which is internal too. It has to be: a plain network has a default route,
  and the app is on it, so it would be the way out.
- **Host ports** are refused. Docker doesn't publish from an internal
  network, and the spec shouldn't promise a port nothing answers on.

Nobody else has to change how they find it. Caddy (and fog's Postgres, if
the app needs it) join its network too, under the names they already answer
to, so `<app>:<port>` routes and `fog-postgres:5432` work as they do
everywhere. The `fog` network is what keeps their way out, for certificates.

A sealed app is wired only to sealed apps and to apps that declare
`sealedPeer: true` (see [Isolation](#isolation-every-app-on-a-network-of-its-own)):
that is how it reaches ntfy or a collector, by name, at the same address any
app would use, through fog-wire. A spec that names any other app gets no wire, and the issues
sweep says so (`sealed-wire-refused`). When the operator connected the two
on the Integrations tab, the sealed app holds a credential for something it
can't reach, and the sweep files `sealed-integration-refused` in its place,
naming the provider to make a sealed peer: a sealed app connected to Glass
before Glass declared `sealedPeer` is the case it is there for.

The deploy checks the result. After the containers are up, fog asks Docker
what every running container of a sealed app is attached to, and fails the
deploy, naming the container, if any of those networks isn't internal or
can't be read. A stale sidecar network is the case it is there for. The
containers are left running, so a failure here wants acting on.

`EnsureIsolatedNetwork` refuses a `fog-sealed-<app>` that exists with a
route out. Docker can't change the flag on a network that exists, so the fix
is to remove it and let fog recreate it.

## How a sealed app reaches fogd

fogd runs on the host, and every app reaches it at
`host.docker.internal:<port>` (`FOG_API_URL`, `FLARE_DSN`,
`FOG_INFERENCE_URL`). An ordinary app's compose file pins that name with
`host-gateway`, which Docker resolves to the *default bridge's* gateway,
172.17.0.1 on most machines. A container on an internal network has no
default route, so that address is unreachable from it.

What it can reach is its own subnet, and on a bridge network the gateway is
on that subnet: an address the host itself holds, on the bridge. So a sealed
app's compose file pins the same name to its own network's gateway instead:

```yaml
extra_hosts:
  - host.docker.internal:172.30.0.1   # read from Docker at each deploy
```

The packets are addressed to the host, so they end at the host's INPUT
chain. Docker's isolation rules for internal networks sit on FORWARD: they
stop traffic passing through the host, which is the boundary, and don't touch
traffic for the host itself. Nothing is routed, so nothing is opened.

This was chosen over the alternatives:

- **A second fogd listener on the gateway address** would add a port and a
  start-up order to depend on (the bridge has to exist before fogd binds),
  and buy nothing while fogd listens on `:8080` on every address, as
  `packaging/fogd.service` sets it. `FOG_LISTEN` set to one address would
  break sealed apps, and would also break Caddy's route to fogd.
- **Attaching fogd to the network** isn't possible; it isn't a container.
- **A fixed subnet** could collide with the LAN or with a network Docker has
  already handed out. fog picks a free one at creation and asks Docker for
  the gateway at each deploy.

Apps need no change: the env is byte-for-byte what any other app gets.

## What sealed does not promise

Being sealed means there is no IP route out of the machine. It doesn't mean
nothing the app knows can ever leave. These all stay open by design, and a
review of a sealed app should read them as its actual boundary:

- **Every sealed peer it is wired to can be talked to.** A sealed peer keeps
  its internet. If it relays what it's sent, the relay is the way out, so
  choosing peers is a trust decision.
- **Caddy forwards by Host header.** A sealed app can send a request for any
  routed app to Caddy on its own network, which reaches that app whether or
  not it is a peer. Those apps are therefore reachable too, on their HTTP
  routes (through the gate, for a gated one), and the same trust applies.
- **fogd is reachable, all of it**, as it is from any app: the `/app/v1`
  surface, and the rest of the host's listening services on the gateway
  address (SSH included), behind their own authentication. fogd acts on the
  outside world for apps that are granted it: chat, notifications, events.
  `uses` grants still apply, and a sealed app should have few.
- **A sealed app's widgets are read by whoever holds the `home` grant**
  (docs/widgets-contract.md). fogd reaches into a sealed app's network and hands the
  answer to that app, which needn't be sealed. Whatever `/fog/widgets` and
  `/fog/search` answer has left the boundary, so they are the app's to keep
  content-free or to not serve.
- **Inference leaves the container by design.** `FOG_INFERENCE_URL` is fogd,
  which hands the prompt to a machine that lends a model server. A sealed app
  that uses it wants `locality: household` (docs/compute-contract.md), and
  that's still only as good as what those machines' servers do with a prompt.
- **Builds aren't sealed.** A build can fetch anything from the internet;
  the seal starts when the container does. It is isolated from other apps,
  though: each git app builds on a BuildKit builder of its own on a network
  of its own (`fog-build-<app>`), with every `RUN` step in a namespace of
  its own behind that builder. A `RUN` step keeps BuildKit's default
  capabilities, `NET_RAW` and `MKNOD` included — BuildKit can't drop them —
  so the network is what keeps one app's build from reaching or spoofing
  another's. Without buildx, fog falls back to the daemon's own builder,
  which runs on Docker's default bridge.
- **What a sealed app is handed by `uses`** is only reachable if the other
  app is a peer: fog leaves that wire out and files `sealed-wire-refused`.

## `lanOnly` routes

A route with `lanOnly: true` answers only clients whose TCP peer is in
Caddy's `private_ranges`, and only requests that didn't come through
Cloudflare. Everything else gets a 403:

```
handle @lan_fit_example_com {
    ...everything the route does...
}
handle {
    respond "This address only answers on the home network" 403
}
```

The whole route goes inside the allowed branch rather than a refusal going
ahead of it. Caddy sorts `handle` blocks with a path before those without,
and runs `handle` before `respond`, so a refusal placed in front would be
passed over by a gated route's `/.fog/gate/*`. Nothing outside the branch
means no path to forget.

The check is `remote_ip`, not `client_ip`: it reads the TCP peer and ignores
`X-Forwarded-For`, which a client can set to anything. fog's own health
probes go straight to the container, so a route's health path needs nothing.

Three other ways in are closed:

- **The direct port.** fog gives a routed app a port on the host, 20000 and
  up, which goes around Caddy. A route that's `lanOnly` gets none, the same
  rule as `auth: true`, and one it already had closes at the next reconcile.
  So on a fog with no domain, there is no address for it at all.
- **Cloudflare Tunnel.** cloudflared runs on the host, so a tunnel route
  pointed at Caddy arrives from a private address, and `private_ranges`
  alone would let the internet in. Requests through it carry
  `Cf-Connecting-Ip`, and the route refuses any request that has it.
  A LAN client that sends the header refuses only itself.
- **The UI.** The spec editor carries both fields through a save.

What it still rests on:

- **The peer address has to be the real one.** On Linux, Docker's published
  ports preserve the source for IPv4 from the LAN. Two paths don't, and
  would make an outside client look private: a router forward that masquerades
  its traffic, and any reverse proxy in front of fog other than Cloudflare's.
  IPv6 can't: Caddy's ports are published on IPv4 alone, because Docker's
  userland proxy answered IPv6 clients and handed Caddy the bridge gateway, a
  private address, as their peer. fog's DNS records are IPv4 only, and an IPv6
  client is refused at once and falls back to IPv4.
- **`private_ranges` is an address check.** A guest on the Wi-Fi is on the
  LAN. So is anyone on the VPN, which is the intent: away access is the
  household at home by another road.

## Verifying on a real server

Nothing below has been run. Do it on a Linux server with a real Docker
daemon, once, before the first sealed app holds real data; Docker Desktop's
networking differs. `fit` stands in for any sealed app whose image has a
shell and `wget`.

Set `C` to the app's main container first; a swap changes its name.

```sh
C=$(docker ps -q --filter label=fog.app=fit --filter label=com.docker.compose.service=fit)
```

The one-time cost: the compose files for Caddy and Postgres change, so
each container is recreated once, at the next `Apply` and the next database
provision. Expect a few seconds of each.

**1. The network.**

```sh
docker network inspect fog-sealed-fit --format '{{.Internal}} {{json .IPAM.Config}}'
```

Expect `true` and a gateway. Note the gateway; below it's `$GW`.

**2. Caddy and Postgres kept working.**

```sh
docker inspect caddy --format '{{json .NetworkSettings.Networks}}' | jq 'keys'
docker inspect fog-postgres --format '{{json .NetworkSettings.Networks}}' | jq 'keys'
docker exec caddy wget -q --spider https://acme-v02.api.letsencrypt.org/directory
```

Expect `fog`, `fog-sealed` and every app's own network on Caddy, `fog`,
`fog-sealed` and the own network of each app with a database on Postgres,
and the third command to succeed: Caddy must keep its default route through
`fog`. Load an existing app in a browser from
the LAN, to confirm published ports survived the second network.

**3. A sealed app has no way out.** Deploy one, then:

```sh
docker exec $C ip route                         # expect no "default" line
docker exec $C wget -T5 -qO- http://1.1.1.1/    # expect it to fail at once
docker exec $C wget -T5 -qO- http://example.com # expect a name error
docker exec $C nslookup example.com 1.1.1.1     # expect it to fail
```

The fourth is the one to watch: Docker's embedded resolver can forward
queries for a container, which would make DNS a tunnel out. Current daemons
don't for a container with only internal networks, but that hasn't been
checked here. A sidecar is checked the same way: `docker exec fit-redis-1 ip route`.

**4. fogd is reachable from it.**

```sh
docker exec $C getent hosts host.docker.internal   # expect $GW
docker exec $C sh -c 'wget -qO- --header "Authorization: Bearer $FOG_API_TOKEN" $FOG_API_URL/app/v1/connections'
```

Expect JSON. A timeout means the host's firewall drops INPUT on the bridge:
fog sets none up, so look for one the operator added.

**5. fogd reaches it, and a swap works.** The app page should show it healthy,
which means fogd's probe got through to the sealed address. Deploy it again
and watch the log for `new replica healthy; draining old`. Then confirm the
route still answers from a LAN client and only one main container remains.

**6. Names resolve across the boundary.**

```sh
docker exec $C getent hosts fog-postgres ntfy      # ntfy declares sealedPeer, and fit names it
docker exec $C getent hosts sonarr                 # an ordinary app: expect nothing
```

**7. The deploy check catches a breach.** With a throwaway sealed app:

```sh
docker network connect fog $C
```

Deploy it, and expect the deploy to fail naming that container and `fog`. Then
remove the app.

**8. Sealing an app with sidecars.** Its private network has to change, and
Docker can't do that in place. Stop the app before the first sealed deploy;
compose may otherwise refuse, or fail, depending on its version. Step 3's
`ip route` on the sidecar proves it took.

**9. `lanOnly`.**

```sh
# The Caddyfile is accepted. Apply already does this, and keeps the old
# routes if it fails.
docker exec caddy caddy validate --config /etc/caddy/Caddyfile
# From the LAN: expect the app.
curl -I https://fit.example.com
# From the LAN, the header a tunnel sets: expect 403.
curl -I -H 'Cf-Connecting-Ip: 8.8.8.8' https://fit.example.com
# Spoofing a private address can't help: expect 403 from outside.
curl -I -H 'X-Forwarded-For: 192.168.1.2' https://fit.example.com   # from a phone on mobile data
```

Then, if the house uses them: the same request from outside through a port
forward, through the tunnel, and over public IPv6. Each should be a 403.
Confirm the app's old direct port is gone: `ss -ltn 'sport >= 20000 and sport <= 20999'`.
