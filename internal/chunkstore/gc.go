package chunkstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"gitea.lan/matthew/fog/internal/fsx"
)

// Collection is where every incremental backup tool has lost data: a chunk
// judged unused is deleted while a backup that is still running has just
// decided to reuse it. The store closes that door three ways:
//
//   - Writers and collection never overlap. A Writer holds the gate shared
//     from Begin to Commit; collection holds it exclusively, so the mark it
//     takes sees every committed snapshot and no half-built one.
//   - A pack is deleted only after two collections at least the grace period
//     apart both found nothing referring to it (the "fossil" idea): a
//     snapshot document written by anyone in between — a second fog pointed
//     at the same folder, a restore of the index — saves it.
//   - The roots are the snapshot documents on disk, not fog's database, so a
//     database rolled back by a recovery kit cannot make newer snapshots look
//     like garbage.

// GCOptions tune a collection.
type GCOptions struct {
	// RepackBelow rewrites a pack whose live share has fallen under it
	// (0.5 when zero; negative turns repacking off).
	RepackBelow float64
	// DryRun reports without changing anything.
	DryRun bool
	// Grace, when set, replaces the store's grace period for this
	// collection — shorter when the disk is filling and space is needed
	// tonight. Writers are still kept out while it runs.
	Grace time.Duration
	// Urgent drops the grace period altogether: two collections in a row
	// then free what the first found unreferenced. Only for a disk that is
	// out of room; writers are still kept out while each runs.
	Urgent bool
}

// GCReport says what a collection did.
type GCReport struct {
	Snapshots    int   `json:"snapshots"`
	LiveChunks   int   `json:"liveChunks"`
	Pending      int   `json:"pending"`
	Deleted      int   `json:"deleted"`
	FreedBytes   int64 `json:"freedBytes"`
	Repacked     int   `json:"repacked"`
	UnreadableSn int   `json:"unreadableSnapshots"`
}

type gcState struct {
	// Pending maps a pack to when a collection first found it unreferenced.
	Pending map[string]time.Time `json:"pending"`
}

func (s *Store) readGCState() gcState {
	st := gcState{Pending: map[string]time.Time{}}
	if b, err := os.ReadFile(s.path("gc.json")); err == nil {
		json.Unmarshal(b, &st)
		if st.Pending == nil {
			st.Pending = map[string]time.Time{}
		}
	}
	return st
}

func (s *Store) writeGCState(st gcState) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return fsx.WriteFileAtomic(s.path("gc.json"), b, 0o600)
}

// live marks every chunk any snapshot on disk uses.
func (s *Store) live(ctx context.Context) (map[ChunkID]bool, int, int, error) {
	bad := 0
	snaps, err := s.Snapshots(func(string, error) { bad++ })
	if err != nil {
		return nil, 0, 0, err
	}
	if bad > 0 {
		// A document that will not open might still be a snapshot someone
		// needs; its chunks cannot be told apart from garbage. Collect
		// nothing until it is dealt with.
		return nil, len(snaps), bad, fmt.Errorf("%d snapshot documents could not be read; not collecting until they are", bad)
	}
	set := map[ChunkID]bool{}
	mark := func(hex string) error {
		id, err := ParseChunkID(hex)
		if err != nil {
			return err
		}
		set[id] = true
		return nil
	}
	for _, snap := range snaps {
		if err := ctx.Err(); err != nil {
			return nil, 0, 0, err
		}
		for _, id := range snap.Tree {
			if err := mark(id); err != nil {
				return nil, 0, 0, err
			}
		}
		err := s.Walk(snap, func(e Entry) error {
			for _, id := range e.Chunks {
				if err := mark(id); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return nil, 0, 0, fmt.Errorf("snapshot %s: %w; not collecting", snap.ID, err)
		}
	}
	return set, len(snaps), 0, nil
}

// GC collects what no snapshot uses.
func (s *Store) GC(ctx context.Context, opt GCOptions) (GCReport, error) {
	var rep GCReport
	if opt.RepackBelow == 0 {
		opt.RepackBelow = 0.5
	}
	grace := s.grace
	if opt.Grace > 0 {
		grace = opt.Grace
	}
	if opt.Urgent {
		grace = 0
	}
	s.gate.Lock()
	defer s.gate.Unlock()

	set, n, bad, err := s.live(ctx)
	rep.Snapshots, rep.UnreadableSn = n, bad
	if err != nil {
		return rep, err
	}
	rep.LiveChunks = len(set)
	packs, err := s.ix.packs()
	if err != nil {
		return rep, err
	}
	now := s.now()
	st := s.readGCState()
	next := gcState{Pending: map[string]time.Time{}}
	var repack []string
	for name, row := range packs {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		locs, err := s.ix.chunksIn(name)
		if err != nil {
			return rep, err
		}
		var liveBytes, allBytes int64
		for _, l := range locs {
			allBytes += int64(l.StoredLen)
			if set[l.ID] {
				liveBytes += int64(l.StoredLen)
			}
		}
		young := now.Sub(row.AddedAt) < grace
		switch {
		case liveBytes == 0 && !young:
			first, seen := st.Pending[name]
			if !seen {
				first = now
			}
			if seen && now.Sub(first) >= grace {
				if opt.DryRun {
					rep.Deleted++
					rep.FreedBytes += row.Size
					continue
				}
				if err := s.ix.dropPack(name); err != nil {
					return rep, err
				}
				if err := os.Remove(s.packPath(name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return rep, err
				}
				rep.Deleted++
				rep.FreedBytes += row.Size
				continue
			}
			next.Pending[name] = first
		case liveBytes > 0 && !young && opt.RepackBelow > 0 && float64(liveBytes) < opt.RepackBelow*float64(row.Size):
			repack = append(repack, name)
		}
	}
	rep.Pending = len(next.Pending)
	if opt.DryRun {
		rep.Repacked = len(repack)
		return rep, nil
	}
	if len(repack) > 0 {
		moved, err := s.repack(ctx, repack, set)
		rep.Repacked = moved
		if err != nil {
			s.writeGCState(next)
			return rep, err
		}
	}
	return rep, s.writeGCState(next)
}

// repack copies the live chunks of sparse packs into new packs and points
// the index at the copies. The old packs are then unreferenced and go the
// usual way — pending, then deleted a grace period later — so a reader
// that looked up an old location just before still finds it.
func (s *Store) repack(ctx context.Context, names []string, set map[ChunkID]bool) (int, error) {
	pw := s.newPackWriter()
	flush := func() error {
		name, entries, size, err := pw.flush()
		if err != nil || name == "" {
			return err
		}
		return s.ix.addPack(name, size, entries, s.now(), true)
	}
	moved := 0
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return moved, err
		}
		locs, err := s.ix.chunksIn(name)
		if err != nil {
			return moved, err
		}
		for _, l := range locs {
			if !set[l.ID] || pw.ids[l.ID] {
				continue
			}
			b, err := s.readChunk(l)
			if err != nil {
				// Leave a damaged pack for check and heal to deal with;
				// moving it would only spread the loss.
				return moved, fmt.Errorf("repacking %s: %w", name, err)
			}
			pw.add(l.ID, b)
			if pw.full() {
				if err := flush(); err != nil {
					return moved, err
				}
			}
		}
		moved++
	}
	return moved, flush()
}
