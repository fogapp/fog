#!/bin/sh
# Entry point for the imaging pipeline. Dispatches each stage into a
# linux/amd64 Debian container so the build works identically on any Docker
# host. Run stages/*.sh directly (as root on Linux) to skip the container.
#
#   imaging/build.sh [rootfs|live|image|all]
set -eu

cd "$(dirname "$0")/.."
STAGE="${1:-all}"
IMG=debian:trixie

for bin in bin/fogd-linux-amd64 bin/fog-setup-linux-amd64 bin/fog-console-linux-amd64; do
  [ -f "$bin" ] || { echo "imaging: $bin missing — run 'make build-linux' first" >&2; exit 1; }
done

# Always build against the current upstream trixie — a months-old local
# debian:trixie would silently produce a different image from the same
# commit on another machine. Offline, a local copy is better than no build:
# the apt cache volume usually carries the packages too.
if ! docker pull -q --platform linux/amd64 "$IMG" >/dev/null 2>&1; then
  docker image inspect "$IMG" >/dev/null 2>&1 || {
    echo "imaging: can't pull $IMG and no local copy exists — connect to the network (or docker pull it once) and retry." >&2
    exit 1
  }
  echo "imaging: pull of $IMG failed — building against the local copy" >&2
fi

run_stage() {  # <stage> [apt-cache-volume]
  echo "imaging: stage $1"
  docker run --rm --platform linux/amd64 \
    -v "$PWD":/src -v "${2:-fog-imaging-apt}":/var/cache/apt/archives \
    -e DEBIAN_FRONTEND=noninteractive \
    "$IMG" bash "/src/imaging/stages/$1.sh"
}

case "$STAGE" in
  rootfs|live|image) run_stage "$STAGE" ;;
  all)
    # rootfs and live are independent (image consumes both), so run them
    # concurrently — with separate apt cache volumes, or they'd serialize on
    # the archive lock anyway.
    run_stage rootfs fog-imaging-apt-rootfs & rootfs_pid=$!
    run_stage live fog-imaging-apt-live & live_pid=$!
    rootfs_rc=0; live_rc=0
    wait "$rootfs_pid" || rootfs_rc=$?
    wait "$live_pid" || live_rc=$?
    [ "$rootfs_rc" -eq 0 ] || { echo "imaging: rootfs stage failed ($rootfs_rc)" >&2; exit 1; }
    [ "$live_rc" -eq 0 ]   || { echo "imaging: live stage failed ($live_rc)" >&2; exit 1; }
    run_stage image
    ;;
  *) echo "usage: imaging/build.sh [rootfs|live|image|all]" >&2; exit 2 ;;
esac
