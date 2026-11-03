#!/bin/sh
# Fog installer for an existing Linux server — the only command you run on
# the host. (For a blank machine, the bootable USB installer is the better
# path: see docs/installer.md. On a Proxmox node, see proxmox-lxc.sh and
# proxmox-vm.sh.)
#
#   curl -fsSL https://raw.githubusercontent.com/fogapp/fog/main/install.sh | sh
#
# Installs /usr/local/bin/fogd, its rollback guard and its systemd unit —
# every file checked against a release manifest whose signature is checked
# against fog's release key — creates /var/lib/fog, and starts the first-run
# wizard on port 8080. Everything after this is done in the web UI. Nothing
# on the system changes until every download has verified; if the new fogd
# then won't start, the previous one is put back.
#
# Where all of that comes from is one parameter, FOG_SOURCE, which takes
# whatever you host the code on. Unset, it is fog's public releases,
# https://github.com/fogapp/fog — or, re-run on a server that already has
# fog, the source that server was installed from (never reset by an upgrade).
#
#   a GitHub repo          https://github.com/fogapp/fog  (the default)
#   a Gitea/Forgejo repo   http://gitea.lan/matthew/fog
#   a GitLab repo          https://gitlab.com/you/fog
#   a plain directory      https://files.example.com/fog
#   a manifest directly    https://files.example.com/fog/latest.json
#
# fogd's own updater resolves the same shapes in the same order
# (internal/selfupdate/resolve.go; internal/selfupdate/shellsource_test.go
# holds the two together), so one URL configures the install and every
# update after it. Escape hatches, when the guessing doesn't fit your host:
# FOG_MANIFEST_URL and FOG_RAW_BASE set the two resolved values outright.
# FOG_BRANCH picks the branch a repo's committed manifest is read from
# (default main). FOG_PRINT_SOURCE=1 prints what it resolved and exits
# without installing; FOG_PRINT_CANDIDATES=1 prints the candidate table and
# exits, no network.
#
# Safety valves, each off unless set to 1: FOG_ALLOW_DOWNGRADE installs a
# release older than the fogd already running; FOG_ALLOW_OLD_RELEASE takes a
# manifest issued more than 180 days ago; FOG_ALLOW_UNSIGNED skips the
# manifest signature (a fork that hasn't set up signing). FOG_RELEASE_KEYS
# replaces the pinned release keys (base64 SPKI, space-separated) for a fork
# that signs with its own.
set -eu
# No pathname expansion anywhere: an unquoted URL holding [ ] (an IPv6
# literal) must never match a file in the current directory.
set -f

DEFAULT_SOURCE=https://github.com/fogapp/fog
SOURCE="${FOG_SOURCE:-${FOG_REPO_URL:-$DEFAULT_SOURCE}}"
SOURCE_GIVEN=0
[ -z "${FOG_SOURCE:-}${FOG_REPO_URL:-}${FOG_MANIFEST_URL:-}" ] || SOURCE_GIVEN=1
BRANCH="${FOG_BRANCH:-main}"
MANIFEST_URL="${FOG_MANIFEST_URL:-}"
RAW_BASE="${FOG_RAW_BASE:-}"
ALLOW_UNSIGNED="${FOG_ALLOW_UNSIGNED:-0}"
ALLOW_OLD="${FOG_ALLOW_OLD_RELEASE:-0}"
PRINT_ONLY="${FOG_PRINT_SOURCE:-0}"
PRINT_CANDIDATES="${FOG_PRINT_CANDIDATES:-0}"
# FOG_INSTALL_ROOT prefixes every path written, for the tests that run this
# whole script against a scratch directory (shellsource_test.go).
ROOT="${FOG_INSTALL_ROOT:-}"
BIN="$ROOT/usr/local/bin/fogd"
GUARD="$ROOT/usr/local/bin/fogd-rollback"
UNIT="$ROOT/etc/systemd/system/fogd.service"
SOCKET="$ROOT/etc/systemd/system/fogd.socket"
DATA="$ROOT/var/lib/fog"
WORK=""

# fog's release key (internal/selfupdate/sign.go's ReleaseKeys, as SPKI DER
# in base64). shellsource_test.go fails if the copies differ.
RELEASE_KEYS="${FOG_RELEASE_KEYS:-MCowBQYDK2VwAyEAzrGQX7hwCDQn0KNY31d+VHb3CU2wIg/qt7CeHpDzMUg=}"

die() { printf 'fog: %s\n' "$*" >&2; exit 1; }
say() { [ "$PRINT_ONLY" = 1 ] || printf 'fog: %s\n' "$*"; }

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

# An upgrade keeps the server's source. Re-running this on a server fog is
# already on, with no source given, reads the one its unit carries (the
# drop-in written below at the first install) instead of the public default:
# a server that installs from a LAN Gitea keeps doing so.
DROPIN="$UNIT.d/10-update-source.conf"
dropin_value() { # <name> → its value in the drop-in, %% undone; quoted or not
  sed -n -e "s/^Environment=\"$1=\(.*\)\"\$/\1/p" -e "s/^Environment=$1=\([^\"]*\)\$/\1/p" "$DROPIN" 2>/dev/null \
    | head -n 1 | sed 's/%%/%/g'
}
if [ "$SOURCE_GIVEN" != 1 ] && [ -f "$DROPIN" ]; then
  KEPT=$(dropin_value FOG_UPDATE_BASE_URL)
  if [ -n "$KEPT" ]; then
    SOURCE=$KEPT
    SOURCE_GIVEN=1
    [ -n "${FOG_BRANCH:-}" ] || BRANCH=$(dropin_value FOG_UPDATE_BRANCH)
    [ -n "$BRANCH" ] || BRANCH=main
    [ "$PRINT_CANDIDATES" = 1 ] || say "keeping this server's source, $SOURCE (FOG_SOURCE picks another)."
  fi
fi

fog_normalize

if [ "$PRINT_ONLY" != 1 ] && [ "$PRINT_CANDIDATES" != 1 ]; then
  if [ "$(id -u)" -ne 0 ]; then
    die "this installer must run as root (it installs a systemd service)."
  fi
  command -v systemctl >/dev/null 2>&1 || die "systemd is required."
fi

case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) die "unsupported architecture $(uname -m)" ;;
esac

# The candidate table alone, for the tests that pin these shapes to the Go
# resolver's without touching the network.
if [ "$PRINT_CANDIDATES" = 1 ]; then
  candidates
  exit 0
fi

command -v curl >/dev/null 2>&1 || die "curl is required."
WORK=$(mktemp -d)
cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT
trap 'exit 1' HUP INT TERM

if [ "$ALLOW_UNSIGNED" = 1 ]; then
  echo "fog: FOG_ALLOW_UNSIGNED=1 — the manifest's signature is NOT checked; only its checksums protect this install." >&2
elif ! find_openssl; then
  die "checking the release signature needs openssl with ed25519 (OpenSSL 1.1.1 or newer): apt-get install openssl, then run this again."
fi
[ "$ALLOW_OLD" != 1 ] || echo "fog: FOG_ALLOW_OLD_RELEASE=1 — a manifest of any age is accepted." >&2

say "reading release manifest..."
if ! fog_resolve; then
  default_source_hint
  die "set FOG_MANIFEST_URL (and FOG_RAW_BASE) if your host lays files out differently."
fi

BIN_ART=$(artifact "linux-$ARCH") || die "$MANIFEST_URL has no linux-$ARCH artifact with a sha256."
URL=${BIN_ART% *}

if [ "$PRINT_ONLY" = 1 ]; then
  echo "source=$SRC"
  echo "manifest=$MANIFEST_URL"
  echo "raw=$RAW_BASE"
  echo "version=$VERSION"
  echo "issued=$ISSUED"
  # The first place the binary answers: the host the download will really
  # come from.
  echo "artifact=$(fog_reachable "$URL")"
  exit 0
fi

UNIT_ART=$(artifact fogd.service) || die "$MANIFEST_URL lists no fogd.service with a sha256 — it predates this installer; use a newer release."
SOCKET_ART=$(artifact fogd.socket) || die "$MANIFEST_URL lists no fogd.socket with a sha256 — it predates this installer; use a newer release."
GUARD_ART=$(artifact fogd-rollback) || die "$MANIFEST_URL lists no fogd-rollback with a sha256 — it predates this installer; use a newer release."

# --- downgrade protection -------------------------------------------------------

# The installed fogd's version, read from the binary itself — root's file,
# which nothing else on the machine can answer for (a port can be). Its
# version is stamped in it behind a marker (internal/config's VersionStamp);
# it is read, not run: a fogd from before --version existed would start a
# second daemon. A fogd from before the stamp reads as unknown. A fresh
# install has nothing to compare with; the manifest's age limit is what
# stops it landing on an old release.
RUNNING=""
if [ -f "$BIN" ]; then
  for rv in $(LC_ALL=C grep -ao 'fogd-version=v[0-9][0-9A-Za-z.+-]*' "$BIN" 2>/dev/null | sed 's/^fogd-version=//'); do
    if semver_valid "$rv"; then RUNNING=$rv; break; fi
  done
  [ -n "$RUNNING" ] || RUNNING="unknown (a fogd from before versions were stamped)"
fi
# Semver precedence, as fogd's own updater orders releases (semver_cmp).
# A running fogd whose version isn't a release can't be placed in that
# order, so replacing it is a downgrade as far as anyone can tell.
if [ -n "$RUNNING" ]; then
  if ! semver_valid "$RUNNING"; then
    [ "${FOG_ALLOW_DOWNGRADE:-0}" = 1 ] || die "this server runs fogd '$RUNNING', which isn't a release version, so $VERSION can't be told from a downgrade — refusing. FOG_ALLOW_DOWNGRADE=1 replaces it anyway."
    echo "fog: FOG_ALLOW_DOWNGRADE=1 — replacing the unversioned fogd '$RUNNING' with $VERSION." >&2
  elif [ "$(semver_cmp "$VERSION" "$RUNNING")" = -1 ]; then
    [ "${FOG_ALLOW_DOWNGRADE:-0}" = 1 ] || die "this server runs fogd $RUNNING, and $MANIFEST_URL offers the older $VERSION — refusing to downgrade. FOG_ALLOW_DOWNGRADE=1 does it anyway."
    echo "fog: FOG_ALLOW_DOWNGRADE=1 — replacing $RUNNING with the older $VERSION." >&2
  fi
fi

# --- download and verify everything, before touching the system -------------

say "downloading fogd $VERSION (linux/$ARCH)..."
# The binary lands beside $BIN, not in /tmp: /tmp is usually tmpfs, and mv
# across filesystems is a copy — non-atomic, and writing over a RUNNING fogd
# fails with "text file busy" mid-upgrade. Same directory means the final
# mv is an atomic rename. The unit and the guard are staged the same way.
mkdir -p "$(dirname "$BIN")" "$(dirname "$UNIT")"
BIN_TMP=$(mktemp "$(dirname "$BIN")/.fogd.XXXXXX")
GUARD_TMP=$(mktemp "$(dirname "$GUARD")/.fogd-rollback.XXXXXX")
UNIT_TMP=$(mktemp "$UNIT.tmp.XXXXXX")
SOCKET_TMP=$(mktemp "$SOCKET.tmp.XXXXXX")
trap 'rm -f "$BIN_TMP" "$GUARD_TMP" "$UNIT_TMP" "$SOCKET_TMP"; cleanup' EXIT
fog_get "$BIN_ART" "$BIN_TMP" 268435456
fog_get "$GUARD_ART" "$GUARD_TMP" 65536
fog_get "$UNIT_ART" "$UNIT_TMP" 65536
fog_get "$SOCKET_ART" "$SOCKET_TMP" 65536
grep -q '^ExecStart=' "$UNIT_TMP" || die "the release's fogd.service has no ExecStart= — refusing it."
grep -q '^ListenStream=' "$SOCKET_TMP" || die "the release's fogd.socket has no ListenStream= — refusing it."
chmod 0755 "$BIN_TMP" "$GUARD_TMP"
chmod 0644 "$UNIT_TMP" "$SOCKET_TMP"

# --- install ----------------------------------------------------------------

mkdir -p "$DATA"
chmod 0700 "$DATA"

# The previous binary stays as fogd.prev: the rollback guard and the restart
# check below both put it back when the new one won't run. An update still
# on trial from fogd's own updater is settled by this install.
HAD_PREV=0
if [ -f "$BIN" ]; then
  cp -p "$BIN" "$BIN.prev"
  HAD_PREV=1
fi
rm -f "$BIN.update-pending"
mv -f "$GUARD_TMP" "$GUARD"
mv -f "$UNIT_TMP" "$UNIT"
mv -f "$SOCKET_TMP" "$SOCKET"
mv -f "$BIN_TMP" "$BIN"
trap cleanup EXIT

# Self-update should come from wherever this install came from, without the
# operator having to paste the same URL into the UI. It is only a default:
# the setting, once written in Settings › Server › Updates, wins. A manifest
# named outright is carried as-is (nothing could guess it again); otherwise
# the source and branch, which the updater resolves exactly as above.
# valid_url has already kept quotes, backslashes and whitespace out of these
# values; % is a systemd specifier, so it is doubled.
UPDATE_BASE="$SOURCE"
[ -z "${FOG_MANIFEST_URL:-}" ] || UPDATE_BASE="$FOG_MANIFEST_URL"
unit_quote() { printf '"%s"' "$(printf '%s' "$1" | sed 's/%/%%/g')"; }
mkdir -p "$UNIT.d"
{
  echo "[Service]"
  echo "Environment=$(unit_quote "FOG_UPDATE_BASE_URL=$UPDATE_BASE")"
  echo "Environment=$(unit_quote "FOG_UPDATE_BRANCH=$BRANCH")"
  [ "$ALLOW_UNSIGNED" != 1 ] || echo "Environment=FOG_UPDATE_ALLOW_UNSIGNED=1"
} > "$UNIT.d/10-update-source.conf.tmp"
mv -f "$UNIT.d/10-update-source.conf.tmp" "$UNIT.d/10-update-source.conf"

# fogd.socket holds the port from here on, across every restart. fogd is
# stopped first, because a fogd from before the socket binds the port
# itself; then the socket takes the port, and fogd starts on it. A restart,
# not `enable --now`: --now doesn't restart an already-running service, and
# a re-run would then report the new version while the old binary still
# serves. fogd is Type=notify, so start returns once it is ready — or fails
# when it can't get there.
# A server whose fogd listens elsewhere (a FOG_LISTEN drop-in: the Proxmox
# scripts' --port) keeps that port: the socket gets the same port in a
# drop-in of its own, unless it already has drop-ins — exactly what the
# self-update path does (fogd-rollback --install-units). The server's own
# drop-ins are never touched.
# fogd's FOG_LISTEN as systemd will give it: Environment= lines (the unit
# and its drop-ins), then each EnvironmentFile= in order, which override
# them — systemd's own precedence.
fogd_listen() {
  fl_v=$(systemctl show -p Environment fogd 2>/dev/null | tr ' ' '\n' | sed -n 's/^.*FOG_LISTEN=//p' | tail -n 1)
  fl_files=$(systemctl show -p EnvironmentFiles fogd 2>/dev/null | sed -n 's/^EnvironmentFiles=//p' | sed 's/ (ignore_errors=[a-z]*)$//')
  while IFS= read -r fl_f; do
    fl_f=${fl_f#-}
    [ -n "$fl_f" ] && [ -f "$fl_f" ] || continue
    fl_x=$(sed -n 's/^[[:space:]]*\(export[[:space:]][[:space:]]*\)\{0,1\}FOG_LISTEN=//p' "$fl_f" | tail -n 1 \
      | sed -e 's/[[:space:]]*$//' -e 's/^"\(.*\)"$/\1/' -e "s/^'\\(.*\\)'\$/\\1/")
    [ -z "$fl_x" ] || fl_v=$fl_x
  done <<EOF
$fl_files
EOF
  printf '%s\n' "$fl_v"
}

socket_port_dropin() {
  systemctl daemon-reload || return 1
  sp_listen=$(fogd_listen)
  case "$sp_listen" in ''|*[!0-9A-Za-z.:\[\]-]*) return 0 ;; esac
  sp_default=$(sed -n 's/^ListenStream=//p' "$SOCKET" | tail -n 1)
  [ "${sp_listen#:}" != "$sp_default" ] || return 0
  if [ -d "$SOCKET.d" ] && find "$SOCKET.d" -name '*.conf' | grep -q .; then return 0; fi
  mkdir -p "$SOCKET.d"
  printf '[Socket]\nListenStream=\nListenStream=%s\n' "${sp_listen#:}" > "$SOCKET.d/10-listen.conf.tmp"
  mv -f "$SOCKET.d/10-listen.conf.tmp" "$SOCKET.d/10-listen.conf"
  say "fogd listens on ${sp_listen#:} here: fogd.socket takes the same port."
}

say "starting fogd $VERSION..."
started=0
if socket_port_dropin && systemctl daemon-reload && systemctl enable --quiet fogd.socket fogd \
  && { systemctl stop fogd >/dev/null 2>&1 || true; } \
  && systemctl restart fogd.socket && systemctl start fogd; then
  sleep 5
  if systemctl is-active --quiet fogd; then started=1; fi
fi
if [ "$started" != 1 ]; then
  journalctl -u fogd -n 20 --no-pager >&2 2>/dev/null || true
  if [ "$HAD_PREV" = 1 ]; then
    cp -p "$BIN.prev" "$BIN.tmp.$$" && mv -f "$BIN.tmp.$$" "$BIN"
    systemctl restart fogd || true
    die "fogd $VERSION didn't start (its log is above), so the previous fogd binary was put back and restarted."
  fi
  die "fogd $VERSION didn't start — its log is above; 'journalctl -u fogd' has the rest."
fi

IP="$(hostname -I 2>/dev/null | awk '{print $1}')"
echo
echo "fog: installed and running ($VERSION, from $MANIFEST_URL)."
echo "fog: open http://${IP:-<server-ip>}:8080 to finish setup in your browser."
