-- A replica remembers how far its hub's log reached when it last mirrored.
--
-- An unchanged room's idle re-sync costs a 304 on its snapshot (0046), but
-- the follower then still asked the hub for events after its own seq. The
-- snapshot names the hub's seq, so hub_etag_seq records it beside hub_etag:
-- a 304 means the hub is still at that seq, and a replica already there has
-- nothing to fetch. 0 until a snapshot has been applied since this column
-- arrived, and read as "unknown": the events are fetched as before.
ALTER TABLE chat_rooms ADD COLUMN hub_etag_seq INTEGER NOT NULL DEFAULT 0;
