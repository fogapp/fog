# From this repo to a running Fog server

The complete path, start to finish: build the installer image from the code
in this repo, flash it, boot a machine from it, and end up signed in to the
dashboard of a new Fog server. Allow about an hour, most of it unattended.

For background on how any of this works, see [installer.md](installer.md)
(operator guide) and [imaging/README.md](../imaging/README.md) (design and
image pipeline internals).

## What you need

| | Requirement |
|---|---|
| **Build machine** | This repo, plus Go 1.26+, Node 20+, `make`, and Docker running (any host — on Apple Silicon the build runs under emulation, it's just slower). ~10 GB free disk. |
| **USB stick** | 4 GB or larger. Everything on it will be erased. |
| **Target box** | x86-64 with UEFI (NUC-class mini-PC, old desktop…), at least 24 GB internal disk, at least 2 GB RAM. Everything on its disk will be erased. |
| **Network** | An Ethernet cable from the target box to your router. |

## 1. Build the installer image

```sh
make installer-image
```

This cross-compiles `fogd`, `fog-setup`, and `fog-console` for linux/amd64,
builds both frontends, then runs the three imaging stages inside a
`debian:trixie` container: the target OS rootfs (squashfs), the live
installer environment, and the final image assembly.

- First run: ~30–40 minutes on Apple Silicon (package downloads + emulation);
  reruns are much faster thanks to the `fog-imaging-apt*` cache volumes.
- Result: `imaging/out/fog-installer-x86_64.img` (~2 GB) plus its `.sha256`.

## 2. (Recommended) Run the release gate

```sh
make installer-e2e
```

Boots the image you just built in QEMU, installs onto a virtual disk through
the real wizard API, reboots into the installed disk, asserts the dashboard
answers signed-in, then boots the stick once more to check it recognizes the
server it just built. Needs `qemu` (`brew install qemu` on macOS). Slow
without KVM (~25 minutes on a Mac) — but a `PASS — installed, booted, claimed,
signed in, and safe to boot again` at the end means the exact bytes on your
stick have already installed and booted a machine today.

## 3. Flash the USB stick

Any of these; they're equivalent:

- **balenaEtcher** (simplest, all platforms): select
  `imaging/out/fog-installer-x86_64.img`, select the stick, Flash.
- **macOS terminal**:
  ```sh
  diskutil list                     # find the stick, e.g. /dev/disk4
  diskutil unmountDisk /dev/disk4
  sudo dd if=imaging/out/fog-installer-x86_64.img of=/dev/rdisk4 bs=4m
  diskutil eject /dev/disk4
  ```
- **Linux terminal**: `sudo dd if=... of=/dev/sdX bs=4M status=progress conv=fsync`

Triple-check the device name — `dd` does not ask twice.

## 4. Boot the target box from the stick

1. Plug in the stick and the Ethernet cable; connect a monitor if you have
   one (optional — see step 5).
2. Power on and get into the firmware boot menu — usually **F12** (Dell,
   Lenovo), **F10** (HP), **F8/F11** (ASUS/MSI/ASRock), or **Esc**. Pick the
   USB entry (often listed as "UEFI: <stick name>").
3. **If it refuses to boot the stick, disable Secure Boot** in firmware
   setup — the image's bootloader is unsigned. (Legacy/CSM-only machines
   won't boot it at all; the installer is UEFI-only.)

Within a minute or two you'll see the Fog boot splash, then the setup
wizard.

## 5. Run the setup wizard

Two equivalent ways to drive it — they're the same wizard, live-synced:

- **On the attached display**, if there is one.
- **From your phone or laptop**: open `http://fog.local`. (If your network
  doesn't resolve `.local` names, find the box's IP in your router's client
  list and open `http://<ip>`.)

The wizard walks through:

1. **Welcome** — "Set up a new Fog server" (or "Restore from a backup" if
   you have a recovery kit from a previous server; it verifies the kit
   before anything is erased, and can optionally take the old admin
   password to restore console/SSH access too).
2. **Disk** — the internal disk is auto-picked and described honestly; tick
   the explicit erase confirmation. (The stick itself is never offered.) A
   banner shows the network state — plugging in a cable is noticed
   automatically, and Continue waits for a connection.
3. **Your server** — server name (default `fog` — this becomes
   `http://<name>.local`), your admin username and password (10+ chars),
   timezone auto-detected.
4. **Review → Erase disk and install Fog** — 2–4 minutes of progress. The
   payload is checksummed before the disk is touched; the install is fully
   offline, nothing is downloaded.
5. **Restart into Fog** — click it, and remove the stick when the screen
   goes dark. Keep the stick: it's also your recovery tool.

## 6. Land on the dashboard

If you drove the wizard from a browser, just wait: the page watches for the
new server and then takes you straight to the dashboard, **already signed
in** (fresh installs use a one-shot handoff token; restores land on the
restored server's own login).

Otherwise, open `http://<name>.local:8080` from any browser on the LAN and
sign in with the account you created. A monitor plugged into the server
shows the exact URLs and Fog's health at any time, above a normal Debian
login prompt — the account you just created logs in there for a bash shell
(`sudo` and `docker` included), same as over SSH.

## 7. First minutes on a new server

- **Install your first app** from the catalog on the dashboard.
- **Set a recovery-kit passphrase and a nightly backup hour** (Settings →
  Storage → Recovery kit, and Settings → Storage → Backups; the hour is in
  UTC) — without the kit, losing the machine loses every app's encrypted
  credentials. See [disaster-recovery.md](disaster-recovery.md).
- **SSH works too**: `ssh <your-username>@<name>.local` with the same
  password you set in the wizard (the account is in `sudo` and `docker`).

## If something goes wrong

- **Install failed in the wizard** — the error screen offers a retry and a
  diagnostics download; a copy of the install log is also written to the
  stick's `FOG-DATA` partition, readable on any computer.
- **The installed box misbehaves later** — hard-restart it; the system is
  built to recover itself in stages (see "When something goes wrong" in
  [installer.md](installer.md)). If it can't, it boots **Fog Recovery**
  from its own disk — repair menu on the display and at `http://fog.local`.
- **`fog.local` doesn't resolve** — some networks/browsers don't do mDNS;
  use the IP from your router's client list or the on-screen console panel.
