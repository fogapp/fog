-- When a paired machine's service first polled fog: pairing runs from the
-- owner's terminal, the service after it, and only the service's poll says
-- the machine really reaches fog. '' until it has (the never-connected
-- issue reads it).
ALTER TABLE compute_nodes ADD COLUMN connected_at TEXT NOT NULL DEFAULT '';
