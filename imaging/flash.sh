#!/bin/sh
# Fog installer flasher — download the bootable installer image, verify it,
# write it to a USB stick, and get you booted into it. Runs on macOS or
# Linux, from a brand-new shell:
#
#   curl -fsSL https://raw.githubusercontent.com/fogapp/fog/main/imaging/flash.sh | sh
#
# Where the image comes from, in order:
#   FOG_IMAGE_URL       direct URL to fog-installer-x86_64.img (its .sha256
#                       is fetched from the same URL + ".sha256", or pass
#                       FOG_IMAGE_SHA256 outright) — you vouch for that URL
#   FOG_MANIFEST_URL    a release manifest with an "installer-x86_64" entry
#                       (default: fog's public releases on GitHub,
#                       https://github.com/fogapp/fog). Its signature is checked
#                       against fog's release key, so the checksum it gives
#                       is one fog published; a relative image name resolves
#                       beside the manifest, as in a plain release directory.
#
# A locally built image can be served straight from the repo checkout:
#   (cd imaging/out && python3 -m http.server 8000)   # on the build machine
#   FOG_IMAGE_URL=http://<build-host>:8000/fog-installer-x86_64.img sh flash.sh
#
# Other knobs: FOG_DISK=/dev/sdX (or diskN on macOS) skips the picker;
# FOG_CACHE names the download directory (default ~/.cache/fog-installer);
# FOG_REBOOT=1 skips the "reboot now?" question on Linux;
# FOG_FLASH_RESOLVE_ONLY=1 prints the image URL and checksum and stops.
# FOG_ALLOW_UNSIGNED=1 skips the manifest signature; FOG_ALLOW_OLD_RELEASE=1
# takes a manifest issued more than 180 days ago; FOG_RELEASE_KEYS replaces
# the pinned keys (a fork that signs with its own).
#
# The target disk is wiped. The script refuses the disk the running system
# lives on, and asks you to type the device name back before writing.
set -eu

DEFAULT_MANIFEST=https://github.com/fogapp/fog/releases/latest/download/latest.json
MANIFEST="${FOG_MANIFEST_URL:-$DEFAULT_MANIFEST}"
CACHE="${FOG_CACHE:-$HOME/.cache/fog-installer}"
IMG_NAME=fog-installer-x86_64.img
RESOLVE_ONLY="${FOG_FLASH_RESOLVE_ONLY:-0}"
# fog's release key, as install.sh pins it (internal/selfupdate/sign.go is
# the canonical list; shellsource_test.go holds the copies together).
RELEASE_KEYS="${FOG_RELEASE_KEYS:-MCowBQYDK2VwAyEAzrGQX7hwCDQn0KNY31d+VHb3CU2wIg/qt7CeHpDzMUg=}"

say() { printf '%s\n' "$*"; }
die() { printf 'fog-flash: %s\n' "$*" >&2; exit 1; }

# The same URL rules install.sh holds a source to.
valid_url() {
  case "$1" in http://?*|https://?*) ;; *) return 1 ;; esac
  case "${1#*://}" in *[!]A-Za-z0-9._~:/%+=,[-]*) return 1 ;; esac
  return 0
}

# Prompts must come from the terminal even when the script itself arrives
# on stdin via curl | sh.
ask() { printf '%s' "$1" >&2; read -r REPLY < /dev/tty; }

OS=$(uname -s)
case "$OS" in Darwin|Linux) ;; *) die "unsupported OS: $OS" ;; esac

command -v curl >/dev/null 2>&1 || die "curl is required"
if [ "$OS" = Linux ] && [ "$RESOLVE_ONLY" != 1 ]; then
  [ "$(id -u)" -eq 0 ] || die "run as root (writing a disk): sudo sh, then paste again"
fi
sha256_of() {
  { sha256sum "$1" 2>/dev/null || shasum -a 256 "$1" 2>/dev/null; } | awk '{print $1}'
}
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

# An openssl that knows ed25519 (OpenSSL 1.1.1+). macOS's own is LibreSSL,
# which doesn't; Homebrew's openssl@3 does.
OPENSSL=""
find_openssl() {
  n=0
  for k in $RELEASE_KEYS; do
    n=$((n + 1))
    printf -- '-----BEGIN PUBLIC KEY-----\n%s\n-----END PUBLIC KEY-----\n' "$k" > "$WORK/key$n.pem"
  done
  [ "$n" -gt 0 ] || return 1
  for o in openssl /opt/homebrew/bin/openssl /opt/homebrew/opt/openssl@3/bin/openssl /usr/local/opt/openssl@3/bin/openssl; do
    command -v "$o" >/dev/null 2>&1 || continue
    if "$o" pkey -pubin -in "$WORK/key1.pem" -noout >/dev/null 2>&1; then
      OPENSSL=$o
      return 0
    fi
  done
  return 1
}
verify_manifest() { # <manifest file> <signature file>
  base64 -d < "$2" > "$WORK/sig.bin" 2>/dev/null || return 1
  [ "$(wc -c < "$WORK/sig.bin" | tr -d ' ')" = 64 ] || return 1
  i=0
  while [ -f "$WORK/key$((i + 1)).pem" ]; do
    i=$((i + 1))
    if "$OPENSSL" pkeyutl -verify -pubin -inkey "$WORK/key$i.pem" -rawin -in "$1" -sigfile "$WORK/sig.bin" >/dev/null 2>&1; then
      return 0
    fi
  done
  return 1
}

# --- resolve the image URL --------------------------------------------------

IMG_URL="${FOG_IMAGE_URL:-}"
IMG_SHA="${FOG_IMAGE_SHA256:-}"

if [ -z "$IMG_URL" ]; then
  valid_url "$MANIFEST" || die "FOG_MANIFEST_URL must be an http(s) URL without spaces, quotes, ?, # or @ (got '$MANIFEST')."
  [ "$RESOLVE_ONLY" = 1 ] || say "==> resolving image from manifest: $MANIFEST"
  if ! curl -fsSL --proto '=http,https' --max-filesize 1048576 -o "$WORK/latest.json" "$MANIFEST"; then
    if [ -z "${FOG_MANIFEST_URL:-}" ]; then
      printf '%s\n' "fog-flash: FOG_MANIFEST_URL wasn't set, so this tried fog's public releases," \
        "fog-flash: $DEFAULT_MANIFEST. To flash from somewhere else, set" \
        "fog-flash: FOG_MANIFEST_URL to the latest.json of the release you want, or" \
        "fog-flash: FOG_IMAGE_URL to an image you trust." >&2
    fi
    die "cannot fetch manifest $MANIFEST"
  fi
  if [ "${FOG_ALLOW_UNSIGNED:-0}" = 1 ]; then
    say "fog-flash: FOG_ALLOW_UNSIGNED=1 — the manifest's signature is NOT checked." >&2
  else
    find_openssl || die "checking the release signature needs openssl with ed25519 (OpenSSL 1.1.1+;
on macOS: brew install openssl@3). FOG_ALLOW_UNSIGNED=1 skips the check."
    curl -fsSL --proto '=http,https' --max-filesize 4096 -o "$WORK/latest.json.sig" "$MANIFEST.sig" \
      || die "$MANIFEST has no signature beside it ($MANIFEST.sig) — refusing an unsigned image."
    verify_manifest "$WORK/latest.json" "$WORK/latest.json.sig" \
      || die "$MANIFEST's signature doesn't match fog's release key — refusing it."
  fi
  # The signed issue date: a manifest more than 180 days old is refused, as
  # install.sh and fogd refuse it (MaxManifestAge), so a stale mirror can't
  # hand out an old installer as current. FOG_ALLOW_OLD_RELEASE=1 takes it.
  ISSUED=$(tr -d ' \t\r\n' < "$WORK/latest.json" | sed -n 's/.*"issued_at":\([0-9]\{1,12\}\)[,}].*/\1/p')
  if [ "${FOG_ALLOW_OLD_RELEASE:-0}" != 1 ]; then
    [ -n "$ISSUED" ] || die "$MANIFEST is undated (made before releases carried issued_at) — refusing it. FOG_ALLOW_OLD_RELEASE=1 takes it anyway."
    AGE=$(( $(date +%s) - ISSUED ))
    [ "$AGE" -le 15552000 ] || die "$MANIFEST was issued $((AGE / 86400)) days ago, past the 180-day limit — refusing it. FOG_ALLOW_OLD_RELEASE=1 takes it anyway."
  fi
  # The manifest is the small JSON release.sh writes; pull the installer
  # entry with sed so a bare rescue shell without python still works.
  ENTRY=$(tr -d '\n' < "$WORK/latest.json" | sed -n 's/.*"installer-x86_64"[^{]*{\([^}]*\)}.*/\1/p')
  [ -n "$ENTRY" ] || die "manifest has no installer-x86_64 artifact yet.
Publish one with: FOG_RELEASE_IMAGE=1 ./scripts/release.sh vX.Y.Z
Or point at any hosted copy: FOG_IMAGE_URL=http://host/path/$IMG_NAME"
  IMG_URL=$(printf '%s' "$ENTRY" | sed -n 's/.*"url"[^"]*"\([^"]*\)".*/\1/p')
  IMG_SHA=$(printf '%s' "$ENTRY" | sed -n 's/.*"sha256"[^"]*"\([^"]*\)".*/\1/p')
  [ -n "$IMG_URL" ] && [ -n "$IMG_SHA" ] || die "manifest installer entry is missing url/sha256"
  # A relative name resolves beside the manifest: a plain release directory
  # (FOG_MANIFEST_RELATIVE=1) holds the image next to latest.json.
  case "$IMG_URL" in
    http://*|https://*) ;;
    *[!A-Za-z0-9._-]*) die "manifest names the image '$IMG_URL', which is neither a URL nor a file name" ;;
    *) IMG_URL="${MANIFEST%/*}/$IMG_URL" ;;
  esac
fi
valid_url "$IMG_URL" || die "image URL '$IMG_URL' isn't an http(s) URL fog will use."

if [ -z "$IMG_SHA" ]; then
  IMG_SHA=$(curl -fsSL --proto '=http,https' "$IMG_URL.sha256" | awk '{print $1}') \
    || die "no FOG_IMAGE_SHA256 given and $IMG_URL.sha256 not fetchable"
fi
printf '%s' "$IMG_SHA" | grep -Eq '^[0-9a-f]{64}$' || die "bad sha256: $IMG_SHA"

if [ "$RESOLVE_ONLY" = 1 ]; then
  echo "image=$IMG_URL"
  echo "sha256=$IMG_SHA"
  exit 0
fi

# --- download (or reuse a verified copy) ------------------------------------

mkdir -p "$CACHE"
IMG="$CACHE/$IMG_NAME"
verify() { [ -f "$IMG" ] && [ "$(sha256_of "$IMG")" = "$IMG_SHA" ]; }

if verify; then
  say "==> reusing verified image at $IMG"
else
  say "==> downloading $IMG_URL (~2GB)"
  # Bounded: the image is ~2GB, and a source that streams forever must not
  # fill the disk.
  curl -fL --proto '=http,https' --max-filesize 17179869184 --progress-bar -o "$IMG.part" "$IMG_URL"
  mv "$IMG.part" "$IMG"
  say "==> verifying checksum"
  verify || { rm -f "$IMG"; die "checksum mismatch — the download isn't the image the manifest names; removed it, retry"; }
fi

# --- pick the target disk ---------------------------------------------------

DISK="${FOG_DISK:-}"

if [ "$OS" = Darwin ]; then
  if [ -z "$DISK" ]; then
    say ""
    say "External disks:"
    diskutil list external physical
    ask "Write to which disk (e.g. disk4)? "
    DISK=$REPLY
  fi
  DISK=${DISK#/dev/}
  case "$DISK" in ''|*[!A-Za-z0-9]*) die "'$DISK' isn't a disk name like disk4" ;; esac
  diskutil info "$DISK" >/dev/null 2>&1 || die "no such disk: $DISK"
  diskutil info "$DISK" | grep -q "Internal:.*Yes" && die "$DISK is an internal disk — refusing"
  DESC=$(diskutil info "$DISK" | sed -n 's/.*Device \/ Media Name: *//p')
  SIZE=$(diskutil info "$DISK" | sed -n 's/.*Disk Size: *\([^(]*\).*/\1/p')
else
  ROOT_SRC=$(findmnt -no SOURCE / || true)
  ROOT_DISK=$(lsblk -no PKNAME "$ROOT_SRC" 2>/dev/null | head -n1 || true)
  if [ -z "$DISK" ]; then
    say ""
    say "Disks (RM=1 means removable):"
    lsblk -d -o NAME,SIZE,RM,TRAN,MODEL | grep -v '^loop' || true
    ask "Write to which disk (e.g. sdb)? "
    DISK=$REPLY
  fi
  DISK=${DISK#/dev/}
  case "$DISK" in ''|*[!A-Za-z0-9]*) die "'$DISK' isn't a disk name like sdb" ;; esac
  [ -b "/dev/$DISK" ] || die "no such block device: /dev/$DISK"
  [ -n "$ROOT_DISK" ] && [ "$DISK" = "$ROOT_DISK" ] \
    && die "/dev/$DISK holds the running system — refusing"
  DESC=$(lsblk -dno MODEL "/dev/$DISK" | sed 's/ *$//')
  SIZE=$(lsblk -dno SIZE "/dev/$DISK")
fi

say ""
say "About to ERASE /dev/$DISK — $SIZE ${DESC:-disk} — and write the Fog installer."
ask "Type the disk name ($DISK) to confirm: "
[ "$REPLY" = "$DISK" ] || die "aborted (typed \"$REPLY\")"

# --- write ------------------------------------------------------------------

# The image is checked again while it is written: the bytes dd writes are
# hashed on their way to the stick, so a file swapped in the cache after the
# check above (by anything running as this user) can't reach the stick
# unnoticed. ibs/obs differ so dd gathers whole output blocks from the pipe,
# which a raw device needs.
write_image() { # <device> <ibs> <obs> [sudo] [dd conv]
  rm -f "$WORK/feed"
  mkfifo "$WORK/feed"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum < "$WORK/feed" > "$WORK/written.sha" &
  else
    shasum -a 256 < "$WORK/feed" > "$WORK/written.sha" &
  fi
  hasher=$!
  tee "$WORK/feed" < "$IMG" | ${4:-} dd of="$1" ibs="$2" obs="$3" ${5:-} status=progress
  wait "$hasher"
  [ "$(awk '{print $1}' "$WORK/written.sha")" = "$IMG_SHA" ] \
    || die "the bytes written to $1 aren't the verified image (the file changed after it was checked) — don't boot this stick; run flash.sh again."
  say "==> written and verified"
}

say "==> writing image to /dev/$DISK"
if [ "$OS" = Darwin ]; then
  diskutil unmountDisk "/dev/$DISK" >/dev/null
  write_image "/dev/r$DISK" 1m 4m sudo
  sync
  diskutil eject "/dev/$DISK" >/dev/null || true
  say ""
  say "Done. Plug the stick into the target machine and boot from it"
  say "(usually F12/F11/Esc at power-on for a one-time boot menu)."
  say "The wizard comes up on the machine's display and at http://fog.local"
else
  # Anything auto-mounted from a previous life of the stick gets in dd's way.
  for p in /dev/$DISK?*; do umount "$p" 2>/dev/null || true; done
  write_image "/dev/$DISK" 1M 4M "" conv=fsync
  sync
  say ""
  # If this machine is itself the install target, hand the firmware a
  # one-shot boot into the stick (partition 1 is the ESP; systemd-boot
  # lives at the removable-media fallback path).
  if command -v efibootmgr >/dev/null 2>&1 && [ -d /sys/firmware/efi ]; then
    NUM=$(efibootmgr | sed -n 's/^Boot\([0-9A-F]\{4\}\)\** Fog Installer.*/\1/p' | head -n1)
    if [ -z "$NUM" ]; then
      efibootmgr -q -c -d "/dev/$DISK" -p 1 -L "Fog Installer" -l '\EFI\BOOT\BOOTX64.EFI'
      NUM=$(efibootmgr | sed -n 's/^Boot\([0-9A-F]\{4\}\)\** Fog Installer.*/\1/p' | head -n1)
    fi
    [ -n "$NUM" ] && efibootmgr -q -n "$NUM" \
      && say "Firmware set to boot the installer once on next boot."
    if [ "${FOG_REBOOT:-0}" = 1 ]; then REPLY=y; else ask "Reboot into the installer now? [y/N] "; fi
    case "$REPLY" in
      y|Y) say "Rebooting."; reboot ;;
      *) say "Not rebooting. When ready: reboot (or move the stick to the target machine)." ;;
    esac
  else
    say "Done. Boot this or the target machine from the stick"
    say "(one-time boot menu: usually F12/F11/Esc at power-on)."
  fi
  say "The wizard comes up on the machine's display and at http://fog.local"
fi
