---
name: ship
description: Commit the working tree to main, push, and cut the next patch release of fog via scripts/release.sh. Use when the user says "ship", "release", "cut a release", or "commit and release".
argument-hint: "[commit subject] [--minor | vX.Y.Z]"
---

# Ship: commit → push → patch release

Every fog release is the same four moves. Do them in order; each step's
check is what makes the next one safe.

## 1. Look before committing

```
git status --short
git branch --show-current
git diff --stat
```

- Must be on `main`. If not, stop and say so — releases are cut from main only.
- Read the untracked list. `release.sh` refuses to run while
  `git status --porcelain` shows anything at all (untracked files included),
  so each untracked file must be either added — because it is meant to be in
  the release — or left out of the tree by its owner. Never add scratch files,
  stray binaries at the root, `bin/`, `frontend/dist/`, `.devdata/`, or
  anything you didn't mean to create, and never delete someone else's: if the
  list holds files you can't account for, stop and ask.
- If there is nothing to commit, skip to step 3 (a manifest-only re-release
  is legitimate, e.g. after a failed upload).

## 2. Check, commit, push

```
make check
```

`release.sh` runs `make check` again, but run it first: a commit that fails
checks is a broken `main`, and CI at gitea.lan is slow (~5 min/job) to tell
you. Fix anything red before committing.

Then:

```
git add <the intended paths>        # explicit paths only — never `git add -A` or `git add .`
git commit -F - <<'MSG'
<subject>

<body>

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>
Claude-Session: <session URL from the harness instructions>
MSG
git push origin main
```

**Message style — match the log, it is deliberate:**

- **Subject** is one declarative sentence stating what is now true, in the
  repo's voice: *"BookOrbit joins the app store, on fog's managed Postgres"*,
  *"gofmt is a check: the four stragglers are formatted, and CI and make check
  gate on it"*, *"An app's library lives in Files › Apps, and the install pane
  says so instead of asking"*. Not "Add X", not "Fix Y", no trailing period,
  no conventional-commit prefix. If the user gave a subject as `$ARGUMENTS`,
  use it verbatim.
- **Body** is prose paragraphs explaining the design and the why — what
  changed in behaviour, which pieces enable it, what was deliberately not
  done. Written for the person reading `git log` in a year. Wrap at ~76
  columns. Backticks for identifiers are fine.
- Trailers as above (the Claude-Session URL is in the harness's git
  instructions for this session).

## 3. Pick the version

```
git fetch --tags -q
git describe --tags --abbrev=0        # e.g. v0.12.4
```

Bump the **patch** number: `v0.12.4 → v0.12.5`. Only use a minor bump
(`v0.13.0`) if the user asked for it (`--minor`), and only use an explicit
version if they gave one. Confirm the tag does not already exist
(`git tag -l vX.Y.Z` prints nothing).

## 4. Cut the release

```
./scripts/release.sh vX.Y.Z
```

Run it with a long timeout (it builds the frontend with `npm ci`, cross-compiles
the binaries, and uploads to gitea.lan — allow 10 minutes; run in the
background if the harness's foreground limit is shorter). It:

1. refuses to start unless the tree is fully clean (untracked files too —
   step 1's list must be empty, not just committed), the signing key at
   `~/.config/fog/release-signing-key.pem` is yours with mode 0600 and pinned
   in the code, and the git host is https or exactly the http host this
   clone allows (`git config fog.insecureCredentialHost`, set to `gitea.lan`
   here — so gitea.lan works with no flag),
2. runs `make check`,
3. builds everything into `bin/release/` (binaries, `fogd.service`,
   `fogd-rollback`, `install.sh`, `flash.sh`, a signed relative `latest.json`
   and its `.sig`, `SHA256SUMS`) and writes the signed, dated
   `release/latest.json` (+ `.sig`),
4. force-tags `vX.Y.Z` and pushes the tag,
5. creates (or replaces) the Gitea release with the commit subjects since the
   previous tag as notes, and uploads everything in `bin/release/`,
6. commits `release/latest.json` and `release/latest.json.sig` as `Release
   vX.Y.Z manifest` and pushes them.

It needs a gitea.lan credential (macOS keychain, or `FOG_GIT_TOKEN`). If it
fails partway, fix the cause and **re-run the same command** — every step is
idempotent (tag is forced, an existing release for the tag is deleted first,
the manifest commit is skipped when unchanged). If it stops on "would be
sent over plain http", the clone has lost its `fog.insecureCredentialHost`
setting: say so and let the user restore it (`git config
fog.insecureCredentialHost gitea.lan`) — don't work around it. Keep
`bin/release/` after the run: `/release` publishes it to GitHub.

## 5. Verify, then report

```
git log --oneline -3                                   # top: "Release vX.Y.Z manifest"
git status --short                                     # clean
cat release/latest.json                                # version matches, both sha256 present
go run ./internal/selfupdate/signtool verify release/latest.json   # silent: signed by the pinned key
go run ./internal/selfupdate/signtool verify bin/release/latest.json  # the copy /release publishes
curl -fsS http://gitea.lan/api/v1/repos/matthew/fog/releases/tags/vX.Y.Z \
  | python3 -c 'import json,sys; r=json.load(sys.stdin); print(r["name"], [a["name"] for a in r["assets"]])'
```

Expect thirteen assets: `fogd-linux-amd64`, `fogd-linux-arm64`,
`fog-node-{darwin,linux}-{amd64,arm64}` (four), `fogd.service`,
`fogd-rollback`, `install.sh`, `flash.sh`, `latest.json`, `latest.json.sig`, and
`SHA256SUMS` (plus `download.html` with `FOG_RELEASE_IMAGE=1`). `release/latest.json.sig`
must be in the manifest commit too: a server refuses a manifest without its
signature. fog.lan picks the release up on its next self-update poll;
if the fog MCP tools are connected, `fog_updates_check` / `fog_system_status`
can confirm the running version afterwards.

Report: the commit subject, the version, the Gitea release URL
(`http://gitea.lan/matthew/fog/releases/tag/vX.Y.Z`), and anything you
skipped. Strangers install from GitHub, which this doesn't touch: say that
`/release` publishes this version there. If a step failed, say which and show the output — never report a
release as cut unless step 5 showed it.
