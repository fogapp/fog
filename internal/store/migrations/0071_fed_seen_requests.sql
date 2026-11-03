-- The fed plane's replay record. A signed request is good for five minutes
-- either side of its timestamp; inside that window a captured one would
-- verify again. Every request signature a fog accepts is written here until
-- its window has passed, and a signature already here is refused. Fog signs
-- each request with a unique nanosecond timestamp, so a genuine request is
-- never the same bytes twice. Kept in the database, not memory, so a restart
-- doesn't re-open the window.
CREATE TABLE fed_seen_requests (
    digest     TEXT PRIMARY KEY,   -- sha256hex(instance "\n" signature)
    expires_at TEXT NOT NULL       -- RFC 3339 UTC: past it, the timestamp no longer verifies
);
CREATE INDEX idx_fed_seen_requests_expiry ON fed_seen_requests (expires_at);
