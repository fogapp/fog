#!/bin/sh
# Cut a Fog release: tag, cross-compile, checksum, sign, and publish to Gitea
# with a latest.json manifest that fogd's self-update and install.sh consume.
#
#   ./scripts/release.sh v0.1.0
#
# Requires: go, node/npm, python3, the release signing key, and a credential
# for the git host (OS keychain, or FOG_GIT_TOKEN).
# Where it publishes is parameterized — move the code to another
# Gitea/Forgejo and only these change:
#
#   FOG_GIT_HOST     http://gitea.lan      origin of the API and downloads
#   FOG_GIT_OWNER    matthew
#   FOG_GIT_REPO     fog
#   FOG_GIT_USER     $FOG_GIT_OWNER        API user, if it differs
#   FOG_GIT_TOKEN    (keychain)            skips the credential helper
#   FOG_SIGNING_KEY  ~/.config/fog/release-signing-key.pem
#                    the ed25519 key latest.json is signed with; its public
#                    half must be pinned in internal/selfupdate/sign.go,
#                    install.sh and imaging/flash.sh (signtool keygen makes one)
#   git config fog.insecureCredentialHost HOST   the one http:// FOG_GIT_HOST
#                    the credential may be sent to (repo-local; refused otherwise)
#   FOG_ARTIFACT_BASE  release download URL written into latest.json —
#                    set it to serve binaries from a mirror or CDN
#   FOG_RELEASE_IMAGE=1  also build, gate (QEMU) and publish the installer image
#
# Everything a release is lands in bin/release/ first — the binaries, the
# unit, the rollback guard, install.sh, flash.sh, the signed manifest,
# SHA256SUMS, and the installer image when built. That directory is self-contained: serve it
# from any static host and FOG_SOURCE=<its URL> installs and updates from it.
# The same files are then uploaded as the Gitea release's assets (the image
# goes to the package registry instead; see below).
#
# The release directory's latest.json names its files relatively, so the
# directory resolves wherever it is served: copied to any host, or uploaded
# as a GitHub release — which scripts/publish-public.sh does for fogapp/fog
# (the /release skill), so strangers install and update from GitHub.
set -eu

VERSION="${1:?usage: release.sh vX.Y.Z}"
GITEA="${FOG_GIT_HOST:-http://gitea.lan}"
OWNER="${FOG_GIT_OWNER:-matthew}"
REPO="${FOG_GIT_REPO:-fog}"
GIT_USER="${FOG_GIT_USER:-$OWNER}"
BASE_DL="${FOG_ARTIFACT_BASE:-$GITEA/$OWNER/$REPO/releases/download/$VERSION}"
SIGNING_KEY="${FOG_SIGNING_KEY:-$HOME/.config/fog/release-signing-key.pem}"
REL=bin/release

cd "$(dirname "$0")/.."

# The version reaches JSON, URLs, a git tag and the manifest the updater
# orders releases by: a semver release version exactly as fog parses one
# (internal/selfupdate/version.go) — vMAJOR.MINOR.PATCH, no leading zeros,
# optionally -prerelease of dot-separated identifiers; no build metadata
# in a tag. This pattern is checked against the Go parser by
# release_preflight_test.go, and the Go parser itself checks the tag again
# below (signtool version) once it is built.
VERSION_RE='v(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})(-(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?'
printf '%s\n' "$VERSION" | grep -Eqx "$VERSION_RE" \
  || { echo "version must be a semver release like v0.1.0 or v0.2.0-rc.1 (got '$VERSION')" >&2; exit 1; }
GIT_SCHEME=${GITEA%%://*}
GIT_HOSTPORT=${GITEA#*://}; GIT_HOSTPORT=${GIT_HOSTPORT%%/*}
# The credential is a write token for the release repo. Over plain http it
# crosses the network readable by anyone on the path, so it goes only to the
# one http host this repo names in its local git config — a server on your
# own LAN, say — and to no other:
#   git config fog.insecureCredentialHost gitea.lan
# The setting is per clone and never committed. An https host needs none.
if [ "$GIT_SCHEME" != https ]; then
  INSECURE_HOST=$(git config --get fog.insecureCredentialHost 2>/dev/null || true)
  if [ -z "$INSECURE_HOST" ] || [ "$INSECURE_HOST" != "$GIT_HOSTPORT" ]; then
    echo "FOG_GIT_HOST is $GITEA: the git host credential would be sent over plain http." >&2
    if [ -n "$INSECURE_HOST" ]; then
      echo "this repo allows that only for '$INSECURE_HOST' (git config fog.insecureCredentialHost)." >&2
    fi
    echo "use an https host, or, for a server on a network you trust, allow exactly that host in this clone:" >&2
    echo "  git config fog.insecureCredentialHost $GIT_HOSTPORT" >&2
    exit 1
  fi
  echo "==> sending the git credential over http to $GIT_HOSTPORT (allowed by git config fog.insecureCredentialHost)"
fi

# Exactly the committed tree is built and signed: `git diff` alone misses
# untracked files, and an untracked .go file in a package compiles straight
# into a signed release. Anything .gitignore doesn't cover must be
# committed or removed first.
if [ -n "$(git status --porcelain --untracked-files=all)" ]; then
  echo "working tree isn't clean — commit or remove these first:" >&2
  git status --short --untracked-files=all >&2
  exit 1
fi

# The signing key, checked before anything is built or tagged: a release
# its servers would refuse is worse than no release.
echo "==> signing key"
[ -f "$SIGNING_KEY" ] || {
  echo "no release signing key at $SIGNING_KEY (FOG_SIGNING_KEY)." >&2
  echo "a new one: go run ./internal/selfupdate/signtool keygen <path>, then pin the key it prints" >&2
  echo "in internal/selfupdate/sign.go, install.sh, imaging/flash.sh and the Proxmox scripts' resolver block." >&2
  exit 1
}
# The key signs what every server installs as root: it must be this user's
# alone — a regular file, owned by them, mode 0600.
if [ -L "$SIGNING_KEY" ] || [ -z "$(find "$SIGNING_KEY" -prune -type f -user "$(id -u)" -perm 600 2>/dev/null)" ]; then
  echo "$SIGNING_KEY must be a regular file owned by you with mode 0600 (chmod 600 '$SIGNING_KEY')." >&2
  exit 1
fi
mkdir -p bin
go build -o bin/signtool ./internal/selfupdate/signtool
bin/signtool version "$VERSION" || { echo "version '$VERSION' isn't one fog's updater would order" >&2; exit 1; }
printf 'release key check %s\n' "$VERSION" > bin/.keycheck
bin/signtool sign "$SIGNING_KEY" bin/.keycheck
bin/signtool verify bin/.keycheck || {
  echo "$SIGNING_KEY isn't one of the keys pinned in internal/selfupdate/sign.go — servers would refuse this release." >&2
  exit 1
}
rm -f bin/.keycheck bin/.keycheck.sig

# Gate the release on the same checks CI runs. Self-update pushes this binary
# to the server unattended, so a broken build here is a broken server later —
# and the CI runner is not guaranteed to exist on the Gitea instance.
#
# One pass per tree: `make check` records what it passed on
# (bin/.check-passed, scripts/check-id), and the tree is clean here, so a
# matching id means these exact bytes, on these toolchains, already passed —
# typically the run /ship makes just before committing. FOG_RECHECK=1 runs
# the checks regardless.
echo "==> checks"
CHECK_ID=$(scripts/check-id)
if [ "${FOG_RECHECK:-}" != "1" ] && [ -f bin/.check-passed ] && [ "$(cat bin/.check-passed)" = "$CHECK_ID" ]; then
  echo "    passed already for this tree (${CHECK_ID%% *}); FOG_RECHECK=1 runs them again"
else
  make check
fi

# The credential is looked up for whatever host FOG_GIT_HOST names, so a
# different server needs no edit here — just its own keychain entry or token.
# It reaches curl through a config file, never argv, which any local user
# can read with ps.
PW="${FOG_GIT_TOKEN:-}"
if [ -z "$PW" ]; then
  PW=$(printf 'protocol=%s\nhost=%s\n\n' "$GIT_SCHEME" "$GIT_HOSTPORT" \
    | git credential fill | awk -F= '/^password/{print $2}')
fi
[ -n "$PW" ] || { echo "no credential for $GIT_HOSTPORT (keychain or FOG_GIT_TOKEN)" >&2; exit 1; }
CURLCFG=$(mktemp)
trap 'rm -f "$CURLCFG"' EXIT
chmod 0600 "$CURLCFG"
printf 'user = "%s"\n' "$(printf '%s:%s' "$GIT_USER" "$PW" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g')" > "$CURLCFG"
# Never --location: on a redirect curl would carry the credential along.
api() { curl -K "$CURLCFG" "$@"; }

# Every released binary is built with -trimpath (no build machine paths, so
# no home directory or user name, inside it), -buildvcs=false (no commit
# hash or time of this private repo) and -s -w (no symbol table or DWARF);
# release_preflight_test.go checks each go build here says so, and
# scripts/check-release-binaries checks the binaries themselves below. The
# version is stamped with its marker (internal/config): install.sh reads an
# installed fogd's version from the file.
echo "==> building $VERSION"
# fog's own catalogs ride along in the binary as the store's first copy.
scripts/catalog-seed.sh
(cd frontend && npm ci --silent && npm run build >/dev/null)
rm -rf "$REL"
mkdir -p "$REL"
for arch in amd64 arm64; do
  CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -buildvcs=false \
    -ldflags "-s -w -X gitea.lan/matthew/fog/internal/config.Version=fogd-version=$VERSION" \
    -o "bin/fogd-linux-$arch" ./cmd/fogd
  cp "bin/fogd-linux-$arch" "$REL/"
done
cp packaging/fogd.service packaging/fogd.socket packaging/fogd-rollback install.sh imaging/flash.sh "$REL/"

# Whichever tool exists; the pipeline masks a missing one (awk always exits
# 0), so validate the result — a manifest with an empty sha256 breaks every
# self-update downstream.
sha() {
  s=$( { sha256sum "$1" 2>/dev/null || shasum -a 256 "$1"; } | awk '{print $1}')
  echo "$s" | grep -Eq '^[0-9a-f]{64}$' || { echo "sha256 failed for $1" >&2; exit 1; }
  echo "$s"
}

# fog-node, for the machines that lend fog their hardware: fogd fetches the
# build a paired machine needs from this same manifest, by these keys.
for p in darwin-arm64 darwin-amd64 linux-amd64 linux-arm64; do
  CGO_ENABLED=0 GOOS=${p%-*} GOARCH=${p#*-} go build -trimpath -buildvcs=false \
    -ldflags "-s -w -X gitea.lan/matthew/fog/internal/config.Version=fogd-version=$VERSION" \
    -o "bin/fog-node-$p" ./cmd/fog-node
  cp "bin/fog-node-$p" "$REL/"
done

# The bootable installer rides along when asked for (it adds ~2GB and a
# Docker-driven build): FOG_RELEASE_IMAGE=1 builds the image, runs the QEMU
# end-to-end gate, and puts image + checksum + the download page in the
# release directory.
IMG=fog-installer-x86_64.img
if [ "${FOG_RELEASE_IMAGE:-}" = "1" ]; then
  echo "==> building installer image"
  for arch_bin in fog-setup fog-console; do
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false \
      -ldflags "-s -w -X gitea.lan/matthew/fog/internal/config.Version=fogd-version=$VERSION" \
      -o "bin/$arch_bin-linux-amd64" "./cmd/$arch_bin"
  done
  imaging/build.sh all
  echo "==> installer end-to-end gate (QEMU)"
  imaging/test/e2e.sh
  # bin/release/ holds exactly what the signed manifest names (plus the
  # manifest, its signature and SHA256SUMS): the image goes in, the image's
  # .sha256 and the download page stay beside it in bin/, for the Gitea
  # registry and release.
  cp "imaging/out/$IMG" "$REL/"
  cp "imaging/out/$IMG.sha256" "bin/$IMG.sha256"
  # The page ships with the real checksum baked into its placeholder span.
  sed "s|<span id=\"sha\">[^<]*</span>|<span id=\"sha\">$(sha "$REL/$IMG")</span>|" \
    site/download/index.html > bin/download.html
fi

# manifest <prefix> <image url>: latest.json for files named <prefix><file>.
# The keys are what fogd (linux-*, fog-node-*), install.sh (linux-*,
# fogd.service, fogd.socket, fogd-rollback) and flash.sh (installer-x86_64)
# look up.
manifest() {
  printf '{\n  "version": "%s",\n  "issued_at": %s,\n  "artifacts": {' "$VERSION" "$ISSUED_AT"
  sep=""
  for key_file in linux-amd64:fogd-linux-amd64 linux-arm64:fogd-linux-arm64 \
      fog-node-darwin-arm64:fog-node-darwin-arm64 fog-node-darwin-amd64:fog-node-darwin-amd64 \
      fog-node-linux-amd64:fog-node-linux-amd64 fog-node-linux-arm64:fog-node-linux-arm64 \
      fogd.service:fogd.service fogd.socket:fogd.socket fogd-rollback:fogd-rollback install.sh:install.sh flash.sh:flash.sh; do
    key=${key_file%%:*}; file=${key_file#*:}
    printf '%s\n    "%s": { "url": "%s%s", "sha256": "%s" }' "$sep" "$key" "$1" "$file" "$(sha "$REL/$file")"
    sep=","
  done
  if [ -n "$2" ]; then
    printf ',\n    "installer-x86_64": { "url": "%s", "sha256": "%s" }' "$2" "$(sha "$REL/$IMG")"
  fi
  printf '\n  }\n}\n'
}

# Two manifests, each signed. The release directory's always names its files
# relatively — the portable form, which resolves correctly as a Gitea or
# GitHub release asset too, since the assets sit beside it (GitHub's public
# installs and updates read exactly this one). The one committed to the repo (release/latest.json, read at
# the raw-file path, which never changes across releases) always carries
# absolute URLs: relative names there would resolve inside the repo, where
# no binaries are.
#
# The image ships through the generic package registry, not as a release
# attachment: attachments arrive as multipart and Gitea buffers them to temp
# before storage, which 502s on multi-GB files — package uploads stream
# straight through. A mirror (FOG_ARTIFACT_BASE) or a relative directory
# holds the image beside everything else.
# The signing time, inside the signed bytes: installers and fogd refuse a
# manifest more than 180 days old (MaxManifestAge in
# internal/selfupdate/sign.go), so an old release can't be served as current.
ISSUED_AT=$(date +%s)
IMG_ABS=""; IMG_REL=""
if [ "${FOG_RELEASE_IMAGE:-}" = "1" ]; then
  if [ -n "${FOG_ARTIFACT_BASE:-}" ]; then
    IMG_ABS="$BASE_DL/$IMG"
  else
    IMG_ABS="$GITEA/api/packages/$OWNER/generic/fog-installer/$VERSION/$IMG"
  fi
  IMG_REL="$IMG"
fi
mkdir -p release
manifest "$BASE_DL/" "$IMG_ABS" > release/latest.json
manifest "" "$IMG_REL" > "$REL/latest.json"
# No private commit hash or time, no build-machine path, in anything that
# ships — checked on the binaries themselves, before they are signed for.
scripts/check-release-binaries "$REL"
bin/signtool sign "$SIGNING_KEY" release/latest.json
bin/signtool sign "$SIGNING_KEY" "$REL/latest.json"
bin/signtool verify release/latest.json
bin/signtool verify "$REL/latest.json"
(cd "$REL" && for f in *; do printf '%s  %s\n' "$(sha "$f")" "$f"; done) > bin/SHA256SUMS.tmp
mv bin/SHA256SUMS.tmp "$REL/SHA256SUMS"

echo "==> tagging"
git tag -f "$VERSION"
git push -q origin "$VERSION" --force

echo "==> creating Gitea release"
# Drop any existing release for this tag (re-runs replace it).
EXISTING=$(api -s "$GITEA/api/v1/repos/$OWNER/$REPO/releases/tags/$VERSION" | python3 -c "import json,sys
try: print(json.load(sys.stdin).get('id',''))
except Exception: pass" 2>/dev/null || true)
if [ -n "$EXISTING" ]; then
  api -fsS -X DELETE "$GITEA/api/v1/repos/$OWNER/$REPO/releases/$EXISTING" > /dev/null
fi
# Release notes = commit subjects since the previous tag; shown in Fog's
# header update dropdown. The version reaches python through the
# environment, never spliced into its source.
PREV_TAG=$(git describe --tags --abbrev=0 "$VERSION"^ 2>/dev/null || true)
if [ -n "$PREV_TAG" ]; then
  NOTES=$(git log --pretty='• %s' "$PREV_TAG..$VERSION" | grep -v '^• Release v.*manifest$' | head -20)
else
  NOTES="Initial release"
fi
RELEASE_BODY=$(printf '%s' "$NOTES" | REL_VERSION="$VERSION" python3 -c "import json,os,sys
v=os.environ['REL_VERSION']; print(json.dumps({'tag_name':v,'name':v,'body':sys.stdin.read()}))")
RELEASE_ID=$(api -fsS -X POST "$GITEA/api/v1/repos/$OWNER/$REPO/releases" \
  -H 'Content-Type: application/json' \
  -d "$RELEASE_BODY" \
  | python3 -c "import json,sys; print(json.load(sys.stdin)['id'])")

# Every upload is checked: a silent failure here would push a manifest whose
# URLs 404 on the next self-update.
upload() { # upload <name> <file>
  api -fsS -X POST \
    "$GITEA/api/v1/repos/$OWNER/$REPO/releases/$RELEASE_ID/assets?name=$1" \
    -F "attachment=@$2" > /dev/null || { echo "uploading $1 failed" >&2; exit 1; }
  echo "    $1"
}

echo "==> uploading assets"
for f in "$REL"/*; do
  name=${f##*/}
  case "$name" in "$IMG") continue ;; esac
  upload "$name" "$f"
done
[ "${FOG_RELEASE_IMAGE:-}" != "1" ] || upload download.html bin/download.html
if [ "${FOG_RELEASE_IMAGE:-}" = "1" ]; then
  # See the manifest note above: the image goes to the package registry.
  # Re-runs replace the version wholesale (a second PUT of the same file
  # would 409).
  api -fsS -X DELETE \
    "$GITEA/api/v1/packages/$OWNER/generic/fog-installer/$VERSION" > /dev/null 2>&1 || true
  for f in "$REL/$IMG" "bin/$IMG.sha256"; do
    api -fsS -X PUT --upload-file "$f" \
      "$GITEA/api/packages/$OWNER/generic/fog-installer/$VERSION/${f##*/}" > /dev/null \
      || { echo "uploading ${f##*/} failed" >&2; exit 1; }
    echo "    ${f##*/} (package registry)"
  done
fi

echo "==> publishing manifest"
git add release/latest.json release/latest.json.sig
git commit -q -m "Release $VERSION manifest" || true
git push -q origin HEAD

echo
echo "released $VERSION"
echo "release directory (self-contained; serve it anywhere): $REL/"
echo "source URL — installs and self-update both take this one value:"
echo "  $GITEA/$OWNER/$REPO"
echo "    curl -fsSL $GITEA/$OWNER/$REPO/raw/branch/main/install.sh | sh"
