#!/usr/bin/env bash
# Fills internal/catalog/seed/ with fog's own catalogs, as committed, so the
# binary built next carries them: a fog reads its store from this copy
# until it first fetches each catalog (internal/catalog/registry.go). Reads
# checkouts of the catalog repositories from $FOG_CATALOGS, default the
# directory holding this fog checkout. release.sh runs it before building.
set -euo pipefail
cd "$(dirname "$0")/.."
root=${FOG_CATALOGS:-..}
seed=internal/catalog/seed
for name in fog-apps fog-catalog; do
  src=$root/$name
  if ! git -C "$src" rev-parse --verify -q HEAD >/dev/null 2>&1; then
    echo "no checkout of $name at $src (set FOG_CATALOGS)" >&2
    exit 1
  fi
  # What is committed, not what is in the working tree: the seed is a
  # catalog as fog would fetch it.
  if [ -n "$(git -C "$src" status --porcelain)" ]; then
    echo "note: $src has uncommitted changes; seeding its HEAD" >&2
  fi
  rm -rf "${seed:?}/$name"
  mkdir -p "$seed/$name"
  git -C "$src" archive --format=tar HEAD | tar -x -C "$seed/$name"
  echo "seeded $name at $(git -C "$src" rev-parse --short=12 HEAD)"
done
# The seed must load clean, or the store a fresh fog shows is short.
FOG_CATALOGS="$PWD/$seed" go test -count=1 -run 'TestOwnCatalogsLoadClean' ./internal/catalog >/dev/null
