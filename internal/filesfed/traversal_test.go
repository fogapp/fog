package filesfed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitea.lan/matthew/fog/internal/fed"
	"gitea.lan/matthew/fog/internal/files"
	"gitea.lan/matthew/fog/internal/store"
	"gitea.lan/matthew/fog/internal/vault"
)

// originFixture is a subscriber with one subscription to a share at a
// paired origin, whose answers the test writes and signs as that origin.
type originFixture struct {
	s      *Service
	st     *store.Store
	root   string
	origin *fed.Identity
	peer   *store.FedPeer
	sub    *store.FileFedSubscription
	answer func(w http.ResponseWriter, r *http.Request)
	raw    func(w http.ResponseWriter, r *http.Request)
}

func newOriginFixture(t *testing.T, detail string) *originFixture {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	vlt, err := vault.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	fm := files.New(dir, st, log)
	t.Cleanup(fm.Close)
	f := &originFixture{st: st, root: fm.Root()}
	f.s = New(fm, &fed.Service{Store: st, Vault: vlt, Log: log}, st, log)

	ost, err := store.Open(filepath.Join(t.TempDir(), "origin.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ost.Close() })
	ovlt, err := vault.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if f.origin, err = fed.EnsureIdentity(ost, ovlt); err != nil {
		t.Fatal(err)
	}
	// The origin's handlers see inner requests and write plain answers;
	// SealedHandler does the envelope, as fogd's server does. raw, when
	// set, answers at the wire instead — whatever a stranger or a proxy
	// would send.
	sealed := fed.SealedHandler(f.origin, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { f.answer(w, r) }))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.raw != nil {
			f.raw(w, r)
			return
		}
		sealed.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	if f.peer, err = st.CreatePairedPeer(f.origin.InstanceID, "north", srv.URL, f.origin.PublicKeyB64()); err != nil {
		t.Fatal(err)
	}
	if f.sub, err = st.CreateFileFedSubscription(&store.FileFedSubscription{PeerID: f.peer.ID, RemoteShareID: 1,
		Title: "Photos", Folder: "Peers/north/Photos", Mode: ModeRead, DetailOffered: detail, DetailWanted: detail}); err != nil {
		t.Fatal(err)
	}
	return f
}

// serveManifest answers the manifest with entries, signed by the origin.
func (f *originFixture) serveManifest(entries []ManifestEntry, blob []byte) {
	f.answer = func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/blob/") {
			w.Write(blob)
			return
		}
		body, _ := json.Marshal(Manifest{Revision: 1, Mode: ModeRead, Detail: f.sub.DetailWanted, Apps: AppsDenied, Entries: entries, ETag: "e1"})
		w.Header().Set("ETag", "e1")
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	}
}

// A paired origin is trusted with what it shares, never with where the
// subscriber keeps it. A manifest naming a path that climbs out of the
// subscription folder — to overwrite, dehydrate or prune the subscriber's
// own files — is refused whole, before anything is written.
func TestManifestCannotReachOutsideTheSubscription(t *testing.T) {
	for _, detail := range []string{DetailFull, DetailPreviews} {
		for _, evil := range []string{
			"../../../Documents/taxes.pdf",
			"a/../../../../Documents/taxes.pdf",
			"/Documents/taxes.pdf",
			"./taxes.pdf",
			".fog/state",
			"a//b",
		} {
			f := newOriginFixture(t, detail)
			victim := filepath.Join(f.root, "Documents", "taxes.pdf")
			os.MkdirAll(filepath.Dir(victim), 0o755)
			os.WriteFile(victim, []byte("mine"), 0o644)
			blob := []byte("pwned")
			f.serveManifest([]ManifestEntry{{Path: evil, Kind: "file", Size: int64(len(blob)), SHA256: sha(blob), Mtime: "2026-11-02T10:00:00Z"}}, blob)
			err := f.s.pull(context.Background(), f.sub, f.peer, nil)
			if err == nil {
				t.Errorf("%s/%q: a manifest reaching outside the share was applied", detail, evil)
			}
			if got, rerr := os.ReadFile(victim); rerr != nil || string(got) != "mine" {
				t.Fatalf("%s/%q: the subscriber's own file was touched: %q %v", detail, evil, got, rerr)
			}
		}
	}

	// A directory entry outside the share would be recorded as a replica
	// and pruned — RemoveAll — once a later manifest left it out.
	f := newOriginFixture(t, DetailFull)
	os.MkdirAll(filepath.Join(f.root, "Documents"), 0o755)
	os.WriteFile(filepath.Join(f.root, "Documents", "taxes.pdf"), []byte("mine"), 0o644)
	f.serveManifest([]ManifestEntry{{Path: "../../../Documents", Kind: "dir"}}, nil)
	if err := f.s.pull(context.Background(), f.sub, f.peer, nil); err == nil {
		t.Fatal("a directory entry outside the share was applied")
	}
	f.serveManifest(nil, nil)
	f.s.pull(context.Background(), f.sub, f.peer, nil)
	if _, err := os.Stat(filepath.Join(f.root, "Documents", "taxes.pdf")); err != nil {
		t.Fatalf("pruning reached outside the share: %v", err)
	}

	// A plain path is placed where it belongs.
	blob := []byte("a photo")
	f.serveManifest([]ManifestEntry{{Path: "2026/beach.jpg", Kind: "file", Size: int64(len(blob)), SHA256: sha(blob), Mtime: "2026-11-02T10:00:00Z"}}, blob)
	if err := f.s.pull(context.Background(), f.sub, f.peer, nil); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(f.root, "Peers", "north", "Photos", "2026", "beach.jpg")); err != nil || string(got) != "a photo" {
		t.Fatalf("a plain entry did not land: %q %v", got, err)
	}
}

// "Stopped sharing" is the origin's word. A 404 or an "unknown peer"
// from whatever answers at its address now — a proxy, a stranger on a
// recycled lease — must not end the subscription.
func TestOnlyTheOriginCanRevokeASubscription(t *testing.T) {
	f := newOriginFixture(t, DetailFull)
	for _, body := range []string{`{"error":"no such share"}`, `{"error":"unknown peer","code":"unknown_peer"}`} {
		f.raw = func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(map[bool]int{true: http.StatusForbidden, false: http.StatusNotFound}[strings.Contains(body, "unknown")])
			io.WriteString(w, body)
		}
		if err := f.s.Sync(context.Background(), f.sub.ID, nil); err == nil {
			t.Fatal("an unproven refusal synced")
		}
		got, _ := f.st.FileFedSubscriptionByID(f.sub.ID)
		if got.Status == StatusRevoked {
			t.Fatalf("an unsigned %s revoked the subscription", body)
		}
	}
	// The origin's own signed 404 does.
	f.raw = nil
	f.answer = func(w http.ResponseWriter, r *http.Request) {
		body := []byte(`{"error":"no such share"}`)
		w.WriteHeader(http.StatusNotFound)
		w.Write(body)
	}
	if err := f.s.Sync(context.Background(), f.sub.ID, nil); err == nil || errors.Is(err, ErrNotActive) {
		t.Fatalf("the origin's 404: %v", err)
	}
	if got, _ := f.st.FileFedSubscriptionByID(f.sub.ID); got.Status != StatusRevoked {
		t.Fatalf("the origin's own 404 did not revoke: %s", got.Status)
	}
}

func sha(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Narrowing a share to a listing scrubs the bytes already here on the next
// pull, pinned or not, and what the origin withdrew is pruned in the same
// pull.
func TestNarrowingToAListingScrubsAndPrunes(t *testing.T) {
	f := newOriginFixture(t, DetailFull)
	blob := []byte("a photo")
	keep := ManifestEntry{Path: "beach.jpg", Kind: "file", Size: int64(len(blob)), SHA256: sha(blob), Mtime: "2026-11-02T10:00:00Z"}
	gone := ManifestEntry{Path: "old.jpg", Kind: "file", Size: int64(len(blob)), SHA256: sha(blob), Mtime: "2026-11-02T10:00:00Z"}
	f.serveManifest([]ManifestEntry{keep, gone}, blob)
	if err := f.s.pull(context.Background(), f.sub, f.peer, nil); err != nil {
		t.Fatal(err)
	}
	local := func(name string) string { return filepath.Join(f.root, "Peers", "north", "Photos", name) }
	if _, err := os.Stat(local("beach.jpg")); err != nil {
		t.Fatal(err)
	}

	// The origin narrows to a listing and stops sharing old.jpg; a new file
	// appears, which a listing never fetches.
	fresh := ManifestEntry{Path: "new.jpg", Kind: "file", Size: 3, SHA256: sha([]byte("new")), Mtime: "2026-11-02T11:00:00Z"}
	f.answer = func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/blob/") {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		body, _ := json.Marshal(Manifest{Revision: 2, Mode: ModeRead, Detail: DetailListing, Apps: AppsDenied,
			Entries: []ManifestEntry{keep, fresh}, ETag: "e2"})
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	}
	f.sub, _ = f.st.FileFedSubscriptionByID(f.sub.ID)
	f.s.pull(context.Background(), f.sub, f.peer, nil)
	if _, err := os.Stat(local("old.jpg")); !os.IsNotExist(err) {
		t.Fatalf("what the origin withdrew is still here: %v", err)
	}
	if got, err := os.ReadFile(local("beach.jpg")); err == nil && string(got) == "a photo" {
		t.Fatal("bytes stayed after the origin narrowed to a listing")
	}
}

// A filtered share lists the folders leading to what crosses, not the
// folder tree of everything else.
func TestFilteredManifestHidesOtherFolders(t *testing.T) {
	s, peer, root := manifestFixture(t)
	os.MkdirAll(filepath.Join(root, "Home", "Holidays"), 0o755)
	os.MkdirAll(filepath.Join(root, "Home", "Divorce lawyer"), 0o755)
	os.WriteFile(filepath.Join(root, "Home", "Holidays", "beach.jpg"), []byte("img"), 0o644)
	os.WriteFile(filepath.Join(root, "Home", "Divorce lawyer", "letter.txt"), []byte("txt"), 0o644)
	sh, err := s.Store.CreateFileFedShare(&store.FileFedShare{PeerID: peer.ID, Path: "Home", Title: "Home",
		Mode: ModeRead, Scope: ScopeFiltered, Filters: []string{"kind:image"}, Detail: DetailFull, Apps: AppsDenied})
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.buildManifest(sh)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range m.Entries {
		got = append(got, e.Path)
	}
	if strings.Join(got, ",") != "Holidays,Holidays/beach.jpg" {
		t.Fatalf("filtered manifest lists %v", got)
	}
}

// A copy shared for people and not for apps can't be reached by mounting a
// folder above it.
func TestParentMountsHonourEachCopysAppsLens(t *testing.T) {
	s, peer, root := manifestFixture(t)
	if _, err := s.Store.CreateFileFedSubscription(&store.FileFedSubscription{PeerID: peer.ID, RemoteShareID: 1,
		Title: "Diary", Folder: "Peers/north/Diary", Mode: ModeRead, DetailOffered: DetailFull, DetailWanted: DetailFull}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{root, filepath.Join(root, "Peers"), filepath.Join(root, "Peers", "north"), filepath.Join(root, "Peers", "north", "Diary")} {
		if _, err := s.MountPolicy(p); err == nil {
			t.Errorf("mounting %s reached an apps-denied copy", p)
		}
	}
	if _, err := s.MountPolicy(filepath.Join(root, "Documents")); err != nil {
		t.Errorf("an unrelated folder: %v", err)
	}
}

// A copy shared "not for apps" is held to that when it lands, not only when
// an app deploys: an app already mounting Peers (or the drive) can't come
// to see it. A new subscription is refused; a live copy is taken off the
// disk and paused.
func TestAppsDeniedCopyNeverLandsWhereAnAppSees(t *testing.T) {
	f := newOriginFixture(t, DetailFull)
	blob := []byte("a diary")
	f.serveManifest([]ManifestEntry{{Path: "2026/day.txt", Kind: "file", Size: int64(len(blob)), SHA256: sha(blob), Mtime: "2026-11-02T10:00:00Z"}}, blob)
	if err := f.s.pull(context.Background(), f.sub, f.peer, nil); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(f.root, "Peers", "north", "Photos", "2026", "day.txt")
	if _, err := os.Stat(local); err != nil {
		t.Fatal(err)
	}

	// An app now mounts the whole Peers folder.
	f.s.Files.MountedPaths = func() []files.MountedPath {
		return []files.MountedPath{{Path: filepath.Join(f.root, "Peers"), App: "gallery"}}
	}
	f.sub, _ = f.st.FileFedSubscriptionByID(f.sub.ID)
	f.sub.ManifestETag = ""
	if err := f.s.pull(context.Background(), f.sub, f.peer, nil); !errors.Is(err, ErrNotActive) {
		t.Fatalf("pull under an app's mount: %v", err)
	}
	if _, err := os.Stat(local); !os.IsNotExist(err) {
		t.Fatalf("the copy stayed where an app can read it: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.root, "Peers", "north", "Photos", "2026")); !os.IsNotExist(err) {
		t.Fatalf("its folders stayed: %v", err)
	}
	got, _ := f.st.FileFedSubscriptionByID(f.sub.ID)
	if got.Status != StatusPaused {
		t.Fatalf("subscription status %q, want paused", got.Status)
	}
	if ok, _ := f.s.Hydrate(context.Background(), "Peers/north/Photos/2026/day.txt"); ok {
		t.Fatal("an open fetched bytes under an app's mount")
	}

	// A new subscription to an apps-denied share under that mount is refused.
	f.answer = func(w http.ResponseWriter, r *http.Request) {
		body, _ := json.Marshal(map[string]any{"shares": []CatalogEntry{{ID: 9, Title: "Letters", Mode: ModeRead, Detail: DetailFull, Apps: AppsDenied}}})
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	}
	if _, err := f.s.Subscribe(context.Background(), f.peer, 9, DetailFull); !errors.Is(err, ErrReachedByApps) {
		t.Fatalf("subscribing under an app's mount: %v", err)
	}
}
