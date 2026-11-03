# fog-files/1 — the app-side Files contract

*Sibling of fog-connect/1 (docs/connect-contract.md) and fog-backup/1
(docs/backup-contract.md).*

An app gets a folder as a volume in its spec. A volume that lives in
Drive names its folder relative to the Drive root — `files: Content/Books`
— and fog resolves it against the live root at deploy time, creates it
if it is missing, and binds it; the spec never carries the root's
absolute path, so the tree can move house and the app follows. The spec
editor's volume picker offers the places a folder can be — Fog-managed
storage, a **Drive folder**, a NAS share, a custom host path — and, with
folders shared across peers, one more: **Shared by a peer**, a copy of a folder
another fog shares with this one. This contract is how an app learns
where a folder came from and hears when it changes underneath it. An app
that never implements it loses nothing but latency: the mount is an
ordinary bind mount, and re-reading it on the app's own schedule works
exactly as before.

**Apps and Content.** Drive keeps two reserved folders at its root, and
the line between them is who owns what is inside.

`Apps/` holds one folder per app, named after it — `Apps/Paperless`,
`Apps/Immich` — and inside it that app's own data, one subfolder per
volume (`Apps/<App>/<volume>`). The install pane fills that in for every
folder it lists, so a new app's data is somewhere the operator can open
rather than somewhere under `/var/lib/fog` they cannot — and so what was
on screen is what got stored. Nothing rewrites a spec behind the volume
editor, where *Fog-managed* is a choice somebody made on purpose. What is
under an app's own folder travels in that app's backups
(`Volume.IsAppData`); nothing else does.

`Content/` holds the household's own libraries — `Content/Photos`,
`Content/Movies`, `Content/TV`, `Content/Music`, `Content/Books`,
`Content/Documents`, made once when fog first prepares a root, plus
whatever else an app or the operator adds. These belong to nobody in
particular: several apps naming one folder share it (the arr stack and
Jellyfin all read `Content`, so hardlinks work), and they are covered by
Drive's nightly snapshot rather than by any one app's backups.

Catalog entries default to both, so installing BookOrbit asks nothing
about paths: the install pane itemizes every folder it will mount, says
which of the two each is in and what the app sees it as, with *Change*
for a library that already exists elsewhere. The Drive page badges each
folder with the app that sees it and the container path, and refuses to
move or trash it while an app depends on it — an app's own folder moves
through the app's Drive tab (*Move…*), which stops the app, carries the
files, rewrites the spec and starts it again as one operation.

### Apps and Content on disks of their own

Either folder can live somewhere other than the Drive root: Apps on a small
fast disk, Content on a big NAS (Settings › Storage › Drive › *Apps, Content
and Backups*, settings `files_apps_location` / `files_content_location`). Fog
bind-mounts the location over `<root>/Apps` or `<root>/Content` at every
start, after a NAS share mounts late, and before any app that uses the
folder deploys. Nothing that names a path changes: specs still say
`Apps/Sonarr/config`, compose files still bind `<root>/Apps/Sonarr/config`,
and backups, the index and shares are unaware of the move.

- Changing a location is a job, like moving the root: *Move* copies the
  folder's contents and removes the old copy; *Adopt* uses what is already
  at the new place (refused when it would hide files Drive holds now).
  Apps whose volumes touch the folder are stopped for the job and started
  after it, because a container keeps the mount it was created with.
- Renames can't cross filesystems, so what Drive stages before a rename
  stays on the folder's own disk: `Apps/.fog` and `Content/.fog` hold that
  folder's uploads in progress and its trash (receipts stay in the root's
  `.fog/trash`, with `bin` naming the folder). Both names are reserved,
  like the root's `.fog`. A move between Apps and Content on different
  disks is a copy job; over WebDAV it is refused with a "copy, then delete"
  message.
- A placed folder whose disk is missing is **closed**, not empty: listing
  it fails, writes into it are refused, the walk keeps its index rows, the
  nightly snapshot is skipped, and an app in it won't start. The bind
  comes back on its own when the disk does.
- Settings › Storage › Disks & shares charges each folder's bytes to the disk
  they are on.
- Linux only (it needs `mount --bind` in the host's namespace).

### The backup directory, beside them

The same section lists **Backups**: where app snapshots, Drive snapshots
(`files/`) and recovery kits (`fog-recovery/`) are kept — `backup_dir`,
default `<datadir>/backups`. It is not in Drive and nothing is bound; the
backup manager simply writes there. It is changed only through the same
move-or-adopt job (`backup_dir` is no longer a writable setting), because
app backups and Drive snapshots are recorded by absolute path:

- *Move* copies the tree (hard links kept, so Drive snapshots stay one copy
  plus changes), verifies it, re-points every record and the setting in one
  transaction, and only then removes the old copy. On one filesystem it is
  a rename.
- *Adopt* uses what is already at the new place and leaves the old one
  alone; only records whose snapshot is found there are re-pointed.
  Backups it doesn't hold a record for are not imported.
- The job waits for running backups, restores, verifies, Drive snapshots
  and offsite uploads to finish, and turns new ones away until it is done
  (scheduled ones simply run on the next tick after). Works on any host.

## What fog does without asking

- A peer's copy mounts **read-only** (`:ro` in the compose file), whatever
  the spec says. The file has one origin — the fog that created it — and
  that origin is the only place it is edited. Nothing an app writes to a
  peer's copy would ever reach the peer, so the container cannot write it.
- A peer's copy mounts only when the peer's lens allows apps
  (`apps: allowed`) and the copy is **full**. A previews subscription
  lists files it does not hold, which a container has no way to fetch;
  the picker says *keep a full copy first* and the deploy refuses with the
  same reason if the subscription changed since.
- A copy an app mounts refuses to be removed from the Connect page
  without a warning naming the app (*Remove anyway* overrides). The Drive
  page shows *mounted by* on the folder.

## Declaring

In the app repo's `fog.yaml`:

```yaml
files:
  contract: fog-files/1
```

Fog records the declaration at build time. Only contracted apps receive
the notice below; `GET /app/v1/files/mounts` answers any app.

## The notice: `POST /fog/files/changed`

Served by the app inside its existing `scope "fog"` routes — bearer-
authenticated by `FOG_API_TOKEN`, outside session auth, like
`fog-backup/1` and `fog-connect/1`. Fog calls it whenever a folder the
app mounts changed under it:

```json
{
  "mount":     "/album",
  "hostPath":  "/var/lib/fog/files/Peers/dave/Holiday",
  "reason":    "synced",
  "changedAt": "2026-08-26T06:43:51Z"
}
```

`reason` is one of:

| reason     | what happened                                                      |
|------------|--------------------------------------------------------------------|
| `changed`  | a write landed in the folder here (an upload, WebDAV, a rename)    |
| `synced`   | a sync from the peer landed: files arrived, changed, or went       |
| `hydrated` | a file listed here was fetched from the peer (or freed again)      |
| `revoked`  | the peer stopped sharing; the copy stays until the operator decides|
| `adopted`  | the operator kept the copy as their own — it is an ordinary folder now |
| `rewired`  | an app this one uses was installed, removed, or changed its folders (no `mount`; see Wiring) |

The notice carries no listing. It says "this mount changed" and nothing
more, so two notices racing cannot write a stale state into an app, and
an app that missed one while redeploying is corrected by its own next
read. The right response is to re-read the mount: Stream rescans the
library, feed re-scans the folder, hub refreshes an attachment listing.

Fog coalesces a burst — a sync landing fifty files — into **one notice per
app and mount** after a few quiet seconds, and never retries: an app that
answers 404 has simply not implemented it. Reasons are diagnostic; an app
may treat every notice the same way.

## Discovery: `GET /app/v1/files/mounts`

Bearer-authenticated by `FOG_API_TOKEN`, rate limited per app, returns
the calling app's own Drive-backed volumes with provenance:

```json
{
  "mounts": [
    {
      "mount":        "/album",
      "hostPath":     "/var/lib/fog/files/Peers/dave/Holiday",
      "kind":         "peer",
      "peer":         "dave",
      "share":        "Holiday",
      "readOnly":     true,
      "hydrated":     true,
      "status":       "active",
      "lastSyncedAt": "2026-08-26T06:43:51Z"
    },
    { "mount": "/uploads", "hostPath": "/var/lib/fog/files/Uploads", "kind": "local", "readOnly": false, "hydrated": true }
  ]
}
```

`kind` is `local` (a folder in this fog's Drive, whether the spec named
it with `files:` or with a host path under the root) or `peer` (a copy of
a peer's share). Volumes that do not point into Drive are not listed — they
are the app's own business. `status` follows the subscription
(`active`, `paused`, `error`, `offer_changed`, `revoked`), so an app can
explain to its own users where a folder comes from and whether it is
still being refreshed.

## Wiring: apps that share folders

Some apps work on the same files: a downloader and the app that files
what it downloads, a library and the app that delivers into it. Each sees
the shared folder at its own container path — SABnzbd's `/data` is
Hook's `/data/Downloads` — and fog is the only party that knows both, so
fog does the translating. Nobody types a path mapping.

**Declaring.** A provider names the volumes a consumer must see in its
connection; a consumer names the providers in `uses`, which connecting an
`app` want on its Integrations tab fills in
([integrations-contract.md](integrations-contract.md)):

```yaml
# SABnzbd
connection: { port: 8080, secret: SABNZBD_API_KEY, shares: [media] }
volumes:
  - { name: media, mount: /data, files: Content/Downloads }

# Hook
uses: [sabnzbd, stream, bookorbit]
volumes:
  - { name: content, mount: /data, files: Content }
```

**Fog mounts what is missing.** Installing a consumer mounts every
shared folder of its installed providers that none of its volumes
already covers; installing a provider does the same to every app that
uses it (a revision, `wiring: mount …`, and a redeploy if it runs). The
added volume sits at `/shared/<provider>/<volume>` and **follows** the
provider's:

```yaml
- { name: stream-stock, mount: /shared/stream/stock, host: /mnt/stock, follows: stream/stock }
```

A volume with `follows: <app>/<volume>` takes that volume's Drive folder
or host path whenever the target's spec changes — Drive's *Move…* on the
provider carries its followers along. Only a volume the target shares
(`connection.shares`), followed by an app that uses the target, is ever
copied: a `follows` naming any other volume of another app (its own data,
its credentials) is left as it is. Anything that still cannot be seen
afterwards is an Inbox issue (`app-unwired`) whose fix mounts it.

**Fog hands over the translations.** Each entry of
`GET /app/v1/connections` carries them:

```json
{
  "app": "sabnzbd", "url": "http://sabnzbd:8080", "credential": "…",
  "paths":  [{ "remote": "/data", "local": "/data/Downloads" }],
  "unseen": []
}
```

`paths` turns a path the provider reports into the caller's own, for
every folder both mount — equal folders map mount to mount, and a folder
one mounts inside the other's maps through the path between them. Rows
are longest `remote` first; translate with the first whose `remote` is
the path or a parent folder of it, and pass a path no row matches through
unchanged. `unseen` names the provider's shared volumes the caller cannot
see. The shape is the familiar *remote path mapping*: an app that keeps
those already can take these rows as they are.

**Fog says when it changed.** A contracted app gets
`POST /fog/files/changed` with `"reason": "rewired"` (and no mount)
whenever an app it uses is installed, removed, or revised. The right
response is to fetch its connections again and re-derive whatever it
built from them.

## Searching Drive for the operator: `GET /app/v1/files/search`

An app the operator is using right now can search Drive by name on their
behalf — newtab's search box does. It is default-deny on three counts:

- **Granted.** The app has `drive` in its spec's `uses`, which the
  operator turns on as **Search Drive for you** under the app's Config, App
  access (a catalog entry may propose it; installing it is the operator
  agreeing). Without it: `403`.
- **Present.** The request carries the app's `FOG_API_TOKEN` *and* the
  `Fog-Assertion` the gate handed it for this request, fresh, for one of
  the app's own gated hosts, naming the operator
  ([signin-contract.md](signin-contract.md)). Anyone else at the door:
  `403`.
- **Scoped.** Results never include another app's own folder
  (`Apps/<other>`), a copy of a peer's share (`Peers/…` — a peer says
  whether apps may read its files, and only through a mount), or anything
  hidden: a dotfile, anything inside a dot-folder, or what the Drive page
  hides. The app's own `Apps/<App>` folder and every Drive folder it mounts
  are included.

`q` is the name to find (two characters at least; shorter answers
nothing), `limit` at most 50. Each entry is what a search box shows and
links to, and nothing else the index knows — no digests:

```json
{ "entries": [
  { "path": "Content/Books/Dune.epub", "name": "Dune.epub", "kind": "file",
    "size": 1048576, "modified": "2026-08-26T06:43:51Z", "mime": "application/epub+zip" }
] }
```

## Links in Drive

Drive never follows a symbolic link. An app can write into the folders it
mounts, and a link is how a program would make fog read or write somewhere
other than where it was asked to: a share link of `Public` serving
`Private/secret.txt` because `Public` grew a link to `../Private`, a
volume bound to another app's data. So a link is an entry of kind `other`:
listed, never opened, never descended into, never copied as what it
points at. A path that runs through one is refused, whoever asks — the
Drive page, WebDAV, a share link, a peer, the snapshot, an app's volume at
deploy. Fog reaches every file and folder one name at a time from the
root with `O_NOFOLLOW`, so a folder swapped for a link between a check and
a read is caught, not followed. Copies (a copy job, a move between disks,
a root or placement move) recreate a link as a link, pointing where it
pointed. The nightly snapshot leaves links out.

Nor does fog open anything but a regular file or a folder: a FIFO or a
device node an app made is listed as `other` and refused.

Names are shown, not trusted. A name an app wrote may hold control
characters, bidirectional overrides (`invoice\u202Efdp.exe` reads as
`invoiceexe.pdf`), line separators (U+2028, U+2029) or characters that are
not drawn (U+200B, U+2060–2064, U+FEFF; a zero-width joiner or non-joiner
except between two non-ASCII characters, where emoji and scripts such as
Persian need it). Wherever fog shows a Drive name — the Drive page, search,
share pages, Inbox notices, push notifications, MCP tool output (`display`)
— those characters are made visible (`␊`, `\x85`, `⟨U+202E⟩`) and the name
is isolated from the text around it. Operations and downloads keep the real
name; a download's `Content-Disposition` carries it RFC 5987-encoded.

Depth is bounded. An app can nest folders as deep as it likes; fog walks
no more than 256 levels below the Drive root, holding a bounded number of
descriptors however deep it goes. Past that, a zip, a copy or a move is
refused ("nested more than 256 levels deep"), the index leaves the deeper
part out, and the nightly snapshot backs up everything else and logs what
it left out. Deleting such a tree works at any depth.

A move to another disk (the Drive root, Apps or Content, an app's folder)
copies by descriptor on both sides — a link is copied as a link and never
written through, even where the destination folds case — refuses writes
into what it moves and stops the apps using it while it copies, and checks
every file's size and modification time before the source is removed. It
copies sparse files sparse (an app's 16 TiB file of holes costs its real
blocks; runs of zeros are left as holes even where the source cannot report
them), checks there is room for those blocks before it starts, and can be
cancelled at any moment, even mid-file; a cancelled or failed copy is
removed and the source is left as it was.

The nightly snapshot never fails on one app's names, and never lets one
entry's copy replace another's: on a backup disk that folds names together
— case ("X" beside "x/"), Unicode normalisation (an NFC "café" beside an
NFD one, on APFS, ZFS formD or a share), or anything else the disk does —
the disk itself is asked (the name already leads to an entry this snapshot
placed), the later entry is left out and named in the Inbox, and
everything else is backed up.

Writes stage under a fixed-length random name beside their target, never
the target's name plus a suffix, so a name at the 255-byte limit backs up,
copies and restores like any other.

A rename that changes only case (`notes.txt` → `Notes.txt`) replaces
nothing: on a case-sensitive disk where `Notes.txt` is another file, it is
a conflict like any other rename onto a taken name.

## Guarantees fog makes

1. A peer's copy is never writable from a container.
2. A mount is either a full copy or refused; an app never sees a folder
   whose files are somewhere else.
3. Every change fog knows about reaches a contracted app as a notice,
   coalesced, within seconds.
4. Nothing in the notice or the mounts list is a secret: paths, names, a
   peer's display name. No keys, no addresses.
5. A Drive or host-path volume binds the folder its spec names, never a
   link in its place, at every start — the restart policy's and Docker's
   own at boot included. A path is resolved again by Docker at each start,
   and an app that can write a folder above another's (a downloader with
   `Content`, a library with `Content/Movies`) could swap it for a link to
   `/` between two. So fog **pins** the folder: it opens it with no link
   followed anywhere on the way (openat2 `RESOLVE_NO_SYMLINKS`), bind-mounts
   that descriptor at `<data>/binds/<app>/<volume>`, a path only fogd
   writes, and compose binds the pin, with `create_host_path: false`. A link
   swapped in afterwards changes nothing; a pinned folder deleted out from
   under it makes the start fail, never bind something else. Pins are made
   at each deploy and again before each start, and live on a tmpfs: after a
   reboot they are absent until fogd makes them, and until then Docker
   refuses to start those containers (fogd then starts them). A file is
   pinned as a file (a config file, `/etc/localtime`'s zone). No link is
   ever followed — an app running as root makes root's links — with one
   exception: `/etc/localtime` and `/etc/timezone`, bound read-only, into
   `/usr/share/zoneinfo`. Any other link in a bind source is refused, at
   deploy and at start (give the real path), and the app's containers are
   removed rather than left for a restart policy to bring back. A writable
   volume holding another volume of the same app is refused outright. Where
   fog can't
   pin (macOS development) it binds paths as they are.
6. An app sees nothing of Drive beyond its mounts unless the operator
   grants `drive`, and then only names, outside other apps' folders and
   peers' copies.

## Adopters

Stream and Office declare the contract and serve `/fog/files/changed`.
The `fog-client` gem has `FogClient.files.mounts` for discovery.

hub's attachment storage can point a `Storage::Location` at a fog-mounted
peer folder, and the `/fog/files/changed` hook invalidates its file
listings. That work lives in hub's repo and follows this contract.
