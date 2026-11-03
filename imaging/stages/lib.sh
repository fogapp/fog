#!/bin/bash
# Shared helpers for the imaging stages. Every stage runs as root inside a
# linux/amd64 debian:trixie container with the repo mounted at /src.
set -euo pipefail

SRC=/src
OUT=$SRC/imaging/out
MIRROR=${FOG_DEBIAN_MIRROR:-http://deb.debian.org/debian}
SUITE=trixie
# One kernel+firmware set for both the live environment and the target OS —
# hardware that can boot the installer must boot the installed system too,
# so a firmware addition for new hardware is made exactly once.
KERNEL_PKGS="linux-image-amd64 firmware-linux-free firmware-realtek firmware-iwlwifi"
mkdir -p "$OUT"

prep_host() {
  # Keep downloaded .debs in the cache volume across runs.
  rm -f /etc/apt/apt.conf.d/docker-clean
  apt-get update -q
  apt-get install -qy --no-install-recommends "$@"
}

# run_chroot <rootdir> <command...>
run_chroot() {
  local root=$1; shift
  chroot "$root" /usr/bin/env DEBIAN_FRONTEND=noninteractive PATH=/usr/sbin:/usr/bin:/sbin:/bin "$@"
}

# bootstrap <rootdir> <extra-packages-csv>
bootstrap() {
  local root=$1 include=$2
  debootstrap --variant=minbase --components=main,contrib,non-free-firmware \
    --include="$include" "$SUITE" "$root" "$MIRROR"
  # Divert service starts inside the chroot — packages must install, not run.
  cat > "$root/usr/sbin/policy-rc.d" <<'PEOF'
#!/bin/sh
exit 101
PEOF
  chmod +x "$root/usr/sbin/policy-rc.d"
  echo "deb $MIRROR $SUITE main contrib non-free-firmware" > "$root/etc/apt/sources.list"
  # "$MIRROR-security" only forms a real URL for the default mirror layout;
  # a custom FOG_DEBIAN_MIRROR would 404 apt-get update in install_pkgs, so
  # it gets just the main line.
  if [ "$MIRROR" = "http://deb.debian.org/debian" ]; then
    echo "deb $MIRROR-security $SUITE-security main contrib non-free-firmware" >> "$root/etc/apt/sources.list"
  fi
}

# install_pkgs <rootdir> <packages...>
install_pkgs() {
  local root=$1; shift
  # Docker's default seccomp profile blocks mount(2) in this unprivileged
  # container, so the chroot normally runs without /proc — today's package
  # set installs fine that way. Say so instead of hiding it: if a future
  # package's postinst needs /proc, the cause is in the log, not a mystery.
  if ! mount -t proc proc "$root/proc" 2>/dev/null; then
    echo "imaging: note — /proc not mounted in the chroot (unprivileged container); a postinst failure below may mean a package now needs it"
  fi
  cp /etc/resolv.conf "$root/etc/resolv.conf"
  run_chroot "$root" apt-get update -q
  run_chroot "$root" apt-get install -qy --no-install-recommends "$@"
  umount "$root/proc" 2>/dev/null || true
}

# finish_rootfs <rootdir> — trims what an image never needs.
finish_rootfs() {
  local root=$1
  run_chroot "$root" apt-get clean
  rm -rf "$root"/var/lib/apt/lists/* "$root"/var/cache/apt/archives/*.deb
  rm -f "$root/usr/sbin/policy-rc.d" "$root/etc/resolv.conf"
  # A fresh machine identity on every boot/install.
  : > "$root/etc/machine-id"
  # dbus's copy is the one systemd reads *first*: machine_id_setup() adopts
  # /var/lib/dbus/machine-id whenever it holds a value, so dbus's postinst
  # writing a real file here at build time silently clones one identity onto
  # every machine built from this image — blanking /etc/machine-id alone does
  # nothing. That is not cosmetic: systemd-networkd defaults to
  # ClientIdentifier=duid with DUIDType=vendor, which hashes the machine ID
  # into the DHCP client identifier, so two Fog servers ask the router as the
  # same client and are handed the same lease. A symlink is both Debian's
  # intended layout and the shape systemd skips when hunting for an identity
  # to adopt.
  ln -sf /etc/machine-id "$root/var/lib/dbus/machine-id"
  assert_no_baked_identity "$root"
}

# assert_no_baked_identity <rootdir> — fails the build if anything that must
# be unique per machine survived into the image.
#
# Every one of these is written by a package postinst as a side effect of
# being installed in the build chroot, which means the next package added to
# a stage can reintroduce one without anybody touching this file. The failure
# mode is invisible on a single machine and only appears once two Fog servers
# exist on one LAN, so it is worth a hard gate at build time.
assert_no_baked_identity() {
  local root=$1 bad=()
  [ -s "$root/etc/machine-id" ] && bad+=("/etc/machine-id is not empty")
  [ -f "$root/var/lib/dbus/machine-id" ] && [ ! -L "$root/var/lib/dbus/machine-id" ] &&
    bad+=("/var/lib/dbus/machine-id is a real file (must be a symlink to /etc/machine-id)")
  compgen -G "$root/etc/ssh/ssh_host_*" >/dev/null && bad+=("SSH host keys are baked in")
  [ -e "$root/var/lib/systemd/random-seed" ] && bad+=("/var/lib/systemd/random-seed is baked in")
  if [ ${#bad[@]} -gt 0 ]; then
    printf 'imaging: per-machine identity leaked into the image:\n' >&2
    printf '  - %s\n' "${bad[@]}" >&2
    return 1
  fi
}

# enable_unit <rootdir> <unit> [wantedby]
enable_unit() {
  local root=$1 unit=$2 target=${3:-multi-user.target}
  enable_unit_in "$root" "$unit" "$target.wants"
}

# enable_unit_in <rootdir> <unit> <dependency-dir> — e.g. foo.target.requires
enable_unit_in() {
  local root=$1 unit=$2 dir=$3
  mkdir -p "$root/etc/systemd/system/$dir"
  ln -sf "/etc/systemd/system/$unit" "$root/etc/systemd/system/$dir/$unit"
}

# squash <rootdir> <outfile>
squash() {
  mksquashfs "$1" "$2" -noappend -comp zstd -quiet
  (cd "$(dirname "$2")" && sha256sum "$(basename "$2")" > "$2.sha256")
}
