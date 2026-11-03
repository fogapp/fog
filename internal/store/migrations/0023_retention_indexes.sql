-- The daily retention sweeps filter on columns only the read paths ever
-- indexed. PruneErrorEvents selects on received_at, so every 5000-row batch
-- re-scans the whole table; PruneStaleErrorGroups and the open-group
-- listings filter on status, which the triage index (app_id-leading) can't
-- serve. A sweep should cost what it deletes, not the size of what it keeps.
CREATE INDEX idx_error_events_received ON error_events (received_at);
CREATE INDEX idx_error_groups_status ON error_groups (status);
