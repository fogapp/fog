#!/bin/bash
# Stage: the live installer environment — what the USB stick boots. Minimal
# Debian with live-boot (squashfs + tmpfs overlay), the fog-setup agent on
# :80, a cage+chromium kiosk when a display is attached, and avahi answering
# fog.local either way.
. "$(dirname "$0")/lib.sh"

ROOT=/build/live
mkdir -p /build

prep_host debootstrap squashfs-tools

bootstrap "$ROOT" systemd,systemd-sysv,systemd-resolved,udev,dbus,kmod,ca-certificates

install_pkgs "$ROOT" \
  $KERNEL_PKGS \
  live-boot \
  avahi-daemon libnss-mdns iproute2 \
  util-linux fdisk e2fsprogs dosfstools squashfs-tools efibootmgr \
  cage chromium fonts-inter seatd \
  less nano

# The installer agent and its wizard.
install -m 0755 "$SRC/bin/fog-setup-linux-amd64" "$ROOT/usr/local/bin/fog-setup"

# Kiosk runs unprivileged; chromium won't (and shouldn't) run as root.
run_chroot "$ROOT" useradd -m -s /usr/sbin/nologin -G video,render,input kiosk

cp -a "$SRC/imaging/overlay/live/." "$ROOT/"

echo fog > "$ROOT/etc/hostname"
printf '127.0.0.1\tlocalhost\n127.0.1.1\tfog\n' > "$ROOT/etc/hosts"

enable_unit "$ROOT" fog-setup.service
enable_unit "$ROOT" fog-bootdisk.service
enable_unit "$ROOT" fog-kiosk.service graphical.target
# A shell on tty1 for whoever is standing at the machine (see the autologin
# drop-in): the kiosk covers the screen where there's a GPU, this is what a
# headless or unhappy install has.
run_chroot "$ROOT" systemctl enable getty@tty1.service >/dev/null
run_chroot "$ROOT" systemctl enable avahi-daemon systemd-networkd systemd-resolved seatd >/dev/null
run_chroot "$ROOT" systemctl set-default graphical.target >/dev/null

# live-boot rebuilds the initramfs with squashfs-mounting support.
run_chroot "$ROOT" update-initramfs -u

# Kernel + initrd boot from the ESP; keep copies out of the squashfs.
cp "$ROOT"/boot/vmlinuz-* "$OUT/live-vmlinuz"
cp "$ROOT"/boot/initrd.img-* "$OUT/live-initrd"

finish_rootfs "$ROOT"
squash "$ROOT" "$OUT/live.squashfs"
echo "live: $(du -h "$OUT/live.squashfs" | cut -f1)"
