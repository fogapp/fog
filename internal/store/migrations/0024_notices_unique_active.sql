-- CreateNotice's one-active-notice-per-(kind, app) guarantee was a
-- check-then-insert with nothing beneath it: two producers racing could each
-- miss the other's row and stack duplicate active notices. Put the invariant
-- in the schema — a partial unique index over the active rows — so
-- CreateNotice becomes a single atomic upsert. Dedupe first: of each
-- duplicated (kind, app) group keep the newest active row (a repeat
-- refreshes created_at in place, so order by it, id as tiebreak).
DELETE FROM notices AS n
WHERE n.dismissed_at IS NULL
  AND n.id <> (
    SELECT id FROM notices
    WHERE dismissed_at IS NULL AND kind = n.kind AND app = n.app
    ORDER BY created_at DESC, id DESC LIMIT 1
  );
CREATE UNIQUE INDEX idx_notices_active_kind_app ON notices (kind, app) WHERE dismissed_at IS NULL;
