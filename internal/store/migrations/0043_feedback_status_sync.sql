-- A report's status travels back to where it was written.
--
-- Until now a report went one way: written here, relayed to a peer, and
-- closed there without a word coming back. The person who took the time
-- to write it never learned it had been dealt with. Now the receiver's
-- status rides its heartbeats home, and the origin mirrors it onto the
-- row the person can see.
--
-- status_at is when the status last changed, on the clock of whoever
-- changed it; NULL until it does. On a relayed row (peer_id set) it is
-- what this instance reports back to the origin. On a local row it is
-- copied from the peer's notice when the peer set the status, so both
-- sides show the same moment.
--
-- status_by_peer_id names the peer whose word set the current status;
-- NULL means it was set here. peer_status_at is the status_at the peer
-- reported in the last notice applied here: a notice with the same stamp
-- has been seen and is not applied twice, so a person reopening a report
-- the peer closed is not overruled by the same closure arriving again.
ALTER TABLE feedback ADD COLUMN status_at TEXT;
ALTER TABLE feedback ADD COLUMN status_by_peer_id INTEGER REFERENCES fed_peers(id) ON DELETE SET NULL;
ALTER TABLE feedback ADD COLUMN peer_status_at TEXT;
