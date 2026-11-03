#!/bin/bash
# The installer's release gate: boot the built image in QEMU, drive the
# wizard API exactly as the browser would, install onto a virtual disk,
# then boot the installed disk alone and assert Fog answers with the
# preseeded identity — and finally boot the stick again beside that disk,
# where the installer must recognize the server it just built instead of
# offering to erase it. An image that hasn't survived this doesn't ship.
#
#   imaging/test/e2e.sh [--keep]
#
# Requires qemu-system-x86_64 + EDK2 UEFI firmware, and imaging/out/
# artifacts from `make installer-image`. Slow under emulation (Apple
# Silicon runs x86 via TCG); on x86 Linux with KVM it's minutes.
set -euo pipefail

cd "$(dirname "$0")/../.."
OUT=imaging/out
WORK=$(mktemp -d)
KEEP=${1:-}
# Two host ports per run, derived from the PID so concurrent runs (CI, a
# second terminal) don't collide at QEMU's bind. FOG_E2E_PORT_BASE overrides.
PORT_BASE=${FOG_E2E_PORT_BASE:-$((18000 + ($$ % 2000) * 2))}
API_PORT=$PORT_BASE
FOGD_PORT=$((PORT_BASE + 1))
API=http://127.0.0.1:$API_PORT
FOGD=http://127.0.0.1:$FOGD_PORT
QEMU_PID=""

log()  { echo "e2e: $*"; }
fail() { echo "e2e: FAIL — $*" >&2; exit 1; }
cleanup() {
  [ -n "$QEMU_PID" ] && kill "$QEMU_PID" 2>/dev/null || true
  [ "$KEEP" = "--keep" ] || rm -rf "$WORK"
}
trap cleanup EXIT

[ -f "$OUT/fog-installer-x86_64.img" ] || fail "no image — run make installer-image"

FW=""
for c in /opt/homebrew/share/qemu/edk2-x86_64-code.fd \
         /usr/share/OVMF/OVMF_CODE.fd /usr/share/OVMF/OVMF_CODE_4M.fd \
         /usr/share/edk2/ovmf/OVMF_CODE.fd /usr/share/qemu/OVMF.fd; do
  [ -f "$c" ] && FW=$c && break
done
[ -n "$FW" ] || fail "no UEFI firmware (edk2/OVMF) found"

ACCEL=tcg
if [ "$(uname -s)" = Linux ] && [ -w /dev/kvm ]; then ACCEL=kvm; fi

# Installer stick (copy — the test must not dirty the artifact) + blank
# 32G target disk (sparse).
cp "$OUT/fog-installer-x86_64.img" "$WORK/installer.img"
truncate -s 32G "$WORK/target.img"

boot_qemu() { # boot_qemu <drives...>
  qemu-system-x86_64 \
    -machine q35,accel=$ACCEL -cpu max -smp 2 -m 2048 -vga none \
    -drive "if=pflash,format=raw,readonly=on,file=$FW" \
    -netdev "user,id=n0,hostfwd=tcp:127.0.0.1:$API_PORT-:80,hostfwd=tcp:127.0.0.1:$FOGD_PORT-:8080" \
    -device virtio-net-pci,netdev=n0 \
    -display none -serial "file:$WORK/serial-$1.log" \
    "${@:2}" &
  QEMU_PID=$!
}

wait_http() { # wait_http <url> <seconds> <what>
  local url=$1 deadline=$((SECONDS + $2)) what=$3
  while [ $SECONDS -lt $deadline ]; do
    if curl -fsS -m 3 "$url" >/dev/null 2>&1; then return 0; fi
    kill -0 "$QEMU_PID" 2>/dev/null || fail "$what: qemu exited (see $WORK/serial-*.log)"
    sleep 5
  done
  fail "$what never answered at $url"
}

# ---- act 1: boot the installer, drive the wizard ----
log "booting installer (accel=$ACCEL)"
boot_qemu install \
  -device qemu-xhci \
  -drive "file=$WORK/installer.img,format=raw,if=none,id=stick" -device usb-storage,drive=stick \
  -drive "file=$WORK/target.img,format=raw,if=none,id=disk0" -device virtio-blk-pci,drive=disk0
wait_http "$API/api/setup/state" 900 "fog-setup agent"
log "agent up; inventory:"
curl -fsS "$API/api/setup/inventory" | tee "$WORK/inventory.json" | head -c 400; echo

TARGET=$(python3 - "$WORK/inventory.json" <<'PY'
import json,sys
inv=json.load(open(sys.argv[1]))
cands=[d["path"] for d in inv["disks"] if not d["removable"] and not d["bootMedium"]]
print(cands[0] if cands else "")
PY
)
[ -n "$TARGET" ] || fail "no installable disk in inventory"
log "installing onto $TARGET"
curl -fsS -X POST "$API/api/setup/install" -H 'Content-Type: application/json' -d "{
  \"mode\": \"fresh\", \"disk\": \"$TARGET\", \"confirmDisk\": \"$TARGET\",
  \"serverName\": \"e2e\", \"username\": \"tester\",
  \"password\": \"e2e-password-long\", \"timezone\": \"UTC\"
}" | grep -q '"ok":true' || fail "install refused"

DEADLINE=$((SECONDS + 1800))
while :; do
  [ $SECONDS -lt $DEADLINE ] || fail "install never finished (state: $(cat "$WORK/state.json" 2>/dev/null))"
  curl -fsS "$API/api/setup/state" > "$WORK/state.json" 2>/dev/null || true
  PHASE=$(python3 -c "import json;print(json.load(open('$WORK/state.json')).get('phase',''))" 2>/dev/null || echo "")
  case "$PHASE" in
    done) break ;;
    failed) fail "install failed: $(python3 -c "import json;print(json.load(open('$WORK/state.json')).get('error',''))")" ;;
  esac
  sleep 10
done
# The token is never in /api/setup/state — it is handed out exactly once,
# the way the wizard takes it right before the restart.
CLAIM=$(curl -fsS -X POST "$API/api/setup/claim-token" | python3 -c "import json,sys;print(json.load(sys.stdin)['claimToken'])")
[ -n "$CLAIM" ] || fail "no claim token from /api/setup/claim-token"
log "install done; claim token taken"
kill "$QEMU_PID"; wait "$QEMU_PID" 2>/dev/null || true; QEMU_PID=""

# ---- act 2: boot the installed disk alone ----
log "booting the installed system"
boot_qemu installed \
  -drive "file=$WORK/target.img,format=raw,if=none,id=disk0" -device virtio-blk-pci,drive=disk0
wait_http "$FOGD/api/setup/status" 900 "installed fogd"

STATUS=$(curl -fsS "$FOGD/api/setup/status")
echo "$STATUS" | grep -q '"needsSetup":false' || fail "firstboot preseed didn't apply: $STATUS"
# The one-shot claim token signs in — the full handoff, end to end.
HTTPCODE=$(curl -s -o /dev/null -w '%{http_code}' -c "$WORK/cookies" "$FOGD/api/setup/claim?token=$CLAIM")
[ "$HTTPCODE" = 303 ] || fail "claim redirect: $HTTPCODE"
grep -q fog_session "$WORK/cookies" || fail "claim minted no session"
curl -fsS -b "$WORK/cookies" "$FOGD/api/auth/me" | grep -q '"tester"' || fail "session is not the preseeded admin"

# ---- act 3: the stick left plugged in ----
# The likeliest way to meet the installer twice: restart without unplugging.
# It must recognize the machine it just built — the completed-install marker
# on the target's ESP — instead of opening with an offer to erase it.
kill "$QEMU_PID"; wait "$QEMU_PID" 2>/dev/null || true; QEMU_PID=""
log "booting the installer again with the stick still plugged in"
# Explicit bootindex: with an installed disk present the firmware could pick
# either, and this test is about the boot that lands back in the installer.
boot_qemu restick \
  -device qemu-xhci \
  -drive "file=$WORK/installer.img,format=raw,if=none,id=stick" \
  -device usb-storage,drive=stick,bootindex=0 \
  -drive "file=$WORK/target.img,format=raw,if=none,id=disk0" \
  -device virtio-blk-pci,drive=disk0,bootindex=1
wait_http "$API/api/setup/state" 900 "fog-setup agent (stick left in)"
curl -fsS "$API/api/setup/inventory" > "$WORK/inventory2.json"
python3 - "$WORK/inventory2.json" <<'EX' || fail "the installer did not recognize the server it had just installed"
import json, sys
inv = json.load(open(sys.argv[1]))
ex = inv.get("existing")
assert ex, "no existing install reported: %s" % inv
assert ex.get("serverName") == "e2e", ex
assert ex.get("disk"), ex
assert ex.get("installedAt"), ex
EX
log "the installer recognized the existing server"

log "PASS — installed, booted, claimed, signed in, and safe to boot again"
