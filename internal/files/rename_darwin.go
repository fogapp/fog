package files

import (
	"errors"

	"golang.org/x/sys/unix"
)

// renameatNoReplace renames without replacing whatever holds the new name,
// in one call (renameatx_np RENAME_EXCL), falling back to a check and a
// plain renameat where the filesystem does not support it.
func renameatNoReplace(oldfd int, oldname string, newfd int, newname string) error {
	err := unix.RenameatxNp(oldfd, oldname, newfd, newname, unix.RENAME_EXCL)
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOTSUP) {
		return renameatCheck(oldfd, oldname, newfd, newname)
	}
	return err
}
