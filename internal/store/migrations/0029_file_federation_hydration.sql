-- A replica the operator keeps offline: hydrated on purpose, refetched on
-- change rather than dehydrated, in a previews subscription.
ALTER TABLE file_fed_replicas ADD COLUMN pinned INTEGER NOT NULL DEFAULT 0;
