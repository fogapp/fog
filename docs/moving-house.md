# Moving the server to another network

Unplugging the box and plugging it in at a new house (new router, new
subnet, new DHCP) is a supported move. This page says what heals itself,
what asks for one edit, and the order that makes the day painless.

## What heals itself

- **The box's own address** — DHCP everywhere, nothing persisted. The
  console banner shows the new IP within seconds; `<name>.local` re-announces
  on the new network.
- **Public DNS** — fogd watches the address `*.<domain>` is meant to
  hold — the LAN address, or the house's public one when outside access
  is on — and re-points the record through Cloudflare when the new lease
  lands (plus a boot pass). A notice announces the change and
  names the one router-side step a new network sometimes needs:
  allowlisting the domain in the router's DNS-rebind protection. The
  kernel's own address/route events drive this, so it lands seconds
  after the lease, not minutes. With outside access on, fog also learns
  the new public address, asks the new router to forward port 443
  (NAT-PMP/UPnP), re-runs its from-inside self-test, and says so if
  either fails — see [remote-access.md](remote-access.md).
- **Cloudflare Tunnel** — outbound-only; reconnects on its own. It is the
  remote-access path that works before anything else settles.
- **HTTPS certificates** — DNS-01 issuance; renews from any network.
- **NAS mounts** — a reconcile loop retries with backoff forever, so a NAS
  that powers up after fog (the normal order) mounts itself; apps whose
  volumes waited on the share are restarted automatically. A share that
  stays away becomes an issue, not a mystery.
- **Federation addresses** — while either direction still connects,
  heartbeats carry each side's current address and the stale one is
  corrected. Domainless boxes advertise their live LAN address.

## What asks for one edit

Each of these announces itself as a notice or issue when it applies:

- **A NAS at a new address** — edit the share in Settings → Storage → Disks & shares
  (the stored password survives an edit; only delete loses it).
- **An update source that stayed behind** (`gitea.lan`, a LAN mirror) —
  point Settings → Server → Updates at a reachable source; app repos pinned to old
  LAN IPs are edited on each app.
- **A peer that also moved** — if *both* sides changed address, neither
  can tell the other; set the peer's address on the Connect page, or
  re-pair.

## Entirely outside fog

- Router port forwards from the old house, when the new router doesn't
  open them on request — recreate them by hand (Settings → Network →
  Domain & HTTPS says which port and which address), or use the tunnel and
  skip forwarding.
- MCP/API client configs on laptops and phones that stored the old URL.

## The order that works

1. Shut the box down cleanly **before** unplugging (or unmount NAS shares
   first) — a yanked NFS mount is detachable from the UI now, but a clean
   stop skips the question.
2. In the new house: router and NAS first, fog box last.
3. Open `http://<name>.local:8080` or the IP on the console banner. Log
   in — auth works from any address.
4. Read the notices. Everything that needs a hand is listed there; the
   rest has already fixed itself.

Backups refuse to write while their NAS share is unmounted — loudly, so
the staleness alarm fires rather than snapshots silently landing on the
wrong disk. A restore performed in the new house gets a
verify-your-addresses notice on first boot.
