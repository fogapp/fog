-- Contribute mode. On the receiving side a replica row can be our own
-- contribution (origin = this instance): when it was pushed, whether the
-- origin rejected it, or a third party's file we only know as a stub. On
-- the origin side a contribution withdrawn on unpair keeps its row as a
-- stub: the listing stays, the bytes left with the relationship.
ALTER TABLE file_fed_replicas ADD COLUMN pushed_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE file_fed_replicas ADD COLUMN rejected INTEGER NOT NULL DEFAULT 0;
ALTER TABLE file_fed_replicas ADD COLUMN stub INTEGER NOT NULL DEFAULT 0;
ALTER TABLE file_fed_replicas ADD COLUMN origin_share INTEGER NOT NULL DEFAULT 0; -- the share the author contributed through, at the origin
ALTER TABLE file_fed_origins ADD COLUMN withdrawn INTEGER NOT NULL DEFAULT 0;
