#!/bin/bash
# Stage: assemble the flashable image. Deliberately unprivileged: no loop
# devices, no mounts — filesystems are built onto bare files (mtools for
# FAT, mke2fs -d for ext4) and dd'd into a partitioned image.
. "$(dirname "$0")/lib.sh"

prep_host dosfstools mtools e2fsprogs util-linux fdisk systemd-boot-efi

for f in live.squashfs live-vmlinuz live-initrd fog-rootfs.squashfs; do
  [ -f "$OUT/$f" ] || { echo "image: $OUT/$f missing — run earlier stages" >&2; exit 1; }
done
# The squashfs inputs must match the checksums their stages recorded — a
# stale or corrupted out/ artifact fails here, not on someone's machine.
for f in live.squashfs fog-rootfs.squashfs; do
  (cd "$OUT" && sha256sum -c "$f.sha256" >/dev/null) \
    || { echo "image: $f does not match its build checksum — rerun its stage" >&2; exit 1; }
done

WORK=/build/image
rm -rf "$WORK"; mkdir -p "$WORK"

# ---- p1: ESP with systemd-boot and the live kernel ----
ESP=$WORK/esp.img
ESP_MB=512
truncate -s ${ESP_MB}M "$ESP"
mkfs.vfat -F 32 -n FOG-BOOT "$ESP" >/dev/null
mmd -i "$ESP" ::/EFI ::/EFI/BOOT ::/loader ::/loader/entries
mcopy -i "$ESP" /usr/lib/systemd/boot/efi/systemd-bootx64.efi ::/EFI/BOOT/BOOTX64.EFI
mcopy -i "$ESP" "$OUT/live-vmlinuz" ::/vmlinuz
mcopy -i "$ESP" "$OUT/live-initrd" ::/initrd.img
cat > "$WORK/loader.conf" <<'LEOF'
default fog-installer.conf
timeout 0
editor no
LEOF
cat > "$WORK/fog-installer.conf" <<'LEOF'
title Fog Installer
linux /vmlinuz
initrd /initrd.img
options boot=live quiet loglevel=3 systemd.show_status=false
LEOF
mcopy -i "$ESP" "$WORK/loader.conf" ::/loader/loader.conf
mcopy -i "$ESP" "$WORK/fog-installer.conf" ::/loader/entries/fog-installer.conf

# ---- p2: payload — live squashfs where live-boot looks, OS payload beside ----
TREE=$WORK/payload
mkdir -p "$TREE/live" "$TREE/fog"
cp "$OUT/live.squashfs" "$TREE/live/filesystem.squashfs"
cp "$OUT/fog-rootfs.squashfs" "$TREE/fog/rootfs.squashfs"
# The checksum must name the file as it exists ON THE MEDIUM — the install
# engine verifies it in place before extracting.
(cd "$TREE/fog" && sha256sum rootfs.squashfs > rootfs.squashfs.sha256)
PAYLOAD=$WORK/payload.img
# ext4 needs headroom over the tree size.
PAYLOAD_MB=$(( $(du -sm "$TREE" | cut -f1) * 110 / 100 + 64 ))
truncate -s ${PAYLOAD_MB}M "$PAYLOAD"
mke2fs -q -t ext4 -L FOG-INSTALL -d "$TREE" "$PAYLOAD"

# ---- p3: FOG-DATA — diagnostics exports, readable on any computer ----
DATA=$WORK/data.img
DATA_MB=256
truncate -s ${DATA_MB}M "$DATA"
mkfs.vfat -F 32 -n FOG-DATA "$DATA" >/dev/null

# ---- assemble GPT ----
IMG=$OUT/fog-installer-x86_64.img
ALIGN_MB=1
P1_START=$ALIGN_MB
P2_START=$(( P1_START + ESP_MB ))
P3_START=$(( P2_START + PAYLOAD_MB ))
TOTAL_MB=$(( P3_START + DATA_MB + 1 ))
rm -f "$IMG"
truncate -s ${TOTAL_MB}M "$IMG"
sfdisk -q "$IMG" <<SEOF
label: gpt
start=${P1_START}MiB, size=${ESP_MB}MiB, type=uefi, name="fog-boot"
start=${P2_START}MiB, size=${PAYLOAD_MB}MiB, type=linux, name="fog-install"
start=${P3_START}MiB, size=${DATA_MB}MiB, type=EBD0A0A2-B9E5-4433-87C0-68B6B72699C7, name="fog-data"
SEOF
dd if="$ESP"     of="$IMG" bs=1M seek=$P1_START conv=notrunc status=none
dd if="$PAYLOAD" of="$IMG" bs=1M seek=$P2_START conv=notrunc status=none
dd if="$DATA"    of="$IMG" bs=1M seek=$P3_START conv=notrunc status=none

(cd "$OUT" && sha256sum "$(basename "$IMG")" > "$IMG.sha256")
echo "image: $IMG ($(du -h "$IMG" | cut -f1))"
