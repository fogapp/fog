package filesfed

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/files"
	"gitea.lan/matthew/fog/internal/store"
)

func manifestFixture(t *testing.T) (*Service, *store.FedPeer, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	fm := files.New(dir, st, log)
	t.Cleanup(fm.Close)
	peer, err := st.CreatePairedPeer("peer-1", "north", "", "key")
	if err != nil {
		t.Fatal(err)
	}
	return New(fm, nil, st, log), peer, fm.Root()
}

func share(t *testing.T, s *Service, peer *store.FedPeer, path string) *store.FileFedShare {
	t.Helper()
	sh, err := s.Store.CreateFileFedShare(&store.FileFedShare{PeerID: peer.ID, Path: path, Title: path,
		Mode: ModeRead, Scope: ScopeAll, Detail: DetailFull, Apps: AppsDenied})
	if err != nil {
		t.Fatal(err)
	}
	return sh
}

// indexed refreshes dirs so the Files index holds their entries, as it does
// for any folder fog has seen. A first build over unindexed files indexes
// them as it hashes, which is itself a change and drops the manifest it
// built.
func indexed(s *Service, dirs ...string) { s.Files.QueueHashes(dirs) }

func entry(m *Manifest, p string) ManifestEntry {
	for _, e := range m.Entries {
		if e.Path == p {
			return e
		}
	}
	return ManifestEntry{}
}

// A manifest hashes what it can inside a small budget and ships the rest
// without a digest instead of hashing inside the peer's request; the
// digest follows in a later manifest, with a new ETag, once it lands.
func TestManifestShipsUnhashedEntriesWithoutADigest(t *testing.T) {
	old := manifestHashBudget
	manifestHashBudget = 8
	t.Cleanup(func() { manifestHashBudget = old })

	s, peer, root := manifestFixture(t)
	os.MkdirAll(filepath.Join(root, "Album"), 0o755)
	os.WriteFile(filepath.Join(root, "Album", "small.txt"), []byte("tiny"), 0o644)
	os.WriteFile(filepath.Join(root, "Album", "big.bin"), []byte("more than the budget"), 0o644)
	sh := share(t, s, peer, "Album")

	m, err := s.buildManifest(sh)
	if err != nil {
		t.Fatal(err)
	}
	if entry(m, "small.txt").SHA256 == "" {
		t.Error("a file inside the budget shipped without its digest")
	}
	if e := entry(m, "big.bin"); e.Kind != "file" || e.SHA256 != "" {
		t.Fatalf("a file past the budget: %+v", e)
	}

	// The queued work lands (QueueHashes rows it; the hasher digests it).
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := s.Files.Node("Album/big.bin"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the unhashed file was never queued into the index")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := s.Files.HashNow("Album/big.bin"); err != nil {
		t.Fatal(err)
	}
	m2, err := s.buildManifest(sh)
	if err != nil {
		t.Fatal(err)
	}
	if entry(m2, "big.bin").SHA256 == "" {
		t.Fatal("the digest did not follow once it landed")
	}
	if m2.ETag == m.ETag {
		t.Fatal("a manifest that gained a digest kept its ETag: receivers would never see it")
	}
}

// A change drops the cached manifests of the shares it touches, and only
// those.
func TestChangeInvalidatesOnlyAffectedManifests(t *testing.T) {
	s, peer, root := manifestFixture(t)
	for _, d := range []string{"A", "B"} {
		os.MkdirAll(filepath.Join(root, d), 0o755)
		os.WriteFile(filepath.Join(root, d, "f.txt"), []byte(d), 0o644)
	}
	indexed(s, "A", "B")
	a, b := share(t, s, peer, "A"), share(t, s, peer, "B")
	for _, sh := range []*store.FileFedShare{a, b} {
		if _, err := s.Manifest(sh); err != nil {
			t.Fatal(err)
		}
	}
	s.onChange("A/f.txt")
	s.manifestMu.Lock()
	_, aCached := s.manifests[a.ID]
	_, bCached := s.manifests[b.ID]
	s.manifestMu.Unlock()
	if aCached {
		t.Error("the changed share's manifest is still cached")
	}
	if !bCached {
		t.Error("an untouched share's manifest was dropped")
	}
}

// A share stays dirty until the next nudge goes out; its manifest must not
// be rebuilt on every request in the meantime, and must still reflect the
// changes that dirtied it.
func TestManifestCachedWhileShareIsDirty(t *testing.T) {
	s, peer, root := manifestFixture(t)
	os.MkdirAll(filepath.Join(root, "A"), 0o755)
	os.WriteFile(filepath.Join(root, "A", "one.txt"), []byte("1"), 0o644)
	sh := share(t, s, peer, "A")
	if _, err := s.Manifest(sh); err != nil {
		t.Fatal(err)
	}

	os.WriteFile(filepath.Join(root, "A", "two.txt"), []byte("2"), 0o644)
	indexed(s, "A") // the refresh that notices it calls onChange
	sh, err := s.Store.FileFedShareByID(sh.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !sh.Dirty {
		t.Fatal("the change did not mark the share dirty")
	}
	m1, err := s.Manifest(sh)
	if err != nil {
		t.Fatal(err)
	}
	if entry(m1, "two.txt").Kind != "file" {
		t.Fatal("the manifest after a change does not show it")
	}
	m2, err := s.Manifest(sh)
	if err != nil {
		t.Fatal(err)
	}
	if m1 != m2 {
		t.Fatal("a dirty share's manifest was rebuilt instead of served from the cache")
	}

	// Another change while the share is still dirty drops it again.
	os.WriteFile(filepath.Join(root, "A", "three.txt"), []byte("3"), 0o644)
	indexed(s, "A")
	m3, err := s.Manifest(sh)
	if err != nil {
		t.Fatal(err)
	}
	if entry(m3, "three.txt").Kind != "file" {
		t.Fatal("a second change to an already-dirty share was served stale")
	}
}

// Renaming an entry inside a shared folder dirties the share like any other
// change, though the index rows move before the folder is re-read and the
// re-read finds nothing new; otherwise peers would wait out the manifest
// TTL to see it.
func TestRenameInsideShareMarksItDirty(t *testing.T) {
	s, peer, root := manifestFixture(t)
	os.MkdirAll(filepath.Join(root, "A"), 0o755)
	os.WriteFile(filepath.Join(root, "A", "one.txt"), []byte("1"), 0o644)
	indexed(s, "", "A")
	sh := share(t, s, peer, "A")
	if _, err := s.Manifest(sh); err != nil {
		t.Fatal(err)
	}
	// The nudge goes out: the share is clean.
	if _, err := s.Store.TakeDirtyFileFedShares(); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Files.Rename("A/one.txt", "renamed.txt"); err != nil {
		t.Fatal(err)
	}
	sh, err := s.Store.FileFedShareByID(sh.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !sh.Dirty {
		t.Fatal("a rename inside the share did not mark it dirty")
	}
	m, err := s.Manifest(sh)
	if err != nil {
		t.Fatal(err)
	}
	if entry(m, "renamed.txt").Kind != "file" || entry(m, "one.txt").Kind != "" {
		t.Fatal("the manifest after a rename still shows the old name")
	}
}

// Callers who arrive while a manifest is building wait for that build
// instead of each walking the folder.
func TestConcurrentManifestCallersShareOneBuild(t *testing.T) {
	s, peer, root := manifestFixture(t)
	os.MkdirAll(filepath.Join(root, "A"), 0o755)
	os.WriteFile(filepath.Join(root, "A", "f.txt"), []byte("x"), 0o644)
	sh := share(t, s, peer, "A")

	var builds atomic.Int32
	release := make(chan struct{})
	old := manifestBuilding
	manifestBuilding = func(int64) {
		builds.Add(1)
		<-release
	}
	t.Cleanup(func() { manifestBuilding = old })

	const callers = 8
	var wg sync.WaitGroup
	got := make([]*Manifest, callers)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i], _ = s.Manifest(sh)
		}()
	}
	time.Sleep(100 * time.Millisecond) // let them all reach the build
	close(release)
	wg.Wait()
	if n := builds.Load(); n != 1 {
		t.Fatalf("%d builds for %d concurrent callers, want 1", n, callers)
	}
	for i, m := range got {
		if m == nil || m != got[0] {
			t.Fatalf("caller %d got a different manifest", i)
		}
	}
}

// A change that lands while a manifest is building leaves nothing cached:
// the build may have walked the folder before the change.
func TestChangeDuringBuildIsNotCachedOver(t *testing.T) {
	s, peer, root := manifestFixture(t)
	os.MkdirAll(filepath.Join(root, "A"), 0o755)
	sh := share(t, s, peer, "A")

	old := manifestBuilding
	t.Cleanup(func() { manifestBuilding = old })
	manifestBuilding = func(int64) {
		manifestBuilding = old
		s.onChange("A")
	}
	if _, err := s.Manifest(sh); err != nil {
		t.Fatal(err)
	}
	s.manifestMu.Lock()
	_, cached := s.manifests[sh.ID]
	s.manifestMu.Unlock()
	if cached {
		t.Fatal("a manifest built across a change was cached")
	}
}
