-- Chat across households (docs/chat-contract.md): peers advertise their
-- people in the heartbeat, alongside their apps.
ALTER TABLE fed_peers ADD COLUMN people TEXT NOT NULL DEFAULT '[]';
