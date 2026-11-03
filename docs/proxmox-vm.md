# Fog as a Proxmox VM

`proxmox-vm.sh` builds a new Fog server as a VM on a Proxmox VE node:
Debian 13's official cloud image as its disk, Docker, `fogd`, and the
first-run wizard waiting on port 8080. Nothing to build or flash — about five
minutes, most of it the VM installing packages.

## VM or container?

| | VM (`proxmox-vm.sh`) | LXC ([`proxmox-lxc.sh`](proxmox-lxc.md)) |
|---|---|---|
| NAS shares (NFS/SMB) | Fog mounts them itself, from Settings › Storage › Disks & shares | Only bound in from the node, or with a privileged container |
| Docker storage | `overlay2` on any Proxmox storage | `fuse-overlayfs` on ZFS/Ceph |
| Behaves like metal | Yes — its own kernel | Shares the node's kernel |
| Memory | Reserved for the VM | Shared with the node |
| Boot | ~30 s | ~2 s |

Choose the VM when Fog should manage network shares or behave exactly as on
metal; the container when the node is short on memory and shares aren't
needed. For testing the bootable installer image itself, see
[proxmox-testing.md](proxmox-testing.md).

## Run it

In the Proxmox web UI, open the node → **Shell**, and paste:

```sh
bash -c "$(curl -fsSL https://raw.githubusercontent.com/fogapp/fog/main/proxmox-vm.sh)"
```

You get a menu: **default settings** (VM at the next free id, `fog`, 2 cores,
4 GB RAM, 32 GB disk, DHCP) or **advanced**, which asks for every value. Then
it downloads the Debian image (once — later runs reuse it), creates the VM,
watches the install inside it, and prints the dashboard URL.

Nothing outside the new VM is touched, apart from the cached image in
`/var/cache/fog-proxmox-vm`. If a step fails, the VM is left in place with the
failure shown — `qm terminal <vmid>` to look, `qm destroy <vmid> --purge` to
discard.

## Parameters

Every value has a flag and an environment variable. With a terminal you're
asked; without one the defaults and environment are used. `--help` prints
this table.

| Flag | Env var | Default |
|---|---|---|
| `--vmid N` | `FOG_VMID` | next free id |
| `--hostname NAME` | `FOG_HOSTNAME` | `fog` — also `http://<name>.local` |
| `--root-password PW` | `FOG_ROOT_PASSWORD` | none; root can't log in on the console |
| `--disk GB` | `FOG_DISK` | `32` |
| `--cores N` | `FOG_CORES` | `2` |
| `--ram MB` | `FOG_RAM` | `4096` |
| `--cpu TYPE` | `FOG_CPU` | `host` |
| `--storage NAME` | `FOG_STORAGE` | asked, or the only `images` storage |
| `--iso-storage NAME` | `FOG_ISO_STORAGE` | asked, or the only `iso` storage |
| `--image-url URL` | `FOG_IMAGE_URL` | Debian 13 `generic` amd64 cloud image |
| `--no-onboot` | `FOG_ONBOOT=0` | starts at boot |
| `--tags A;B` | `FOG_TAGS` | `fog` |
| `--bridge NAME` | `FOG_BRIDGE` | `vmbr0` |
| `--ipv4 CIDR\|dhcp` | `FOG_IPV4` | `dhcp` |
| `--gateway IP` | `FOG_GATEWAY` | required with a static IPv4 |
| `--ipv6 MODE\|CIDR` | `FOG_IPV6` | `none` |
| `--vlan TAG` | `FOG_VLAN` | untagged |
| `--mtu N` | `FOG_MTU` | bridge default |
| `--dns IP` | `FOG_DNS` | DHCP's; with a static IPv4, the node's |
| `--searchdomain DOM` | `FOG_SEARCHDOMAIN` | none |
| `--ssh-keys FILE` | `FOG_SSH_KEYS` | none |
| `--port N` | `FOG_PORT` | `8080` |
| `--source URL` | `FOG_SOURCE` | `https://github.com/fogapp/fog` |
| `--branch NAME` | `FOG_BRANCH` | `main` |
| `--manifest-url URL` | `FOG_MANIFEST_URL` | resolved from `--source` |
| `--raw-base URL` | `FOG_RAW_BASE` | resolved from `--source` (passed to `install.sh`; every installed file comes from the manifest) |
| `--timezone TZ` | `FOG_TZ` | the node's |
| `--admin-user NAME` | `FOG_ADMIN_USER` | the web wizard creates it |
| `--admin-password PW` | `FOG_ADMIN_PASSWORD` | the web wizard creates it |
| `--server-name NAME` | `FOG_SERVER_NAME` | the hostname |
| `--base-domain DOMAIN` | `FOG_BASE_DOMAIN` | none |
| `--yes` | `FOG_YES=1` | asks to confirm |
| `--dry-run` | | prints the `qm create` command and stops |

`--cpu host` passes every feature of the node's CPU through — some apps'
databases need AVX. In a cluster whose nodes have different CPUs, pick a
common baseline such as `x86-64-v3` so the VM can migrate.

## How it works

1. **Before anything is created**, the node resolves `--source` to a release
   manifest and verifies it — its signature against fog's release key, its
   age — with the same resolver `install.sh` carries, then takes `install.sh`
   from the manifest and checks it against its sha256. Nothing fetched is
   ever run on the node (`--dry-run` included); only the verified
   `install.sh` goes into the VM. It also checks that every host the release
   names resolves and the fogd binary downloads.
   The manifest names its downloads by absolute URL (`http://gitea.lan/...`),
   so a source given as an IP doesn't route around a name the node can't
   resolve.
2. **The image** is Debian 13's `generic` cloud image — the full kernel, so
   NFS and CIFS work — checked against Debian's published SHA512 sums and
   imported as the VM's disk, then grown to `--disk`.
3. **A cloud-init seed** — a small ISO labelled `cidata` on the `iso`
   storage — carries the network config (matched to the NIC by MAC),
   `install.sh`, and the setup script. The script builds it rather than using
   Proxmox's cloud-init drive, so no storage needs `snippets` enabled, and it
   is ejected and deleted as soon as the install finishes or fails: it holds
   any passwords you passed.
4. **On first boot** the VM installs the QEMU guest agent, which is how the
   node watches the rest: base packages, Docker CE (same packages and log
   caps as the USB image, Docker's apt key checked against its published
   fingerprint), then `install.sh` — the same script an existing Linux
   server runs, which checks the manifest's signature and every file's
   sha256 before it installs anything. Any source host the VM's DNS doesn't know gets the
   node's answer as a line in the VM's `/etc/hosts`, and the script says so.
5. **cloud-init is then disabled** in the VM, so the network config it wrote
   stays exactly as written on every later boot.

## Afterwards

```sh
qm terminal 210                  # the serial console (Ctrl+O to leave)
qm guest exec 210 -- journalctl -u fogd -n 50   # a command inside, no login needed
qm set 210 --memory 8192         # more RAM (applies at the next boot)
qm destroy 210 --purge           # discard the whole server
```

A bigger disk is two steps, because cloud-init — which grew the root
partition on first boot — is disabled afterwards:

```sh
qm disk resize 210 scsi0 +32G
qm guest exec 210 -- growpart /dev/sda 1
qm guest exec 210 -- resize2fs /dev/sda1
```

Snapshots (`qm snapshot 210 before-x`, `qm rollback`) cover the whole VM —
take one before a risky change.

## If something doesn't come up

- **"No source was given, so this used fog's public releases"** — the
  default, `https://github.com/fogapp/fog`, didn't answer from this node (no
  route to GitHub, or a proxy in the way). Fix the node's internet access, or
  pass `--source` (or `FOG_SOURCE`) with wherever else you get fog.
- **"this node can't resolve `gitea.lan`"** — with a source on your own
  network (a LAN Gitea, say), the
  node's DNS doesn't know the LAN name. Point the node at the resolver that
  does (node → System → DNS), or add `<ip> gitea.lan` to the node's
  `/etc/hosts`. Nothing is created when this fails.
- **"signature doesn't match fog's release key" / "unsigned"** — the
  manifest under `--source` wasn't signed by fog's release process: a stale
  or tampered mirror, or a fork's own release (which passes its key as
  `FOG_RELEASE_KEYS`). Nothing is created when this fails.
- **"no release manifest found under …"** — no verified manifest at any
  layout the node knows for that source; it lists what it tried and why
  each failed. `--manifest-url` names the manifest outright.
- **"past the 180-day limit"** — the newest release the source offers was
  signed more than 180 days ago ([hosting-the-source.md](hosting-the-source.md)).
- **"the guest agent never answered"** — cloud-init didn't get far enough
  to install it, usually because the VM has no network or no route to
  `deb.debian.org`. `qm terminal <vmid>` shows cloud-init's progress; check
  the bridge, VLAN, and DHCP.
- **"the install inside the VM failed"** — the end of the VM's
  `/var/log/fog-setup.log` is printed. The VM is left running so you can
  look: `qm guest exec <vmid> -- cat /var/log/fog-setup.log`.
- **`fog.local` doesn't resolve** — mDNS doesn't cross VLANs or VPNs; use
  the IP the script printed.
