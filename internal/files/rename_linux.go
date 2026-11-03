package files

import (
	"errors"

	"golang.org/x/sys/unix"
)

// renameatNoReplace renames without replacing whatever holds the new name,
// in one call (renameat2 RENAME_NOREPLACE). A filesystem that does not
// support the flag (some network shares) falls back to a check and a plain
// renameat, which can race only on the replacing, never on which folders
// are involved: both are descriptors.
func renameatNoReplace(oldfd int, oldname string, newfd int, newname string) error {
	err := unix.Renameat2(oldfd, oldname, newfd, newname, unix.RENAME_NOREPLACE)
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EOPNOTSUPP) {
		return renameatCheck(oldfd, oldname, newfd, newname)
	}
	return err
}
