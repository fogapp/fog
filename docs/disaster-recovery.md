# Disaster recovery

The design in one sentence: **every app's state is one verified artifact,
and fog's own state is one encrypted kit — recovery is those files plus a
fresh install.** Contract details live in [backup-contract.md](backup-contract.md).

## What exists, automatically

With a nightly backup hour set (Settings → Storage → Backups):

- **Per-app artifacts** — apps on `backup.mode: delegated` (most of the
  catalog's own apps, manor, hub and feed among them) produce a single `artifact.tar` (database dump + file state + manifest)
  that fog checksum-verifies at download. Legacy apps still get volume
  tars + pg_dumps.
- **Verification** — each newest artifact is re-hashed and **test-restored
  into a scratch database** on a rolling weekly schedule; only then does it
  carry a `verified` badge. A backup that never restored is not trusted, and
  the staleness alarm keys on absence of recent success (notice + ntfy),
  not on failure events.
- **The recovery kit** — with a kit passphrase set (Settings → Storage →
  Recovery kit), fog writes
  `fog-recovery/fog-recovery-kit-<stamp>.enc` beside the backups daily:
  the vault master key + a consistent database snapshot, sealed with
  Argon2id + AES-256-GCM (kits an older fog wrote under scrypt still restore).
  The kit carries its own stamp and origin inside the seal, checked on a
  cloud recovery so an old kit copied to a newer-dated name in a bucket
  cannot roll the restore back. A kit written by an older fog has no sealed
  date; recovering from one in the cloud shows a warning with the date found
  inside it and asks for an explicit go-ahead. The passphrase itself is held vault-encrypted,
  never in the clear in fog's database. Without this file, losing the machine
  bricks every app's encrypted credentials even with perfect app artifacts.
  **Keep the passphrase somewhere that survives the machine** — a strong one
  (at least 16 characters); it is the single key to everything fog encrypts.
- **Retention** — keep-newest-N per app (default 7), pre-update snapshots on
  their own clock (the newest 3, none older than 21 days), and the last
  remaining backup of an app is never
  deleted. Keep the backup directory on a NAS mount so all of it leaves the
  box: Settings → Storage → Drive → Apps, Content and Backups moves it there
  (or adopts backups already there), records and all.
- **Files** — fog's own file tree (the Drive page) is snapshotted nightly
  into `files/<stamp>/` beside the app backups, fifteen minutes after the
  backup hour. Each snapshot is a complete directory: unchanged files are
  hardlinks to the previous night, so seven nights cost one copy plus what
  changed. `files/latest` points at the newest; `.fog-snapshot.json` in
  each lists every file with its digest, and a random sample is re-hashed
  weekly. Restore anything from Drive's clock menu — or with `cp -a`, since
  it is just a tree. On a target without hardlinks (SMB), fog keeps a
  single rolling `files/mirror/` instead and says so in the Inbox. Anything
  marked *Exclude from backups* in Drive is left out; the same retention
  count applies. Copies of peers' shares (the `Peers` folder in Drive) are
  left out too unless *Back up my copy* is on for that subscription on the
  Connect page — they are caches of what the peer holds, not fog's own
  state, and the recovery kit never includes them.
- **Offsite** — with a locker set up (Settings → Storage → Offsite), sealed
  copies of the backups, the kit, and the Files snapshots also live in an
  object-storage bucket. [locker-contract.md](locker-contract.md) is the
  format, readable without a fog.

Feed's media archive is deliberately outside the artifact (multi-TB host
path). Its artifact manifest declares the archive; back the tree up with
restic (feed:docs/operations.md), and after a restore the nightly media
integrity sweep re-fetches anything the tree is missing.

## Restore one app

- **Same fog, bad data**: app → Drive → Backups → Restore. Fog stops the app,
  stages the artifact, and the app's own boot guard restores it (as its own
  DB role — ownership comes out right).
- **Fresh instance, same or new fog**: install the app (catalog), then either
  upload the artifact on the app's own first-run screen, or create it with
  `restoreFrom` via the API:

  ```
  POST /api/system/backup-uploads      (multipart: artifact)  → {path}
  POST /api/apps  {"name": …, "spec": …, "secrets": …,
                   "restoreFrom": {"path": "…", "confirmIdentity": true}}
  ```

  The app's preflight has the final word: wrong app, newer schema, corrupt
  components, or mismatched encryption keys refuse; a changed host identity
  (feed's `FEED_HOST`, manor's WebAuthn RP ID) demands explicit confirmation
  because federation follows / passkeys break silently otherwise.

Secrets note: artifacts carry key **fingerprints**, never values. On the
same fog (or one restored from its kit) the vault injects the original
secrets automatically. **Artifact + recovery kit is complete; an artifact
alone is not.**

## Full-server recovery

**With the Fog USB stick (recommended):** boot the new machine from the
installer stick and choose **"Restore from a backup"** — the kit is
decrypt-verified before anything is erased, and the wizard can also take
the old admin password to restore console/SSH access. The wizard takes the
kit as a file, so if a locker holds the only copy, fetch it first with
`fog-locker restore-kit`. See [installer.md](installer.md). Then continue from
step 3 below.

**On an existing Linux server:**

1. Install fog on the new machine (`install.sh`).
2. On the setup screen: **"Recovering this server? Restore a recovery kit"**
   — upload `fog-recovery-kit-*.enc` + passphrase. fogd swaps in the old
   master key and database and restarts with the old server's identity,
   apps, secrets, and settings. If the kit is only in a locker, **The kit is
   in an offsite locker? Restore from the cloud** takes the bucket, its key,
   and the passphrase, shows what the locker holds and which kit it would
   restore, and restores exactly that kit (by name and digest) the same way.
   It prefers the kits the locker's sealed index records fog uploading,
   ignores kit names dated in the future, tries every current-format kit
   before an older fog's legacy one, and is not stopped by junk planted in
   the bucket. If the kit it would restore is not the newest — a newer one
   is missing or will not open — the screen says so in a warning box: that
   restore brings the machine back as it was then. Likewise if the
   locker's newest index could not be read.
3. The kit carries the old `backup_dir`, so with the NAS mounted at the same
   path fog sees the existing artifacts at once. If it mounts elsewhere now,
   adopt it: Settings → Storage → Drive → Backups → Change… → **Adopt that
   folder** re-points every recorded backup found there.
4. Redeploy each app (the specs survived in the kit), restoring delegated
   apps from their newest artifact. With a locker and no local backups,
   **Recover everything…** on Settings → Storage → Offsite restores every
   app and Drive from it instead.
5. Files: open Drive, pick the newest snapshot from the clock menu, and
   **Restore everything** — or copy `files/latest/` from the backup
   directory into the files root by hand (`cp -a`; the root is in
   Settings → Storage → Drive). The index rebuilds itself. Copies of
   peers' shares are not in a snapshot unless they were opted in: the
   subscriptions themselves are in the kit, so once the Connect page
   shows the peers again each copy refills on its next sync (*Sync now*
   on the peer's card hurries it). Files you contributed to a peer's drop
   box are in the snapshot like any other file of yours.
6. Feed only: remount the media host path (restic-restore it first if the
   tree was lost); the integrity sweep reconciles the rest.

## Rebuilding an app from its manifest

Settings → Recovery → **Export all app manifests** writes every app's spec
(no secrets) to `fog-apps.yaml`; an app page's export writes one,
`<app>.fog.yaml`. To bring one back — on this server or another — use
Add app → Image or compose → **Import a Fog manifest**.

A manifest is a whole spec, and whoever wrote it decided what it mounts.
So an import never deploys: the app is created **held**, and its page opens
on a review of everything it reaches beyond its own data — every host
folder (marked when it is dangerous, and whether the manifest acknowledges
it), any other app's own Drive data folder (`Apps/<Name>/…`, where its
credentials often live) and any volume that follows another app's, the
apps it can reach on the network (its `uses` and the apps an env URL
names), published host port, web address and whether it is gated, network,
what it reads from fog (`uses`), what it connects to, the folders it shares,
the secrets it expects, a Postgres database, household sign-in, release
commands and executable files. **I've reviewed it — deploy** releases the
hold for the revision you read and deploys it (a git app goes on to its
deploy-key setup instead). Until then every deploy path refuses the app —
the Deploy button, an AI assistant's `fog_app_deploy`, a webhook, wiring —
and a confirmation of a revision that is no longer the latest is refused,
so an edit made after you read it cannot ride along. Apps that use the
imported one are wired to it only after the confirmation.

**Host paths.** fog's own storage binds freely: the app's own Drive folder
(`Apps/<App>`), a library or any other ordinary Drive folder, a folder an
app it uses shares with it, a NAS share set up under Settings › Storage,
the app's own data under fog's data directory. Drive's root, `Apps`
itself, another app's `Apps/<Other>` and `Peers` are other apps' (or other
households') data, credentials included: by `files:` or by a `host:` path
that lands in Drive alike, they bind only when acknowledged, spelled as
the volume spells them (`acknowledgeHostAccess: [Apps/Vault]`), and the
review shows them as it shows a dangerous host path. Any other host path — wherever the spec comes from (the form,
the store's install pane, an import, a compose file) — binds only when the
spec names it in `acknowledgeHostAccess`, as the volume spells it; the
volume form and the install pane each have a checkbox for it. A list of
dangerous paths could never be complete (`/run/systemd/private` is root on
the host through D-Bus), so this is the other way round. Drive's whole root
needs acknowledging too: it is every app's data at once.

```yaml
volumes:
  - { name: archive, mount: /archive, host: /srv/archive }
acknowledgeHostAccess: [/srv/archive]
```

A few paths are never bound, acknowledged or not: `/`, `/proc`, `/sys`,
`/dev`, a container runtime's socket or state (Docker, containerd, Podman,
CRI-O), and fog's own data directory beyond the app's own corner of it.
Paths are compared cleaned, and at deploy also with symlinks resolved. An
entry no volume binds is refused, and fog never `chown`s a system folder
(`owner:` on `/etc`, `/var/lib` and their like is refused). `/etc/localtime`
and `/etc/timezone` mounted read-only need nothing.

**Approvals are yours, not the spec's.** An acknowledgement in a spec is
what the app asks for; it is approved when you save it in fog's UI or API,
or confirm an import's review having read it — and taken back when you save
the spec without it. Every deploy, of any revision, checks what that
revision binds against your approvals as they stand: a revision written by
an import, a repo, an integration or an assistant can ask, never grant.
After an import's review is confirmed, its earlier revisions don't deploy
until you confirm one of them again. An AI assistant's `fog_app_deploy`
deploys an older revision only when it is the latest's equal in everything
that matters — image or repo, commands, sidecars, ports, routes, mounts and
follows, uses and reach, environment, acknowledgements.

**Every start renders afresh.** Starting or restarting an app — from the
UI, an assistant, the health monitor, or fog itself at boot — writes its
compose file again from its current revision, under the rules and your
approvals as they stand, and recreates what changed: the exact images it
last ran — its own and each sidecar's, which fog keeps named as
`fog-pinned/<app>` — nothing pulled or built, wherever their tags have
moved since. Only a deploy moves an app to a new image; a failed update,
or another app's update of a sidecar image both use, changes nothing. So an upgrade's protections reach every running
app at its first boot, and a withdrawn approval holds from the next start.
An app the rules refuse — a bind no longer approved, a link where its
folder was — has its containers removed and is filed as an issue
(`app-not-reapplied`). An app whose storage isn't there yet — a NAS share
still mounting after a power cut — isn't refused: it waits, stopped, with
an informational issue (`app-waiting-for-storage`), and fog starts it the
moment the share mounts, however it was mounted; storage fog doesn't mount
(a USB disk, an fstab share) is looked for again every minute, then less
often. A start that keeps failing at boot for another reason (a database
not up yet) is stopped, filed, and tried again the same way. A refused app's compose file is emptied, and a restore brings the
app up the way a start does, never from the file on disk. Before anything
else at boot, fog takes the restart policy from every app container it has
no record of bringing up under today's rules, so neither Docker's
restart nor a reboot runs one on its old binds before fog has rendered it
again. A secret the running revision lists but fog no longer holds (a
disconnect, an edit) is started empty and filed (`app-secret-missing`);
the app's next deploy settles it. `/etc/localtime`, `/etc/timezone` and
`/usr/share/zoneinfo` bind read-only with no acknowledgement.

Deleting an app that shared one of its own folders (`Apps/<Name>/…`) with
another leaves that folder in the other app's spec, asked for but not
approved: it stops at its next start until you approve it, and a notice
says so — an app installed later under the deleted name would own it.

**Host ports** fog holds are never an app's, acknowledged or not: 80 and
443 (the proxy), fogd's own port, the direct ports 20000–20999 — the one
list is `internal/fogports`, which fog's own compose files publish from.
Docker starts apps before fog at boot, so an app published on one would
take it first and answer in fog's place. An app saved with one before
this rule is filed as an issue and doesn't deploy or start, and if it is
running fog stops it, says why, and brings its own service up on the
port. App names starting `fog-` are fog's own.

**Upgrading** to this rule acknowledges, once, the host folders each
existing app already binds (and approves the apps it already reaches), as
a revision of its own: what you chose before keeps working.

**Deleting an app** takes it out of every other app: the apps that used,
reached or followed it forget it, and the apps connected to it are
disconnected. An app that later takes the name inherits none of it. Its
data, when you keep it — the data under fog's data directory, its Drive
folder, its Postgres role and databases — would be the next app's of that
name: an import's review lists it, and confirming needs your yes to let
the import take it over.

A compose import never acknowledges on your behalf, and the app it makes is
**held for review** like an imported manifest: it deploys only after you
confirm it. It drops `privileged`, `cap_add`, `devices`, `network_mode`,
`pid`, `ipc`, `security_opt` and the like with a warning saying what they
would have granted — fog's spec has none of them. The review lists every
host path, port, variable name, where the settings point (URLs, any
password masked) and every app it asks to reach.

## Quarterly drill

A backup you haven't restored is a hope, not a backup. Once a quarter:

- [ ] Pick one app; restore its newest artifact into a scratch instance
      (`restoreFrom` at create) and log in.
- [ ] Decrypt the newest recovery kit somewhere off-box
      (proves the passphrase is still known and the file intact).
- [ ] Check every delegated app shows a recent `verified` badge under
      Settings → Storage → Backups.
- [ ] Check the backup dir is actually on the NAS and growing.
