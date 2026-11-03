-- NAS shares fogd mounts on the host (NFS or SMB/CIFS). Mounts are not
-- written to /etc/fstab — fogd re-mounts enabled shares at startup, so the
-- database stays the single source of desired state. CIFS passwords live in
-- the vault (system secret nas_<name>_password), never here.
CREATE TABLE nas_shares (
    id          INTEGER PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,      -- slug; default mount dir under /mnt/nas/
    proto       TEXT NOT NULL,             -- 'nfs' | 'cifs'
    server      TEXT NOT NULL,             -- host or IP
    remote      TEXT NOT NULL,             -- NFS export path or SMB share name
    mount_point TEXT NOT NULL UNIQUE,
    options     TEXT NOT NULL DEFAULT '',  -- extra mount -o options
    username    TEXT NOT NULL DEFAULT '',  -- cifs only
    enabled     INTEGER NOT NULL DEFAULT 1,
    created_at  TEXT NOT NULL
);
