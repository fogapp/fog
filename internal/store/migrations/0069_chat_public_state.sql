-- The hub's verified public state of each room's MLS group: the full public
-- ratchet tree, GroupContext and interim transcript hash (internal/chat/
-- mlsview PublicGroup), seeded from the creator's GroupInfo and advanced only
-- by commits whose signatures and tree the hub verified. '' for rooms from
-- before it, which seed it from the GroupInfo on file for their epoch.
ALTER TABLE chat_rooms ADD COLUMN public_state TEXT NOT NULL DEFAULT '';

-- A replica keeps its DM's fixed set too (so its people's clients hold the
-- room to it, and it holds if the room's hub moves here). A DM's set is
-- unique among rooms hubbed here only.
DROP INDEX IF EXISTS idx_chat_rooms_dm;
CREATE UNIQUE INDEX idx_chat_rooms_dm ON chat_rooms(dm_key) WHERE dm_key <> '' AND hub_instance = '';

-- The sender's role in the room when the hub sequenced the event: what
-- clients judge a moderation act by (a delete of someone else's message, a
-- pin), rather than the role the sender holds whenever a device catches up.
ALTER TABLE chat_events ADD COLUMN sender_role INTEGER NOT NULL DEFAULT 0;
