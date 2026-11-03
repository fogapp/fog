-- Stable per-app host port (20000+), published in the compose file so apps
-- are reachable at http://<server>:<port> without the proxy/domain layer.
ALTER TABLE apps ADD COLUMN host_port INTEGER NOT NULL DEFAULT 0;
