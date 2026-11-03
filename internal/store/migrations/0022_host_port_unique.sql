-- Port allocation reads the taken set and then writes its pick; nothing in
-- the schema stopped two concurrent allocations from landing on the same
-- port. The index turns that race into a constraint violation the allocator
-- can retry, instead of two apps silently sharing a port. 0 is the "no port
-- assigned" sentinel, carried by every unrouted app, so it stays out.
--
-- If the race already happened, the later claimant loses its port (back to
-- 0, reallocated on its next deploy) so the index can build; the two were
-- already colliding on the host, so neither assignment was sound to keep.
UPDATE apps SET host_port = 0
WHERE host_port != 0
  AND id NOT IN (SELECT MIN(id) FROM apps WHERE host_port != 0 GROUP BY host_port);

CREATE UNIQUE INDEX idx_apps_host_port ON apps(host_port) WHERE host_port != 0;
