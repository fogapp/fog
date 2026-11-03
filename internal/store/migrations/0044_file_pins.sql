-- Drive pins: the files and folders a person keeps one click away, listed
-- under Drive in their sidebar.
--
-- Pins are per person — the sidebar is personal, the Drive is not. A pin
-- names a path, so a rename or move inside fog carries it along (the files
-- manager's rename hook), and trashing the entry leaves it in place: a
-- restore puts the entry back at the same path, and the pin with it. Until
-- then the sidebar shows the pin as missing.
--
-- kind is what the entry was when pinned (file or dir), so the sidebar can
-- link a missing pin sensibly without asking the disk.
CREATE TABLE file_pins (
    person_id  INTEGER NOT NULL REFERENCES people(id) ON DELETE CASCADE,
    path       TEXT NOT NULL,
    kind       TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (person_id, path)
);
CREATE INDEX file_pins_path ON file_pins(path);
