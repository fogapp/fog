# Fog as a Proxmox LXC container

`proxmox-lxc.sh` builds a new Fog server as an LXC container on a Proxmox VE
node: Debian 13, Docker, `fogd`, and the first-run wizard waiting on port
8080. It is the fastest way to get a Fog server — a couple of minutes, no
image to build or flash — and the natural fit when the box you already own is
a Proxmox host.

For a Fog **VM** — its own kernel, NAS shares Fog mounts itself, full parity
with a metal install — use [`proxmox-vm.sh`](proxmox-vm.md), which compares
the two. For metal, see [installer.md](installer.md).

## Run it

In the Proxmox web UI, open the node → **Shell**, and paste:

```sh
bash -c "$(curl -fsSL https://raw.githubusercontent.com/fogapp/fog/main/proxmox-lxc.sh)"
```

You get a menu: **default settings** (CT at the next free id, `fog`, 2 cores,
4 GB RAM, 32 GB disk, DHCP) or **advanced**, which asks for every value. Then
it creates the container, installs into it, and prints the dashboard URL.

Nothing is asked twice and nothing outside the new container is touched. If a
step fails, the container is left in place with the failing command named —
`pct enter <ctid>` to look, `pct destroy <ctid>` to discard.

## Parameters

Every value has a flag and an environment variable. With a terminal you're
asked; without one (a cron job, a pipe) the defaults and environment are
used, so the same script serves both. `--help` prints this table.

| Flag | Env var | Default |
|---|---|---|
| `--ctid N` | `FOG_CTID` | next free id |
| `--hostname NAME` | `FOG_HOSTNAME` | `fog` — also `http://<name>.local` |
| `--root-password PW` | `FOG_ROOT_PASSWORD` | none; use `pct enter` |
| `--disk GB` | `FOG_DISK` | `32` |
| `--cores N` | `FOG_CORES` | `2` |
| `--ram MB` | `FOG_RAM` | `4096` |
| `--swap MB` | `FOG_SWAP` | `512` |
| `--storage NAME` | `FOG_STORAGE` | asked, or the only `rootdir` storage |
| `--template-storage NAME` | `FOG_TEMPLATE_STORAGE` | first `vztmpl` storage |
| `--template NAME` | `FOG_TEMPLATE` | newest `debian-13-standard` |
| `--privileged` | `FOG_UNPRIVILEGED=0` | unprivileged |
| `--no-onboot` | `FOG_ONBOOT=0` | starts at boot |
| `--tags A;B` | `FOG_TAGS` | `fog` |
| `--bridge NAME` | `FOG_BRIDGE` | `vmbr0` |
| `--ipv4 CIDR\|dhcp` | `FOG_IPV4` | `dhcp` |
| `--gateway IP` | `FOG_GATEWAY` | required with a static IPv4 |
| `--ipv6 MODE\|CIDR` | `FOG_IPV6` | `none` |
| `--vlan TAG` | `FOG_VLAN` | untagged |
| `--mtu N` | `FOG_MTU` | bridge default |
| `--dns IP` | `FOG_DNS` | the node's resolver |
| `--searchdomain DOM` | `FOG_SEARCHDOMAIN` | the node's |
| `--ssh-keys FILE` | `FOG_SSH_KEYS` | none |
| `--port N` | `FOG_PORT` | `8080` |
| `--source URL` | `FOG_SOURCE` | `https://github.com/fogapp/fog` |
| `--branch NAME` | `FOG_BRANCH` | `main` |
| `--manifest-url URL` | `FOG_MANIFEST_URL` | resolved from `--source` |
| `--raw-base URL` | `FOG_RAW_BASE` | resolved from `--source` (passed to `install.sh`; every installed file comes from the manifest) |
| `--timezone TZ` | `FOG_TZ` | the node's timezone |
| `--admin-user NAME` | `FOG_ADMIN_USER` | the web wizard creates it |
| `--admin-password PW` | `FOG_ADMIN_PASSWORD` | " |
| `--server-name NAME` | `FOG_SERVER_NAME` | the hostname |
| `--base-domain DOMAIN` | `FOG_BASE_DOMAIN` | none |
| `--yes` | `FOG_YES=1` | confirms the summary |
| `--dry-run` | | prints the `pct create` and stops |

### Unattended

Give `--admin-user` and `--admin-password` and the first-run wizard is
completed for you (the same `POST /api/setup` the browser would send), so the
dashboard is ready to sign in to when the script finishes:

```sh
FOG_ADMIN_PASSWORD='a long passphrase' \
  ./proxmox-lxc.sh --yes --ctid 210 --hostname mist \
    --ipv4 192.168.1.60/24 --gateway 192.168.1.1 \
    --disk 64 --ram 8192 --admin-user matthew --base-domain home.example.com
```

Passwords on the command line are visible in the node's process list — pass
them in the environment, as above, or leave them out and use the browser.
Fog requires an admin password of 10 characters or more; the script checks
that before it creates anything.

`--dry-run` prints the exact `pct create` line and exits, which is the honest
way to see what a set of flags means before running it for real.

## What the container gets

- **Debian 13**, unprivileged, `nesting=1` and `keyctl=1` — the two features
  the Docker engine needs inside a user namespace.
- **Docker CE** from `download.docker.com` (engine, CLI, buildx, compose) —
  the same source and package set as the USB image, so an app behaves the
  same on either; its apt key is checked against Docker's published
  fingerprint before apt trusts it. Log rotation is capped at 10 MB × 3,
  journald at 200 MB.
- **`fogd`**, installed by the repo's own [`install.sh`](../install.sh) — the
  same script an existing Linux server would run: the release manifest's
  signature is checked against fog's release key, and the binary, unit and
  rollback guard against the manifest's sha256. The node resolves
  `--source` and verifies the release with the same resolver `install.sh`
  carries, takes `install.sh` from the signed manifest by its sha256, and
  pushes only those verified bytes into the container — nothing fetched is
  ever run on the node, `--dry-run` included. One place knows how fogd is
  installed, and one place the source can fail. `--source` takes a Gitea,
  GitHub, or GitLab repo, a plain directory of release files, or a
  `latest.json` URL — see
  [hosting-the-source.md](hosting-the-source.md).
- **avahi**, so the server answers at `http://<hostname>.local:8080`, and
  **sshd**, reachable at the container's own IP.

## Living with a container instead of a machine

A container shares the node's kernel, and that is visible in a few places.
None of it blocks day-to-day use; all of it is worth knowing before you pick
this path over a VM.

- **Docker's storage driver.** On `dir`, LVM-thin, and ext4/XFS-backed
  storage, Docker uses `overlay2` normally. On **ZFS or Ceph** storage an
  unprivileged container can't stack overlay2, so the script enables the
  container's `fuse` feature and installs `fuse-overlayfs` instead — slower
  than overlay2 but correct. The install prints which driver it ended up
  with. If you ever see `vfs`, every image layer is being *copied*: that's a
  misconfiguration, not a slow disk.
- **Network shares.** Fog's NAS mounts (NFS/CIFS) need the `mount` syscall,
  which an unprivileged container never gets — Fog says so on the share
  rather than failing with a bare "Operation not permitted". Three ways round
  it:
  - **Mount on the node, bind into the container** (keeps it unprivileged).
    Fog accepts a share it finds already mounted at its mount point:

    ```sh
    mkdir -p /mnt/nas/media
    echo 'nas.lan:/volume1/media /mnt/nas/media nfs defaults,_netdev 0 0' >> /etc/fstab
    mount /mnt/nas/media
    pct set <ctid> -mp0 /mnt/nas/media,mp=/mnt/nas/media && pct reboot <ctid>
    ```

    Inside an unprivileged container uid 1000 is uid 101000 on the wire, so
    a share that should be writable needs the NAS to squash writes to an
    owner that can write (Synology: "Map all users to admin").
  - **A VM instead** ([proxmox-vm.md](proxmox-vm.md)), where Fog mounts
    shares itself with no trade-off.
  - **`--privileged`**, and Fog mounts shares itself; the script then also
    sets `mount=nfs;cifs` on the container. It's a real security trade — a
    privileged container's root is the node's root.
- **Disks and SMART.** Fog reports the container's root filesystem, not the
  node's physical disks, and there is no SMART data to read. Disk health for
  this machine lives in the Proxmox UI, which is where it belongs.
- **The OS-level resilience from the USB image isn't here** — no boot-check,
  no bootloader rollback, no earlyoom, no console banner. Proxmox owns the
  boot path now. Its own equivalents are better: snapshot the container
  before a risky change (`pct snapshot <ctid> before-x`) and roll back with
  `pct rollback`.
- **Everything else is the same Fog.** Apps, compose materialization, the
  Caddy proxy on the container's own IP, backups, the recovery kit, and
  `fogd`'s self-update all work exactly as on metal.

## Afterwards

```sh
pct enter 210                    # a shell inside, from the node
pct set 210 --memory 8192        # more RAM; --cores likewise
pct resize 210 rootfs +32G       # a bigger disk, online
journalctl -u fogd -f            # inside the container
pct destroy 210                  # discard the whole server
```

Run the script again with a different `--ctid` and `--hostname` for a second
Fog server on the same node — federation between them works over the LAN like
any other pair.

## If something doesn't come up

- **"No source was given, so this used fog's public releases"** — the
  default, `https://github.com/fogapp/fog`, didn't answer from this node (no
  route to GitHub, or a proxy in the way). Fix the node's internet access, or
  pass `--source` (or `FOG_SOURCE`) with wherever else you get fog.
- **"this node can't resolve `gitea.lan`"** — with a source on your own
  network (a LAN Gitea, say), the
  node's DNS (often the router, or a public resolver set at install time)
  doesn't know the LAN name. Point the node at the resolver that does (node → System → DNS), or
  add `<ip> gitea.lan` to the node's `/etc/hosts`. Replacing the hostname with
  an IP in `--source` isn't enough: the release manifest names its downloads
  as `http://gitea.lan/...`, and the script checks those hosts too. Nothing
  is created when this fails.
- **"pinned it to … in its /etc/hosts"** — the node could resolve a source
  host but the container couldn't (a name in the node's `/etc/hosts`, or a
  `--dns` that doesn't know it), so the container was given the node's
  answer. If that host's address ever changes, edit the line or fogd's
  updates stop.
- **"no release manifest found under …"** — the node found no verified
  manifest at any layout it knows for that source. It lists what it tried
  and why each failed; `--manifest-url` names the manifest outright. Nothing
  is created when this fails.
- **"lists no install.sh"** — the release predates releases that carry
  `install.sh` in their manifest; use a newer release.
- **"past the 180-day limit"** — the newest release the source offers was
  signed more than 180 days ago ([hosting-the-source.md](hosting-the-source.md)).
- **"signature doesn't match fog's release key" / "unsigned"** — the
  manifest under `--source` wasn't signed by fog's release process: a stale
  or tampered mirror, or a fork's own release (which passes its key as
  `FOG_RELEASE_KEYS`). Nothing is created when this fails.
- **"the Docker engine won't start"** — check that the container really has
  `nesting=1` and `keyctl=1` (`pct config <ctid>`), then read
  `journalctl -u docker` inside it. An unprivileged container without keyctl
  is the usual cause.
- **fogd isn't answering on :8080** — `journalctl -u fogd` inside the
  container. The wizard binds every interface, so a reply on `127.0.0.1` but
  not the LAN IP is a firewall on the node or bridge, not Fog.
- **`fog.local` doesn't resolve** — mDNS doesn't cross VLANs or VPNs, and
  a second Fog on the LAN takes `fog-2.local`. The container's IP always
  works; the script prints it.
