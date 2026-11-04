package chunkstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

type env struct {
	t     *testing.T
	dir   string
	index string
	clk   *clock
	key   []byte
}

func newEnv(t *testing.T) *env {
	t.Helper()
	base := t.TempDir()
	return &env{t: t, dir: filepath.Join(base, "store"), index: filepath.Join(base, "index", "ix.db"),
		clk: &clock{t: time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC)}}
}

func (e *env) open() *Store {
	e.t.Helper()
	s, err := Open(e.dir, Options{IndexPath: e.index, Create: true, Key: e.key,
		Params: smallParams, PackSize: 64 << 10, Now: e.clk.now})
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { s.Close() })
	return s
}

func snapshot(t *testing.T, s *Store, series, stamp string, files map[string][]byte) *Snapshot {
	t.Helper()
	w, err := s.Begin(series)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Add(Entry{Path: "d", Type: TypeDir, Mode: 0o755}, nil); err != nil {
		t.Fatal(err)
	}
	for _, name := range sortedKeys(files) {
		if _, err := w.AddBytes(name, 0o640, files[name]); err != nil {
			t.Fatal(err)
		}
	}
	snap, err := w.Commit(stamp, time.Now(), json.RawMessage(`{"app":"hub"}`))
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func sortedKeys(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func readFile(t *testing.T, s *Store, snap *Snapshot, p string) ([]byte, error) {
	t.Helper()
	e, err := s.Find(snap, p)
	if err != nil {
		return nil, err
	}
	r, err := s.Open(e)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

func TestRoundTripAndReopen(t *testing.T) {
	e := newEnv(t)
	s := e.open()
	files := map[string][]byte{"artifact.tar": randBytes(1, 700<<10), "d/manifest.json": []byte(`{"a":1}`), "empty": nil}
	snap := snapshot(t, s, "app/hub", "20261101-030000", files)
	s.Close()

	s = e.open()
	got, err := s.Snapshot(snap.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Series != "app/hub" || got.Files != 3 || got.Bytes != int64(700<<10+7) || string(got.Meta) != `{"app":"hub"}` {
		t.Fatalf("doc = %+v", got)
	}
	for name, want := range files {
		b, err := readFile(t, s, got, name)
		if err != nil || !bytes.Equal(b, want) {
			t.Fatalf("%s: %v (len %d want %d)", name, err, len(b), len(want))
		}
	}
	out := t.TempDir()
	if err := s.Materialize(context.Background(), got, out, nil); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(out, "d", "manifest.json"))
	if string(b) != `{"a":1}` {
		t.Fatalf("materialized manifest %q", b)
	}
	if st, _ := os.Stat(filepath.Join(out, "artifact.tar")); st == nil || st.Size() != 700<<10 || st.Mode().Perm() != 0o640 {
		t.Fatalf("materialized artifact %+v", st)
	}
}

func TestDedupAcrossSnapshots(t *testing.T) {
	e := newEnv(t)
	s := e.open()
	data := randBytes(2, 2<<20)
	first := snapshot(t, s, "app/hub", "20261101-030000", map[string][]byte{"artifact.tar": data})
	edited := append([]byte{}, data...)
	copy(edited[1<<20:], []byte("one small change"))
	second := snapshot(t, s, "app/hub", "20261102-030000", map[string][]byte{"artifact.tar": edited})
	if second.New*10 > first.New {
		t.Fatalf("second snapshot added %d bytes, first %d: the edit should cost a sliver", second.New, first.New)
	}
	// And the same bytes under another app dedupe too.
	third := snapshot(t, s, "app/feed", "20261102-030000", map[string][]byte{"x": data})
	if third.New*10 > first.New {
		t.Fatalf("identical content in another series added %d bytes", third.New)
	}
	if b, err := readFile(t, s, second, "artifact.tar"); err != nil || !bytes.Equal(b, edited) {
		t.Fatalf("edited read back wrong: %v", err)
	}
}

func TestEncryptedStore(t *testing.T) {
	e := newEnv(t)
	e.key = bytes.Repeat([]byte{7}, 32)
	s := e.open()
	secret := bytes.Repeat([]byte("top secret household ledger "), 4000)
	snap := snapshot(t, s, "app/fin", "20261101-030000", map[string][]byte{"ledger": secret})
	s.Close()

	if _, err := Open(e.dir, Options{IndexPath: e.index}); !errors.Is(err, ErrLocked) {
		t.Fatalf("open without key: %v", err)
	}
	if _, err := Open(e.dir, Options{IndexPath: e.index, Key: bytes.Repeat([]byte{8}, 32)}); !errors.Is(err, ErrWrongKey) {
		t.Fatalf("open with wrong key: %v", err)
	}
	// Nothing on disk says what is inside.
	filepath.Walk(e.dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() {
			b, _ := os.ReadFile(p)
			if bytes.Contains(b, []byte("household")) || bytes.Contains(b, []byte("app/fin")) || bytes.Contains(b, []byte("ledger")) {
				t.Errorf("%s holds plaintext", p)
			}
		}
		return nil
	})
	// The index is a cache: lose it and the packs give it back.
	os.Remove(e.index)
	s = e.open()
	got, err := s.Snapshot(snap.ID)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := readFile(t, s, got, "ledger"); err != nil || !bytes.Equal(b, secret) {
		t.Fatalf("encrypted read back: %v", err)
	}
}

func TestIndexRebuiltWhenPackVanishes(t *testing.T) {
	e := newEnv(t)
	s := e.open()
	a := snapshot(t, s, "app/a", "20261101-030000", map[string][]byte{"f": randBytes(3, 300<<10)})
	b := snapshot(t, s, "app/b", "20261101-030000", map[string][]byte{"f": randBytes(4, 300<<10)})
	s.Close()
	// Remove one of a's packs behind the store's back.
	ents, _ := os.ReadDir(filepath.Join(e.dir, "packs"))
	var victim string
	s2 := e.open()
	ea, _ := s2.Find(a, "f")
	loc, _, _ := s2.ix.lookup(mustID(t, ea.Chunks[0]))
	victim = s2.packPath(loc.Pack)
	s2.Close()
	_ = ents
	os.Remove(victim)

	s = e.open()
	if miss, err := s.Missing(b); err != nil || len(miss) != 0 {
		t.Fatalf("b missing %v %v", miss, err)
	}
	if miss, err := s.Missing(a); err != nil || len(miss) == 0 {
		t.Fatalf("a should miss chunks: %v %v", miss, err)
	}
	if _, err := readFile(t, s, a, "f"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reading a damaged snapshot: %v", err)
	}
}

func mustID(t *testing.T, s string) ChunkID {
	id, err := ParseChunkID(s)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestGCTwoPhaseKeepsSharedChunks(t *testing.T) {
	e := newEnv(t)
	s := e.open()
	shared := randBytes(5, 400<<10)
	a := snapshot(t, s, "app/hub", "20261101-030000", map[string][]byte{"shared": shared, "only-a": randBytes(6, 400<<10)})
	b := snapshot(t, s, "app/hub", "20261102-030000", map[string][]byte{"shared": shared})
	before, _ := s.Stats()
	if err := s.Delete(a.ID); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// Young packs are never touched.
	rep, err := s.GC(ctx, GCOptions{RepackBelow: -1})
	if err != nil || rep.Deleted != 0 || rep.Pending != 0 {
		t.Fatalf("young collection %+v %v", rep, err)
	}
	e.clk.t = e.clk.t.Add(25 * time.Hour)
	rep, err = s.GC(ctx, GCOptions{RepackBelow: -1})
	if err != nil || rep.Deleted != 0 || rep.Pending == 0 {
		t.Fatalf("first mark should only mark pending: %+v %v", rep, err)
	}
	e.clk.t = e.clk.t.Add(25 * time.Hour)
	rep, err = s.GC(ctx, GCOptions{RepackBelow: -1})
	if err != nil || rep.Deleted == 0 {
		t.Fatalf("second mark should delete: %+v %v", rep, err)
	}
	after, _ := s.Stats()
	if after.Bytes >= before.Bytes {
		t.Fatalf("nothing freed: %d → %d", before.Bytes, after.Bytes)
	}
	if got, err := readFile(t, s, b, "shared"); err != nil || !bytes.Equal(got, shared) {
		t.Fatalf("surviving snapshot damaged: %v", err)
	}
	if miss, _ := s.Missing(b); len(miss) != 0 {
		t.Fatalf("surviving snapshot misses %d chunks", len(miss))
	}
}

// A pack found dead once and then needed again (a new snapshot reused its
// chunks) leaves the pending list and survives.
func TestGCPendingRevivedByNewSnapshot(t *testing.T) {
	e := newEnv(t)
	s := e.open()
	data := randBytes(7, 300<<10)
	a := snapshot(t, s, "app/hub", "20261101-030000", map[string][]byte{"f": data})
	s.Delete(a.ID)
	e.clk.t = e.clk.t.Add(25 * time.Hour)
	ctx := context.Background()
	if rep, _ := s.GC(ctx, GCOptions{RepackBelow: -1}); rep.Pending == 0 {
		t.Fatal("expected pending packs")
	}
	b := snapshot(t, s, "app/hub", "20261102-030000", map[string][]byte{"f": data})
	if b.New > 64<<10 {
		t.Fatalf("reuse should have deduped, added %d", b.New)
	}
	e.clk.t = e.clk.t.Add(25 * time.Hour)
	rep, err := s.GC(ctx, GCOptions{RepackBelow: -1})
	if err != nil || rep.Deleted != 0 {
		t.Fatalf("revived pack deleted: %+v %v", rep, err)
	}
	if got, err := readFile(t, s, b, "f"); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("revived snapshot: %v", err)
	}
}

func TestGCRefusesWithUnreadableDocument(t *testing.T) {
	e := newEnv(t)
	s := e.open()
	snapshot(t, s, "app/hub", "20261101-030000", map[string][]byte{"f": randBytes(8, 100<<10)})
	os.WriteFile(filepath.Join(e.dir, "snapshots", "0123456789abcdef0123456789abcdef.json"), []byte("{garbage"), 0o600)
	e.clk.t = e.clk.t.Add(100 * time.Hour)
	if _, err := s.GC(context.Background(), GCOptions{}); err == nil {
		t.Fatal("collected with an unreadable snapshot document present")
	}
}

func TestRepackKeepsData(t *testing.T) {
	e := newEnv(t)
	s := e.open()
	// One writer, so both files share packs; then drop the bigger half.
	w, _ := s.Begin("app/hub")
	keep := randBytes(9, 16<<10)
	w.AddBytes("keep", 0o600, keep)
	w.AddBytes("drop", 0o600, randBytes(10, 200<<10))
	a, err := w.Commit("20261101-030000", time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	b := snapshot(t, s, "app/hub", "20261102-030000", map[string][]byte{"keep": keep})
	s.Delete(a.ID)
	e.clk.t = e.clk.t.Add(25 * time.Hour)
	rep, err := s.GC(context.Background(), GCOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Repacked == 0 {
		t.Fatalf("expected a repack: %+v", rep)
	}
	e.clk.t = e.clk.t.Add(50 * time.Hour)
	for range 2 {
		if _, err := s.GC(context.Background(), GCOptions{}); err != nil {
			t.Fatal(err)
		}
		e.clk.t = e.clk.t.Add(25 * time.Hour)
	}
	if got, err := readFile(t, s, b, "keep"); err != nil || !bytes.Equal(got, keep) {
		t.Fatalf("after repack: %v", err)
	}
}

func TestCheckFindsDamageAndHealRepairs(t *testing.T) {
	e := newEnv(t)
	s := e.open()
	data := randBytes(11, 500<<10)
	snap := snapshot(t, s, "app/hub", "20261101-030000", map[string][]byte{"f": data})
	if rep, err := s.CheckPacks(context.Background(), 0); err != nil || !rep.OK() || rep.Packs == 0 {
		t.Fatalf("clean check %+v %v", rep, err)
	}
	// Flip a byte inside the first chunk's record.
	ent, _ := s.Find(snap, "f")
	loc, _, _ := s.ix.lookup(mustID(t, ent.Chunks[0]))
	f, _ := os.OpenFile(s.packPath(loc.Pack), os.O_RDWR, 0)
	var one [1]byte
	f.ReadAt(one[:], loc.Offset+5)
	one[0] ^= 0xff
	f.WriteAt(one[:], loc.Offset+5)
	f.Close()

	if _, err := readFile(t, s, snap, "f"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("damaged read: %v", err)
	}
	rep, err := s.CheckPacks(context.Background(), 0)
	if err != nil || rep.OK() || len(rep.BadChunks) != 1 || rep.BadChunks[0] != ent.Chunks[0] {
		t.Fatalf("check %+v %v", rep, err)
	}
	// Heal from a clean copy of the content.
	byID := map[ChunkID][]byte{}
	c, _ := NewChunker(bytes.NewReader(data), s.gear, s.params)
	for {
		b, err := c.Next()
		if err != nil {
			break
		}
		byID[s.chunkID(b)] = append([]byte(nil), b...)
	}
	failed, err := s.Heal(context.Background(), rep.BadChunks, func(id ChunkID) ([]byte, error) {
		if b, ok := byID[id]; ok {
			return b, nil
		}
		return nil, ErrNotFound
	})
	if err != nil || len(failed) != 0 {
		t.Fatalf("heal failed %v %v", failed, err)
	}
	if got, err := readFile(t, s, snap, "f"); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("after heal: %v", err)
	}
}

func TestDocumentTamperRefused(t *testing.T) {
	e := newEnv(t)
	s := e.open()
	snap := snapshot(t, s, "app/hub", "20261101-030000", map[string][]byte{"f": []byte("x")})
	p := filepath.Join(e.dir, "snapshots", snap.ID+".json")
	b, _ := os.ReadFile(p)
	os.WriteFile(p, bytes.Replace(b, []byte("app/hub"), []byte("app/hux"), 1), 0o600)
	if _, err := s.Snapshot(snap.ID); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("tampered doc: %v", err)
	}
}

func TestBadPathsAndSeries(t *testing.T) {
	e := newEnv(t)
	s := e.open()
	if _, err := s.Begin("../x"); err == nil {
		t.Fatal("series with .. accepted")
	}
	w, _ := s.Begin("app/hub")
	defer w.Abort()
	for _, p := range []string{"", "/abs", "../up", "a/../b", "a//b"} {
		if _, err := w.AddBytes(p, 0, nil); err == nil {
			t.Fatalf("path %q accepted", p)
		}
	}
}

func TestRefusesForeignDirectory(t *testing.T) {
	e := newEnv(t)
	os.MkdirAll(filepath.Join(e.dir, "20261101-030000"), 0o755)
	if _, err := Open(e.dir, Options{IndexPath: e.index, Create: true}); err == nil {
		t.Fatal("made a store inside an app's backups folder")
	}
}

// Writers and collection racing: every committed snapshot must read back
// whole however the two interleave.
func TestConcurrentWritersAndGC(t *testing.T) {
	e := newEnv(t)
	s := e.open()
	ctx := context.Background()
	shared := randBytes(20, 200<<10)
	done := make(chan *Snapshot, 8)
	var gcs sync.WaitGroup
	defer gcs.Wait()
	for i := range 8 {
		go func() {
			w, err := s.Begin("app/hub")
			if err != nil {
				t.Error(err)
				done <- nil
				return
			}
			w.AddBytes("shared", 0o600, shared)
			w.AddBytes("own", 0o600, randBytes(uint64(30+i), 100<<10))
			snap, err := w.Commit("20261101-0300"+string(rune('0'+i))+"0", time.Now(), nil)
			if err != nil {
				t.Error(err)
			}
			done <- snap
		}()
		gcs.Go(func() { s.GC(ctx, GCOptions{}) })
	}
	for range 8 {
		snap := <-done
		if snap == nil {
			continue
		}
		if got, err := readFile(t, s, snap, "shared"); err != nil || !bytes.Equal(got, shared) {
			t.Fatalf("snapshot %s: %v", snap.ID, err)
		}
	}
}

// A crash after a pack is durable but before the index records it: the
// next open reads the pack in.
func TestOpenAdoptsUnindexedPack(t *testing.T) {
	e := newEnv(t)
	s := e.open()
	snap := snapshot(t, s, "app/hub", "20261101-030000", map[string][]byte{"f": randBytes(21, 300<<10)})
	packs, _ := s.ix.packs()
	for name := range packs {
		s.ix.dropPack(name) // the index forgets; the files stay
		break
	}
	s.Close()
	s = e.open()
	if miss, err := s.Missing(snap); err != nil || len(miss) != 0 {
		t.Fatalf("after reopen missing %v %v", miss, err)
	}
}

func FuzzDecodeHeader(f *testing.F) {
	f.Add(encodeHeader([]PackEntry{{Offset: 8, StoredLen: 4, RawLen: 4}}), int64(100))
	f.Fuzz(func(t *testing.T, b []byte, size int64) {
		entries, err := decodeHeader(b, size)
		if err == nil {
			for _, e := range entries {
				if e.Offset < 8 || e.Offset+int64(e.StoredLen) > size {
					t.Fatalf("accepted entry outside the pack: %+v", e)
				}
			}
		}
	})
}

// After a heal, an index rebuilt from the packs points at the healed copy,
// not the damaged pack collection is waiting to remove.
func TestRebuildPrefersHealedCopy(t *testing.T) {
	e := newEnv(t)
	s := e.open()
	data := randBytes(40, 200<<10)
	snap := snapshot(t, s, "app/hub", "20261101-030000", map[string][]byte{"f": data})
	ent, _ := s.Find(snap, "f")
	loc, _, _ := s.ix.lookup(mustID(t, ent.Chunks[0]))
	f, _ := os.OpenFile(s.packPath(loc.Pack), os.O_RDWR, 0)
	f.WriteAt(bytes.Repeat([]byte{0x55}, 64), loc.Offset+2)
	f.Close()
	rep, _ := s.CheckPacks(context.Background(), 0)
	byID := map[ChunkID][]byte{}
	c, _ := NewChunker(bytes.NewReader(data), s.gear, s.params)
	for {
		b, err := c.Next()
		if err != nil {
			break
		}
		byID[s.chunkID(b)] = append([]byte(nil), b...)
	}
	time.Sleep(10 * time.Millisecond) // the healed pack is newer on disk
	if failed, err := s.Heal(context.Background(), rep.BadChunks, func(id ChunkID) ([]byte, error) { return byID[id], nil }); err != nil || len(failed) != 0 {
		t.Fatalf("heal %v %v", failed, err)
	}
	if err := s.RebuildIndex(); err != nil {
		t.Fatal(err)
	}
	if got, err := readFile(t, s, snap, "f"); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("after rebuild: %v", err)
	}
}
