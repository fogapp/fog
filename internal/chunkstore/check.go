package chunkstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"sort"
)

// CheckReport is what a scrub found.
type CheckReport struct {
	Packs  int `json:"packs"`
	Chunks int `json:"chunks"`
	// BadPacks are packs whose bytes no longer hash to their name, or
	// whose header will not read.
	BadPacks []string `json:"badPacks,omitempty"`
	// BadChunks are chunks the index places in a damaged pack and that
	// did not read back whole.
	BadChunks []string `json:"badChunks,omitempty"`
}

// OK is a scrub that found nothing wrong.
func (r CheckReport) OK() bool { return len(r.BadPacks) == 0 && len(r.BadChunks) == 0 }

// CheckPacks reads up to limit packs end to end — the ones checked longest
// ago first — hashing each against its name and decoding every chunk the
// index places in it. Run a slice of the store each night and the whole of
// it is re-read on a rolling schedule (limit ≤ 0 reads everything).
func (s *Store) CheckPacks(ctx context.Context, limit int) (CheckReport, error) {
	var rep CheckReport
	packs, err := s.ix.packs()
	if err != nil {
		return rep, err
	}
	order := make([]packRow, 0, len(packs))
	for _, p := range packs {
		order = append(order, p)
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].CheckedAt != order[j].CheckedAt {
			return order[i].CheckedAt < order[j].CheckedAt
		}
		return order[i].Name < order[j].Name
	})
	if limit > 0 && len(order) > limit {
		order = order[:limit]
	}
	for _, p := range order {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		rep.Packs++
		bad, n, err := s.checkPack(p.Name)
		if err != nil {
			return rep, err
		}
		rep.Chunks += n
		if bad != nil {
			rep.BadPacks = append(rep.BadPacks, p.Name)
			rep.BadChunks = append(rep.BadChunks, bad...)
			continue
		}
		s.ix.markChecked(p.Name, s.now())
	}
	return rep, nil
}

// checkPack verifies one pack. bad is non-nil when anything failed: the
// ids of the chunks the index places there that would not read back (every
// one, when the pack itself is unreadable).
func (s *Store) checkPack(name string) (bad []string, n int, err error) {
	locs, err := s.ix.chunksIn(name)
	if err != nil {
		return nil, 0, err
	}
	all := func() []string {
		out := make([]string, 0, len(locs)+1)
		for _, l := range locs {
			out = append(out, l.ID.String())
		}
		return append(out, "") // non-nil even for a pack the index uses for nothing
	}
	f, err := os.Open(s.packPath(name))
	if err != nil {
		return all(), 0, nil
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil || hex.EncodeToString(h.Sum(nil)) != name {
		// The file changed. Some chunks may still be fine: test each, so
		// heal knows exactly what to replace.
		for _, l := range locs {
			if _, err := s.readChunk(l); err != nil {
				bad = append(bad, l.ID.String())
			}
		}
		if bad == nil {
			bad = []string{}
		}
		return bad, len(locs), nil
	}
	if _, _, err := s.readHeader(f); err != nil {
		return all(), 0, nil
	}
	for _, l := range locs {
		if _, err := s.readChunk(l); err != nil {
			bad = append(bad, l.ID.String())
		}
		n++
	}
	return bad, n, nil
}

// Missing lists the chunks a snapshot uses that the index does not have —
// a cheap whole-snapshot check that reads no pack.
func (s *Store) Missing(snap *Snapshot) ([]string, error) {
	var out []string
	check := func(hex string) error {
		id, err := ParseChunkID(hex)
		if err != nil {
			return err
		}
		ok, err := s.ix.has(id)
		if err == nil && !ok {
			out = append(out, hex)
		}
		return err
	}
	for _, id := range snap.Tree {
		if err := check(id); err != nil {
			return nil, err
		}
	}
	// The tree itself may be what is missing; then there is nothing more
	// to walk.
	if len(out) > 0 {
		return out, nil
	}
	err := s.Walk(snap, func(e Entry) error {
		for _, id := range e.Chunks {
			if err := check(id); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

// Heal rewrites chunks from another copy: fetch returns a chunk's plaintext
// by id (from the offsite locker, say). Each is checked against its name
// before it is stored, and the index then points at the new copy; the
// damaged pack, once nothing points into it, is collected as usual. It
// returns the ids it could not heal.
func (s *Store) Heal(ctx context.Context, ids []string, fetch func(ChunkID) ([]byte, error)) ([]string, error) {
	s.gate.RLock()
	defer s.gate.RUnlock()
	pw := s.newPackWriter()
	var failed []string
	for _, hexID := range ids {
		if err := ctx.Err(); err != nil {
			return failed, err
		}
		if hexID == "" {
			continue
		}
		id, err := ParseChunkID(hexID)
		if err != nil {
			failed = append(failed, hexID)
			continue
		}
		b, err := fetch(id)
		if err != nil || s.chunkID(b) != id {
			failed = append(failed, hexID)
			continue
		}
		pw.add(id, bytes.Clone(b))
		if pw.full() {
			if err := s.flushReplace(pw); err != nil {
				return failed, err
			}
		}
	}
	return failed, s.flushReplace(pw)
}

func (s *Store) flushReplace(pw *packWriter) error {
	name, entries, size, err := pw.flush()
	if err != nil || name == "" {
		return err
	}
	if err := s.ix.addPack(name, size, entries, s.now(), true); err != nil {
		return fmt.Errorf("recording healed pack: %w", err)
	}
	return nil
}

// PacksOf names the packs holding every chunk a snapshot uses, as the index
// places them now — what another copy of the store needs in order to read
// the snapshot back.
func (s *Store) PacksOf(snap *Snapshot) ([]string, error) {
	set := map[string]bool{}
	add := func(hexID string) error {
		id, err := ParseChunkID(hexID)
		if err != nil {
			return err
		}
		loc, ok, err := s.ix.lookup(id)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: chunk %s", ErrNotFound, hexID)
		}
		set[loc.Pack] = true
		return nil
	}
	for _, id := range snap.Tree {
		if err := add(id); err != nil {
			return nil, err
		}
	}
	err := s.Walk(snap, func(e Entry) error {
		for _, id := range e.Chunks {
			if err := add(id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

// PackPath is where a pack's file is.
func (s *Store) PackPath(name string) string { return s.packPath(name) }

// PackOf names the pack the index places a chunk in.
func (s *Store) PackOf(hexID string) (string, bool, error) {
	id, err := ParseChunkID(hexID)
	if err != nil {
		return "", false, err
	}
	loc, ok, err := s.ix.lookup(id)
	return loc.Pack, ok, err
}

// ChunkFromPackFile reads one chunk out of a copy of a pack held in a file
// (fetched from elsewhere), checking the copy is the pack its name says and
// the chunk is the one its id names.
func (s *Store) ChunkFromPackFile(path, name string, id ChunkID) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	if hex.EncodeToString(h.Sum(nil)) != name {
		return nil, ErrCorrupt
	}
	entries, _, err := s.readHeader(f)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.ID != id {
			continue
		}
		rec := make([]byte, e.StoredLen)
		if _, err := f.ReadAt(rec, e.Offset); err != nil {
			return nil, err
		}
		return s.decode(id, rec, e.RawLen)
	}
	return nil, fmt.Errorf("%w: chunk %s in pack %s", ErrNotFound, id, name)
}

// ReadBack reads a snapshot back in full — its entry list and every file,
// each chunk checked against its id and each file against its digest. It
// is how a copy is proven before the original is let go.
func ReadBack(ctx context.Context, s *Store, snap *Snapshot) error {
	return s.Walk(snap, func(e Entry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if e.Type != TypeFile {
			return nil
		}
		r, err := s.Open(e)
		if err != nil {
			return err
		}
		if _, err := io.Copy(io.Discard, r); err != nil {
			return fmt.Errorf("%s: %w", e.Path, err)
		}
		return nil
	})
}
