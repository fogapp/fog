-- Transfer stats on the receiving side: what a subscription has pulled,
-- and when it last did — the mirror of bytes_served on a share.
ALTER TABLE file_fed_subscriptions ADD COLUMN bytes_fetched INTEGER NOT NULL DEFAULT 0;
ALTER TABLE file_fed_subscriptions ADD COLUMN last_fetched_at TEXT NOT NULL DEFAULT '';
