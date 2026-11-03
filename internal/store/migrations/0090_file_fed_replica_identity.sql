-- A replica a peer sync placed is recognised by the file itself, not by
-- what its origin claimed: the device and inode it landed as, beside the
-- size and modification time the disk gave it then, all four compared. A
-- save through a new file (as editors save) is a new inode, so it reads as
-- the subscriber's even at the same size and time. 0 means not yet
-- recorded (rows from before this): the next pull records them for a file
-- still matching its size and time, and every placing records them.
ALTER TABLE file_fed_replicas ADD COLUMN dev INTEGER NOT NULL DEFAULT 0;
ALTER TABLE file_fed_replicas ADD COLUMN ino INTEGER NOT NULL DEFAULT 0;
