-- An origin-side cap on how many bytes a contribute-mode share will hold, so
-- a peer granted contribute cannot fill the origin's disk. 0 = unlimited.
-- The receiving side already has a per-subscription quota; this is its mirror
-- on the side that actually stores the bytes.
ALTER TABLE file_fed_shares ADD COLUMN contribute_quota_bytes INTEGER NOT NULL DEFAULT 0;
