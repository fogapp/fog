# fog-backup/2 — the delegated backup contract

Fog schedules, stores, verifies, and restores backups. It never learns how to
dump an app. An app that opts into delegated backups (`backup.mode: delegated`
in its spec) implements this contract; fog drives it. The contract is a fixed
convention — same paths, same auth, same envelope for every app — so fog
carries no per-app knowledge beyond the spec flag.

Reference implementation: feed. Adopters: fit, hook, hub, glass, manor, office,
once, stream, newtab (files only, in Go).

> Fog's own **Drive** tree is not an app and does not use this contract:
> fog snapshots it directly (into the incremental store beside the apps';
> see [disaster-recovery.md](disaster-recovery.md)).

## Auth

Fog generates a per-app secret (`_fog_api_token` in the vault) and injects it
into the app's environment as `FOG_API_TOKEN`. Every request below carries
`Authorization: Bearer <token>`; the app compares in constant time and answers
`401` without a body on mismatch. The `/fog/*` namespace sits outside the
app's own session/user auth — it must work on a fresh instance with zero
users.

## Endpoints

All request and response bodies are JSON unless noted. Every non-2xx response
uses the house error envelope: `{"errors": ["..."]}`.

### `GET /fog/backup` — descriptor

Answers instantly. Fog uses it for preflight, dashboards, and post-restore
verification.

```json
{
  "contract": "fog-backup/1",
  "app": "feed",
  "version": { "git_sha": "53e7f3f…", "schema_version": "2026_08_10_120000" },
  "pg": { "server_major": 18, "extensions": { "vector": "0.8.6", "pg_trgm": "1.6" } },
  "components": [ { "name": "primary", "kind": "db" },
                  { "name": "storage", "kind": "files" } ],
  "external": [ { "name": "media", "kind": "cas", "root": "/data/media" } ],
  "fresh": false,
  "last_restore": { "restored_at": "…", "artifact_created_at": "…" },
  "estimated_bytes": 1073741824
}
```

`app` names the **software** ("hub", "feed"), not the fog instance: fog may
call an installation anything ("hub-lxc"), and it compares an artifact's
`app` against the descriptor's `app` — never against its own label — so an
artifact from any hub restores into any hub. `fresh` is the app's own
first-run predicate (no users exist). `last_restore`
is the receipt written by the most recent successful restore, or `null`.

### `POST /fog/backup` — produce an artifact

Starts the producer job and returns immediately:

- `202 {"job": "<id>"}` — producing.
- `409 {"job": "<id>"}` — a producer job is already running; fog adopts it
  and polls rather than stacking a second run.

The job dumps the primary database with the container's own `pg_dump -Fc
--no-owner --no-privileges`, archives file state, writes the manifest, and
assembles the artifact under its staging directory
(`<storage volume>/fog/backup/<job>/`). Staging is excluded from the
artifact itself.

### `GET /fog/backup/jobs/:id` — poll

```json
{ "status": "running", "progress": "dumping primary database" }
{ "status": "failed",  "error": "pg_dump exited 1: …" }
{ "status": "completed", "artifact": { "bytes": 48211934, "sha256": "9f2c…" } }
```

`status` is one of `running | completed | failed`. Unknown job id → `404`.

### `GET /fog/backup/jobs/:id/artifact` — download

Streams the finished artifact (`application/x-tar`) with an exact
`Content-Length`. Fog hashes while downloading and verifies **both** the
length and the SHA-256 against the job status before recording anything.
Any mismatch, truncation, non-2xx, or timeout means the backup run **failed**:
no backup row, no retention advance, a loud notification. A backup that
cannot be verified does not exist. The download is **size-capped** — a
claimed or actual length past the cap is refused rather than filling the
backup disk; the cap defaults generously and is raised per app with
`backup.maxBytes` (bytes) or fleet-wide with the `backup_max_artifact_bytes`
setting.

### `DELETE /fog/backup/jobs/:id` — clean up

Removes the staged artifact. Fog calls it after a verified download. The app
also sweeps staging older than 24 h on its own, so a crashed fog can't leak
disk forever.

### `POST /fog/restore` — stage a restore

Body: multipart upload of an artifact (field `artifact`), or
`{"staged": true}` when the file is already at the staging path.

This is the app's own door; fog doesn't call it. Fog's doors write
`pending.tar` and `pending.json` straight into the staging volume: before the
app first boots for the create-with-`restoreFrom` flow, or with the app stopped
for an in-place restore.

The app runs **preflight** (below) and, on success, arms restore-on-boot:
the artifact sits at `<staging volume>/fog/restore/pending.tar` beside an
arming marker `pending.json` (`{armed, confirm_identity, staged_by,
staged_at}`, and `forget_secrets` when the operator chose it — see
[Restoring without the original secrets](#restoring-without-the-original-secrets)). **The staging volume is, by convention, the app's first own
volume**, a managed directory or the app's own folder in Drive, never a host
path or a shared library — that's where fog writes when it stages directly
(restore-at-create, in-place restore), so app and platform agree on the path
without configuration. Fog never writes there by path: it opens the
volume from a trusted anchor (its data directory, or Drive's root) one
directory at a time without following a link, so a volume root — or a folder
above it — that an app has swapped for a symlink is refused, not followed
into another app's volume or onto the host. pending.tar and pending.json
replace whatever entry was there with a fresh file. Response:

- `200 {"preflight": {...}, "restart_required": true}` — armed. The app (or
  fog) restarts the containers; the entrypoint performs the restore before
  serving.
- `422 {"errors": [...]}` — preflight refused.
- `409 {"errors": [...]}` — instance is not fresh. Only fog may stage onto a
  non-fresh instance (it stops the app first); the app's own surface refuses.

## The artifact

A single **uncompressed outer tar, manifest first**, so it can be produced
and consumed as a forward-only stream. Inner payloads carry the compression.

```
<app>-<utc-stamp>.fogpack.tar
├── manifest.json
├── db/primary.dump          pg_dump -Fc (custom format, compressed)
└── files/<component>.tar.zst
```

### manifest.json

```json
{
  "format": "fog-backup/1",
  "app": "hub",
  "created_at": "2026-08-10T03:15:00Z",
  "produced_by": { "git_sha": "…", "schema_version": "…" },
  "pg": { "server_major": 18, "dump_tool": "pg_dump (PostgreSQL) 18.6",
          "extensions": { "pg_trgm": "1.6" } },
  "components": [
    { "name": "primary", "path": "db/primary.dump", "kind": "db",
      "bytes": 123, "sha256": "…" },
    { "name": "storage", "path": "files/storage.tar.zst", "kind": "files",
      "bytes": 456, "sha256": "…" }
  ],
  "external": [
    { "name": "media", "kind": "cas", "root": "/data/media",
      "assets": 184201, "bytes": 2913041824512,
      "reconcile": "MediaIntegritySweepJob" }
  ],
  "excluded": ["cache", "queue"],
  "identity": { "host": "feed.example.com", "webauthn_rp_id": null },
  "secrets_fingerprint": {
    "AR_ENCRYPTION_PRIMARY_KEY": "sha256:9f2c11ab04d2",
    "AR_ENCRYPTION_DETERMINISTIC_KEY": "sha256:aa10…",
    "AR_ENCRYPTION_KEY_DERIVATION_SALT": "sha256:07b3…",
    "SECRET_KEY_BASE": "sha256:c44e…"
  }
}
```

Rules:

- **Databases**: primary only. Solid Cache / Solid Queue state is never in
  the artifact — restoring stale queue state resurrects dead claimed jobs.
  `db:prepare` recreates them empty on restore. Apps whose queue/cache tables
  share the primary database (manor) include them in the dump but **truncate
  them during restore**.
- **No database**: an app whose state is all files (newtab) lists only
  `files` components and omits `db/primary.dump`; its descriptor's `pg` is
  `null`. Fog's scheduled verification then has no dump to test-restore and
  proves each component against its manifest digest instead.
- **Untrusted dump bytes**: `db/primary.dump` is produced by the app, which
  fog does not trust. Fog never restores it with more than the app's own
  privileges — not even its scheduled **verification**, which loads the dump
  into a scratch database as a dedicated non-superuser role (`--no-owner
  --no-privileges`, extensions pre-created by the admin and their TOC entries
  skipped, exactly as restore-on-boot does). A doctored dump therefore cannot
  `COPY … TO/FROM PROGRAM`, create untrusted-language functions, or reach
  another app's database. The same rule binds the legacy volume path.
  Schema and extension names are read from the dump statement by statement
  (a quoted name may hold a newline or a `;`), and an extension is never
  placed in a system schema (`pg_catalog`, `pg_toast`, `information_schema`,
  any `pg_*`). A restore goes into a fresh `fog_rst_*` database created
  closed to connections and opened only to its owner, and swaps in only on
  success; the database it replaces is renamed `fog_old_*` and closed to
  connections in the same transaction, then dropped. Any such leftover from
  a crash is swept the next time Postgres is provisioned. Every restore of an
  app's database is stopped once it makes no progress for 20 minutes (no
  more of the dump read, no new statement, no COPY row, no index block),
  and in any case after 6 hours — bounds the app cannot stretch; a dump
  whose functions hang the restore is stopped, its sessions terminated,
  and the live database left as it was. Restores never hold up other apps'
  deploys. Sparse tar entries are refused, and no entry may claim more
  bytes than the artifact that holds it.
- **secrets_fingerprint**: `sha256:` + first 12 hex chars of the SHA-256 of
  each restore-critical env value. Never the value. Restore preflight
  compares against the running environment and refuses on mismatch — the
  loud version of "wrong encryption keys."
- **identity**: values that bind data to a hostname (feed's `FEED_HOST`,
  manor's WebAuthn RP ID). Mismatch at preflight is a warning requiring
  explicit confirmation, never a silent pass.
- **external**: state deliberately outside the artifact (feed's media CAS).
  Declared with enough metadata to verify and reconcile after restore.

## Preflight (staging time, re-checked at restore time)

| Check | On mismatch |
|---|---|
| `format` is `fog-backup/1`; `app` matches | refuse |
| schema version same-or-older than running code | refuse (migrate forward after load; never restore a newer artifact) |
| component checksums match | refuse |
| required PG extensions available, same-or-newer | refuse |
| secrets fingerprints match running env | refuse (unless `forget_secrets`: warn, and clear — below) |
| identity values match | warn + explicit confirm |
| instance fresh | app UI/API door refuses when not; fog's door stops the app first |

Fog runs the subset it can answer alone — `format`, `app`, component
checksums, secrets fingerprints, identity — **before it stages anything**, on
both restore doors and on restore-at-create. Staging arms a marker the app
consumes on its next boot, so an artifact accepted blindly becomes an app that
boot-loops on a restore it can never finish; refusing early puts the answer on
the request the operator made. Schema version and extension availability stay
the app's alone: they depend on the code and database it is about to boot with,
which fog cannot see. The app re-checks everything regardless and has the
final word.

## Restore-on-boot

The entrypoint (and the release task — fog runs release once, alone, before
any replica serves) checks the staging path. If `pending.json` is armed:
take a Postgres advisory lock (only one process restores; siblings wait) →
re-run preflight → `pg_restore --clean --if-exists` the primary dump **as the
app's own database role** (objects come out correctly owned), restoring
from its `pg_restore --list` with the `EXTENSION` and `COMMENT - EXTENSION`
entries removed — fog creates the declared extensions as its admin role at
provisioning, so the app's role can neither drop nor recreate them → unpack file
components → app-specific reconciliation → `db:prepare` → write the restore
receipt → remove staging → continue boot. Failure leaves the marker in place
with the error recorded, and the app refuses to serve rather than booting
half-restored.

## Restoring without the original secrets

A server rebuilt without its recovery kit has lost the values its apps' data
was encrypted with, and nothing anywhere can produce them again. Refusing
then protects nothing — it only makes the backup useless — so the operator
may restore knowing it: fog's restore doors take `forgetSecrets`, fog's
preflight turns each mismatched or missing secret into a warning, and the
staged marker carries `"forget_secrets": true`.

An app that honours the marker:

- treats each secrets mismatch in its own preflight as a warning, and every
  other check exactly as before;
- after the primary dump is restored, and before it serves, **clears every
  value its secrets encrypted** — those values are ciphertext no key left can
  read, and reading one would fail. A nullable value becomes null; a
  required one is handled so the app keeps the row and can say what needs
  entering again (a credential the operator re-enters, an identity the app
  re-mints, an account the user re-links);
- records what it cleared in its activity trail and the restore receipt
  (`forgotten`: the attributes cleared, by model), so the operator knows what
  to re-enter.

The field is additive: an app that predates it ignores it, refuses the
mismatch as it always has, and leaves the marker armed — cancel the restore
from fog to start it again.

## Failure semantics (normative)

- The app never advances its own success state on a failed step; errors are
  returned in job status and logged to the app's activity/audit trail.
- Fog records a backup only after independent checksum verification.
- Fog's staleness alert fires on **absence of a verified recent backup**,
  not on failure events — the dead-man's switch is the backstop for every
  silent failure mode.

## fog-backup/2 — uncompressed payloads

Fog keeps every artifact in its incremental store
([store-contract.md](store-contract.md)), which cuts bytes into chunks by
content and stores each chunk once, compressed. A payload compressed as a
whole defeats that: one changed row or file reshuffles the whole compressed
stream, and nothing is shared with the night before. `fog-backup/2` is
`fog-backup/1` with exactly that changed:

| | fog-backup/1 | fog-backup/2 |
|---|---|---|
| `manifest.format`, descriptor `contract` | `fog-backup/1` | `fog-backup/2` |
| primary dump | `pg_dump -Fc` (compressed) | `pg_dump -Fc -Z0` — still custom format, `pg_restore` reads it unchanged |
| file components | `files/<component>.tar.zst` | `files/<component>.tar`, plain |

Everything else — endpoints, auth, staging, preflight, restore-on-boot,
`forget_secrets`, failure semantics — is identical. The artifact grows on
the app's staging disk while it is produced; the store compresses what it
keeps, so nothing grows at rest.

- **Producers** write `fog-backup/2` only.
- **Restorers** accept both: a component is read from the path its manifest
  entry names, unpacked by its extension (`.tar.zst` with zstd, `.tar` as
  is), so an app can restore any backup it has ever made.
- **Fog** accepts both formats at backup, preflight and restore.

## Versioning

`fog-backup/1` and `fog-backup/2` are frozen by this document. Additive
manifest fields are allowed (consumers ignore unknown keys). Anything that
changes restore behavior bumps the version; producers write only the newest
version, restorers accept all versions they know.
