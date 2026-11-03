-- A share link is of one item, not of whatever holds its path.
--
-- inode: the shared entry's inode when the link was made. A link whose path
-- now holds a different inode (the item was deleted and something else took
-- its name) leads nowhere. 0 for links made before this column; such a link
-- is bound to what it finds on its next use.
--
-- trashed_with: the trash receipt the item went with. While it is set the
-- link is suspended; restoring the item brings the link back (wherever the
-- item is restored to), and purging it deletes the link.
ALTER TABLE file_shares ADD COLUMN inode INTEGER NOT NULL DEFAULT 0;
ALTER TABLE file_shares ADD COLUMN trashed_with TEXT NOT NULL DEFAULT '';
CREATE INDEX file_shares_trashed_with ON file_shares(trashed_with) WHERE trashed_with <> '';
