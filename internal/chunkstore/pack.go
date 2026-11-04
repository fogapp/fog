package chunkstore

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"strings"
)

// A pack is one file holding many chunks:
//
//	"FOGPACK1" | record… | header | u32 header length | "FOGPEND1"
//
// A record is one chunk as stored: a codec byte (0 raw, 1 zstd) and the
// payload, the whole sealed (nonce ‖ XChaCha20-Poly1305, bound to the chunk
// id) in an encrypted store. The header lists every record — chunk id,
// offset, stored length, plaintext length — sealed in an encrypted store so
// it reveals neither names nor sizes. The pack's name is the SHA-256 of the
// whole file, so a pack is checked by hashing it, and copies of it
// (offsite) are checked the same way.
const (
	packMagic   = "FOGPACK1"
	packEnd     = "FOGPEND1"
	entrySize   = 32 + 8 + 4 + 4
	codecRaw    = 0
	codecZstd   = 1
	trailerSize = 4 + 8 // u32 header length, packEnd
	// maxHeader bounds what a pack may claim its header is, so a damaged
	// length cannot make a reader allocate gigabytes.
	maxHeader = 64 << 20
)

var headerAD = []byte("fog-store/1 pack header")

// PackEntry is one record a pack's header lists.
type PackEntry struct {
	ID         ChunkID
	Offset     int64
	StoredLen  uint32
	RawLen     uint32
	storedData []byte // set while the pack is being built
}

// packWriter collects records until the pack is full.
type packWriter struct {
	s       *Store
	entries []PackEntry
	size    int
	ids     map[ChunkID]bool
}

func (s *Store) newPackWriter() *packWriter {
	return &packWriter{s: s, ids: map[ChunkID]bool{}}
}

// add encodes one chunk (plaintext b, named id) into the pack.
func (w *packWriter) add(id ChunkID, b []byte) {
	rec := w.s.encode(id, b)
	w.entries = append(w.entries, PackEntry{ID: id, StoredLen: uint32(len(rec)), RawLen: uint32(len(b)), storedData: rec})
	w.size += len(rec)
	w.ids[id] = true
}

func (w *packWriter) full() bool { return w.size >= w.s.packSize }

// encode is a chunk as a record: compressed when that helps, sealed when
// the store is encrypted.
func (s *Store) encode(id ChunkID, b []byte) []byte {
	var rec []byte
	if z := s.enc.EncodeAll(b, make([]byte, 1, len(b)/2+16)); len(z) < len(b)+1 {
		z[0] = codecZstd
		rec = z
	} else {
		rec = make([]byte, 1+len(b))
		rec[0] = codecRaw
		copy(rec[1:], b)
	}
	if s.aead != nil {
		rec = s.seal(rec, id[:])
	}
	return rec
}

// decode reverses encode and checks the result is the chunk it claims.
func (s *Store) decode(id ChunkID, rec []byte, rawLen uint32) ([]byte, error) {
	if s.aead != nil {
		var err error
		if rec, err = s.open(rec, id[:]); err != nil {
			return nil, err
		}
	}
	if len(rec) < 1 {
		return nil, ErrCorrupt
	}
	var b []byte
	switch rec[0] {
	case codecRaw:
		b = rec[1:]
	case codecZstd:
		var err error
		if b, err = s.dec.DecodeAll(rec[1:], make([]byte, 0, rawLen)); err != nil {
			return nil, ErrCorrupt
		}
	default:
		return nil, ErrCorrupt
	}
	if uint32(len(b)) != rawLen || s.chunkID(b) != id {
		return nil, ErrCorrupt
	}
	return b, nil
}

// flush writes the pack, durably, and returns its name and entries. The
// records go out in a random order: the order chunks arrived in would
// otherwise say which came from the same file.
func (w *packWriter) flush() (string, []PackEntry, int64, error) {
	if len(w.entries) == 0 {
		return "", nil, 0, nil
	}
	for i := len(w.entries) - 1; i > 0; i-- {
		j, _ := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		k := int(j.Int64())
		w.entries[i], w.entries[k] = w.entries[k], w.entries[i]
	}
	f, err := os.CreateTemp(w.s.path("tmp"), "pack-*")
	if err != nil {
		return "", nil, 0, err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	h := sha256.New()
	out := io.MultiWriter(f, h)
	off := int64(len(packMagic))
	if _, err := io.WriteString(out, packMagic); err != nil {
		f.Close()
		return "", nil, 0, err
	}
	for i := range w.entries {
		e := &w.entries[i]
		e.Offset = off
		if _, err := out.Write(e.storedData); err != nil {
			f.Close()
			return "", nil, 0, err
		}
		off += int64(len(e.storedData))
		e.storedData = nil
	}
	hdr := encodeHeader(w.entries)
	if w.s.aead != nil {
		hdr = w.s.seal(hdr, headerAD)
	}
	var tl [4]byte
	binary.BigEndian.PutUint32(tl[:], uint32(len(hdr)))
	_, err = out.Write(hdr)
	if err == nil {
		_, err = out.Write(tl[:])
	}
	if err == nil {
		_, err = io.WriteString(out, packEnd)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", nil, 0, err
	}
	size := off + int64(len(hdr)) + int64(trailerSize)
	name := hex.EncodeToString(h.Sum(nil))
	dst := w.s.packPath(name)
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", nil, 0, err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return "", nil, 0, err
	}
	syncDir(filepath.Dir(dst))
	entries := w.entries
	w.entries, w.size, w.ids = nil, 0, map[ChunkID]bool{}
	return name, entries, size, nil
}

func encodeHeader(entries []PackEntry) []byte {
	b := make([]byte, 4, 4+len(entries)*entrySize)
	binary.BigEndian.PutUint32(b, uint32(len(entries)))
	for _, e := range entries {
		b = append(b, e.ID[:]...)
		b = binary.BigEndian.AppendUint64(b, uint64(e.Offset))
		b = binary.BigEndian.AppendUint32(b, e.StoredLen)
		b = binary.BigEndian.AppendUint32(b, e.RawLen)
	}
	return b
}

func decodeHeader(b []byte, size int64) ([]PackEntry, error) {
	if len(b) < 4 {
		return nil, ErrCorrupt
	}
	n := binary.BigEndian.Uint32(b)
	b = b[4:]
	if uint64(len(b)) != uint64(n)*entrySize {
		return nil, ErrCorrupt
	}
	out := make([]PackEntry, n)
	for i := range out {
		e := &out[i]
		copy(e.ID[:], b[:32])
		e.Offset = int64(binary.BigEndian.Uint64(b[32:]))
		e.StoredLen = binary.BigEndian.Uint32(b[40:])
		e.RawLen = binary.BigEndian.Uint32(b[44:])
		// Written so a huge offset cannot overflow past the comparison.
		if e.Offset < int64(len(packMagic)) || e.Offset > size || int64(e.StoredLen) > size-e.Offset {
			return nil, ErrCorrupt
		}
		b = b[entrySize:]
	}
	return out, nil
}

// packPath is packs/<first two hex>/<name>.pack.
func (s *Store) packPath(name string) string {
	return s.path("packs", name[:2], name+".pack")
}

// validPackName is 64 lowercase hex characters.
func validPackName(n string) bool {
	if len(n) != 64 {
		return false
	}
	for _, c := range n {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// listPacks names every pack file on disk.
func (s *Store) listPacks() ([]string, error) {
	var out []string
	dirs, err := os.ReadDir(s.path("packs"))
	if err != nil {
		return nil, err
	}
	for _, d := range dirs {
		if !d.IsDir() || len(d.Name()) != 2 {
			continue
		}
		ents, err := os.ReadDir(s.path("packs", d.Name()))
		if err != nil {
			return nil, err
		}
		for _, e := range ents {
			n, ok := strings.CutSuffix(e.Name(), ".pack")
			if ok && validPackName(n) && n[:2] == d.Name() {
				out = append(out, n)
			}
		}
	}
	return out, nil
}

// readHeader reads a pack's header from its tail without reading the
// records.
func (s *Store) readHeader(f *os.File) ([]PackEntry, int64, error) {
	st, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	size := st.Size()
	if size < int64(len(packMagic)+trailerSize+4) {
		return nil, size, ErrCorrupt
	}
	var tail [trailerSize]byte
	if _, err := f.ReadAt(tail[:], size-trailerSize); err != nil {
		return nil, size, err
	}
	if string(tail[4:]) != packEnd {
		return nil, size, ErrCorrupt
	}
	hl := int64(binary.BigEndian.Uint32(tail[:4]))
	if hl > maxHeader || hl > size-int64(len(packMagic)+trailerSize) {
		return nil, size, ErrCorrupt
	}
	hdr := make([]byte, hl)
	if _, err := f.ReadAt(hdr, size-trailerSize-hl); err != nil {
		return nil, size, err
	}
	if s.aead != nil {
		if hdr, err = s.open(hdr, headerAD); err != nil {
			return nil, size, err
		}
	}
	entries, err := decodeHeader(hdr, size-trailerSize-hl)
	return entries, size, err
}

// PackHeader reads the header of a pack held as bytes — a copy fetched
// from elsewhere — checking it is the pack its name says.
func (s *Store) PackHeader(name string, b []byte) ([]PackEntry, error) {
	sum := sha256.Sum256(b)
	if hex.EncodeToString(sum[:]) != name {
		return nil, ErrCorrupt
	}
	if len(b) < len(packMagic)+trailerSize+4 || !bytes.HasPrefix(b, []byte(packMagic)) {
		return nil, ErrCorrupt
	}
	tail := b[len(b)-trailerSize:]
	if string(tail[4:]) != packEnd {
		return nil, ErrCorrupt
	}
	hl := int(binary.BigEndian.Uint32(tail[:4]))
	if hl > len(b)-len(packMagic)-trailerSize {
		return nil, ErrCorrupt
	}
	hdr := b[len(b)-trailerSize-hl : len(b)-trailerSize]
	if s.aead != nil {
		var err error
		if hdr, err = s.open(hdr, headerAD); err != nil {
			return nil, err
		}
	}
	return decodeHeader(hdr, int64(len(b)-trailerSize-hl))
}

// readChunk reads and verifies one chunk at a known place.
func (s *Store) readChunk(loc chunkLoc) ([]byte, error) {
	f, err := os.Open(s.packPath(loc.Pack))
	if err != nil {
		return nil, fmt.Errorf("pack %s: %w", loc.Pack, err)
	}
	defer f.Close()
	rec := make([]byte, loc.StoredLen)
	if _, err := f.ReadAt(rec, loc.Offset); err != nil {
		return nil, fmt.Errorf("pack %s: %w", loc.Pack, err)
	}
	b, err := s.decode(loc.ID, rec, loc.RawLen)
	if err != nil {
		return nil, fmt.Errorf("chunk %s in pack %s: %w", loc.ID, loc.Pack, err)
	}
	return b, nil
}
