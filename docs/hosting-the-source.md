# Hosting Fog's code somewhere else

Everything that installs or updates Fog takes **one value**: the source URL.
Point it at a different host and the installer, the systemd unit, and every
self-update follow it — there is no second place to edit.

```sh
# an existing Linux server
curl -fsSL https://example.com/fog/install.sh | FOG_SOURCE=https://example.com/fog sh

# a Proxmox node
./proxmox-lxc.sh --source https://github.com/you/fog
```

Unset, the source is fog's public releases, **`https://github.com/fogapp/fog`**
(`install.sh`, both Proxmox scripts, and `flash.sh`, which reads
`https://github.com/fogapp/fog/releases/latest/download/latest.json`). When
the default doesn't answer, they say which one they tried and how to point
`FOG_SOURCE` elsewhere.

An upgrade never changes a server's source. fogd reads its own (the
setting, else the `FOG_UPDATE_BASE_URL` on its unit) and self-update
touches neither; `install.sh` re-run on a server fog is already on, with no
`FOG_SOURCE`, keeps the source that server's unit names instead of the
public default (`TestInstallShUpgradeKeepsTheServersSource`). So servers
installed from a private Gitea keep updating from it.

## What a source has to serve

| File | Who reads it | Why |
|---|---|---|
| `latest.json` | `install.sh`, fogd's updater, `flash.sh` | version + artifact URLs + sha256 of each |
| `latest.json.sig` | the same | ed25519 signature over `latest.json`'s exact bytes, by fog's release key |
| `fogd-linux-amd64`, `fogd-linux-arm64` | `install.sh`, fogd's updater | the binary, checked against the manifest's sha256 |
| `fogd.service` | `install.sh` | the systemd unit, checked against the manifest |
| `fogd.socket` | `install.sh` | the socket unit that holds fogd's port across restarts ([installer.md](installer.md)) |
| `fogd-rollback` | `install.sh` | the guard that puts the previous fogd back after a failed self-update ([installer.md](installer.md)) |
| `fog-node-<os>-<arch>` | fogd, for paired compute machines | the node agent, named in the manifest like the binary |
| `install.sh` | you, `proxmox-lxc.sh`, `proxmox-vm.sh` | the installer itself |
| `flash.sh` | you | writes the installer image to a USB stick ([installer.md](installer.md)) |
| `SHA256SUMS` | people | every file's digest, for checking a copy by hand |

`scripts/release.sh` builds all of these into `bin/release/` and uploads them
as the release's assets — the directory is a complete source on its own. A
repo also serves `release/latest.json` and `release/latest.json.sig`
(committed by `release.sh`) and `install.sh` at their repo paths.

## Shapes it understands

The source is resolved by *probing*, not guessing: candidates are tried in
order and the first manifest that fetches **and verifies** decides where
everything else lives. That's why a directory whose path looks like
`owner/repo` still works — and why a forged manifest at the first candidate
is passed over, not used.

| You give it | It looks for the manifest at, in order |
|---|---|
| `http://gitea.lan/matthew/fog` (Gitea/Forgejo, or any other host) | `…/raw/branch/BRANCH/release/latest.json`, then `…/latest.json` |
| `https://github.com/you/fog` | `…/releases/latest/download/latest.json` (the release only — never the repo's committed manifest, whose URLs name the host the release was cut on), then `…/latest.json` |
| `https://gitlab.com/you/fog` (any host with `gitlab` in its name; subgroups work) | `…/-/raw/BRANCH/release/latest.json`, then `…/latest.json` |
| `https://files.example.com/fog` (any directory) | `…/latest.json` |
| `https://files.example.com/fog/latest.json` | itself |

`BRANCH` is `main` unless `FOG_BRANCH` says otherwise. The host is compared
case-insensitively, empty path segments (`host//owner/repo`) collapse, and a
trailing slash is ignored. A source must be `http://` or `https://` and may
not contain spaces, quotes, `$`, `;`, `?`, `#` or `@` — it ends up in a
systemd unit and in curl's arguments, so anything else is refused outright.

Two implementations hold this table: fogd's updater (`resolve.go`), and the
shell *resolver block* — one block of text that `install.sh`,
`proxmox-lxc.sh` and `proxmox-vm.sh` each carry verbatim
(`scripts/test-source-resolution` fails if the copies differ). They agree on
shapes, order, branch, refusals, signature and age limit, and
`internal/selfupdate/shellsource_test.go` runs both over every shape and
against one live server, failing if they disagree. For a Gitea/Forgejo or
GitHub repo the updater additionally asks the host's API for the release's
notes (shown in the update prompt); that never changes which manifest is
used. `FOG_PRINT_SOURCE=1 sh install.sh` prints what a given source
resolves to without installing anything — the fastest way to check a new
host:

```
$ FOG_PRINT_SOURCE=1 FOG_SOURCE=https://files.example.com/fog sh install.sh
source=https://files.example.com/fog
manifest=https://files.example.com/fog/latest.json
raw=https://files.example.com/fog
version=v0.6.15
artifact=https://files.example.com/fog/fogd-linux-amd64
```

### Where the artifacts come from

Each artifact is tried at the manifest's URL, then at the same path on the
source's own host (a git host names its links by its configured root, which
another network may reach only by IP), then — for a GitHub source — in that
repo's GitHub release of the manifest's version, then beside the manifest
(a mirror serving a manifest whose URLs still name the original host).
`install.sh` and the updater try the same list, and only the manifest's
sha256 is ever accepted: a location that answers with other bytes (another
server where the manifest's host used to be, a captive portal) is passed
over for the next, not taken as the end of the download.

## Signed releases

Every manifest is signed. `release.sh` signs `latest.json` with an ed25519
key only the release process holds (`FOG_SIGNING_KEY`, default
`~/.config/fog/release-signing-key.pem`, which must be the releaser's own
file with mode 0600) and publishes the signature as `latest.json.sig` beside
it. fogd pins the public key in its binary (`internal/selfupdate/sign.go`);
`install.sh`, both Proxmox scripts (in the resolver block) and
`imaging/flash.sh` pin the same key; all of them refuse a manifest whose
signature doesn't verify. The manifest's sha256s then cover every file —
the binaries, the unit, the rollback guard, `install.sh` and `flash.sh`
themselves, and the installer image.

That covers every path a mirror could reach:

- **fogd's self-update** verifies the manifest, then the binary.
- **`install.sh`** verifies the manifest, then every file, before anything
  on the system changes.
- **The Proxmox scripts** never execute anything they fetch on the node:
  they resolve and verify the release with their resolver block, take
  `install.sh` from the signed manifest by its sha256, and push only those
  verified bytes into the container or VM, where `install.sh` verifies
  everything it fetches again. `--dry-run` downloads and checks, and runs
  nothing.
- **`flash.sh`** verifies the manifest, the image's sha256 after download,
  and again on the bytes as they are written to the stick.

So a mirror, a proxy, or anyone on the path to a plain-`http://` source can
withhold an update, but can't make a server install anything fog didn't
release. The trust root is the first script you run: `install.sh` or a
Proxmox script fetched with `curl` is trusted as fetched, so fetch it from a
source you trust (over https where you can). `flash.sh` given
`FOG_IMAGE_URL` instead of a manifest checks only the image's own
`.sha256`, beside it — that URL is yours to vouch for.

Manifests are also **dated**. `release.sh` writes `issued_at` (Unix seconds)
into the signed bytes, and fogd, `install.sh`, the Proxmox scripts and
`flash.sh` all refuse a manifest issued more than **180 days** ago, or
undated. Signatures don't expire on their own, so without this a mirror
could hand a fresh install an old release forever. A source that can offer
nothing newer is withholding: fogd raises an Inbox notice saying so (once,
apart from "can't reach the source") and keeps running as it is. A project
quiet for longer than 180 days re-runs `release.sh` to stay installable;
`FOG_ALLOW_OLD_RELEASE=1` takes an old release on purpose.

Updates also only move forward: fogd never installs a release older than or
equal to the one running, so a replayed old (validly signed) manifest is
reported, not offered. Versions are ordered by semver precedence exactly
(`v1.0.0-rc.9` < `v1.0.0-rc.10` < `v1.0.0`; no leading zeros), the same in
fogd, `install.sh` and the Proxmox scripts, and `release.sh` only tags
versions that order. `install.sh` refuses to replace an installed fogd
with an older release unless `FOG_ALLOW_DOWNGRADE=1`; it reads the installed
version from the binary itself (a marker stamped beside the version), never
over the network, where another process could answer for it. A fogd whose
own version isn't a release (a dev build) can't tell an update from a
downgrade, so it takes none — unless `FOG_UPDATE_FROM_UNVERSIONED=1` is set
on its unit by hand — and `install.sh` treats replacing one, or one from
before versions were stamped, as a downgrade.

An update-source access token (for a private repo) is bound to the source
it was saved for. It is only ever sent over https, only to that source's
own host (for a GitHub source, also `raw.githubusercontent.com` and
`api.github.com`), never to a host a manifest points downloads at. Saving a
token for a plain-http source is refused, and changing the source to
another host deletes the saved token — so no one can collect it by pointing
the source at a server of their own.

A fork that releases its own builds generates its own key
(`go run ./internal/selfupdate/signtool keygen <path>`), pins it in those
places (sign.go, `install.sh`'s resolver block — copied into both Proxmox
scripts — and `flash.sh`), and its users pass it as `FOG_RELEASE_KEYS`
(base64 SPKI). Rotating fog's own key is the same list growing by one: a
release signed by the old key pins both, and a later one drops the old.

## Where the value goes

| | How |
|---|---|
| **`install.sh`** | `FOG_SOURCE=…` (the old `FOG_REPO_URL` still works) |
| **`proxmox-lxc.sh`, `proxmox-vm.sh`** | `--source …` or `FOG_SOURCE`; they resolve and verify once on the node and carry the verified `install.sh` into the container or VM |
| **fogd's self-update** | Settings › Server › Updates › *Update source* › *Repository URL*. `install.sh` seeds it from wherever it installed from — a drop-in on the unit setting `FOG_UPDATE_BASE_URL` (the source, or `FOG_MANIFEST_URL` when that was given) and `FOG_UPDATE_BRANCH` — so a fresh server already updates from the right place, whatever shape its source is; the setting, once written, wins |
| **`scripts/release.sh`** | `FOG_GIT_HOST`, `FOG_GIT_OWNER`, `FOG_GIT_REPO`, `FOG_GIT_USER`, `FOG_GIT_TOKEN`; `FOG_ARTIFACT_BASE` serves the binaries from a mirror or CDN; `FOG_SIGNING_KEY`. It sends the git credential over `http://` only to the one host this clone names in `git config fog.insecureCredentialHost` (a server on your own network; never committed), and refuses to build a tree with untracked files |
| **`scripts/publish-public.sh`** | pushes the released tree to `github.com/fogapp/fog` and publishes `bin/release/` as the GitHub release of the same version (`scripts/publish-github-release`, as the `fogapp` account through `gh`) — the `/release` skill |

## When the guessing doesn't fit

Three environment variables override it outright, in `install.sh` and both
Proxmox scripts alike:

- `FOG_MANIFEST_URL` — the manifest, exactly. Its signature is still checked.
- `FOG_RAW_BASE` — where repo-relative files are served; only reported now
  (`FOG_PRINT_SOURCE=1`), since every installed file comes from the manifest.
- `FOG_BRANCH` — the branch a repo's committed manifest and raw files come
  from; default `main`.

And four safety valves, all off by default and none reachable from the
settings page:

- `FOG_ALLOW_UNSIGNED=1` skips the signature in `install.sh` and `flash.sh`,
  and puts `FOG_UPDATE_ALLOW_UNSIGNED=1` on the unit so the updater skips it
  too — for a host that builds its own unsigned releases.
- `FOG_RELEASE_KEYS=<base64 SPKI …>` replaces the keys `install.sh` and
  `flash.sh` pin, for a fork's own key.
- `FOG_ALLOW_DOWNGRADE=1` lets `install.sh` install an older release than
  the fogd running.
- `FOG_ALLOW_OLD_RELEASE=1` lets `install.sh`, the Proxmox scripts and
  `flash.sh` take a manifest issued more than 180 days ago.

## Portable release directories

`bin/release/` is a complete release: copy it to any static host and
`FOG_SOURCE=<its URL>` installs and updates from it. Its `latest.json` names
the files relatively, never by URL:

```json
{ "version": "v0.6.15",
  "artifacts": { "linux-amd64": { "url": "fogd-linux-amd64", "sha256": "…" } } }
```

Relative names resolve against wherever the manifest is served, so the
directory can be copied to another host, mirrored, or served from a CDN
without rewriting anything inside it — and the same manifest is correct as a
Gitea or GitHub release asset, where the files sit beside it — which is how
fog's public GitHub releases work. `install.sh`, fogd's updater, and
`imaging/flash.sh` all resolve them. The installer image
(`FOG_RELEASE_IMAGE=1`) is in the directory too, named relatively the same
way; on Gitea it's uploaded to the package registry rather than as a release
asset (multi-GB attachments fail there), and the committed manifest names it
there. GitHub takes assets under 2 GiB, and `publish-github-release` skips
(and says so) any file larger.

The manifest committed to the repo (`release/latest.json`, read at the
raw-file path) always carries absolute URLs to the release it was cut on,
since relative names there would resolve inside the repo, where no binaries
are. Each of the two is signed separately. A GitHub source never reads the
committed one: with no release marked latest, a GitHub install stops and
says what it tried.

## How fog's own releases reach GitHub

fog is cut on a private Gitea (`/ship`, `release.sh`) and published to
`github.com/fogapp/fog` (`/release`, `publish-public.sh`): the released tree
as one commit, then `bin/release/` as the GitHub release of that version,
tagged at that commit and marked latest. `publish-github-release` uploads
exactly the files the signed manifest names, each checked against the
sha256 inside it, plus the manifest, its signature and `SHA256SUMS` (which
must agree with the manifest); anything else in `bin/release/` is refused.
It also checks the version and the signature against the pinned key, and
that `gh` is logged in as `fogapp`; a re-run replaces the assets of an
existing release rather than making a second. A pre-release (`v1.2.0-rc.1`)
is published as one and never marked latest; a release is marked latest
only when it is at least the release GitHub calls latest, so re-running an
older version never takes latest back.

The sync commit is made under the public identity the clone names
(`git config fog.publicAuthorName` / `fog.publicAuthorEmail`), never the
local git identity, with UTC timestamps, and `publish-public.sh` refuses
without it. Its scan covers binary files, treats a rename or copy to a
name like `.env` as a new file, and refuses a binary over 1 MiB that isn't
a web asset, font or icon. Release binaries are built with `-trimpath` and
`-buildvcs=false`, so they carry no build-machine paths and no record of
the private repo's commits; `scripts/check-release-binaries` checks every
binary for both (`go version -m`, and the bytes themselves) in `release.sh`
and again in `publish-github-release`, which refuses to upload one that
fails.

A fork does the same with its own repo (`FOG_PUBLIC_REPO`,
`FOG_PUBLIC_GH_USER`) and its own key, or uploads its `bin/release/*` with
`gh release create`. To move an existing server to a GitHub source, set
Settings › Server › Updates › *Update source* › *Repository URL* to the repo
URL; nothing else changes: the manifest format, the signature, the
checksums, and the unit are identical.

## What this does not cover

- **The Go module path** (`gitea.lan/matthew/fog`, in `go.mod` and every
  import) is a compile-time identifier, not a URL anything fetches. Moving
  hosts doesn't require changing it; renaming it is a separate mechanical
  change across all imports.
- **`git remote`** — `release.sh` pushes tags to whatever `origin` is. Point
  it wherever the code lives.
- **`Documentation=` in `packaging/fogd.service`** and the example URLs in
  these docs are prose; they're stale after a move, and harmless.
- **Withholding.** Signing stops a source from serving a bad release; it
  can't make an unreachable or stale source serve a new one. The Inbox says
  when update checks keep failing.
