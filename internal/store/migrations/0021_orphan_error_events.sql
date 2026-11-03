-- 0013 originally dropped peer-owned error groups during the federation
-- rebuild without dropping their stored events: foreign keys were off, so
-- ON DELETE CASCADE never fired. An instance that upgraded through that
-- version with peer groups on disk carries orphaned error_events rows —
-- unreachable (every read joins through the group) and uncounted. Sweep
-- them. Instances that haven't run 0013 yet get its fixed version and
-- this finds nothing.
DELETE FROM error_events
WHERE NOT EXISTS (SELECT 1 FROM error_groups g WHERE g.id = error_events.group_id);
