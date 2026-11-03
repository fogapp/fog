-- A replica remembers which state of its room it last mirrored.
--
-- A follower re-pulls every replica room from its hub on an idle cadence,
-- and used to rewrite the room's participants and tell every local member
-- (and the bot) to refetch their room list each time, whether anything had
-- changed or not. The hub now tags each room snapshot with an ETag derived
-- from its content; hub_etag is the tag of the snapshot this replica last
-- applied, sent back as If-None-Match so an unchanged room costs a 304.
-- Empty until a hub that tags snapshots has been heard from.
ALTER TABLE chat_rooms ADD COLUMN hub_etag TEXT NOT NULL DEFAULT '';
