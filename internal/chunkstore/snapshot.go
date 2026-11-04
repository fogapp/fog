package chunkstore

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gitea.lan/matthew/fog/internal/fsx"
)

// Entry types.
const (
	TypeFile    = "file"
	TypeDir     = "dir"
	TypeSymlink = "symlink"
)

// Entry is one path in a snapshot. A file lists its chunks in order and the
// SHA-256 of its whole content, checked on every read back.
type Entry struct {
	Path   string   `json:"path"`
	Type   string   `json:"type"`
	Mode   uint32   `json:"mode"`
	MTime  int64    `json:"mtime,omitempty"` // Unix nanoseconds
	Size   int64    `json:"size,omitempty"`
	SHA256 string   `json:"sha256,omitempty"`
	Chunks []string `json:"chunks,omitempty"`
	Target string   `json:"target,omitempty"`
}

// Snapshot is a snapshot document: everything needed to put a backup back,
// standing alone. Its chunks may be shared with any number of other
// snapshots; it shares nothing else with them.
type Snapshot struct {
	ID        string          `json:"-"`
	Format    string          `json:"format"`
	Series    string          `json:"series"`
	Stamp     string          `json:"stamp"`
	CreatedAt string          `json:"created_at"`
	Meta      json.RawMessage `json:"meta,omitempty"`
	// Tree is the chunks of the entry list: one JSON object per line, in
	// the order they were added.
	Tree      []string `json:"tree"`
	TreeBytes int64    `json:"tree_bytes"`
	Files     int      `json:"files"`
	Bytes     int64    `json:"bytes"`
	// New is what this snapshot added to the store: chunks no earlier
	// snapshot had, as stored.
	New int64 `json:"new"`
}

// Writer builds one snapshot. Not safe for concurrent use; open one Writer
// per snapshot (several may run at once).
type Writer struct {
	s       *Store
	series  string
	pack    *packWriter
	tree    bytes.Buffer
	files   int
	bytes   int64
	newSize int64
	done    bool
	last    string
}

var seriesRe = func(s string) bool {
	if s == "" || len(s) > 200 || strings.HasPrefix(s, "/") || strings.Contains(s, "..") {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '/' || c == '.') {
			return false
		}
	}
	return true
}

// Begin starts a snapshot in series ("app/hub", "drive"). Collection
// waits until the snapshot is committed or aborted.
func (s *Store) Begin(series string) (*Writer, error) {
	if !seriesRe(series) {
		return nil, fmt.Errorf("chunkstore: bad series name %q", series)
	}
	s.gate.RLock()
	return &Writer{s: s, series: series, pack: s.newPackWriter()}, nil
}

// Abort drops the snapshot. Packs already written stay until collection
// finds nothing refers to them.
func (w *Writer) Abort() {
	if !w.done {
		w.done = true
		w.s.gate.RUnlock()
	}
}

// storeChunk stores b unless the store already has it.
func (w *Writer) storeChunk(b []byte) (ChunkID, error) {
	id := w.s.chunkID(b)
	if w.pack.ids[id] {
		return id, nil
	}
	have, err := w.s.ix.has(id)
	if err != nil {
		return id, err
	}
	if have {
		return id, nil
	}
	w.pack.add(id, b)
	if w.pack.full() {
		if err := w.flush(); err != nil {
			return id, err
		}
	}
	return id, nil
}

func (w *Writer) flush() error {
	name, entries, size, err := w.pack.flush()
	if err != nil || name == "" {
		return err
	}
	w.newSize += size
	return w.s.ix.addPack(name, size, entries, w.s.now(), false)
}

// writeStream chunks r into the store, returning the chunk ids, the byte
// count and the SHA-256 of what was read.
func (w *Writer) writeStream(r io.Reader) ([]string, int64, string, error) {
	c, err := NewChunker(r, w.s.gear, w.s.params)
	if err != nil {
		return nil, 0, "", err
	}
	h := sha256.New()
	var ids []string
	var n int64
	for {
		b, err := c.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, 0, "", err
		}
		h.Write(b)
		n += int64(len(b))
		id, err := w.storeChunk(b)
		if err != nil {
			return nil, 0, "", err
		}
		ids = append(ids, id.String())
	}
	return ids, n, hex.EncodeToString(h.Sum(nil)), nil
}

// Add records one entry. For a file, r is its content: Size, SHA256 and
// Chunks are filled in from what r yields. Paths are slash-separated and
// relative, each added once.
func (w *Writer) Add(e Entry, r io.Reader) (Entry, error) {
	if w.done {
		return e, errors.New("chunkstore: snapshot already finished")
	}
	if err := checkPath(e.Path); err != nil {
		return e, err
	}
	switch e.Type {
	case TypeFile:
		if r == nil {
			return e, fmt.Errorf("chunkstore: file %s has no content", e.Path)
		}
		ids, n, sum, err := w.writeStream(r)
		if err != nil {
			return e, fmt.Errorf("%s: %w", e.Path, err)
		}
		e.Chunks, e.Size, e.SHA256 = ids, n, sum
		w.files++
		w.bytes += n
	case TypeDir:
		e.Size, e.Chunks, e.SHA256 = 0, nil, ""
	case TypeSymlink:
		e.Size, e.Chunks, e.SHA256 = 0, nil, ""
	default:
		return e, fmt.Errorf("chunkstore: unknown entry type %q", e.Type)
	}
	line, err := json.Marshal(e)
	if err != nil {
		return e, err
	}
	w.tree.Write(line)
	w.tree.WriteByte('\n')
	return e, nil
}

// AddBytes is Add for a file held in memory.
func (w *Writer) AddBytes(p string, mode uint32, b []byte) (Entry, error) {
	return w.Add(Entry{Path: p, Type: TypeFile, Mode: mode}, bytes.NewReader(b))
}

func checkPath(p string) error {
	if p == "" || p != path.Clean(p) || strings.HasPrefix(p, "/") || p == ".." || strings.HasPrefix(p, "../") || strings.ContainsRune(p, 0) {
		return fmt.Errorf("chunkstore: bad entry path %q", p)
	}
	return nil
}

// Commit finishes the snapshot: every chunk durable and indexed first, the
// document last, so a document that exists can be trusted to be whole.
func (w *Writer) Commit(stamp string, createdAt time.Time, meta json.RawMessage) (*Snapshot, error) {
	if w.done {
		return nil, errors.New("chunkstore: snapshot already finished")
	}
	defer w.Abort()
	tree, treeBytes, _, err := w.writeStream(bytes.NewReader(w.tree.Bytes()))
	if err != nil {
		return nil, err
	}
	if err := w.flush(); err != nil {
		return nil, err
	}
	snap := &Snapshot{
		Format: Format, Series: w.series, Stamp: stamp,
		CreatedAt: createdAt.UTC().Format(time.RFC3339),
		Meta:      meta, Tree: tree, TreeBytes: treeBytes,
		Files: w.files, Bytes: w.bytes, New: w.newSize,
	}
	if err := w.s.writeDoc(snap); err != nil {
		return nil, err
	}
	return snap, nil
}

var docAD = []byte("fog-store/1 snapshot")

// writeDoc names the document after a keyed hash of its content and writes
// it exclusively: two identical documents are one snapshot.
func (s *Store) writeDoc(snap *Snapshot) error {
	b, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	m := hmac.New(sha256.New, s.idKey)
	m.Write([]byte("snapshot\x00"))
	m.Write(b)
	snap.ID = hex.EncodeToString(m.Sum(nil)[:16])
	if s.aead != nil {
		b = s.seal(b, docAD)
	}
	err = fsx.WriteFileExclusive(s.docPath(snap.ID), b, 0o600)
	if errors.Is(err, fs.ErrExist) {
		err = nil
	}
	if err == nil {
		syncDir(s.path("snapshots"))
	}
	return err
}

func (s *Store) docPath(id string) string { return s.path("snapshots", id+".json") }

func validSnapID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil && strings.ToLower(id) == id
}

// Snapshot reads one document.
func (s *Store) Snapshot(id string) (*Snapshot, error) {
	if !validSnapID(id) {
		return nil, fmt.Errorf("%w: snapshot %q", ErrNotFound, id)
	}
	b, err := os.ReadFile(s.docPath(id))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: snapshot %s", ErrNotFound, id)
	}
	if err != nil {
		return nil, err
	}
	return s.parseDoc(id, b)
}

// ParseDoc reads a document held as bytes (a copy fetched from elsewhere),
// checking it is the document its id names.
func (s *Store) parseDoc(id string, b []byte) (*Snapshot, error) {
	if s.aead != nil {
		var err error
		if b, err = s.open(b, docAD); err != nil {
			return nil, fmt.Errorf("snapshot %s: %w", id, err)
		}
	}
	m := hmac.New(sha256.New, s.idKey)
	m.Write([]byte("snapshot\x00"))
	m.Write(b)
	if hex.EncodeToString(m.Sum(nil)[:16]) != id {
		return nil, fmt.Errorf("snapshot %s: %w", id, ErrCorrupt)
	}
	var snap Snapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		return nil, fmt.Errorf("snapshot %s: %w", id, err)
	}
	if snap.Format != Format {
		return nil, fmt.Errorf("snapshot %s declares %q", id, snap.Format)
	}
	snap.ID = id
	return &snap, nil
}

// Snapshots reads every document, oldest stamp first. One that cannot be
// read is reported through bad and left out.
func (s *Store) Snapshots(bad func(id string, err error)) ([]*Snapshot, error) {
	ents, err := os.ReadDir(s.path("snapshots"))
	if err != nil {
		return nil, err
	}
	var out []*Snapshot
	for _, e := range ents {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !validSnapID(id) {
			continue
		}
		snap, err := s.Snapshot(id)
		if err != nil {
			if bad != nil {
				bad(id, err)
			}
			continue
		}
		out = append(out, snap)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Stamp != out[j].Stamp {
			return out[i].Stamp < out[j].Stamp
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// Delete removes a snapshot. Its chunks stay until collection finds no
// other snapshot uses them.
func (s *Store) Delete(id string) error {
	if !validSnapID(id) {
		return fmt.Errorf("%w: snapshot %q", ErrNotFound, id)
	}
	err := os.Remove(s.docPath(id))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err == nil {
		syncDir(s.path("snapshots"))
	}
	return err
}

// Walk calls fn for every entry of a snapshot, in order.
func (s *Store) Walk(snap *Snapshot, fn func(Entry) error) error {
	r := s.chunkReader(snap.Tree, snap.TreeBytes, "")
	defer r.Close()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 64<<20)
	for sc.Scan() {
		var e Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return fmt.Errorf("snapshot %s: entry list: %w", snap.ID, err)
		}
		if err := checkPath(e.Path); err != nil {
			return fmt.Errorf("snapshot %s: %w", snap.ID, err)
		}
		if err := fn(e); err != nil {
			return err
		}
	}
	return sc.Err()
}

// Entries is Walk collected.
func (s *Store) Entries(snap *Snapshot) ([]Entry, error) {
	var out []Entry
	err := s.Walk(snap, func(e Entry) error { out = append(out, e); return nil })
	return out, err
}

// Find returns one entry by path.
func (s *Store) Find(snap *Snapshot, p string) (Entry, error) {
	var found *Entry
	stop := errors.New("found")
	err := s.Walk(snap, func(e Entry) error {
		if e.Path == p {
			found = &e
			return stop
		}
		return nil
	})
	if found != nil {
		return *found, nil
	}
	if err != nil && err != stop {
		return Entry{}, err
	}
	return Entry{}, fmt.Errorf("%w: %s in snapshot %s", ErrNotFound, p, snap.ID)
}

// Open reads a file entry back, chunk by chunk, each verified against its
// name; the whole is checked against the entry's SHA-256 and size at the
// end, and a mismatch is the error the last Read returns.
func (s *Store) Open(e Entry) (io.ReadCloser, error) {
	if e.Type != TypeFile {
		return nil, fmt.Errorf("chunkstore: %s is not a file", e.Path)
	}
	return s.chunkReader(e.Chunks, e.Size, e.SHA256), nil
}

type chunkReader struct {
	s      *Store
	ids    []string
	size   int64
	want   string
	h      hash.Hash
	n      int64
	cur    []byte
	err    error
	closed bool
}

func (s *Store) chunkReader(ids []string, size int64, sum string) *chunkReader {
	return &chunkReader{s: s, ids: ids, size: size, want: sum, h: sha256.New()}
}

func (r *chunkReader) Read(p []byte) (int, error) {
	for len(r.cur) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		if len(r.ids) == 0 {
			r.err = io.EOF
			if r.n != r.size || (r.want != "" && hex.EncodeToString(r.h.Sum(nil)) != r.want) {
				r.err = fmt.Errorf("%w: read back %d bytes that do not match the %d recorded", ErrCorrupt, r.n, r.size)
			}
			return 0, r.err
		}
		b, err := r.s.Chunk(r.ids[0])
		if err != nil {
			r.err = err
			return 0, err
		}
		r.ids = r.ids[1:]
		r.h.Write(b)
		r.n += int64(len(b))
		r.cur = b
	}
	n := copy(p, r.cur)
	r.cur = r.cur[n:]
	return n, nil
}

func (r *chunkReader) Close() error { r.closed = true; return nil }

// Chunk reads one chunk by its hex id.
func (s *Store) Chunk(hexID string) ([]byte, error) {
	id, err := ParseChunkID(hexID)
	if err != nil {
		return nil, err
	}
	loc, ok, err := s.ix.lookup(id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: chunk %s", ErrNotFound, hexID)
	}
	return s.readChunk(loc)
}

// Materialize writes a snapshot out under dir (which must exist), entry by
// entry, every file verified as it is written. keep, when set, picks the
// entries to write.
func (s *Store) Materialize(ctx context.Context, snap *Snapshot, dir string, keep func(Entry) bool) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	type dirTime struct {
		p string
		t time.Time
	}
	var dirs []dirTime
	err = s.Walk(snap, func(e Entry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if keep != nil && !keep(e) {
			return nil
		}
		p := filepath.FromSlash(e.Path)
		if d := filepath.Dir(p); d != "." {
			if err := root.MkdirAll(d, 0o755); err != nil {
				return err
			}
		}
		mode := fs.FileMode(e.Mode).Perm()
		switch e.Type {
		case TypeDir:
			if err := root.MkdirAll(p, 0o755); err != nil {
				return err
			}
			root.Chmod(p, mode|0o700)
			dirs = append(dirs, dirTime{p, time.Unix(0, e.MTime)})
			return nil
		case TypeSymlink:
			root.Remove(p)
			return root.Symlink(e.Target, p)
		}
		f, err := root.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		r, _ := s.Open(e)
		_, err = io.Copy(f, r)
		if err == nil {
			err = f.Sync()
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return fmt.Errorf("%s: %w", e.Path, err)
		}
		if mode != 0 {
			root.Chmod(p, mode)
		}
		if e.MTime != 0 {
			t := time.Unix(0, e.MTime)
			root.Chtimes(p, t, t)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if !dirs[i].t.IsZero() && dirs[i].t.Unix() != 0 {
			root.Chtimes(dirs[i].p, dirs[i].t, dirs[i].t)
		}
	}
	return nil
}

// Stats are what the store occupies.
type Stats struct {
	Packs  int
	Bytes  int64
	Chunks int
}

// Stats reads the index's totals.
func (s *Store) Stats() (Stats, error) {
	p, b, c, err := s.ix.totals()
	return Stats{Packs: p, Bytes: b, Chunks: c}, err
}

// DocBytes is a snapshot document exactly as stored — sealed, in an
// encrypted store — for copying to another place the same store is read
// from.
func (s *Store) DocBytes(id string) ([]byte, error) {
	if !validSnapID(id) {
		return nil, fmt.Errorf("%w: snapshot %q", ErrNotFound, id)
	}
	return os.ReadFile(s.docPath(id))
}

// ConfigBytes is store.json as stored.
func (s *Store) ConfigBytes() ([]byte, error) { return os.ReadFile(s.path("store.json")) }

// PutDoc places a document copied from another copy of this store, after
// checking it is the document its id names.
func (s *Store) PutDoc(id string, b []byte) error {
	if _, err := s.parseDoc(id, b); err != nil {
		return err
	}
	if err := fsx.WriteFileExclusive(s.docPath(id), b, 0o600); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return nil
}

// AdoptPack places a pack file copied from another copy of this store and
// indexes it, after checking it hashes to its name.
func (s *Store) AdoptPack(name, path string) error {
	if !validPackName(name) {
		return fmt.Errorf("bad pack name %q", name)
	}
	sum, err := fsx.FileSHA256(path)
	if err != nil {
		return err
	}
	if sum != name {
		return fmt.Errorf("pack %s: %w", name, ErrCorrupt)
	}
	dst := s.packPath(name)
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	if err := os.Rename(path, dst); err != nil {
		return err
	}
	return s.indexPackFile(name)
}

// Content writes r's bytes into the store without recording an entry:
// for a caller that decides afterwards whether to keep what it read (a
// file that changed while being read is read again). The chunks stay
// unreferenced, and so collectable, until an entry naming them is recorded.
func (w *Writer) Content(r io.Reader) (chunks []string, size int64, sha256hex string, err error) {
	if w.done {
		return nil, 0, "", errors.New("chunkstore: snapshot already finished")
	}
	return w.writeStream(r)
}

// Record adds an entry whose content is already in the store — from
// Content, or carried over unchanged from an earlier snapshot without
// reading it again. Every chunk it names must be in the store.
func (w *Writer) Record(e Entry) error {
	if w.done {
		return errors.New("chunkstore: snapshot already finished")
	}
	if err := checkPath(e.Path); err != nil {
		return err
	}
	if e.Type != TypeFile {
		_, err := w.Add(e, nil)
		return err
	}
	for _, hexID := range e.Chunks {
		id, err := ParseChunkID(hexID)
		if err != nil {
			return err
		}
		if w.pack.ids[id] {
			continue
		}
		ok, err := w.s.ix.has(id)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: chunk %s of %s", ErrNotFound, hexID, e.Path)
		}
	}
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	w.tree.Write(line)
	w.tree.WriteByte('\n')
	w.files++
	w.bytes += e.Size
	return nil
}
