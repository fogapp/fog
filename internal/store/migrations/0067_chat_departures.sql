-- The identity key (base64url) a participant held when they joined a room.
-- A device that later tries to let itself in under a different identity key
-- is someone else wearing the name: an operator's identity reset, say. ''
-- for rows from before this column; the hub records the key on first sight.
ALTER TABLE chat_participants ADD COLUMN identity_key TEXT NOT NULL DEFAULT '';

-- The room's seq when a blob was stored: a former participant may fetch the
-- attachments of the room as it was while they were in it, and nothing
-- stored after they left. 0 for blobs from before this column, which only
-- current participants may fetch.
ALTER TABLE chat_blobs ADD COLUMN room_seq INTEGER NOT NULL DEFAULT 0;

-- A ban used to leave the person in the room, muted. A ban is now a
-- departure: bans made before this, in rooms hubbed here, take effect at
-- the room's current seq (a replica's rows follow its hub's snapshot).
UPDATE chat_participants SET left_seq = (SELECT seq FROM chat_rooms r WHERE r.id = chat_participants.room_id)
    WHERE role_index = 1 AND left_seq IS NULL
    AND room_id IN (SELECT id FROM chat_rooms WHERE hub_instance = '');
