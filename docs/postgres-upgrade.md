# Upgrading the shared Postgres

The design in one sentence: **the data directory decides which Postgres
major runs; fog ships a newer one and offers the move as one button, and
the move is dump → fresh cluster → restore → count every row → roll back
on any mismatch.**

## What "upgrade available" means

`internal/pg` keeps a table of majors fog has an image for and a target
major fog wants every install on. On every deploy fog reads
`<data>/postgres/data` and runs the image for whatever major is *there*.
A fog release that raises the target changes nothing on its own — the
Inbox shows **"fog's Postgres is vN; this fog runs vM"** and the
apps keep running on N. Nothing is broken at that point. What is off is
that the app images pin the client tools for M, and a client newer than
its server dumps fine but its restore trips over settings the older
server does not know (the `pg-dump-skew` warning, one per app).

The other direction is the real outage, which is why app images move to the
new client before the cluster does. `pg_dump` refuses a server newer than
itself, so a client older than the cluster fails every backup, and with
pre-update backups on, every update aborts with it (the same finding, filed
as critical).

Only the shared cluster moves. Immich's own Postgres sidecar is not on it.

## The button

Settings → Storage → Database → **Upgrade to Postgres M**. What it does,
in order, each step shown live on the card:

1. Checks the cluster is healthy, no app is mid-deploy, the target image
   pulls (or is already on the machine), the data volume has room for two
   copies plus the dump, and no `data.pgN` is left from an earlier attempt.
   Then, while every app still runs, checks every database can come across:
   the target image ships each extension it uses, and its schema — dumped
   with the target's tools — reads back exactly as the restore will read it
   (its schemas and extensions parsed statement by statement, so a schema
   an app named with a newline or a `;` in it is one name, not a broken
   line). A database that fails either check refuses the upgrade, naming
   it, with nothing stopped. Upgrading the others and leaving it behind is
   not an option: the old cluster is retired, so its app would be stranded.
2. Stops every app that uses the cluster (`compose stop`, so connections
   close). Remembers which were running. Waits for client sessions to
   drain, terminates any that linger, and then **refuses if they come
   back**: a stopped app cannot reconnect, so a session that returns is a
   live process fog did not stop, and its writes would be missing from
   the dump. The refusal names each session with its client address —
   find that container (or host) and stop it, then try again.
3. Takes inventory: every role, every database, every table's exact row
   count, every extension.
4. Dumps, with the *target* major's tools, over the fog network, into
   `<data>/postgres/upgrade/`: the roles alone (`pg_dumpall --roles-only`),
   and each database's contents on their own in custom format
   (`pg_dump -Fc`, one `db-<name>.dump` per database). It records each
   database's owner first. Re-takes the inventory and refuses to continue if
   anything changed during the dump.
   Then a **trial restore**: a throwaway Postgres M is started (no network,
   its password in a file, removed with its volumes afterwards — it holds a
   plaintext copy of every database), the roles are restored into it, and
   every database's dump is restored into it exactly as step 6 will (same
   code, same role, same extensions). A database that will not come back — a
   CHECK whose function reads a table loaded after it, a schema the new
   version refuses — refuses the upgrade, naming it; the apps restart on
   Postgres N and nothing has changed. The trial needs room for one copy
   of the data in Docker's own storage, checked up front.

   Every restore — the trial's and the real one — is stopped once it makes
   no progress for 20 minutes, and in any case after 6 hours: a dump is the
   app's code, and a function in it can be built to loop forever only while
   it is being restored. Then the restore's sessions are terminated; in the
   trial that refuses the upgrade, after the point of no return it rolls
   back. The dumps in `upgrade/` are removed once the upgrade finishes or
   rolls back (kept only if a rollback itself fails). The upgrade can also be cancelled from the card at
   any time: before the point of no return the apps simply come back on
   Postgres N, after it the upgrade rolls back.
5. **Point of no return:** `compose down`, then renames `data` to
   `data.pgN`. The old cluster is intact on disk; nothing is deleted.
6. Writes the new compose file (nested layout, `PGDATA` set explicitly —
   see below), starts a fresh Postgres M, and restores: the roles first, as
   the admin; then **each database's contents as that database's own
   non-superuser owner** (the same hardened restore a backup uses —
   `pg.RestoreDatabase`: connect as the owner, `--no-owner --no-privileges`,
   extensions pre-created by the admin from the inventory and their TOC
   entries skipped; never into a system schema such as `pg_catalog`). This is the whole reason the dump is split per database:
   an app controls its own database and can plant a CHECK constraint or
   trigger whose function `COPY … TO PROGRAM`s a shell command, which runs as
   whoever restores the data. Replaying it as the owner, never as the shared
   superuser, denies it that. A database owned by the superuser (none of
   fog's apps are) is restored under a dedicated non-superuser role instead,
   so untrusted dump SQL is never run as the superuser.
   If the fresh cluster fails to start, the tail of its log lands in the
   stage log before the rollback removes the container.
7. Takes inventory again and compares. Any difference — a missing role,
   a table short by one row, an extension that did not come across —
   **rolls back**: the failed cluster is moved to
   `data.pgM.failed-<stamp>`, `data.pgN` is renamed back to `data`,
   Postgres N starts again on the old layout, and the apps come back.
8. On a match: rebuilds planner statistics (`vacuumdb --analyze-in-stages`),
   starts the apps, runs an issues sweep so every finding the move resolved
   clears.

One consequence worth knowing before the move: fog's weekly backup
verification test-restores each artifact with the *cluster's* `pg_restore`.
Once the app images carry a newer `pg_dump` than the cluster, their
artifacts fail verification with `unsupported version (1.16) in file
header` — reported as exactly that, "dumped by pg_dump 18, whose archive
format fog's Postgres 16 cannot read" — until the cluster moves. The
artifacts are fine; verify again after the upgrade.

`data.pgN` stays until you reclaim it (Settings → Storage → Orphaned
data). Give it a week of the apps running on M first. A
`data.pgM.failed-<stamp>` from a rollback is reclaimed the same way.

Minutes, at home-server sizes. The apps are down for the whole of it.

There is no rollback button. Rollback is automatic on any failure during the
move; going back after a successful one would discard everything written
since, so that is the by-hand recipe below.

## Why dump and restore

Not `pg_upgrade`: at home-server sizes the simplicity is worth more than
`--link`'s speed, a fresh `initdb` carries no catalog cruft across, and the
result is checked by exact row counts rather than trusted. Every image fog
runs is a `-trixie` build, so the glibc under every collation stays the same
across majors and nothing needs a reindex.

The dump is deliberately split — roles once, then each database on its own —
rather than one `pg_dumpall` stream fed to `psql` as the superuser. That
single stream would run every database's object SQL with superuser rights,
and an app's own database is full of SQL the app wrote. Restoring each
database as its owner instead keeps an app's trigger or CHECK function at the
app's own privilege, so the move can never be the step that turns app SQL
into code running as the cluster superuser.

## The layout change

Postgres 18's official image (and pgvector's build of it) keeps its data at
`/var/lib/postgresql/18/docker` and declares `/var/lib/postgresql` as the
volume. Fog's compose file therefore:

- for a cluster in the **legacy** layout (`data/PG_VERSION`, every
  pre-upgrade install): mounts `./data:/var/lib/postgresql/data`, which is
  every pre-18 image's default `PGDATA` — the compose file is exactly what
  those installs already have, so the fog release that added layouts does
  not recreate a running cluster;
- for the **nested** layout (`data/<major>/docker/PG_VERSION`, every
  upgraded or fresh install): mounts `./data:/var/lib/postgresql` and sets
  `PGDATA=/var/lib/postgresql/<major>/docker` explicitly.

In the nested layout `data/` itself is `0755` (root-owned, its only entry
the `0700` postgres-owned PGDATA): the image's entrypoint re-runs its
directory setup after dropping to the `postgres` user, and through a
root-owned `0700` parent that user cannot even stat PGDATA — Postgres 18
dies in a second with `mkdir: cannot create directory
'/var/lib/postgresql': Permission denied`. Seen on a Proxmox LXC; Docker
Desktop hides it.

The reason the layout is decided from disk and never from the image: an 18
image pointed at a legacy mount with its default `PGDATA` would find it
empty, `initdb` a fresh cluster on the container layer, and every app would
migrate against nothing — it would look exactly like data loss while the
real data sat untouched in `data/`.

## By hand, without fog

For recovery without fogd, or on a fog too old to have the button. Same
steps, same guarantees only if you keep them.

```sh
cd /var/lib/fog/postgres                      # <data>/postgres
# 1. stop the apps that use the cluster (fog UI, or docker compose stop in each app dir)
# 2. dump with the target's tools; password from .env
docker run --rm --network fog --env-file <(sed 's/^POSTGRES_PASSWORD=/PGPASSWORD=/' .env) \
  pgvector/pgvector:pg18-trixie pg_dumpall -h fog-postgres -U postgres \
  | grep -v -E '^(CREATE|ALTER) ROLE postgres\b' > upgrade/pg16-to-pg18.sql
# 3. retire the old cluster — rename, never delete
docker compose down
mv data data.pg16
# 4. compose.yaml: image pgvector/pgvector:pg18-trixie,
#    volumes: ["./data:/var/lib/postgresql"],
#    environment: {PGDATA: /var/lib/postgresql/18/docker}
mkdir -m 755 data && docker compose up -d --wait     # 755: see "The layout change"
# 5. restore, strict
docker exec -i fog-postgres psql -v ON_ERROR_STOP=1 -q -U postgres -d postgres -f - < upgrade/pg16-to-pg18.sql
# 6. verify: compare databases, roles, and per-table counts against the old cluster
#    (docker compose -f <a copy pointing at data.pg16> up on another port, or trust the
#    inventory fog logged before you started). Then:
docker exec fog-postgres vacuumdb -U postgres --all --analyze-in-stages
# 7. start the apps
```

Rollback by hand: `docker compose down`, `mv data data.pg18.failed`,
`mv data.pg16 data`, restore the old compose file (image `pg16-trixie`,
mount `./data:/var/lib/postgresql/data`, no `PGDATA`),
`docker compose up -d --wait`, start the apps. Nothing was written to the
16 cluster after the dump, so it is exactly as the dump left it, and anything
written to 18 since is gone.

## If fog says the data directory is unrecognizable

`Inspect` refuses to start anything against a data directory it cannot
account for — two majors side by side (an upgrade interrupted between
rename and restore), a `PG_VERSION` for a major fog has no image for, or a
directory with files but no cluster. Deploys fail with that message until
it is resolved. Look in `<data>/postgres/`: the upgrade never deletes, so
the answer is one of the `data*` directories, and the rollback recipe
above is how to put it back.

## Drill

- [ ] Before pressing the button, check every database-backed app shows a
      recent `verified` backup under Settings → Storage → Backups.
- [ ] After: open each app; check the Inbox is clean of `pg-dump-skew`
      and `postgres-upgrade-available`.
- [ ] A week later: reclaim `data.pgN` under Settings → Storage → Orphaned
      data.
