#!/bin/sh
# Fails when a Go function nothing reaches — not even a test — has crept in.
#
# Runs golang.org/x/tools/cmd/deadcode (pinned through go.mod's tool
# directive) with -test, so a helper only tests call is fine; what fails is
# code no binary and no test can get to. The analysis is for linux, the only
# OS fog ships on: darwin-only builds of a few probes would otherwise make
# their linux twins look dead.
set -eu
cd "$(dirname "$0")/.."

# Findings that are dead on purpose, one per line as this script prints
# them ("file: unreachable func: Name"; positions are dropped so edits above
# a kept function don't break the gate), each with a comment saying why.
# Empty is the goal.
ALLOW=''

allow=$(mktemp)
trap 'rm -f "$allow"' EXIT
printf '%s\n' "$ALLOW" | sed '/^$/d' | sort > "$allow"

# Build the tool for this machine, then point it at linux. The patterns
# name fog's own trees: ./... would also reach a Go package an npm
# dependency ships under frontend/node_modules.
tool=$(go tool -n deadcode)
new=$(GOOS=linux "$tool" -test ./cmd/... ./internal/... | sed -E 's/:[0-9]+:[0-9]+:/:/' | sort | comm -23 - "$allow")

if [ -n "$new" ]; then
	echo "unreachable Go code (delete it, or add it to ALLOW in scripts/deadcode.sh with a reason):"
	printf '%s\n' "$new"
	exit 1
fi
