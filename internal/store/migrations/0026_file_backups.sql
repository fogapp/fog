-- Files is backed up as hardlink snapshots of the
-- whole root. Two things about that live in fog's own database rather than
-- in the files index (which is a rebuildable cache) or on the backup
-- target (which may be away):
--
-- file_backup_excludes: paths the operator took out of the nightly
-- snapshot, one row per file or folder, root-relative. A folder covers its
-- subtree. Keyed by path on purpose — the tree is the truth and there is
-- no other stable identity for an entry; fog rewrites the key when it
-- renames or moves the entry itself, and a rename by another tool drops
-- the exclusion (visibly: the badge goes).
--
-- file_snapshots: one row per snapshot written, so the schedule, the
-- retention and the staleness alarm are computed from records, not from
-- reading the backup directory (which is exactly what is unreadable when
-- the NAS is away). path is the snapshot directory; mode is "links" for a
-- hardlink snapshot or "mirror" for a target without hardlinks.
CREATE TABLE file_backup_excludes (
    path       TEXT PRIMARY KEY,
    created_at TEXT NOT NULL
);

CREATE TABLE file_snapshots (
    id          INTEGER PRIMARY KEY,
    path        TEXT NOT NULL,
    created_at  TEXT NOT NULL,
    mode        TEXT NOT NULL,
    files       INTEGER NOT NULL,
    bytes       INTEGER NOT NULL,
    new_bytes   INTEGER NOT NULL,
    warnings    INTEGER NOT NULL DEFAULT 0,
    verified_at TEXT NOT NULL DEFAULT ''
);
CREATE INDEX file_snapshots_created ON file_snapshots(created_at);
