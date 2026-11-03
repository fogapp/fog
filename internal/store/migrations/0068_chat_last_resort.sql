-- A device's last-resort KeyPackage (the RFC 9420 pattern): served, never
-- consumed, when the device has no one-use package left or the asker is past
-- its budget, so draining a device's packages can't keep it out of rooms.
-- The device keeps its private keys for as long as it holds this row.
CREATE TABLE chat_last_resort_kp (
    device_id    INTEGER NOT NULL REFERENCES people_devices(id) ON DELETE CASCADE,
    ciphersuite  INTEGER NOT NULL,
    key_package  BLOB NOT NULL,
    published_at TEXT NOT NULL,
    PRIMARY KEY (device_id, ciphersuite)
);
