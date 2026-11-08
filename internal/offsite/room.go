package offsite

import (
	"errors"
	"fmt"
	"syscall"

	"gitea.lan/matthew/fog/internal/chunkstore"
	"gitea.lan/matthew/fog/internal/fsx"
)

// ErrNoRoom is a check that could not run for want of local disk: the
// spool shares a disk with everything else on the server, and a check is
// never worth filling it.
var ErrNoRoom = errors.New("not enough room on this server")

// diskSpace reports the bytes available and the filesystem's size.
func diskSpace(dir string) (avail, total int64, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, 0, false
	}
	return int64(st.Bavail) * int64(st.Bsize), int64(st.Blocks) * int64(st.Bsize), true
}

// roomFloorMax caps the free space a check leaves untouched: a twentieth
// of a small disk, never more than this on a big one.
const roomFloorMax = 10 << 30

// ensureRoom refuses a fetch of need bytes into the spool that would leave
// its disk with less than a twentieth of itself (at most roomFloorMax)
// free — it is the disk Postgres and fog's own database live on. A disk
// that will not say is let through; the write itself fails if it must.
func (m *Manager) ensureRoom(dir string, need int64) error {
	space := m.DiskSpace
	if space == nil {
		space = diskSpace
	}
	avail, total, ok := space(dir)
	if !ok {
		return nil
	}
	floor := min(total/20, roomFloorMax)
	if avail-need < floor {
		return fmt.Errorf("%w: checking it needs %s of scratch space and %s must stay free, but only %s is free",
			ErrNoRoom, fsx.HumanBytes(need), fsx.HumanBytes(floor), fsx.HumanBytes(avail))
	}
	return nil
}

// storeFetchNeed is the scratch space a store-backed copy takes to check:
// its packs (each at most about a pack's size), and the backup itself when
// it is written out for a test-restore.
func storeFetchNeed(row *AppRow, materialize bool) int64 {
	need := int64(len(row.Packs)) * chunkstore.DefaultPackSize
	if materialize {
		need += row.SizeBytes
	}
	return need
}

// noRoom is a check that failed for this server's disk, not the copy's
// sake: refused up front, or out of space part way.
func noRoom(err error) bool {
	return errors.Is(err, ErrNoRoom) || errors.Is(err, syscall.ENOSPC)
}
