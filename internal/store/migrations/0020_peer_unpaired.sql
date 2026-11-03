-- A peer that unpairs us tells us so, but only by refusing: their fog stops
-- recognising our instance and answers our signed heartbeats with
-- "unknown peer". Nothing is pushed — pushing would mean dialling an
-- instance that has just cut us off, with a key it no longer honours.
--
-- So the receiving side has to remember the refusals. reject_streak counts
-- consecutive "we don't know you" answers, and is reset by any exchange
-- that works in either direction. unpaired_at is stamped once the streak
-- passes the threshold, and is what the UI reads: a peer relationship the
-- other operator has ended.
--
-- A stamp is a claim about them, never an instruction to us. It never
-- deletes the row and never touches capabilities: a peer cannot make this
-- fog forget it — or drop the error groups it relayed — by answering 403.
-- Only the operator removes a peer.
ALTER TABLE fed_peers ADD COLUMN reject_streak INTEGER NOT NULL DEFAULT 0;
ALTER TABLE fed_peers ADD COLUMN unpaired_at   TEXT;
