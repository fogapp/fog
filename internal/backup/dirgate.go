package backup

import (
	"context"
	"errors"
	"time"
)

// The backup directory can be moved to another disk while fog runs
// (files.Manager.MoveBackups). Everything that writes into it, deletes
// from it or reads a snapshot out of it for long holds the directory
// first; the move takes it exclusively, so it waits for work already
// running and turns new work away until the files and the rows that name
// them both point at the new place.
//
// Holding is a try, never a wait: a nightly backup that meets a move fails
// like it would on an absent NAS share, and the next run picks it up.

// ErrDirMoving is what work refused by a running move says.
var ErrDirMoving = errors.New("the backup directory is being moved; try again when the move is done")

// HoldDir holds the backup directory for one piece of work; release when
// done. It fails with ErrDirMoving while a move has it, or waits for it.
func (m *Manager) HoldDir() (release func(), err error) {
	if m.moving.Load() || !m.dirGate.TryRLock() {
		return nil, ErrDirMoving
	}
	return m.dirGate.RUnlock, nil
}

// LockDir takes the backup directory for a move: it waits for the work
// holding it to finish (asking again every second, so ctx can cancel) and
// keeps new work out until unlock. While it waits or holds, the directory
// reads as unavailable, so the offsite copies and the Inbox treat it the
// way they treat a NAS share that is away.
func (m *Manager) LockDir(ctx context.Context) (unlock func(), err error) {
	m.moving.Store(true)
	for !m.dirGate.TryLock() {
		select {
		case <-ctx.Done():
			m.moving.Store(false)
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return func() {
		m.moving.Store(false)
		m.dirGate.Unlock()
	}, nil
}

// Moving reports whether a move holds, or is waiting for, the directory.
func (m *Manager) Moving() bool { return m.moving.Load() }
