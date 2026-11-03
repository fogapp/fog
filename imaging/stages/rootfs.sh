#!/bin/bash
# Stage: the target system — the Debian that actually gets installed onto
# the machine. Complete and preconfigured at build time so installation is
# just an extraction: no mirrors, no package resolution, no surprises.
. "$(dirname "$0")/lib.sh"

ROOT=/build/rootfs
mkdir -p /build

prep_host debootstrap squashfs-tools ca-certificates curl gnupg

bootstrap "$ROOT" systemd,systemd-sysv,systemd-resolved,udev,dbus,kmod,ca-certificates

install_pkgs "$ROOT" \
  $KERNEL_PKGS \
  intel-microcode amd64-microcode \
  openssh-server sudo curl git jq \
  avahi-daemon libnss-mdns chrony earlyoom smartmontools \
  e2fsprogs dosfstools util-linux iproute2 iputils-ping nftables \
  less nano systemd-boot efibootmgr \
  ffmpeg

# Docker from download.docker.com: engine + compose + buildx, which fogd's
# app model (compose apps, git builds) depends on. FOG_DOCKER_VERSION pins
# the engine/cli apt version (e.g. "5:27.5.1-1~debian.13~trixie") for a
# reproducible build; unset, the current stable is baked and the manifest
# below records exactly which.
install -d -m 0755 "$ROOT/etc/apt/keyrings"
curl -fsSL --proto '=https' https://download.docker.com/linux/debian/gpg -o "$ROOT/etc/apt/keyrings/docker.asc"
# Docker's published signing key, by fingerprint — the same pin the Proxmox
# scripts hold: the key comes from the same host as the packages, so this is
# what stops a compromise of that host alone from choosing the image's key.
DOCKER_FPR=9DC858229FC7DD38854AE2D88D81803C0EBFCD88
GOT_FPR=$(gpg --show-keys --with-colons "$ROOT/etc/apt/keyrings/docker.asc" | awk -F: '$1=="fpr" {print $10; exit}')
[ "$GOT_FPR" = "$DOCKER_FPR" ] || { echo "rootfs: Docker's apt key has fingerprint '$GOT_FPR', not $DOCKER_FPR" >&2; exit 1; }
echo "deb [signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/debian $SUITE stable" \
  > "$ROOT/etc/apt/sources.list.d/docker.list"
DOCKER_PKGS=(docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin)
if [ -n "${FOG_DOCKER_VERSION:-}" ]; then
  DOCKER_PKGS=("docker-ce=$FOG_DOCKER_VERSION" "docker-ce-cli=$FOG_DOCKER_VERSION" containerd.io docker-buildx-plugin docker-compose-plugin)
fi
install_pkgs "$ROOT" "${DOCKER_PKGS[@]}"

# Fog itself.
install -m 0755 "$SRC/bin/fogd-linux-amd64" "$ROOT/usr/local/bin/fogd"
# The self-update rollback guard fogd.service's ExecStartPre runs (fogd also
# rewrites it beside itself on every update; this is the first copy).
install -m 0755 "$SRC/packaging/fogd-rollback" "$ROOT/usr/local/bin/fogd-rollback"
install -m 0755 "$SRC/bin/fog-console-linux-amd64" "$ROOT/usr/local/bin/fog-console"
install -m 0644 "$SRC/packaging/fogd.service" "$ROOT/etc/systemd/system/fogd.service"
install -m 0644 "$SRC/packaging/fogd.socket" "$ROOT/etc/systemd/system/fogd.socket"

# Baked-in configuration: watchdog, log caps, OOM policy, mDNS. Everything
# from the resilience design that belongs to the OS, not the installer.
cp -a "$SRC/imaging/overlay/target/." "$ROOT/"

enable_unit "$ROOT" fogd.service
# The socket holds fogd's port across every restart (packaging/fogd.socket).
enable_unit "$ROOT" fogd.socket sockets.target
enable_unit "$ROOT" fog-console.service
enable_unit "$ROOT" fog-sshkeys.service
enable_unit_in "$ROOT" fog-boot-check.service boot-complete.target.requires
# The machine's own display is a normal Debian console: a login prompt on
# tty1 with fog-console's banner above it. Enabled explicitly because the
# console is part of the product, not a leftover of the base system.
run_chroot "$ROOT" systemctl enable getty@tty1.service >/dev/null
run_chroot "$ROOT" systemctl enable ssh avahi-daemon chrony earlyoom smartmontools docker systemd-networkd systemd-resolved systemd-bless-boot >/dev/null

# Root logs in nowhere; the installer creates the admin unix account.
run_chroot "$ROOT" passwd -l root >/dev/null

# Host keys must never be baked: identical keys on every install would be a
# fleet-wide MITM hazard. fog-sshkeys.service regenerates per machine.
rm -f "$ROOT"/etc/ssh/ssh_host_*

# Record exactly what this image contains: two builds of the same commit
# can differ (unpinned Docker, mirror drift), and this is how you tell.
run_chroot "$ROOT" dpkg-query -W -f '${Package} ${Version}\n' \
  > "$ROOT/etc/fog-rootfs-packages" 2>/dev/null || true
cp "$ROOT/etc/fog-rootfs-packages" "$OUT/fog-rootfs-packages.txt" 2>/dev/null || true

finish_rootfs "$ROOT"
squash "$ROOT" "$OUT/fog-rootfs.squashfs"
echo "rootfs: $(du -h "$OUT/fog-rootfs.squashfs" | cut -f1)"
