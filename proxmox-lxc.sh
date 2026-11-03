#!/usr/bin/env bash
# Create a Fog server as an LXC container on a Proxmox VE host. Paste this
# into the node's shell (Proxmox web UI → node → Shell):
#
#   bash -c "$(curl -fsSL https://raw.githubusercontent.com/fogapp/fog/main/proxmox-lxc.sh)"
#
# It creates an unprivileged Debian 13 container with nesting+keyctl, installs
# Docker and fogd inside it, and leaves the first-run wizard listening on
# :8080. With a terminal it asks (default settings / advanced); without one it
# takes every value from the FOG_* environment variables below. Run it again
# with a different --ctid to build a second server.
#
#   ./proxmox-lxc.sh --help            every parameter, its flag and env var
#   ./proxmox-lxc.sh --dry-run         print the pct command and stop
#
# Where Fog comes from is a parameter: --source takes a Gitea/Forgejo repo, a
# GitHub or GitLab repo, a plain directory of release files, or a latest.json
# URL — the same shapes install.sh and fogd's updater accept. See
# docs/hosting-the-source.md.
#
# For a Fog VM — its own kernel, NAS mounts, full parity with metal — use
# proxmox-vm.sh instead (docs/proxmox-vm.md compares the two).
set -Eeuo pipefail

VERSION_NOTE="fog proxmox-lxc"

# ---------------------------------------------------------------- output ---

if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
  BOLD=$'\033[1m'; DIM=$'\033[2m'; GRN=$'\033[32m'; YLW=$'\033[33m'
  RED=$'\033[31m'; CYN=$'\033[36m'; RST=$'\033[0m'
else
  BOLD=; DIM=; GRN=; YLW=; RED=; CYN=; RST=
fi

msg()  { printf '%s  ▸%s %s\n' "$CYN" "$RST" "$*"; }
ok()   { printf '%s  ✔%s %s\n' "$GRN" "$RST" "$*"; }
warn() { printf '%s  !%s %s\n' "$YLW" "$RST" "$*" >&2; }
die()  { printf '%s  ✗ %s%s\n' "$RED" "$*" "$RST" >&2; exit 1; }

header() {
  printf '\n   %s░░░  fog%s\n' "$BOLD" "$RST"
  printf '   %s───────────────────────────────────────────%s\n' "$DIM" "$RST"
  printf '   a new Fog server, as an LXC container on this node\n\n'
}

# A half-built container is left in place on failure: the log above says what
# broke, and `pct enter` reaches the container to see for yourself. Nothing
# here deletes anything on its own.
CREATED=0
on_err() {
  local rc=$? line=${1:-?}
  printf '\n%s  ✗ failed at line %s (exit %s)%s\n' "$RED" "$line" "$rc" "$RST" >&2
  if [ "$CREATED" = 1 ]; then
    warn "container $CTID was created and is left as-is:"
    warn "    pct enter $CTID        # look around"
    warn "    pct destroy $CTID      # discard it and start over"
  fi
  exit "$rc"
}
trap 'on_err $LINENO' ERR

# ------------------------------------------------------------ parameters ---

# Container shape.
CTID="${FOG_CTID:-}"                        # default: cluster's next free id
CT_HOSTNAME="${FOG_HOSTNAME:-fog}"          # also the .local name
CT_PASSWORD="${FOG_ROOT_PASSWORD:-}"        # empty: no root password, use `pct enter`
DISK="${FOG_DISK:-32}"                      # GB — apps, images, backups live here
CORES="${FOG_CORES:-2}"
RAM="${FOG_RAM:-4096}"                      # MB
SWAP="${FOG_SWAP:-512}"                     # MB
STORAGE="${FOG_STORAGE:-}"                  # rootfs storage; asked/auto-picked
TEMPLATE_STORAGE="${FOG_TEMPLATE_STORAGE:-}"
TEMPLATE="${FOG_TEMPLATE:-}"                # full vztmpl name, else newest debian-13
UNPRIVILEGED="${FOG_UNPRIVILEGED:-1}"
ONBOOT="${FOG_ONBOOT:-1}"
TAGS="${FOG_TAGS:-fog}"

# Network.
BRIDGE="${FOG_BRIDGE:-vmbr0}"
IPV4="${FOG_IPV4:-dhcp}"                    # dhcp | 192.168.1.50/24
GATEWAY="${FOG_GATEWAY:-}"                  # required with a static IPv4
IPV6="${FOG_IPV6:-none}"                    # none | auto | dhcp | <cidr>
VLAN="${FOG_VLAN:-}"
MTU="${FOG_MTU:-}"
DNS="${FOG_DNS:-}"                          # default: the node's resolver
SEARCHDOMAIN="${FOG_SEARCHDOMAIN:-}"
SSH_KEYS="${FOG_SSH_KEYS:-}"                # path to a public key file

# Fog itself.
FOG_PORT="${FOG_PORT:-8080}"
# One knob for where the code lives; the rest are escape hatches passed
# straight through to install.sh.
DEFAULT_SOURCE=https://github.com/fogapp/fog
SOURCE_GIVEN=0
[ -z "${FOG_SOURCE:-}${FOG_REPO_URL:-}" ] || SOURCE_GIVEN=1
FOG_SOURCE="${FOG_SOURCE:-${FOG_REPO_URL:-$DEFAULT_SOURCE}}"
FOG_BRANCH="${FOG_BRANCH:-main}"
FOG_MANIFEST_URL="${FOG_MANIFEST_URL:-}"
FOG_RAW_BASE="${FOG_RAW_BASE:-}"
FOG_TZ="${FOG_TZ:-}"                        # default: this node's timezone
FOG_ADMIN_USER="${FOG_ADMIN_USER:-}"        # set both to skip the web wizard
FOG_ADMIN_PASSWORD="${FOG_ADMIN_PASSWORD:-}"
FOG_SERVER_NAME="${FOG_SERVER_NAME:-}"      # display name; defaults to hostname
FOG_BASE_DOMAIN="${FOG_BASE_DOMAIN:-}"      # e.g. home.example.com

ASSUME_YES="${FOG_YES:-0}"
DRY_RUN=0

usage() {
  cat <<EOF
${VERSION_NOTE} — create a Fog LXC on this Proxmox VE node.

  bash -c "\$(curl -fsSL <wherever you host proxmox-lxc.sh>)"

Every parameter has a flag and an environment variable; anything not given is
asked for (with a terminal) or defaulted (without one).

  flag                      env var                default
  --ctid N                  FOG_CTID               next free id
  --hostname NAME           FOG_HOSTNAME           fog
  --root-password PW        FOG_ROOT_PASSWORD      (none — use 'pct enter')
  --disk GB                 FOG_DISK               32
  --cores N                 FOG_CORES              2
  --ram MB                  FOG_RAM                4096
  --swap MB                 FOG_SWAP               512
  --storage NAME            FOG_STORAGE            asked / only rootdir storage
  --template-storage NAME   FOG_TEMPLATE_STORAGE   first vztmpl storage
  --template NAME           FOG_TEMPLATE           newest debian-13-standard
  --privileged              FOG_UNPRIVILEGED=0     unprivileged
  --no-onboot               FOG_ONBOOT=0           start at boot
  --tags A;B                FOG_TAGS               fog
  --bridge NAME             FOG_BRIDGE             vmbr0
  --ipv4 CIDR|dhcp          FOG_IPV4               dhcp
  --gateway IP              FOG_GATEWAY            (required if static)
  --ipv6 MODE|CIDR          FOG_IPV6               none
  --vlan TAG                FOG_VLAN               untagged
  --mtu N                   FOG_MTU                bridge default
  --dns IP                  FOG_DNS                node's resolver
  --searchdomain DOM        FOG_SEARCHDOMAIN       node's search domain
  --ssh-keys FILE           FOG_SSH_KEYS           none
  --port N                  FOG_PORT               8080
  --source URL              FOG_SOURCE             $FOG_SOURCE
  --branch NAME             FOG_BRANCH             main
  --manifest-url URL        FOG_MANIFEST_URL       resolved from --source
  --raw-base URL            FOG_RAW_BASE           resolved from --source
  --timezone TZ             FOG_TZ                 this node's timezone
  --admin-user NAME         FOG_ADMIN_USER         (web wizard creates it)
  --admin-password PW       FOG_ADMIN_PASSWORD     (web wizard creates it)
  --server-name NAME        FOG_SERVER_NAME        hostname
  --base-domain DOMAIN      FOG_BASE_DOMAIN        none
  --yes                     FOG_YES=1              confirm the summary
  --dry-run                                        print pct create and stop
  --help

Passing --admin-user and --admin-password completes Fog's first-run wizard
unattended; the dashboard is then ready to sign in to. Passwords given on the
command line are visible in this node's process list — prefer the env var, or
leave them out and use the wizard.
EOF
}

# A value either came attached with = or is the next argument. optval sets
# globals rather than printing, because a $(...) call would lose the shift.
optval() {
  if [ "$HAS_VAL" = 1 ]; then OPTVAL="$VAL"; SHIFT=1
  else [ $# -ge 2 ] || die "$KEY needs a value"; OPTVAL="$2"; SHIFT=2; fi
}

while [ $# -gt 0 ]; do
  KEY="$1"; VAL=""; HAS_VAL=0; SHIFT=1; OPTVAL=""
  case "$KEY" in --*=*) VAL="${KEY#*=}"; KEY="${KEY%%=*}"; HAS_VAL=1 ;; esac
  case "$KEY" in
    --ctid)             optval "$@"; CTID="$OPTVAL" ;;
    --hostname)         optval "$@"; CT_HOSTNAME="$OPTVAL" ;;
    --root-password)    optval "$@"; CT_PASSWORD="$OPTVAL" ;;
    --disk)             optval "$@"; DISK="$OPTVAL" ;;
    --cores)            optval "$@"; CORES="$OPTVAL" ;;
    --ram|--memory)     optval "$@"; RAM="$OPTVAL" ;;
    --swap)             optval "$@"; SWAP="$OPTVAL" ;;
    --storage)          optval "$@"; STORAGE="$OPTVAL" ;;
    --template-storage) optval "$@"; TEMPLATE_STORAGE="$OPTVAL" ;;
    --template)         optval "$@"; TEMPLATE="$OPTVAL" ;;
    --privileged)       UNPRIVILEGED=0 ;;
    --no-onboot)        ONBOOT=0 ;;
    --tags)             optval "$@"; TAGS="$OPTVAL" ;;
    --bridge)           optval "$@"; BRIDGE="$OPTVAL" ;;
    --ipv4|--ip)        optval "$@"; IPV4="$OPTVAL" ;;
    --gateway|--gw)     optval "$@"; GATEWAY="$OPTVAL" ;;
    --ipv6)             optval "$@"; IPV6="$OPTVAL" ;;
    --vlan)             optval "$@"; VLAN="$OPTVAL" ;;
    --mtu)              optval "$@"; MTU="$OPTVAL" ;;
    --dns|--nameserver) optval "$@"; DNS="$OPTVAL" ;;
    --searchdomain)     optval "$@"; SEARCHDOMAIN="$OPTVAL" ;;
    --ssh-keys)         optval "$@"; SSH_KEYS="$OPTVAL" ;;
    --port)             optval "$@"; FOG_PORT="$OPTVAL" ;;
    --source|--repo-url) optval "$@"; FOG_SOURCE="$OPTVAL"; SOURCE_GIVEN=1 ;;
    --branch)           optval "$@"; FOG_BRANCH="$OPTVAL" ;;
    --manifest-url)     optval "$@"; FOG_MANIFEST_URL="$OPTVAL" ;;
    --raw-base)         optval "$@"; FOG_RAW_BASE="$OPTVAL" ;;
    --timezone|--tz)    optval "$@"; FOG_TZ="$OPTVAL" ;;
    --admin-user)       optval "$@"; FOG_ADMIN_USER="$OPTVAL" ;;
    --admin-password)   optval "$@"; FOG_ADMIN_PASSWORD="$OPTVAL" ;;
    --server-name)      optval "$@"; FOG_SERVER_NAME="$OPTVAL" ;;
    --base-domain)      optval "$@"; FOG_BASE_DOMAIN="$OPTVAL" ;;
    --yes|-y)           ASSUME_YES=1 ;;
    --dry-run)          DRY_RUN=1 ;;
    --help|-h)          usage; exit 0 ;;
    *)                  usage >&2; die "unknown option $KEY" ;;
  esac
  shift "$SHIFT"
done

# >>> fog resolver ------------------------------------------------------------
# This block is identical in install.sh, proxmox-lxc.sh and proxmox-vm.sh
# (scripts/test-source-resolution fails if the copies differ), and it is the
# shell twin of internal/selfupdate/resolve.go: the same source shapes in
# the same order, the same signature check against the same pinned key, the
# same age limit (shellsource_test.go runs the two against each other). The
# Proxmox scripts carry it so that nothing fetched is ever executed on the
# node: they resolve and verify the release here, and take install.sh from
# it only after its sha256 matches the signed manifest.
#
# In:  SOURCE BRANCH MANIFEST_URL RAW_BASE RELEASE_KEYS ALLOW_UNSIGNED
#      ALLOW_OLD SOURCE_GIVEN DEFAULT_SOURCE WORK, and the script's die/say.
# Out: fog_normalize sets SRC SCHEME HOST OWNER REPO NSEGS; fog_resolve sets
#      MANIFEST_URL RAW_BASE MANIFEST VERSION ISSUED.
# POSIX sh, and safe under bash's set -euo pipefail. No unquoted expansion
# is ever globbed: lists are read line by line.

# A release manifest older than this is refused (resolve.go's MaxManifestAge):
# 180 days. A source that can't offer anything newer is withholding updates,
# and a fresh install must not land on a release that old.
MAX_MANIFEST_AGE=15552000

# The source is written into a systemd unit and handed to curl, so it is
# held to the characters a URL needs and nothing a unit file or a shell
# would read as syntax (resolve.go's allowedURLRune is the same set).
valid_url() {
  case "$1" in http://?*|https://?*) ;; *) return 1 ;; esac
  case "${1#*://}" in *[!]A-Za-z0-9._~:/%+=,[-]*) return 1 ;; esac
  return 0
}

# Release versions, as version.go's parseVersion and compareVersions have
# them (semver 2.0.0): vMAJOR.MINOR.PATCH[-prerelease][+build], no leading
# zeros in numbers, build metadata ignored. shellsource_test.go runs these
# against the Go over the spec's own precedence chain.
semver_valid() { # <version>
  sv_v=${1#v}
  [ "$sv_v" != "$1" ] || return 1
  sv_v=${sv_v%%+*}
  case "$sv_v" in ''|*[!0-9A-Za-z.-]*) return 1 ;; esac
  sv_core=${sv_v%%-*}
  sv_pre=""
  case "$sv_v" in *-*) sv_pre=${sv_v#*-}; [ -n "$sv_pre" ] || return 1 ;; esac
  case "$sv_core" in *.*.*.*|*..*|.*|*.) return 1 ;; *.*.*) ;; *) return 1 ;; esac
  sv_rest="$sv_core."
  while [ -n "$sv_rest" ]; do
    sv_x=${sv_rest%%.*}; sv_rest=${sv_rest#*.}
    case "$sv_x" in ''|*[!0-9]*) return 1 ;; 0) ;; 0*) return 1 ;; esac
    [ "${#sv_x}" -le 9 ] || return 1
  done
  [ -n "$sv_pre" ] || return 0
  case "$sv_pre" in .*|*.|*..*) return 1 ;; esac
  sv_rest="$sv_pre."
  while [ -n "$sv_rest" ]; do
    sv_x=${sv_rest%%.*}; sv_rest=${sv_rest#*.}
    case "$sv_x" in '') return 1 ;; *[!0-9]*) ;; 0) ;; 0*) return 1 ;; esac
  done
  return 0
}

# semver_ident_cmp <a> <b> → -1, 0 or 1: numeric identifiers as numbers,
# before any alphanumeric one; text in ASCII order.
semver_ident_cmp() {
  if [ "$1" = "$2" ]; then echo 0; return; fi
  case "$1" in *[!0-9]*) si_a=t ;; *) si_a=n ;; esac
  case "$2" in *[!0-9]*) si_b=t ;; *) si_b=n ;; esac
  if [ "$si_a" = n ] && [ "$si_b" = n ]; then
    if [ "${#1}" -lt "${#2}" ]; then echo -1; return; fi
    if [ "${#1}" -gt "${#2}" ]; then echo 1; return; fi
  elif [ "$si_a" = n ]; then echo -1; return
  elif [ "$si_b" = n ]; then echo 1; return
  fi
  if [ "$(printf '%s\n%s\n' "$1" "$2" | LC_ALL=C sort | head -n 1)" = "$1" ]; then echo -1; else echo 1; fi
}

# semver_cmp <a> <b> → -1, 0 or 1 by precedence; both must be semver_valid.
semver_cmp() {
  sc_a=${1#v}; sc_a=${sc_a%%+*}
  sc_b=${2#v}; sc_b=${sc_b%%+*}
  sc_ac=${sc_a%%-*}; sc_bc=${sc_b%%-*}
  sc_ap=""; case "$sc_a" in *-*) sc_ap=${sc_a#*-} ;; esac
  sc_bp=""; case "$sc_b" in *-*) sc_bp=${sc_b#*-} ;; esac
  for _ in 1 2 3; do
    sc_x=${sc_ac%%.*}; sc_y=${sc_bc%%.*}
    sc_ac=${sc_ac#*.}; sc_bc=${sc_bc#*.}
    if [ "$sc_x" -lt "$sc_y" ]; then echo -1; return; fi
    if [ "$sc_x" -gt "$sc_y" ]; then echo 1; return; fi
  done
  if [ "$sc_ap" = "$sc_bp" ]; then echo 0; return; fi
  if [ -z "$sc_ap" ]; then echo 1; return; fi
  if [ -z "$sc_bp" ]; then echo -1; return; fi
  sc_ap="$sc_ap."; sc_bp="$sc_bp."
  while [ -n "$sc_ap" ] && [ -n "$sc_bp" ]; do
    sc_x=${sc_ap%%.*}; sc_ap=${sc_ap#*.}
    sc_y=${sc_bp%%.*}; sc_bp=${sc_bp#*.}
    sc_c=$(semver_ident_cmp "$sc_x" "$sc_y")
    if [ "$sc_c" != 0 ]; then echo "$sc_c"; return; fi
  done
  if [ -z "$sc_ap" ] && [ -z "$sc_bp" ]; then echo 0
  elif [ -z "$sc_ap" ]; then echo -1
  else echo 1
  fi
}

# Normalized exactly as resolve.go's parseSource: lower-case host, empty path
# segments collapsed, no trailing slash.
fog_normalize() {
  valid_url "$SOURCE" || die "the source (FOG_SOURCE, --source) must be an http:// or https:// URL without spaces, quotes, ?, # or @ (got '$SOURCE')."
  [ -z "$MANIFEST_URL" ] || valid_url "$MANIFEST_URL" || die "'$MANIFEST_URL' isn't a URL fog will use (FOG_MANIFEST_URL)."
  [ -z "$RAW_BASE" ] || valid_url "$RAW_BASE" || die "'$RAW_BASE' isn't a URL fog will use (FOG_RAW_BASE)."
  case "$BRANCH" in
    ''|/*|-*|*..*|*[!A-Za-z0-9._/-]*) die "branch '$BRANCH' isn't a branch name fog will use." ;;
  esac
  SCHEME=${SOURCE%%://*}
  fog_rest=${SOURCE#*://}
  HOST=$(printf '%s' "${fog_rest%%/*}" | tr 'A-Z' 'a-z')
  [ -n "$HOST" ] || die "the source names no host (got '$SOURCE')."
  case "$fog_rest" in
    */*) fog_path=$(printf '%s' "${fog_rest#*/}" | tr -s '/' | sed -e 's|^/||' -e 's|/$||') ;;
    *) fog_path="" ;;
  esac
  SRC="$SCHEME://$HOST${fog_path:+/$fog_path}"
  OWNER=$(printf '%s' "$fog_path" | cut -d/ -f1)
  REPO=$(printf '%s' "$fog_path" | cut -d/ -f2)
  NSEGS=$(printf '%s\n' "$fog_path" | awk -F/ '{print ($1 == "" ? 0 : NF)}')
}

# Each candidate is "<manifest url>|<raw base>": one guess at how this host
# lays out files, paired with where repo-relative files would live under the
# same guess. They are tried in order and the first manifest that fetches
# and verifies decides both — the same list, in the same order, as
# resolve.go's candidates(), because a URL alone can't tell a repo apart
# from a directory.
candidates() {
  case "$SRC" in
    *latest.json) echo "$SRC|${SRC%/*}"; return ;;
  esac
  if [ "$NSEGS" -ge 2 ]; then
    case "$HOST" in
      github.com|www.github.com)
        # Only the release: its manifest is the relative one release.sh signs
        # for the release directory. The repo's committed release/latest.json
        # names the host the release was cut on, which a GitHub install may
        # not reach — or may reach as something else.
        echo "$SRC/releases/latest/download/latest.json|https://raw.githubusercontent.com/$OWNER/$REPO/$BRANCH"
        ;;
      *gitlab*)
        echo "$SRC/-/raw/$BRANCH/release/latest.json|$SRC/-/raw/$BRANCH"
        ;;
      *)
        # Gitea/Forgejo, and the default assumption for anything unknown.
        echo "$SRC/raw/branch/$BRANCH/release/latest.json|$SRC/raw/branch/$BRANCH"
        ;;
    esac
  fi
  echo "$SRC/latest.json|$SRC"
}

# Any download: http(s) only, failing on an HTTP error, bounded in size.
fetch() { # <url> <file> <max bytes>
  curl -fsSL --proto '=http,https' --proto-redir '=http,https' --connect-timeout 15 \
    --max-filesize "$3" -o "$2" "$1"
}

sha256_of() {
  { sha256sum "$1" 2>/dev/null || shasum -a 256 "$1" 2>/dev/null; } | awk '{print $1}'
}

# An openssl that knows ed25519 (OpenSSL 1.1.1+; macOS's LibreSSL doesn't).
OPENSSL=""
find_openssl() {
  fog_n=0
  for fog_k in $RELEASE_KEYS; do
    fog_n=$((fog_n + 1))
    printf -- '-----BEGIN PUBLIC KEY-----\n%s\n-----END PUBLIC KEY-----\n' "$fog_k" > "$WORK/key$fog_n.pem"
  done
  [ "$fog_n" -gt 0 ] || return 1
  for fog_o in openssl /opt/homebrew/bin/openssl /opt/homebrew/opt/openssl@3/bin/openssl /usr/local/opt/openssl@3/bin/openssl; do
    command -v "$fog_o" >/dev/null 2>&1 || continue
    if "$fog_o" pkey -pubin -in "$WORK/key1.pem" -noout >/dev/null 2>&1; then
      OPENSSL=$fog_o
      return 0
    fi
  done
  return 1
}

# verify_manifest <manifest file> <signature file>: the signature is base64
# of an ed25519 signature over the manifest's exact bytes.
verify_manifest() {
  base64 -d < "$2" > "$WORK/sig.bin" 2>/dev/null || return 1
  [ "$(wc -c < "$WORK/sig.bin" | tr -d ' ')" = 64 ] || return 1
  fog_i=0
  while [ -f "$WORK/key$((fog_i + 1)).pem" ]; do
    fog_i=$((fog_i + 1))
    if "$OPENSSL" pkeyutl -verify -pubin -inkey "$WORK/key$fog_i.pem" -rawin -in "$1" -sigfile "$WORK/sig.bin" >/dev/null 2>&1; then
      return 0
    fi
  done
  return 1
}

# try_manifest <url>: fetch it and its .sig; succeed only for a verified,
# recent release manifest. WHY says what was wrong with the last one tried.
WHY=""
try_manifest() {
  rm -f "$WORK/latest.json" "$WORK/latest.json.sig"
  if ! fetch "$1" "$WORK/latest.json" 1048576 2>/dev/null; then WHY="not found"; return 1; fi
  if ! grep -q '"version"' "$WORK/latest.json"; then WHY="not a release manifest"; return 1; fi
  if [ "$ALLOW_UNSIGNED" != 1 ]; then
    if ! fetch "$1.sig" "$WORK/latest.json.sig" 4096 2>/dev/null; then WHY="unsigned (no latest.json.sig beside it)"; return 1; fi
    if ! verify_manifest "$WORK/latest.json" "$WORK/latest.json.sig"; then WHY="signature doesn't match fog's release key"; return 1; fi
  fi
  # The issue date is inside the signed bytes, so it can't be moved.
  fog_iss=$(tr -d ' \t\r\n' < "$WORK/latest.json" | sed -n 's/.*"issued_at":\([0-9]\{1,12\}\)[,}].*/\1/p')
  [ "$ALLOW_OLD" != 1 ] || return 0
  if [ -z "$fog_iss" ]; then WHY="undated (made before releases carried issued_at)"; return 1; fi
  fog_age=$(( $(date +%s) - fog_iss ))
  if [ "$fog_age" -gt "$MAX_MANIFEST_AGE" ]; then
    WHY="issued $((fog_age / 86400)) days ago, past the 180-day limit"
    return 1
  fi
  return 0
}

# fog_resolve: the release manifest for the source, verified. Prints what it
# tried and returns 1 when nothing qualifies.
fog_resolve() {
  case "$SRC" in
    *latest.json) [ -n "$MANIFEST_URL" ] || MANIFEST_URL="$SRC" ;;
  esac
  if [ -n "$MANIFEST_URL" ]; then
    if ! try_manifest "$MANIFEST_URL"; then
      echo "fog: no usable release manifest at $MANIFEST_URL ($WHY)." >&2
      return 1
    fi
    [ -n "$RAW_BASE" ] || RAW_BASE="${MANIFEST_URL%/*}"
  else
    fog_found=""
    fog_tried=""
    fog_cands=$(candidates)
    while IFS= read -r fog_c; do
      if try_manifest "${fog_c%%|*}"; then
        fog_found=1; MANIFEST_URL="${fog_c%%|*}"
        [ -n "$RAW_BASE" ] || RAW_BASE=${fog_c#*|}
        break
      fi
      fog_tried="$fog_tried
fog:   ${fog_c%%|*} ($WHY)"
    done <<EOF
$fog_cands
EOF
    if [ -z "$fog_found" ]; then
      echo "fog: no release manifest found under $SRC. Tried:$fog_tried" >&2
      return 1
    fi
  fi
  MANIFEST=$(tr -d ' \t\r\n' < "$WORK/latest.json")
  VERSION=$(printf '%s' "$MANIFEST" | sed -n 's/.*"version":"\([^"]*\)".*/\1/p')
  ISSUED=$(printf '%s' "$MANIFEST" | sed -n 's/.*"issued_at":\([0-9]\{1,12\}\)[,}].*/\1/p')
  semver_valid "$VERSION" || die "$MANIFEST_URL names version '$VERSION', which isn't a release version."
}

# artifact <key> → "<url> <sha256>", the url made absolute. Relative names
# resolve against the manifest's own URL, so a release directory can be
# copied to any host and nothing inside it needs rewriting.
artifact() {
  fog_key=$(printf '%s' "$1" | sed 's/[.]/\\./g')
  fog_url=$(printf '%s' "$MANIFEST" | sed -n "s/.*\"$fog_key\":{\"url\":\"\([^\"]*\)\",\"sha256\":\"[0-9a-f]\{64\}\"}.*/\1/p")
  fog_sha=$(printf '%s' "$MANIFEST" | sed -n "s/.*\"$fog_key\":{\"url\":\"[^\"]*\",\"sha256\":\"\([0-9a-f]\{64\}\)\"}.*/\1/p")
  [ -n "$fog_url" ] && [ -n "$fog_sha" ] || return 1
  case "$fog_url" in
    http://*|https://*) valid_url "$fog_url" || return 1 ;;
    *[!A-Za-z0-9._-]*) return 1 ;;
    *) fog_url="${MANIFEST_URL%/*}/$fog_url" ;;
  esac
  printf '%s %s\n' "$fog_url" "$fog_sha"
}

# Where an artifact may be fetched from, in the updater's order
# (selfupdate.go's fetchArtifact): the manifest's URL; the same path on the
# source's own origin (a git host names its links by its configured root,
# which this network may reach only by IP); for a GitHub source, the file in
# that repo's release of this version (a committed manifest that names
# another host's downloads still installs from GitHub); the file beside the
# manifest (a mirror serving a manifest whose URLs still name the original
# host). The sha256 decides whether what came back is the artifact,
# wherever it was.
artifact_urls() { # <url>
  printf '%s\n' "$1"
  case "$1" in "$SCHEME://$HOST/"*) ;; *) printf '%s\n' "$SCHEME://$HOST/${1#*://*/}" ;; esac
  case "$HOST" in
    github.com|www.github.com)
      [ "$NSEGS" -lt 2 ] || [ -z "${VERSION:-}" ] \
        || printf '%s\n' "https://github.com/$OWNER/$REPO/releases/download/$VERSION/${1##*/}" ;;
  esac
  [ "${MANIFEST_URL%/*}/${1##*/}" = "$1" ] || printf '%s\n' "${MANIFEST_URL%/*}/${1##*/}"
}

# fog_get <"url sha"> <dest> <max bytes>: the file, checked against the
# manifest; a file that doesn't match is never left at dest.
fog_get() {
  fog_gurl=${1% *}; fog_gsha=${1#* }
  fog_alts=$(artifact_urls "$fog_gurl")
  fog_why=""
  # A location that answers with the wrong bytes (another server where the
  # manifest's host used to be, a captive portal) is passed over like one
  # that doesn't answer; only the manifest's sha256 is ever accepted.
  while IFS= read -r fog_u; do
    if fetch "$fog_u" "$2" "$3" 2>/dev/null; then
      if [ "$(sha256_of "$2")" = "$fog_gsha" ]; then
        return 0
      fi
      rm -f "$2"
      fog_why="$fog_why
fog:   $fog_u (not the manifest's sha256)"
    else
      fog_why="$fog_why
fog:   $fog_u (no answer)"
    fi
  done <<EOF
$fog_alts
EOF
  die "no download of ${fog_gurl##*/} matches the manifest — refusing to install it. Tried:$fog_why"
}

# fog_reachable <url>: the first of its artifact_urls that answers, else the
# url itself — the host a download will really come from.
fog_reachable() {
  fog_alts=$(artifact_urls "$1")
  while IFS= read -r fog_u; do
    if curl -fsS --proto '=http,https' --connect-timeout 10 -r 0-0 -o /dev/null "$fog_u" 2>/dev/null; then
      printf '%s\n' "$fog_u"
      return 0
    fi
  done <<EOF
$fog_alts
EOF
  printf '%s\n' "$1"
}

# Said whenever nothing resolves and nobody chose the source: the default is
# fog's public releases, and someone installing from elsewhere needs to know
# which knob points there.
default_source_hint() {
  [ "$SOURCE_GIVEN" != 1 ] || return 0
  cat >&2 <<EOF
fog: No source was given, so this used fog's public releases at
fog: $DEFAULT_SOURCE. If you install fog from somewhere else (your own
fog: mirror, a fork, a server on your network), point FOG_SOURCE (the Proxmox
fog: scripts: --source) at it:
fog:   curl -fsSL <that place>/install.sh | FOG_SOURCE=<that place> sh
fog: (docs/hosting-the-source.md lists every shape a source can take.)
EOF
}
# <<< fog resolver ------------------------------------------------------------

# -------------------------------------------------------------- preflight ---

header

[ "$(id -u)" -eq 0 ] || die "run this as root on the Proxmox node."
command -v pveversion >/dev/null 2>&1 || \
  die "this doesn't look like a Proxmox VE host (no pveversion). Run it in the node's shell."
for b in pct pvesm pveam; do
  command -v "$b" >/dev/null 2>&1 || die "$b is missing — is this a full PVE install?"
done

PVE_VER="$(pveversion | sed -n 's|^pve-manager/\([0-9.]*\).*|\1|p')"
case "${PVE_VER%%.*}" in
  8|9) ;;
  *) warn "untested on Proxmox VE ${PVE_VER:-unknown} — this script targets 8.x and 9.x." ;;
esac

ARCH="$(dpkg --print-architecture)"
case "$ARCH" in
  amd64|arm64) ;;
  *) die "unsupported node architecture $ARCH (fogd ships linux-amd64 and linux-arm64)." ;;
esac

INTERACTIVE=0
if [ -t 0 ] && [ -t 1 ] && command -v whiptail >/dev/null 2>&1; then INTERACTIVE=1; fi

# ------------------------------------------------------- storage discovery ---

# `pvesm status -content X` lists only storages that can hold that content —
# picking from it is how a wrong answer becomes impossible rather than a later
# error from pct.
storages_for() {
  pvesm status -content "$1" 2>/dev/null | awk 'NR>1 && $3=="active" {print $1}'
}

# Sets PICKED. Like optval, it deliberately isn't a subshell: a die() in
# $(...) would surface as an "error at line N" instead of its own message.
pick_storage() {  # <content> <prompt> <preselected>
  local content=$1 prompt=$2 chosen=$3 list menu=() s
  list=$(storages_for "$content")
  [ -n "$list" ] || die "no active storage on this node accepts '$content' content."
  if [ -n "$chosen" ]; then
    printf '%s\n' "$list" | grep -qx -- "$chosen" || \
      die "storage '$chosen' is not active or doesn't accept '$content' content. Available: $(printf '%s' "$list" | tr '\n' ' ')"
    PICKED="$chosen"; return
  fi
  if [ "$(printf '%s\n' "$list" | wc -l)" -eq 1 ]; then PICKED="$list"; return; fi
  if [ "$INTERACTIVE" = 1 ]; then
    while read -r s; do
      menu+=("$s" "$(pvesm status -storage "$s" | awk 'NR>1 {printf "%s, %.0f GB free", $2, $6/1048576}')")
    done <<< "$list"
    PICKED=$(whiptail --backtitle "fog" --title "$prompt" --menu "$prompt" 16 70 6 \
      "${menu[@]}" 3>&1 1>&2 2>&3) || die "cancelled."
    return
  fi
  # No terminal: prefer the conventional local ones, else the first listed.
  for s in local-lvm local-zfs local; do
    printf '%s\n' "$list" | grep -qx -- "$s" && { PICKED="$s"; return; }
  done
  PICKED=$(printf '%s\n' "$list" | head -1)
}

# ------------------------------------------------------------ the wizard ---

next_ctid() {
  local id
  id=$(pvesh get /cluster/nextid 2>/dev/null | tr -dc '0-9') || true
  [ -n "$id" ] || { id=100; while pct status "$id" >/dev/null 2>&1 || qm status "$id" >/dev/null 2>&1; do id=$((id+1)); done; }
  printf '%s' "$id"
}

ask() {  # <varname> <prompt> [default]
  local var=$1 prompt=$2 def=${3:-} out
  out=$(whiptail --backtitle "fog" --title "Fog LXC" --inputbox "$prompt" 10 70 "$def" 3>&1 1>&2 2>&3) \
    || die "cancelled."
  printf -v "$var" '%s' "$out"
}

[ -n "$CTID" ] || CTID=$(next_ctid)
[ -n "$FOG_TZ" ] || FOG_TZ=$(timedatectl show -p Timezone --value 2>/dev/null || echo UTC)

if [ "$INTERACTIVE" = 1 ] && [ "$ASSUME_YES" != 1 ]; then
  choice=$(whiptail --backtitle "fog" --title "Fog on Proxmox" --menu \
    "A new Fog server as an LXC container on this node." 14 70 3 \
    "default"  "CT $CTID · fog · 2 cores · 4 GB · 32 GB · DHCP" \
    "advanced" "Choose every setting" \
    "cancel"   "Change nothing" 3>&1 1>&2 2>&3) || die "cancelled."
  case "$choice" in
    cancel) msg "nothing was created."; exit 0 ;;
    advanced)
      ask CTID        "Container ID"                              "$CTID"
      ask CT_HOSTNAME "Hostname (also http://<name>.local)"       "$CT_HOSTNAME"
      ask DISK        "Root disk size in GB (apps and backups live here)" "$DISK"
      ask CORES       "CPU cores"                                 "$CORES"
      ask RAM         "Memory in MB"                              "$RAM"
      ask SWAP        "Swap in MB"                                "$SWAP"
      pick_storage rootdir "Storage for the container" "$STORAGE"; STORAGE="$PICKED"
      ask BRIDGE      "Network bridge"                            "$BRIDGE"
      ask IPV4        "IPv4: 'dhcp' or a static address like 192.168.1.50/24" "$IPV4"
      case "$IPV4" in dhcp) ;; *) ask GATEWAY "Gateway" "$GATEWAY" ;; esac
      ask VLAN        "VLAN tag (blank for none)"                 "$VLAN"
      ask DNS         "DNS server (blank to use this node's)"     "$DNS"
      ask FOG_TZ      "Timezone"                                  "$FOG_TZ"
      ask FOG_PORT    "Port for the Fog dashboard"                "$FOG_PORT"
      ask FOG_SOURCE  "Where Fog is hosted (repo URL, directory, or latest.json)" "$FOG_SOURCE"
      if whiptail --backtitle "fog" --title "Root password" --yesno \
        "Set a root password inside the container?\n\nWithout one, root has no password and you reach the shell with 'pct enter $CTID' from this node." \
        12 70 --defaultno; then
        CT_PASSWORD=$(whiptail --backtitle "fog" --title "Root password" --passwordbox \
          "Root password for the container" 10 70 3>&1 1>&2 2>&3) || die "cancelled."
      fi
      if whiptail --backtitle "fog" --title "Admin account" --yesno \
        "Create Fog's admin account now instead of in the browser?\n\nAnswering no leaves the first-run wizard waiting at http://<ip>:$FOG_PORT." \
        12 70 --defaultno; then
        ask FOG_ADMIN_USER "Fog admin username" "${FOG_ADMIN_USER:-admin}"
        FOG_ADMIN_PASSWORD=$(whiptail --backtitle "fog" --title "Admin account" --passwordbox \
          "Fog admin password (10 characters or more)" 10 70 3>&1 1>&2 2>&3) || die "cancelled."
      fi
      ;;
  esac
fi

# ----------------------------------------------------------- validation ---

# A source typed into the advanced dialog was chosen too.
[ "$FOG_SOURCE" = "$DEFAULT_SOURCE" ] || SOURCE_GIVEN=1

printf '%s' "$CTID" | grep -Eq '^[0-9]{3,}$' || die "container id must be a number of 100 or more (got '$CTID')."
printf '%s' "$CT_HOSTNAME" | grep -Eq '^[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$' || \
  die "hostname '$CT_HOSTNAME' isn't a valid DNS label."
printf '%s' "$DISK"  | grep -Eq '^[0-9]+$' || die "disk size must be a whole number of GB."
printf '%s' "$CORES" | grep -Eq '^[0-9]+$' || die "cores must be a number."
printf '%s' "$RAM"   | grep -Eq '^[0-9]+$' || die "memory must be a number of MB."
printf '%s' "$SWAP"  | grep -Eq '^[0-9]+$' || die "swap must be a number of MB."
printf '%s' "$FOG_PORT" | grep -Eq '^[0-9]{2,5}$' && [ "$FOG_PORT" -le 65535 ] || die "port must be a number from 10 to 65535."
# Everything below reaches pct/qm option strings (comma-separated), a
# cloud-init or unit file, or a URL: each is held to what it can be.
valid_url "$FOG_SOURCE" || die "--source must be an http:// or https:// URL without spaces, quotes, ?, # or @ (got '$FOG_SOURCE')."
for v in "$FOG_MANIFEST_URL" "$FOG_RAW_BASE"; do
  [ -z "$v" ] || valid_url "$v" || die "'$v' isn't a URL fog will use (--manifest-url / --raw-base)."
done
case "$FOG_BRANCH" in
  ''|/*|-*|*..*|*[!A-Za-z0-9._/-]*) die "--branch '$FOG_BRANCH' isn't a branch name fog will use." ;;
esac
printf '%s' "$BRIDGE" | grep -Eq '^[A-Za-z0-9_.-]+$' || die "bridge '$BRIDGE' isn't a bridge name."
[ -z "$VLAN" ] || printf '%s' "$VLAN" | grep -Eq '^[0-9]{1,4}$' || die "VLAN must be a number."
[ -z "$MTU" ]  || printf '%s' "$MTU" | grep -Eq '^[0-9]{3,5}$' || die "MTU must be a number."
[ -z "$GATEWAY" ] || printf '%s' "$GATEWAY" | grep -Eq '^[0-9A-Fa-f:.]+$' || die "gateway '$GATEWAY' isn't an IP address."
[ -z "$DNS" ] || printf '%s' "$DNS" | grep -Eq '^[0-9A-Fa-f:. ]+$' || die "DNS '$DNS' isn't a list of IP addresses."
[ -z "$SEARCHDOMAIN" ] || printf '%s' "$SEARCHDOMAIN" | grep -Eq '^[A-Za-z0-9. -]+$' || die "search domain '$SEARCHDOMAIN' isn't a domain."
[ -z "$FOG_TZ" ] || { printf '%s' "$FOG_TZ" | grep -Eq '^[A-Za-z0-9_+/-]+$' && case "$FOG_TZ" in *..*) false ;; esac; } \
  || die "timezone '$FOG_TZ' isn't a zone name like Europe/London."
[ "$DISK" -ge 8 ]   || die "8 GB is the floor for the root disk; Docker images alone outgrow less."
[ "$RAM"  -ge 1024 ] || die "1024 MB is the floor for memory."
[ "$RAM"  -ge 2048 ] || warn "$RAM MB is tight once apps run; 4096 is comfortable."
[ "$DISK" -ge 16 ]  || warn "$DISK GB fills quickly once images and backups accumulate."

if pct status "$CTID" >/dev/null 2>&1; then die "container $CTID already exists on this node."; fi
if qm status "$CTID" >/dev/null 2>&1; then die "VM $CTID already exists — pick another id."; fi

case "$IPV4" in
  dhcp) [ -z "$GATEWAY" ] || warn "gateway is ignored with DHCP." ;;
  */*)  printf '%s' "$IPV4" | grep -Eq '^[0-9.]+/[0-9]{1,2}$' || die "--ipv4 '$IPV4' isn't an address with a prefix, like 192.168.1.50/24."
        [ -n "$GATEWAY" ] || die "a static IPv4 needs --gateway." ;;
  *)    die "--ipv4 must be 'dhcp' or an address with a prefix, like 192.168.1.50/24." ;;
esac
case "$IPV6" in
  none|auto|dhcp) ;;
  */*) printf '%s' "$IPV6" | grep -Eq '^[0-9A-Fa-f:]+/[0-9]{1,3}$' || die "--ipv6 '$IPV6' isn't an address with a prefix." ;;
  *) die "--ipv6 must be none, auto, dhcp, or an address/prefix." ;;
esac

if [ -n "$FOG_ADMIN_USER" ] || [ -n "$FOG_ADMIN_PASSWORD" ]; then
  [ -n "$FOG_ADMIN_USER" ] && [ -n "$FOG_ADMIN_PASSWORD" ] || \
    die "preseeding the admin account needs both --admin-user and --admin-password."
  # Fog's own rule, checked here so it fails in a second rather than at the
  # end of an otherwise complete install.
  [ "${#FOG_ADMIN_PASSWORD}" -ge 10 ] || die "Fog requires an admin password of 10 characters or more."
fi
if [ -n "$SSH_KEYS" ]; then
  [ -f "$SSH_KEYS" ] || die "ssh key file '$SSH_KEYS' not found."
fi
if ! ip link show "$BRIDGE" >/dev/null 2>&1; then
  die "bridge '$BRIDGE' doesn't exist on this node."
fi

pick_storage rootdir "Storage for the container" "$STORAGE"; STORAGE="$PICKED"
pick_storage vztmpl "Storage for the container template" "$TEMPLATE_STORAGE"; TEMPLATE_STORAGE="$PICKED"

# Docker's overlay2 driver can't stack on a ZFS or Ceph-backed container root
# in an unprivileged namespace; fuse-overlayfs is the working substitute and
# needs the fuse feature on the container. Anything else (dir, LVM-thin, ext4)
# takes overlay2 directly.
STORAGE_TYPE=$(pvesm status -storage "$STORAGE" | awk 'NR>1 {print $2}')
USE_FUSE_OVERLAYFS=0
case "$STORAGE_TYPE" in
  zfspool|rbd|cephfs) [ "$UNPRIVILEGED" = 1 ] && USE_FUSE_OVERLAYFS=1 ;;
esac

# --------------------------------------------------------- the template ---

if [ -z "$TEMPLATE" ]; then
  msg "refreshing the template catalog"
  pveam update >/dev/null 2>&1 || warn "pveam update failed — falling back to the cached catalog."
  TEMPLATE=$(pveam available --section system 2>/dev/null \
    | awk '{print $2}' | grep -E "^debian-13-standard_.*_${ARCH}\.tar\.(zst|gz|xz)$" \
    | sort -V | tail -1)
  [ -n "$TEMPLATE" ] || die "no debian-13-standard template for $ARCH in the catalog — pass --template."
fi
TEMPLATE_REF="$TEMPLATE_STORAGE:vztmpl/$TEMPLATE"

# ------------------------------------------------------- where fog lives ---

# Source resolution itself is the resolver block near the top of this
# script (docs/hosting-the-source.md is the contract). These turn its
# failures into what to change on the node.
url_host() {  # <url> → its hostname, no scheme, port, or path
  printf '%s' "$1" | sed -e 's|^[a-zA-Z][a-zA-Z0-9+.-]*://||' -e 's|[:/].*||'
}

# The node's IPv4 address for a name, from whatever told the node — DNS or its
# own /etc/hosts. Empty when the node can't resolve it either.
node_addr() {  # <host>
  getent ahostsv4 "$1" 2>/dev/null | awk 'NR==1 {print $1}'
}

# Why a source host can't be used, in terms of what to change. The usual
# cause is a .lan name only the LAN's own resolver knows, on a node that was
# set up pointing at the router or a public resolver.
unresolvable_hint() {  # <host>
  cat <<EOF
     this node can't resolve '$1'. Either:
       · point the node at the resolver that knows it (Datacenter → node →
         System → DNS), or
       · add it to the node's /etc/hosts:  echo '<ip> $1' >> /etc/hosts
     The container is given the same answer, so fixing it here fixes both.
EOF
}

ENVFILE=""; SETUPFILE=""
WORK=$(mktemp -d)
INSTALL_SH="$WORK/install.sh"
trap 'rm -rf "$WORK" ${ENVFILE:+"$ENVFILE"} ${SETUPFILE:+"$SETUPFILE"}' EXIT
# The release is resolved and verified here on the node, by the resolver
# block above — the same code install.sh runs — and nothing fetched is ever
# executed on the node, not even with --dry-run: install.sh is taken from the
# signed manifest and checked against its sha256, and only those verified
# bytes go into the container (where install.sh verifies everything it fetches
# again). It all fails before anything has been created.
SOURCE=$FOG_SOURCE
BRANCH=$FOG_BRANCH
MANIFEST_URL=$FOG_MANIFEST_URL
RAW_BASE=$FOG_RAW_BASE
RELEASE_KEYS="${FOG_RELEASE_KEYS:-MCowBQYDK2VwAyEAzrGQX7hwCDQn0KNY31d+VHb3CU2wIg/qt7CeHpDzMUg=}"
ALLOW_UNSIGNED="${FOG_ALLOW_UNSIGNED:-0}"
ALLOW_OLD="${FOG_ALLOW_OLD_RELEASE:-0}"
fog_normalize
if [ "$ALLOW_UNSIGNED" = 1 ]; then
  warn "FOG_ALLOW_UNSIGNED=1 — the release manifest's signature is NOT checked."
elif ! find_openssl; then
  die "checking fog's release signature needs openssl with ed25519 on this node: apt-get install openssl."
fi
if ! fog_resolve; then
  while read -r h; do
    [ -z "$h" ] || [ -n "$(node_addr "$h")" ] || unresolvable_hint "$h" >&2
  done < <(candidates | while IFS= read -r c; do url_host "${c%%|*}"; echo; done | sort -u)
  default_source_hint
  die "set --source to where you host Fog, or --manifest-url to its latest.json."
fi
MANIFEST_RESOLVED=$MANIFEST_URL
INSTALL_ART=$(artifact install.sh) || \
  die "the release at $MANIFEST_URL lists no install.sh — it predates this script; use a newer release."
INSTALL_SH_URL=${INSTALL_ART%% *}
fog_get "$INSTALL_ART" "$INSTALL_SH" 1048576
chmod 0600 "$INSTALL_SH"
BIN_ART=$(artifact "linux-$ARCH") || die "the release at $MANIFEST_URL has no linux-$ARCH fogd."
# The binary's URL is absolute and can sit on a different host than
# --source — a source given as an IP still hands out http://gitea.lan/...
# downloads — so each host is checked here, before anything is created, not
# ten minutes into the install.
ARTIFACT_URL=$(fog_reachable "${BIN_ART%% *}")
SOURCE_HOSTS=$(for u in "$INSTALL_SH_URL" "$MANIFEST_RESOLVED" "$ARTIFACT_URL"; do
  [ -z "$u" ] || { url_host "$u"; echo; }
done | sort -u)
for h in $SOURCE_HOSTS; do
  [ -n "$(node_addr "$h")" ] || { unresolvable_hint "$h" >&2
    die "the release manifest at $MANIFEST_RESOLVED points downloads at '$h'."; }
done
curl -fsSL --connect-timeout 10 -r 0-0 -o /dev/null "$ARTIFACT_URL" 2>/dev/null || \
  die "the fogd binary the manifest names isn't downloadable from this node: $ARTIFACT_URL"

# ------------------------------------------------------------- summary ---

NET0="name=eth0,bridge=$BRIDGE,ip=$IPV4"
[ "$IPV4" = dhcp ] || NET0="$NET0,gw=$GATEWAY"
[ "$IPV6" = none ] || NET0="$NET0,ip6=$IPV6"
[ -z "$VLAN" ] || NET0="$NET0,tag=$VLAN"
[ -z "$MTU" ]  || NET0="$NET0,mtu=$MTU"

FEATURES="nesting=1"
if [ "$UNPRIVILEGED" = 1 ]; then
  # keyctl: dockerd needs its own session keyring inside a user namespace.
  FEATURES="$FEATURES,keyctl=1"
else
  # A privileged container can mount network shares, which is what Fog's NAS
  # support needs; an unprivileged one never can.
  FEATURES="$FEATURES,mount=nfs;cifs"
fi
[ "$USE_FUSE_OVERLAYFS" = 0 ] || FEATURES="$FEATURES,fuse=1"

create_args=(
  "$CTID" "$TEMPLATE_REF"
  --hostname "$CT_HOSTNAME"
  --arch "$ARCH"
  --ostype debian
  --cores "$CORES" --memory "$RAM" --swap "$SWAP"
  --rootfs "$STORAGE:$DISK"
  --unprivileged "$UNPRIVILEGED"
  --features "$FEATURES"
  --net0 "$NET0"
  --onboot "$ONBOOT"
  --description "Fog — home server control plane. Dashboard on :$FOG_PORT. Created by proxmox-lxc.sh."
)
[ -z "$TAGS" ]         || create_args+=(--tags "$TAGS")
[ -z "$DNS" ]          || create_args+=(--nameserver "$DNS")
[ -z "$SEARCHDOMAIN" ] || create_args+=(--searchdomain "$SEARCHDOMAIN")
[ -z "$SSH_KEYS" ]     || create_args+=(--ssh-public-keys "$SSH_KEYS")

printf '\n   %sthis will create%s\n' "$BOLD" "$RST"
printf '     container   %s  (%s, %s)\n' "$CTID" "$CT_HOSTNAME" \
  "$([ "$UNPRIVILEGED" = 1 ] && echo unprivileged || echo privileged)"
printf '     resources   %s cores · %s MB RAM · %s MB swap · %s GB on %s (%s)\n' \
  "$CORES" "$RAM" "$SWAP" "$DISK" "$STORAGE" "$STORAGE_TYPE"
printf '     network     %s on %s%s\n' "$IPV4" "$BRIDGE" "$([ -n "$VLAN" ] && echo " · VLAN $VLAN")"
printf '     template    %s\n' "$TEMPLATE"
printf '     install.sh  %s\n' "$INSTALL_SH_URL"
printf '     fog         %s · port %s · %s\n' "$FOG_SOURCE" "$FOG_PORT" "$FOG_TZ"
printf '     admin       %s\n\n' \
  "$([ -n "$FOG_ADMIN_USER" ] && echo "$FOG_ADMIN_USER (created during install)" || echo "created in the browser afterwards")"

if [ "$DRY_RUN" = 1 ]; then
  # Printed to be pasted: quote only what a shell would otherwise split.
  printf '   %spct create%s' "$DIM" "$RST"
  for a in "${create_args[@]}"; do
    case "$a" in *[!A-Za-z0-9=/.:_-]*) printf " '%s'" "$a" ;; *) printf ' %s' "$a" ;; esac
  done
  printf '\n\n'
  exit 0
fi

if [ "$ASSUME_YES" != 1 ]; then
  if [ "$INTERACTIVE" = 1 ]; then
    whiptail --backtitle "fog" --title "Create it?" --yesno \
      "Create container $CTID on $STORAGE and install Fog into it?" 9 70 || { msg "nothing was created."; exit 0; }
  elif [ -t 0 ]; then
    printf '   Continue? [y/N] '; read -r reply
    case "$reply" in y|Y|yes) ;; *) msg "nothing was created."; exit 0 ;; esac
  fi
fi

# -------------------------------------------------- template + container ---

if ! pveam list "$TEMPLATE_STORAGE" 2>/dev/null | awk '{print $1}' | grep -qxF -- "$TEMPLATE_REF"; then
  msg "downloading $TEMPLATE"
  pveam download "$TEMPLATE_STORAGE" "$TEMPLATE" >/dev/null || die "template download failed."
  ok "template ready"
fi

msg "creating container $CTID"
pct create "${create_args[@]}" >/dev/null
CREATED=1
ok "container $CTID created"

msg "starting container"
pct start "$CTID" >/dev/null
for _ in $(seq 1 60); do
  [ "$(pct status "$CTID" 2>/dev/null)" = "status: running" ] && break
  sleep 1
done
[ "$(pct status "$CTID")" = "status: running" ] || die "container $CTID didn't reach 'running'."

if [ -n "$CT_PASSWORD" ]; then
  # Through stdin, never argv: `pct exec` arguments are visible in this node's
  # process list to anyone who can run ps. It needs the container running,
  # so it happens here rather than at creation.
  printf 'root:%s\n' "$CT_PASSWORD" | pct exec "$CTID" -- chpasswd \
    || warn "couldn't set the root password; 'pct enter $CTID' still gets you a shell."
fi

# A container is "running" long before its network answers; installing into it
# any earlier just fails on the first apt call.
CT_IP=""
for _ in $(seq 1 90); do
  CT_IP=$(pct exec "$CTID" -- ip -4 -o addr show dev eth0 2>/dev/null | awk '{print $4}' | cut -d/ -f1 || true)
  [ -n "$CT_IP" ] && break
  sleep 1
done
[ -n "$CT_IP" ] || die "container $CTID never got an IPv4 address on eth0 (bridge $BRIDGE, ip=$IPV4)."
ok "network up — $CT_IP"

for _ in $(seq 1 30); do
  pct exec "$CTID" -- getent hosts deb.debian.org >/dev/null 2>&1 && break
  sleep 2
done
pct exec "$CTID" -- getent hosts deb.debian.org >/dev/null 2>&1 || \
  die "the container can't resolve deb.debian.org — check DNS (--dns) and the bridge's route to the internet."

# fogd itself is downloaded by the container — and later updated by it — from
# hosts that are typically LAN names. The node resolved every one of them
# above; where the container can't (a name in the node's /etc/hosts, or a
# --dns that doesn't know it), it gets the node's answer as a line in its own
# /etc/hosts. Proxmox only rewrites its marked block there, so the line stays.
for h in $SOURCE_HOSTS; do
  pct exec "$CTID" -- getent hosts "$h" >/dev/null 2>&1 && continue
  addr=$(node_addr "$h")
  [ -n "$addr" ] || die "the container can't resolve '$h', and neither can this node."
  printf '%s %s\n' "$addr" "$h" | pct exec "$CTID" -- sh -c 'cat >> /etc/hosts'
  warn "the container's DNS doesn't know '$h' — pinned it to $addr in its /etc/hosts."
  warn "    if $h ever moves, edit that line (or fix the container's DNS) or updates will stop."
done

# --------------------------------------------------- install inside the CT ---

ENVFILE=$(mktemp); SETUPFILE=$(mktemp)

{
  printf 'FOG_SOURCE=%q\n'          "$FOG_SOURCE"
  printf 'FOG_BRANCH=%q\n'          "$FOG_BRANCH"
  printf 'FOG_MANIFEST_URL=%q\n'    "$FOG_MANIFEST_URL"
  printf 'FOG_RAW_BASE=%q\n'        "$FOG_RAW_BASE"
  printf 'FOG_RELEASE_KEYS=%q\n'    "${FOG_RELEASE_KEYS:-}"
  printf 'FOG_ALLOW_UNSIGNED=%q\n'  "${FOG_ALLOW_UNSIGNED:-0}"
  printf 'FOG_PORT=%q\n'            "$FOG_PORT"
  printf 'FOG_TZ=%q\n'              "$FOG_TZ"
  printf 'FOG_ADMIN_USER=%q\n'      "$FOG_ADMIN_USER"
  printf 'FOG_ADMIN_PASSWORD=%q\n'  "$FOG_ADMIN_PASSWORD"
  printf 'FOG_SERVER_NAME=%q\n'     "${FOG_SERVER_NAME:-$CT_HOSTNAME}"
  printf 'FOG_BASE_DOMAIN=%q\n'     "$FOG_BASE_DOMAIN"
  printf 'USE_FUSE_OVERLAYFS=%q\n'  "$USE_FUSE_OVERLAYFS"
} > "$ENVFILE"

cat > "$SETUPFILE" <<'GUEST_EOF'
#!/usr/bin/env bash
# Runs inside the new container: Debian prep, Docker, then Fog's own
# install.sh — the same script an existing Linux server would be given, so
# there is exactly one place that knows how fogd is installed.
set -Eeuo pipefail
. /root/fog-lxc.env
rm -f /root/fog-lxc.env
export DEBIAN_FRONTEND=noninteractive

step() { printf '  ▸ %s\n' "$*"; }

step "updating Debian and installing base packages"
apt-get update -q
apt-get install -y -q --no-install-recommends \
  ca-certificates curl gnupg jq git sudo openssh-server \
  avahi-daemon libnss-mdns tzdata nano less ffmpeg >/dev/null

# timedatectl can't set the clock in a container — the symlink is the whole
# mechanism, and it is what makes Fog's schedules run at local times.
if [ -f "/usr/share/zoneinfo/$FOG_TZ" ]; then
  ln -sf "/usr/share/zoneinfo/$FOG_TZ" /etc/localtime
  printf '%s\n' "$FOG_TZ" > /etc/timezone
fi

# Logs must never be the reason the disk fills — same cap the USB image ships.
install -d -m 0755 /etc/systemd/journald.conf.d
cat > /etc/systemd/journald.conf.d/10-fog-caps.conf <<'EOF'
[Journal]
SystemMaxUse=200M
MaxRetentionSec=1month
EOF
systemctl restart systemd-journald || true

step "installing Docker"
install -d -m 0755 /etc/apt/keyrings
curl -fsSL --proto '=https' https://download.docker.com/linux/debian/gpg -o /etc/apt/keyrings/docker.asc
# Docker's published signing key, by fingerprint: the key file is fetched
# over HTTPS from the same host as the packages, so pinning it means a
# compromise of that host alone can't hand this machine another key.
DOCKER_FPR=9DC858229FC7DD38854AE2D88D81803C0EBFCD88
GOT_FPR=$(gpg --show-keys --with-colons /etc/apt/keyrings/docker.asc 2>/dev/null | awk -F: '$1=="fpr" {print $10; exit}')
if [ "$GOT_FPR" != "$DOCKER_FPR" ]; then
  echo "fog: download.docker.com served an apt key with fingerprint '$GOT_FPR', not Docker's $DOCKER_FPR — refusing it." >&2
  exit 1
fi
chmod a+r /etc/apt/keyrings/docker.asc
. /etc/os-release
printf 'deb [arch=%s signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/debian %s stable\n' \
  "$(dpkg --print-architecture)" "$VERSION_CODENAME" > /etc/apt/sources.list.d/docker.list
apt-get update -q
PKGS=(docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin)
[ "$USE_FUSE_OVERLAYFS" = 1 ] && PKGS+=(fuse-overlayfs)
apt-get install -y -q "${PKGS[@]}" >/dev/null

# The same daemon.json the USB image ships (imaging/overlay/target/etc/
# docker/daemon.json — log caps so app logs can never fill the disk), with
# the storage driver as the one LXC-specific addition, only where the
# node's storage needs it. Keep the shared lines identical to that file.
{
  echo '{'
  [ "$USE_FUSE_OVERLAYFS" = 1 ] && echo '  "storage-driver": "fuse-overlayfs",'
  echo '  "log-driver": "json-file",'
  echo '  "log-opts": { "max-size": "10m", "max-file": "3" }'
  echo '}'
} > /etc/docker/daemon.json

systemctl enable --now docker >/dev/null 2>&1
for _ in $(seq 1 30); do docker info >/dev/null 2>&1 && break; sleep 1; done
docker info >/dev/null 2>&1 || {
  echo "fog: the Docker engine won't start in this container." >&2
  echo "fog: check that the container has nesting=1 and keyctl=1, and see 'journalctl -u docker'." >&2
  exit 1
}
DRIVER=$(docker info --format '{{.Driver}}')
echo "  ✔ Docker running (storage driver: $DRIVER)"
[ "$DRIVER" = vfs ] && echo "  ! vfs is slow and copies every image layer — see docs/proxmox-lxc.md" >&2

# install.sh came in from the node, already resolved. It does the rest:
# manifest, checksum, binary, unit — one place that knows how fogd installs.
step "installing fogd"
FOG_SOURCE="$FOG_SOURCE" FOG_BRANCH="$FOG_BRANCH" \
  FOG_MANIFEST_URL="$FOG_MANIFEST_URL" FOG_RAW_BASE="$FOG_RAW_BASE" \
  FOG_RELEASE_KEYS="$FOG_RELEASE_KEYS" FOG_ALLOW_UNSIGNED="$FOG_ALLOW_UNSIGNED" \
  sh /root/fog-install.sh
rm -f /root/fog-install.sh

# install.sh's unit listens on :8080; a different port is a drop-in over
# both the socket that holds the port and the service, so the unit files
# themselves stay the repo's one copy. fogd stops first: the socket can't
# move while fogd serves on it.
if [ "$FOG_PORT" != "8080" ]; then
  install -d -m 0755 /etc/systemd/system/fogd.service.d /etc/systemd/system/fogd.socket.d
  printf '[Service]\nEnvironment=FOG_LISTEN=:%s\n' "$FOG_PORT" \
    > /etc/systemd/system/fogd.service.d/10-listen.conf
  printf '[Socket]\nListenStream=\nListenStream=%s\n' "$FOG_PORT" \
    > /etc/systemd/system/fogd.socket.d/10-listen.conf
  systemctl daemon-reload
  systemctl stop fogd
  systemctl restart fogd.socket
  systemctl start fogd
fi

# mDNS, so the server answers on <hostname>.local like every other Fog does.
systemctl enable --now avahi-daemon >/dev/null 2>&1 || true
systemctl enable --now ssh >/dev/null 2>&1 || true

for _ in $(seq 1 60); do
  curl -fsS "http://127.0.0.1:$FOG_PORT/api/setup/status" >/dev/null 2>&1 && break
  sleep 1
done
STATUS=$(curl -fsS "http://127.0.0.1:$FOG_PORT/api/setup/status" 2>/dev/null || true)
[ -n "$STATUS" ] || { echo "fog: fogd isn't answering on :$FOG_PORT — see 'journalctl -u fogd'." >&2; exit 1; }
echo "  ✔ fogd $(printf '%s' "$STATUS" | jq -r '.version // "?"') answering on :$FOG_PORT"

if [ -n "$FOG_ADMIN_USER" ] && [ -n "$FOG_ADMIN_PASSWORD" ]; then
  step "creating the Fog admin account"
  BODY=$(jq -nc --arg u "$FOG_ADMIN_USER" --arg p "$FOG_ADMIN_PASSWORD" \
                --arg n "$FOG_SERVER_NAME" --arg d "$FOG_BASE_DOMAIN" \
    '{username:$u, password:$p}
     + (if $n == "" then {} else {serverName:$n} end)
     + (if $d == "" then {} else {baseDomain:$d} end)')
  RESP=$(curl -fsS -X POST -H 'content-type: application/json' \
    --data-binary "$BODY" "http://127.0.0.1:$FOG_PORT/api/setup" 2>&1) || {
      echo "fog: creating the admin account failed: $RESP" >&2
      echo "fog: the wizard is still open in the browser — finish it there." >&2
      exit 1
    }
  echo "  ✔ admin account '$FOG_ADMIN_USER' created"
fi
GUEST_EOF

msg "installing Fog inside the container — a few minutes"
pct push "$CTID" "$INSTALL_SH" /root/fog-install.sh --perms 0700
pct push "$CTID" "$ENVFILE" /root/fog-lxc.env --perms 0600
pct push "$CTID" "$SETUPFILE" /root/fog-lxc-setup.sh --perms 0700
pct exec "$CTID" -- bash /root/fog-lxc-setup.sh
pct exec "$CTID" -- rm -f /root/fog-lxc-setup.sh

# The address can change between creation and now if the lease was renewed.
NEW_IP=$(pct exec "$CTID" -- ip -4 -o addr show dev eth0 2>/dev/null | awk '{print $4}' | cut -d/ -f1 || true)
[ -z "$NEW_IP" ] || CT_IP="$NEW_IP"

# --------------------------------------------------------------- done ---

printf '\n%s   ░░░  fog is up%s\n\n' "$GRN$BOLD" "$RST"
printf '     dashboard   %shttp://%s:%s%s\n' "$BOLD" "$CT_IP" "$FOG_PORT" "$RST"
printf '                 http://%s.local:%s\n' "$CT_HOSTNAME" "$FOG_PORT"
if [ -n "$FOG_ADMIN_USER" ]; then
  printf '     sign in     as %s, with the password you passed in\n' "$FOG_ADMIN_USER"
else
  printf '     first run   open the dashboard to create your admin account\n'
fi
printf '     shell       pct enter %s      (from this node)\n' "$CTID"
printf '     container   CT %s · %s · %s\n' "$CTID" "$CT_HOSTNAME" \
  "$([ "$UNPRIVILEGED" = 1 ] && echo "unprivileged, nesting+keyctl" || echo "privileged, nesting")"
printf '\n     %sNext: install an app from the catalog, then set a recovery-kit%s\n' "$DIM" "$RST"
printf '     %spassphrase in Settings › Storage › Recovery kit and a backup hour%s\n' "$DIM" "$RST"
printf '     %sin Settings › Storage › Backups.%s\n\n' "$DIM" "$RST"
