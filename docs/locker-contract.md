# fog-locker/1 — the offsite layout

A locker is fog's sealed, self-describing copy of its backups in an
object-storage bucket. This document fixes the format so that anything
that can list and fetch objects — fogd, the `fog-locker` command, a script
written years from now — can read one given the bucket credentials and the
passphrase. This page is the bytes.

A locker holds fog's backups as plain sealed objects: no repository and no
lock files. Backups held in fog's incremental store go up as the store's own
packs (§6), so a night copies only what changed; the store does the
chunking, the locker only holds its pieces. A writer only adds,
and retention hides rather than destroys, so the bucket key needs no right to
delete, the bucket can be locked against change, and a locker copies
byte-for-byte to any other provider.

Reference implementation: `internal/offsite`. Reader: `cmd/fog-locker`.

## 1. Where a locker lives

```
<prefix>/fog-locker/1/<locker-id>/
```

- `<prefix>` is whatever the operator configured for the target, with no
  leading or trailing slash (empty for the bucket root).
- `<locker-id>` is 32 lowercase hex characters (128 random bits) minted
  when the locker was created. Several lockers can share a bucket and a
  prefix; listing `<prefix>/fog-locker/1/` with delimiter `/` discovers
  them. The one other name there is `probe`, which target probes write
  under and readers skip.

Object *names* are plaintext. Object *contents* are sealed (§4), with one
exception: `locker.json`.

## 2. Objects

| Name | Sealed | Contents |
|---|---|---|
| `locker.json` | no | §3.1 — format, id, KDF parameters, wrapped keys |
| `kits/fog-recovery-kit-<stamp>.enc` | no (kit-sealed) | the recovery kit exactly as fog wrote it to disk: Argon2id + AES-256-GCM under the same passphrase (`backup.DecryptKit`, which still reads the legacy scrypt format too). A reader takes only objects whose name is exactly `fog-recovery-kit-<stamp>.enc` — a planted name that would sort newest is ignored — and, after decrypting, refuses a kit whose sealed stamp does not match its name. |
| `apps/<app>/<stamp>.snapshot` | yes | a tar of the backup's snapshot directory (§3.2) |
| `apps/<app>/<stamp>.json` | yes | `AppRow` — the backup's record (§3.3) |
| `files/snapshots/<stamp>.json` | yes | `FilesSnapshot` — a Files-tree snapshot manifest with blob ids (§3.4) |
| `files/blobs/<aa>/<blob-id>` | yes | the content of one file; `<aa>` is the id's first two hex characters |
| `index/<stamp>.json` | yes | `Index` — the rollup written last on every successful run (§3.5) |
| `store/<store-id>/store.json` | yes | the incremental store's `store.json` (§6) |
| `store/<store-id>/key` | yes | an encrypted store's key in hex; empty for a plain store (§6) |
| `store/<store-id>/packs/<aa>/<pack>` | yes | one pack of the store, byte for byte (§6) |

A target probe's scratch objects are not in any locker: they go under
`<prefix>/fog-locker/1/probe/probe/`, created and hidden in one pass.

`<stamp>` is `YYYYMMDD-HHMMSS` in UTC — the same stamp fog uses for the
backup directory on disk, so a local snapshot and its offsite copy share a
name. `<app>` is fog's name for the installation.

Rule for writers: **the objects a document refers to exist before the
document does.** Blobs before a Files manifest; the snapshot (or the packs)
before its row; everything before the index. A reader that finds a document can trust
every name in it.

## 3. Documents

All JSON, UTF-8, additive: readers ignore unknown keys, writers may add
keys without a version bump. Anything that changes how a reader must
behave bumps `fog-locker/1` to `/2` in the path and in `format`.

### 3.1 `locker.json`

```json
{
  "format": "fog-locker/1",
  "id": "0123456789abcdef0123456789abcdef",
  "name": "house",
  "created_at": "2026-08-27T01:00:00Z",
  "kdf": { "time": 4, "memory_kib": 262144, "threads": 1 },
  "salt": "<base64, 16 bytes>",
  "keys": [
    { "id": 1, "wrapped_by": "passphrase", "ciphertext": "<base64 nonce‖ct>" },
    { "id": 1, "wrapped_by": "master",     "ciphertext": "<vault ciphertext>" }
  ]
}
```

- Each `keys` entry wraps one **root key** (32 random bytes) under one
  wrapper. `id` numbers root keys from 1; new objects are sealed under the
  highest id, old objects name the id they were sealed under (§4), so a
  rotated locker keeps reading its history.
- `wrapped_by: passphrase`: KEK = `Argon2id(passphrase, salt, time,
  memory_kib, threads, 32)`; ciphertext = `nonce(24) ‖
  XChaCha20-Poly1305(KEK, nonce, root, aad = "fog-locker/1 key <id> locker
  <locker-id>")`. The AAD names the locker id, so a wrap lifted from another
  locker (opened by the same passphrase) cannot be spliced in under a
  matching key id — it fails to open. `locker.json` is unsealed, so a reader
  **bounds** `kdf` before deriving — an out-of-range `time`, `memory_kib`
  (capped near a home server's means, 512 MiB) or `threads` is refused,
  never attempted, so a tampered file cannot turn the derivation into an OOM
  or a hang; derivations are serialised and their result cached so a bucket
  writer cannot force many at once. The parameters feed the KEK, so any
  change also makes the wrap fail to open.
- `wrapped_by: master`: fog's vault (`AES-256-GCM` under
  `<datadir>/master.key`) over the string `"<locker-id>:<id>:<hex root>"` —
  the locker id binds the wrap to its locker, so one spliced from another
  locker (same vault) is refused. Only the fog that holds that master key
  can open it; a locker copied to another machine is rewrapped with the
  passphrase first.
- `"wrap_format": 2` marks a locker whose wraps are all locker-bound. A
  locker written before the binding (`"fog-locker/1 key <id>"` and
  `"<id>:<hex root>"`, no marker) still opens, and fog rewrites every wrap in
  the bound form and sets the marker on its first sync or verify after the
  upgrade — no operator action, as long as fog holds the keys and its stored
  passphrase opens the passphrase wraps. Once the marker is set, readers
  refuse the legacy forms for that locker, so an old-format wrap cannot be
  spliced back in. fog also records the upgrade in its own database, so
  stripping the marker from `locker.json` is refused and raises the
  locker-changed alert rather than re-opening the legacy door.
- Changing the passphrase rewrites `locker.json` with a new salt and new
  wraps. Nothing else changes.

Derived keys, HKDF-SHA256 with an empty salt and the `info` shown:

| Key | info | Used for |
|---|---|---|
| data key *n* | `fog-locker/1 data key` from root *n* | sealing objects (§4) |
| id key | `fog-locker/1 blob id` from root **1** | blob ids (§3.4): `HMAC-SHA256(id key, "sha256:<hex>")` |

The id key always comes from root 1 so that rotation never changes which
blob a file maps to. Root 1 is therefore never removed from the ring.

### 3.2 `apps/<app>/<stamp>.snapshot`

A plain (uncompressed) tar of the backup's snapshot directory as fog keeps
it on disk, entries at the top level, in name order:

- delegated (`fog-backup/1`) apps: `artifact.tar`, `manifest.json`;
- volume-mode apps: `meta.json`, `volume-<name>.tar.gz`…, `db-<name>.dump`….

Restoring a snapshot means unpacking this tar into
`<backup_dir>/<app>/<stamp>/` and handing the directory to the restore path
fog already has for that mode. No knowledge of the app's own contract is
needed to move the bytes.

### 3.3 `apps/<app>/<stamp>.json` — `AppRow`

```json
{
  "format": "fog-locker/1",
  "app": "hub", "identity": "hub", "stamp": "20260827-010314",
  "kind": "scheduled", "mode": "delegated",
  "created_at": "2026-08-27T01:03:14Z",
  "size_bytes": 574643,
  "snapshot_sha256": "<sha256 of the plaintext .snapshot tar>",
  "artifact_format": "fog-backup/1",
  "artifact_sha256": "<sha256 of artifact.tar>",
  "manifest": "<the artifact's manifest.json, verbatim>",
  "verified_at": "2026-08-27T01:44:44Z",
  "files": ["artifact.tar", "manifest.json"],
  "source_backup_id": 123
}
```

`snapshot_sha256` is what a reader checks after unpacking; `artifact_sha256`
is what fog verified against the app at download and re-checks on every
test restore. `mode` is `delegated` or `volumes`.

### 3.4 `files/snapshots/<stamp>.json` — `FilesSnapshot`

```json
{
  "format": "fog-locker/1",
  "stamp": "20260827-011500", "created_at": "2026-08-27T01:15:00Z",
  "root": "/srv/files",
  "files": [
    { "path": "Photos/2026/IMG_0001.jpg", "size": 3145728,
      "mtime": 1787836800000000000, "sha256": "…", "blob": "<64 hex>" }
  ],
  "excluded": ["Scratch"],
  "bytes": 3145728, "count": 1,
  "source_snapshot_id": 41
}
```

- `blob` is `HMAC-SHA256(id key, "sha256:" ‖ lowercase hex sha256 of the
  file content)` in hex. The object `files/blobs/<blob[0:2]>/<blob>` seals
  exactly that content, and a reader checks the content's SHA-256 against
  `sha256` after opening it. Two files with the same bytes share a blob; a
  blob referenced by any retained snapshot is kept. Naming from the
  digest rather than the bytes is what lets a writer decide what to
  upload from the snapshot manifest alone.
- `path` is slash-separated, relative to the Files root; `mtime` is Unix
  nanoseconds.

### 3.5 `index/<stamp>.json` — `Index`

```json
{
  "format": "fog-locker/1", "locker_id": "…", "fog": "house",
  "written_at": "2026-08-27T01:20:00Z",
  "kits":  [ { "name": "fog-recovery-kit-20260827-010000.enc", "bytes": 4194304, "stamp": "20260827-010000",
               "sha256": "<hex of the sealed kit>" } ],
  "latest_kit": { "name": "fog-recovery-kit-20260827-010000.enc", "bytes": 4194304, "stamp": "20260827-010000",
                  "sha256": "<hex>" },
  "apps":  { "hub": [ <AppRow>, … ], "feed": [ … ] },
  "files": [ { "stamp": "20260827-011500", "created_at": "…", "files": 18213, "bytes": 71283718123 } ]
}
```

`kits[].sha256` is the digest of a kit's sealed bytes, set for every kit a
fog recorded uploading (its own, or carried from the index before); a kit
only seen in the bucket listing has none. `latest_kit` is the newest kit the
writing fog uploaded itself. Both are sealed with the locker's keys, so a
bucket writer can neither forge nor alter them — this is what a recovery and
the verifier check the bucket's kits against (§5).

The index to read is the newest **valid** one (highest stamp that opens,
parses and names this locker). The index the reading fog recorded writing
last is a floor: always tried, whatever is planted ahead of it, and
choosing anything older is a fall-back; a newer valid one (a machine
restored from a kit holds the record from the night before the kit) is
preferred. When fog writes an index it hides only its own older
well-formed ones past the newest three, never anything newer than the one
it wrote and never an object it does not recognise: those are reported
(Inbox), so junk under `index/` cannot make fog delete its real indexes.
With no readable index at all, fog raises an Inbox issue, and a recovery
can read the locker's listing and its sealed rows instead — reading at
most 60 rows per app, 2,000 in all, 8 MiB each and 256 MiB together. When
the index fog itself recorded writing is the one missing, that is what
tampering looks like, and the recovery asks the operator before using the
listing. A reader skips a junk object planted
under `index/` and falls back rather than breaking, but bounds the cost: an
object too small to be sealed or larger than 32 MiB is passed over unread,
and at most six are opened. A reader that falls back past the recorded or
newest index says so — fog raises an Inbox issue, and the setup screen shows
it — since a deleted or spoiled index is what tampering looks like; fog
caches a failed or fallen-back read for 15 minutes rather than retrying it
on every poll. The index is a cache: a reader that finds none, or finds
objects newer than it, lists the prefixes instead.

Fog's verify pass also opens the kit a recovery would choose, exactly as
above, and compares it with its own local record of the newest kit it
uploaded (name and digest): a newer kit deleted or spoiled in the bucket,
which would roll a recovery back to older secrets, raises an Inbox alert,
as does a newest kit fog never uploaded.

`locker.json` has no Object Lock (it is rewritten on every passphrase change
and machine add), so fog's weekly verify re-reads it from the bucket and
re-derives its keys, raising an Inbox alert if it has gone missing or no
longer yields the keys fog opened it with — a bucket writer cannot hide or
re-key it unnoticed while verify coasts on keys cached in memory.

## 4. Sealed objects

Every sealed object is one byte stream:

```
header  = "FOGLKR1" ‖ 0x01 ‖ key_id (uint32 BE) ‖ nonce_prefix (19 bytes)
frames  = frame_0 ‖ frame_1 ‖ … ‖ frame_last
frame_i = XChaCha20-Poly1305(data key key_id,
                             nonce = nonce_prefix ‖ i (uint32 BE) ‖ last (0x00 | 0x01),
                             plaintext_i, aad)
aad     = "fog-locker/1" ‖ 0x00 ‖ <object name> ‖ 0x00 ‖ header
```

- `plaintext_i` is exactly 65536 bytes for every frame but the last, which
  holds the remainder (possibly zero bytes). Every frame carries a 16-byte
  tag, so a sealed object is `31 + n + 16·⌈n/65536⌉` bytes (one frame
  minimum).
- `<object name>` is the object's path inside the locker, relative to
  `<prefix>/fog-locker/1/<locker-id>/` — `apps/hub/20260827-010314.snapshot`
  — so a locker copied to another bucket or prefix still opens, and an
  object served under a different name does not.
- The last frame's flag, and the counter in every nonce, are what make
  truncation and reordering detectable: a stream that ends without a
  `last = 1` frame is incomplete; a frame that authenticates under the
  wrong counter does not exist.
- `nonce_prefix` is random per object. Re-sealing the same plaintext with
  the same prefix produces identical bytes, which is how a resumed
  multipart upload keeps the parts it already sent.

## 5. Reading a locker from nothing

1. `GET <prefix>/fog-locker/1/` with delimiter `/` → locker ids (or the
   operator knows theirs).
2. `GET …/locker.json`; derive the KEK from the passphrase; unwrap root
   keys; derive data and id keys.
3. `GET …/kits/` → choose a kit and decrypt it with the same passphrase
   (`fog-recovery-kit` format). That kit restores fogd's identity, vault
   and database, including the target that names this very locker. The
   choice, as fog and `fog-locker` make it:
   - a name dated more than 15 minutes after the bucket received the
     object is ignored (a decoy built to sort newest) — the bucket's clock,
     not the reader's, so a fresh machine with a wrong clock judges
     correctly;
   - kits the index records (§3.5) come first, newest first, and one whose
     bytes do not match the recorded `sha256` is refused; kits only seen in
     the listing follow, newest first;
   - every current-format kit (`FOGKIT2`) is tried before any legacy one
     (`FOGKIT1`), whatever their names, so an old legacy kit renamed to sort
     newest cannot hide the current kits;
   - an object of the wrong size, or that does not start like a kit, is
     passed over without a key derivation. Decryptions are not capped in
     total but paced (ten at once, then one every two seconds), and the
     oldest kit only seen in the listing is always tried within the first
     ten — decoys sort newest — so planted junk can slow a recovery but not
     stop it. Kits are downloaded two at a time at most, each given up once it
     stops arriving for 30 seconds or takes over 10 minutes, and opened one
     at a time in the whole process (shared with the locker's own key
     derivation): a burst of recovery requests queues instead of stacking
     gigabytes, and a slow bucket cannot hold the queue. Kits are read up to
     256 MiB (about 2.5 million Drive files offsite; fog raises an Inbox
     issue once its kit passes half of that).
   The kit seals a `kit-meta.json` naming its own stamp and origin, checked
   against the object's name, so a replayed or renamed kit does not restore
   stale state; a `FOGKIT2` kit without it is refused. A legacy kit carries
   no sealed date and cannot be checked this way: readers offer it only
   when no current kit opens, with a clear warning and the date found inside
   it — fog's setup screen requires the operator's explicit go-ahead, and
   `fog-locker restore-kit` prints the warning. A chosen kit that is not the
   newest (by name, or the index's `latest_kit`) is flagged prominently: it
   rolls the machine back. Fog's setup screen restores exactly the kit its
   preview showed, by name and digest, or nothing.
4. Open the index (§3.5) → everything else by name.
5. For an app: a row with `store_snapshot` is rebuilt from packs (§6);
   otherwise fetch and open `apps/<app>/<stamp>.snapshot`, check
   `snapshot_sha256` from the row, unpack into fog's backup directory,
   restore. For Files: open the manifest, fetch each blob, write each
   file.

`fog-locker` does steps 1–4, the fetches in 5, and a whole Files snapshot, from
any machine with Go installed:

```
fog-locker lockers       --endpoint … --region … --bucket … --prefix … --key-id …
fog-locker ls            … [--locker <id>]
fog-locker restore-kit   … --out ./kit.enc
fog-locker restore-backup … --app hub --stamp 20261104-030000 --out ./hub
fog-locker fetch         … --object apps/hub/20260827-010314.snapshot --out ./hub.tar
fog-locker restore-files … --stamp 20260827-011500 --out ./files
```

`--locker` may be left out when the prefix holds only one. The secret key
and the passphrase are prompted for on the terminal with echo off (or passed
as `--secret` / `--passphrase`); nothing is read from the environment.

## 6. Store-backed backups

Fog keeps its backups in an incremental store
([store-contract.md](store-contract.md)): content-addressed chunks in
packs, one document per snapshot. A backup the store holds is copied
offsite as the store's own pieces, not as a tar of its files:

- `store/<store-id>/store.json` and `store/<store-id>/key`, once per
  store: what a reader needs to open its packs. Both sealed like every
  other object here, so a plain store's secret and an encrypted store's
  key are only as readable as the rest of the locker — with the bucket
  and the passphrase. A store rewritten (encryption turned on or off) has
  a new id and a folder of its own.
- `store/<store-id>/packs/<aa>/<pack>`: each pack, sealed as a whole,
  plaintext byte for byte the local file. `<pack>` is the lowercase hex
  SHA-256 of that plaintext; a reader checks it after opening. A pack is
  uploaded once, however many backups use its chunks.
- the row, `apps/<app>/<stamp>.json`, an `AppRow` (§3.3) with no
  `.snapshot` object beside it and four more fields:

  ```json
  { "store": "<store id>", "store_snapshot": "<snapshot id>",
    "store_doc": "<base64 of the snapshot document as the store wrote it>",
    "packs": ["<pack>", "…"] }
  ```

  `packs` names every pack holding a chunk the snapshot uses (its entry
  list included).

**Reading one back**: fetch `store.json` and `key` into a scratch store
folder, fetch every pack the row names (checking each against its name),
write `store_doc` as `snapshots/<store_snapshot>.json`, and read the
snapshot out with the store's own rules — every chunk checked against its
id, every file against its digest, and a delegated artifact against the row's
`artifact_sha256`. `fog-locker restore-backup` does exactly this.

**Retention** works on rows as before. After it, a pack no stored row names
is hidden — except one uploaded after the newest row, which most likely
belongs to a backup whose upload has not finished. **Locks** are extended on
packs like any other object; every stored pack is one a retained row names.

**Repair**: when the store's nightly scrub finds a damaged chunk, fog fetches
the pack the chunk lives in back from the locker — the same bytes it
uploaded — and rewrites the chunk from it.

A reader that predates this section reads every backup copied before the
store and skips store-backed rows; nothing written before changes.
