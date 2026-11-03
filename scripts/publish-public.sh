#!/bin/sh
# Publish the released tree to the public GitHub repo as one new commit, with
# none of the private history, and the release's signed files (bin/release/,
# from release.sh) as the GitHub release of the same version — the one
# strangers' installs and fogd's updates read. Dry run by default; --push
# publishes.
#
#   ./scripts/publish-public.sh                     check, and show what would go
#   ./scripts/publish-public.sh --push              publish
#   ./scripts/publish-public.sh --push --accept-findings
#                                                   publish past a secret-scan hit
#                                                   a person has judged harmless
#
# The commit is built with `git commit-tree`: same files as HEAD, parent is
# public/main, so only that commit's objects are uploaded. Private subjects
# never appear; the message is just the version. Its author and committer
# are the public identity this clone names, never the local git identity:
#
#   git config fog.publicAuthorName  '<name>'
#   git config fog.publicAuthorEmail '<email>'
#
# and its timestamps are UTC, so they don't carry the local timezone.
#
# A raw `git push public main` would upload the whole history, so this script
# points the `public` remote's push URL at nothing and pushes to the real URL
# itself. That guard is local config, and it is reapplied on every run.
#
#   FOG_PUBLIC_URL   git@github.com:fogapp/fog.git
#   FOG_PUBLIC_KEY   ~/.ssh/fog_ed25519   the agent may offer another account's
#                                         key first, so this one is forced
set -eu

PUBLIC_URL="${FOG_PUBLIC_URL:-git@github.com:fogapp/fog.git}"
PUBLIC_KEY="${FOG_PUBLIC_KEY:-$HOME/.ssh/fog_ed25519}"
NO_PUSH="push-disabled--use-scripts-publish-public"

# Credentials that must never reach the public repo. Only lines this sync
# changes are scanned, so existing test fixtures don't fire on every run.
SECRET_RE='-----BEGIN ([A-Z]+ )?PRIVATE KEY-----|gh[pousr]_[A-Za-z0-9]{36}|github_pat_[A-Za-z0-9_]{22,}|AKIA[0-9A-Z]{16}|sk-ant-[A-Za-z0-9_-]{20,}|sk-[A-Za-z0-9]{32,}|xox[baprs]-[A-Za-z0-9-]{10,}|AIza[0-9A-Za-z_-]{35}'
SECRET_FILE_RE='(^|/)(\.env(\..*)?|id_(rsa|ed25519|ecdsa)|.*\.(pem|p12|pfx|key))$'
# A binary file this large is refused unless its path is one of these: the
# web UI's built assets, fonts and icons. Anything else this big is more
# likely a stray build (a 16 MB binary at the repo root) than source.
BINARY_LIMIT=1048576
BINARY_ALLOW_RE='^frontend/|\.(woff2?|ttf|otf|eot|ico|icns|png|svg|jpe?g|gif|webp|avif)$'

PUSH=0
ACCEPT=0
for arg in "$@"; do
  case "$arg" in
    --push) PUSH=1 ;;
    --accept-findings) ACCEPT=1 ;;
    *) echo "usage: publish-public.sh [--push] [--accept-findings]" >&2; exit 2 ;;
  esac
done

die() { echo "publish-public: $*" >&2; exit 1; }

cd "$(dirname "$0")/.."

# Who the public commit says made it: set once per clone, by its owner.
PUB_NAME=$(git config --get fog.publicAuthorName 2>/dev/null || true)
PUB_EMAIL=$(git config --get fog.publicAuthorEmail 2>/dev/null || true)
if [ -z "$PUB_NAME" ] || [ -z "$PUB_EMAIL" ]; then
  die "no public identity for the sync commit. It must never be your personal git identity, so set the one to publish under, once, in this clone:
  git config fog.publicAuthorName  '<public name>'
  git config fog.publicAuthorEmail '<public email>'
Both are local to this clone and never committed."
fi

# Only what's released goes public.
[ "$(git branch --show-current)" = main ] || die "not on main"
git diff --quiet HEAD || die "working tree is dirty — commit first"
git fetch -q origin
[ "$(git rev-parse HEAD)" = "$(git rev-parse origin/main)" ] \
  || die "main and origin/main differ — sync them first"
VERSION=$(git describe --tags --abbrev=0)
UNRELEASED=$(git log --format=%s "$VERSION..HEAD" | grep -vx "Release $VERSION manifest" || true)
[ -z "$UNRELEASED" ] || die "commits since $VERSION are not released — run /ship first:
$UNRELEASED"

[ -f "$PUBLIC_KEY" ] || die "no key at $PUBLIC_KEY"
GIT_SSH_COMMAND="ssh -i $PUBLIC_KEY -o IdentitiesOnly=yes"
export GIT_SSH_COMMAND

git remote get-url public >/dev/null 2>&1 || git remote add public "$PUBLIC_URL"
git remote set-url public "$PUBLIC_URL"
git remote set-url --push public "$NO_PUSH"
git fetch -q public

git rev-parse -q --verify public/main >/dev/null \
  || die "public/main not found — the first publish is done by hand"
if git merge-base HEAD public/main >/dev/null; then
  die "public/main shares history with main — stop and investigate"
fi
# The sync replaces the whole tree, so a commit made on GitHub would be undone
# by the next one.
OUTSIDE=$(git log --format=%s public/main | grep -Ev '^(Initial public release|Sync to v.*)$' || true)
[ -z "$OUTSIDE" ] || die "public/main has commits this script didn't make — bring them into main first:
$OUTSIDE"

# The GitHub release is checked before anything is pushed: the release
# directory must be this version's, signed, and gh logged in as the public
# account (scripts/publish-github-release says how, and changes nothing).
./scripts/publish-github-release "$VERSION" --check

if [ "$(git rev-parse 'HEAD^{tree}')" = "$(git rev-parse 'public/main^{tree}')" ]; then
  echo "public main is already at $VERSION"
  # A re-run after a failed upload finishes the release; nothing to push.
  if [ "$PUSH" = 1 ]; then
    ./scripts/publish-github-release "$VERSION" --publish "$(git rev-parse public/main)"
  else
    echo "dry run ok — pass --push to make sure the GitHub release of $VERSION is complete"
  fi
  exit 0
fi

echo "==> $VERSION: changes since the last publish"
git diff --stat=100 public/main HEAD | tail -25

# --text: binary files are scanned too (a key baked into a built asset).
# Renames and copies count as new files for the name check: moving
# config.sample to .env is adding a .env.
FINDINGS=$(git -c core.quotePath=false diff --text -G"$SECRET_RE" --name-only public/main HEAD)
NEWFILES=$(git -c core.quotePath=false diff -M -C --diff-filter=ACR --name-only public/main HEAD | grep -E "$SECRET_FILE_RE" || true)

# Large binaries that aren't web assets are refused outright.
BIG=""
CHANGED=$(git -c core.quotePath=false diff -M -C --diff-filter=ACMR --name-only public/main HEAD)
while IFS= read -r f; do
  [ -n "$f" ] || continue
  size=$(git cat-file -s "HEAD:$f" 2>/dev/null || echo 0)
  [ "$size" -gt "$BINARY_LIMIT" ] || continue
  # Binary: a NUL in its first 8000 bytes, as git itself decides.
  nuls=$(git cat-file -p "HEAD:$f" | head -c 8000 | tr -dc '\000' | wc -c | tr -d ' ')
  [ "$nuls" -gt 0 ] || continue
  printf '%s\n' "$f" | grep -Eq "$BINARY_ALLOW_RE" && continue
  BIG="$BIG
    $f ($size bytes)"
done <<EOF
$CHANGED
EOF
[ -z "$BIG" ] || die "binary files over $BINARY_LIMIT bytes that aren't web assets, fonts or icons:$BIG
Remove them from the commit, or, if one truly belongs in the public tree, add its path to BINARY_ALLOW_RE in this script."

if [ -n "$FINDINGS$NEWFILES" ]; then
  echo "==> secret scan hit these files (matches not printed):" >&2
  printf '%s\n' "$FINDINGS" "$NEWFILES" | sed '/^$/d' | sort -u | sed 's/^/    /' >&2
  [ "$ACCEPT" = 1 ] || die "not publishing — inspect them, then pass --accept-findings if harmless"
  echo "==> accepted by --accept-findings" >&2
fi

NOW="$(date -u +%s) +0000"
SHA=$(GIT_AUTHOR_NAME="$PUB_NAME" GIT_AUTHOR_EMAIL="$PUB_EMAIL" GIT_AUTHOR_DATE="$NOW" \
  GIT_COMMITTER_NAME="$PUB_NAME" GIT_COMMITTER_EMAIL="$PUB_EMAIL" GIT_COMMITTER_DATE="$NOW" \
  git commit-tree 'HEAD^{tree}' -p public/main -m "Sync to $VERSION")

if [ "$PUSH" = 0 ]; then
  git push --dry-run --no-follow-tags "$PUBLIC_URL" "$SHA:refs/heads/main"
  echo "dry run ok — would publish $VERSION as $SHA, by $PUB_NAME <$PUB_EMAIL>; pass --push to publish"
  exit 0
fi

# Never forced: if public moved since the fetch, this fails rather than losing it.
git push --no-follow-tags "$PUBLIC_URL" "$SHA:refs/heads/main"
git fetch -q public

[ "$(git rev-parse public/main)" = "$SHA" ] || die "pushed, but public/main is not $SHA"
[ "$(git rev-parse 'public/main^{tree}')" = "$(git rev-parse 'HEAD^{tree}')" ] \
  || die "pushed, but public/main's files differ from HEAD"
echo "pushed $VERSION as $SHA"

# The release, tagged at the sync commit. If this fails, the commit is out
# and the release isn't: re-run with --push, which only finishes the release.
./scripts/publish-github-release "$VERSION" --publish "$SHA"
echo "published $VERSION as $SHA"
