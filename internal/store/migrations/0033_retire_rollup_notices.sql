-- Five notice kinds restated a row the Inbox already showed. Three were
-- counts of another tab's rows ('issues' rolled up the Issues badge,
-- 'app-errors' and 'peer-errors' the Errors badge — the rollup's own body
-- said "review them in the Inbox", read from inside the Inbox); two had an
-- exact twin among the scanner's checks, filed by a subsystem that didn't
-- know the check existed. A count of Inbox rows is a badge, never a row,
-- and a condition a sweep can re-derive belongs to an issue, which
-- auto-resolves, not a notice, which waits for a person.
--
-- Their producers are gone. Retract the ones already sitting in an
-- upgrading install, so nobody is left dismissing notices by hand that
-- nothing will ever file again.
UPDATE notices
SET dismissed_at = strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
WHERE dismissed_at IS NULL
  AND kind IN ('issues', 'app-errors', 'peer-errors',
               'files-fed-sync-failing', 'files-fed-offer-changed');
