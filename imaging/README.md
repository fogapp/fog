# Fog installer imaging

Builds `fog-installer-x86_64.img`: flash it to a USB stick (balenaEtcher, dd),
boot any UEFI x86_64 machine from it, and the Fog setup wizard takes it from
there — on the machine's own display and at http://fog.local from any browser
on the LAN.

## How the image works

The core reliability decision: **installation never runs a package manager.**
At build time this pipeline produces a complete, known-good Debian system as
a checksummed squashfs; installing is partitioning a disk and extracting that
filesystem. The same bytes land on every machine, offline, in minutes.

Three stages, all runnable unprivileged inside a stock Debian container:

- `stages/rootfs.sh` — debootstraps the **target system** (Debian trixie +
  Docker + fogd + fog-console + avahi/chrony/earlyoom/smartmontools, watchdog
  and log caps preconfigured) → `out/fog-rootfs.squashfs` + sha256. tty1 is a
  plain login prompt; `fog-console` only keeps the `/etc/issue` banner above
  it current (name, URLs, health).
- `stages/live.sh` — debootstraps the **live installer environment**
  (live-boot initramfs, fog-setup agent on :80, cage+chromium kiosk, avahi
  announcing fog.local, an autologin root shell on tty1) →
  `out/live.squashfs` + its kernel/initrd.
- `stages/image.sh` — assembles the hybrid GPT/UEFI image *without loop
  devices or root*: mkfs onto bare files (mtools for the ESP, `mke2fs -d`
  for the payload), then dd them into place.
  - p1 ESP (FAT32): systemd-boot + live kernel/initrd
  - p2 payload (ext4): `/live/filesystem.squashfs` (installer),
    `/fog/rootfs.squashfs` (the OS that gets installed), checksums
  - p3 `FOG-DATA` (FAT32): diagnostics exports land here, readable anywhere

The wizard is one SPA served by one agent, on the attached display and at
http://fog.local alike, so both stay in sync. A fresh install hands off
without a second login: the installer stages `/var/lib/fog/firstboot.json`
(the account already hashed, plus a one-shot claim-token digest), fogd
consumes it on first boot, and the wizard page navigates to
`/api/setup/claim?token=…` once the installed server answers.

The installed disk's layout, and how the machine recovers itself, are in
[docs/installer.md](../docs/installer.md).

## Building

    make installer-image          # everything: binaries → stages → .img

Requires Docker (any host — stages run in a linux/amd64 debian:trixie
container; on Apple Silicon that's emulated and slow but correct) with the
Go binaries prebuilt by `make build-linux`. Artifacts land in `imaging/out/`.
Apt cache volumes (`fog-imaging-apt`, and `-rootfs`/`-live` beside it for the
two stages that run concurrently) make rebuilds much faster.

## Flashing

`flash.sh` turns any machine with a shell into the flashing station — it
reads a release manifest (`FOG_MANIFEST_URL`; the default is fog's public
releases on GitHub), checks its signature and age (180 days at most) against fog's
release key, checks the image against the manifest's sha256 after download
and again on the bytes as they are written, writes it to a USB stick you
confirm, and on
a Linux target with EFI even arms a one-shot boot into the installer. A
release directory's relative manifest names the image beside itself, and
flash.sh resolves it there. `FOG_IMAGE_URL` skips the manifest for an image
you vouch for (its `.sha256` is fetched beside it). The signature check
needs an openssl with ed25519 — on macOS, Homebrew's `openssl@3`, since the
system LibreSSL has none.

    curl -fsSL https://github.com/fogapp/fog/releases/latest/download/flash.sh | sh

flashes fog's latest public release; from anywhere else,
`curl -fsSL <release>/flash.sh | FOG_MANIFEST_URL=<release>/latest.json sh`
(`<release>` is a release directory or a GitHub release's download URL —
`release.sh` ships `flash.sh` there beside the image).

The image's target system carries `fogd.service` and `fogd-rollback` from
`packaging/`, so a server installed from USB rolls back a failed
self-update exactly as one installed by `install.sh` does
([docs/installer.md](../docs/installer.md)).

## Testing

    make installer-e2e            # boot the image in QEMU, install, reboot,
                                  # assert the dashboard answers signed-in,
                                  # then boot the stick again beside that disk

This is the release gate: `FOG_RELEASE_IMAGE=1 ./scripts/release.sh vX.Y.Z`
runs it before an image is published.

The wizard itself runs without hardware against a mock agent:

    make dev-setup                # the mock agent, in one terminal
    make dev-setup-frontend       # the wizard, in another

`FOG_SETUP_REPAIR=1` opens the recovery menu instead, and
`FOG_SETUP_MOCK_INSTALLED=1` the "this machine is already a Fog server"
screen.

## Not yet here

- Wi-Fi support in the live environment (Ethernet is the happy path)
- Fault injection in the e2e gate (power cut mid-write, corrupted root)
