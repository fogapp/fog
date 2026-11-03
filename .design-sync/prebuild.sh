#!/bin/sh
# Prepares fog's frontend for the design-system sync (cfg.buildCmd).
# 1. Tailwind stylesheet with the safelist  -> frontend/.ds-styles.css
# 2. Type declarations for the export barrel -> frontend/.ds-types/
#    (frontend/package.json "types" points here so the converter reads real
#    prop contracts instead of `[key: string]: unknown` stubs)
set -e
cd "$(dirname "$0")/.."
node .design-sync/build-css.mjs
cd frontend
rm -rf .ds-types
npx tsc -p tsconfig.json --declaration --emitDeclarationOnly --noEmit false --outDir .ds-types
echo "wrote frontend/.ds-types ($(find .ds-types -name '*.d.ts' | wc -l | tr -d ' ') files)"
