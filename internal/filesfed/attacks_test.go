package filesfed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/store"
)

// A contribute-mode subscriber's own files are decided by its own records.
// An origin that lists the subscriber's contribution — or a file the
// operator put in the folder and hasn't pushed yet — as its own, as a
// third party's stub, or under a narrowed listing, must not overwrite or
// dehydrate it.
func TestOriginCannotOverwriteTheSubscribersOwnFiles(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		detail string
		entry  func(ManifestEntry) ManifestEntry
	}{
		{"as the origin's own", DetailFull, func(e ManifestEntry) ManifestEntry { return e }},
		{"as a third party's stub", DetailFull, func(e ManifestEntry) ManifestEntry {
			e.Origin, e.Stub = "01J0THER000000000000000000", true
			return e
		}},
		{"under a listing", DetailListing, func(e ManifestEntry) ManifestEntry { return e }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newOriginFixture(t, DetailFull)
			f.sub.Mode = ModeContribute
			self := f.s.Fed.InstanceID()
			dir := filepath.Join(f.root, "Peers", "north", "Photos")
			os.MkdirAll(dir, 0o755)
			os.WriteFile(filepath.Join(dir, "mine.txt"), []byte("mine"), 0o644)
			os.WriteFile(filepath.Join(dir, "draft.txt"), []byte("draft"), 0o644)
			// draft.txt is kept on this box: never pushed, so the
			// subscriber holds no replica row for it.
			if err := f.st.AddFileShareExclude("Peers/north/Photos/draft.txt"); err != nil {
				t.Fatal(err)
			}
			fi, _ := os.Stat(filepath.Join(dir, "mine.txt"))
			if err := f.st.UpsertFileFedReplica(store.FileFedReplica{SubscriptionID: f.sub.ID, Rel: "mine.txt", Kind: "file",
				SHA256: sha([]byte("mine")), Size: 4, Mtime: fi.ModTime().UnixNano(), Hydrated: true, Origin: self, PushedAt: 1}); err != nil {
				t.Fatal(err)
			}
			blob := []byte("pwned")
			var entries []ManifestEntry
			for _, p := range []string{"mine.txt", "draft.txt"} {
				entries = append(entries, tc.entry(ManifestEntry{Path: p, Kind: "file", Size: int64(len(blob)), SHA256: sha(blob), Mtime: "2026-11-02T10:00:00Z"}))
			}
			f.answer = func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/blob/") {
					w.Write(blob)
					return
				}
				body, _ := json.Marshal(Manifest{Revision: 1, Mode: ModeContribute, Detail: tc.detail, Apps: AppsDenied, Entries: entries, ETag: "e1"})
				w.Write(body)
			}
			if err := f.s.pull(context.Background(), f.sub, f.peer, nil); err != nil {
				t.Fatal(err)
			}
			for name, want := range map[string]string{"mine.txt": "mine", "draft.txt": "draft"} {
				if got, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(got) != want {
					t.Errorf("%s: the subscriber's own file was overwritten or dehydrated: %q %v", name, got, err)
				}
			}
		})
	}
}

// A download stops at the size its entry declared — the size the quota
// was checked against — however much the peer streams.
func TestDownloadStopsAtTheDeclaredSize(t *testing.T) {
	t.Parallel()

	f := newOriginFixture(t, DetailFull)
	f.sub.QuotaBytes = 10
	big := bytes.Repeat([]byte("x"), 4<<20)
	f.serveManifest([]ManifestEntry{{Path: "tiny.txt", Kind: "file", Size: 1, SHA256: sha(big), Mtime: "2026-11-02T10:00:00Z"}}, big)
	err := f.s.pull(context.Background(), f.sub, f.peer, nil)
	if !errors.Is(err, errOversize) {
		t.Fatalf("a download past its declared size: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.root, "Peers", "north", "Photos", "tiny.txt")); !os.IsNotExist(err) {
		t.Fatalf("the oversized download landed: %v", err)
	}
	if got, _ := f.st.FileFedSubscriptionByID(f.sub.ID); got.BytesFetched > 1 {
		t.Fatalf("counted %d bytes fetched against a 1-byte entry", got.BytesFetched)
	}
}

// A symlink anywhere along a path — not only at its end — does not lead
// a peer out of a share: nothing reads or writes through it.
func TestSymlinkInsideAShareLeadsNowhere(t *testing.T) {
	t.Parallel()

	s, peer, root := manifestFixture(t)
	sh := share(t, s, peer, "Drop")
	os.MkdirAll(filepath.Join(root, "Drop"), 0o755)
	os.MkdirAll(filepath.Join(root, "Private"), 0o755)
	os.WriteFile(filepath.Join(root, "Private", "secret.txt"), []byte("secret"), 0o644)
	if err := os.Symlink("../Private", filepath.Join(root, "Drop", "lnk")); err != nil {
		t.Skip("no symlinks here:", err)
	}
	for _, inner := range []string{"lnk/secret.txt", "lnk"} {
		if _, ok := s.InLens(sh, inner); ok {
			t.Errorf("%s: in the lens through a symlink", inner)
		}
		if err := s.CanServe(peer, sh, inner); err == nil {
			t.Errorf("%s: servable through a symlink", inner)
		}
		if f, _, _, err := s.OpenBlob(peer, sh, inner); err == nil {
			f.Close()
			t.Errorf("%s: opened through a symlink", inner)
		}
	}
	sh.Mode = ModeContribute
	if cb, err := s.BeginContribution(peer, sh, "lnk/planted.txt", 3, "", ""); err == nil {
		s.AppendContribution(peer, sh, cb.ID, 0, strings.NewReader("bad"))
		s.CompleteContribution(peer, sh, cb.ID)
	}
	if _, err := os.Lstat(filepath.Join(root, "Private", "planted.txt")); !os.IsNotExist(err) {
		t.Fatalf("a contribution was written through a symlink: %v", err)
	}
}

// On a case-insensitive disk another spelling of a name opens the same
// file. It must not open it past the checks made by name: keep on this
// box, and a contribution's author.
func TestAnotherSpellingIsNotAnotherFile(t *testing.T) {
	t.Parallel()

	s, peer, root := manifestFixture(t)
	sh := share(t, s, peer, "Drop")
	os.MkdirAll(filepath.Join(root, "Drop"), 0o755)
	os.WriteFile(filepath.Join(root, "Drop", "secret.txt"), []byte("secret"), 0o644)
	os.WriteFile(filepath.Join(root, "Drop", "theirs.jpg"), []byte("theirs"), 0o644)
	if err := s.Store.AddFileShareExclude("Drop/secret.txt"); err != nil {
		t.Fatal(err)
	}
	other, err := s.Store.CreatePairedPeer("01J0THER000000000000000000", "south", "", "key2")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Store.UpsertFileFedOrigin(store.FileFedOrigin{Path: "Drop/theirs.jpg", PeerID: other.ID, ShareID: sh.ID,
		InstanceID: other.InstanceID, SHA256: sha([]byte("theirs")), Size: 6}); err != nil {
		t.Fatal(err)
	}
	if err := s.CanServe(peer, sh, "theirs.jpg"); !errors.Is(err, ErrNotAuthor) {
		t.Fatalf("another author's contribution: %v", err)
	}
	for _, inner := range []string{"SECRET.txt", "Secret.TXT", "THEIRS.jpg", "theirs.JPG"} {
		if err := s.CanServe(peer, sh, inner); err == nil {
			t.Errorf("%s: served under another spelling", inner)
		}
	}
	for _, inner := range []string{"SECRET.txt", "secret.TXT"} {
		sh.Mode = ModeContribute
		if _, err := s.BeginContribution(peer, sh, inner, 3, "", ""); !errors.Is(err, ErrKeptHere) {
			t.Errorf("%s: a contribution onto a kept file under another spelling: %v", inner, err)
		}
	}
}

// A replace is begun over the peer's own contribution and lands only over
// that: once the file there is no longer theirs — withdrawn, or since
// overwritten by the operator — the session ends instead of landing.
func TestReplaceDoesNotOutliveTheFileItReplaces(t *testing.T) {
	t.Parallel()

	s, peer, root := manifestFixture(t)
	sh := share(t, s, peer, "Drop")
	sh.Mode = ModeContribute
	os.MkdirAll(filepath.Join(root, "Drop"), 0o755)
	contribute := func(body string) (ContribBegin, error) {
		cb, err := s.BeginContribution(peer, sh, "a.txt", int64(len(body)), "", "")
		if err != nil {
			return cb, err
		}
		if _, err := s.AppendContribution(peer, sh, cb.ID, 0, strings.NewReader(body)); err != nil {
			return cb, err
		}
		return cb, nil
	}
	cb, err := contribute("one")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompleteContribution(peer, sh, cb.ID); err != nil {
		t.Fatal(err)
	}

	for _, takeover := range []struct {
		name string
		do   func()
	}{
		{"the operator overwrote it", func() {
			os.WriteFile(filepath.Join(root, "Drop", "a.txt"), []byte("operator's"), 0o644)
		}},
		{"withdrawn, then the operator's", func() {
			if err := s.WithdrawContribution(peer, sh, "a.txt"); err != nil {
				t.Fatal(err)
			}
			os.WriteFile(filepath.Join(root, "Drop", "a.txt"), []byte("operator's"), 0o644)
		}},
	} {
		// Back to the peer's own file, then a replace begun over it.
		os.Remove(filepath.Join(root, "Drop", "a.txt"))
		s.Store.DeleteFileFedOrigin("Drop/a.txt")
		cb, err := contribute("one")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.CompleteContribution(peer, sh, cb.ID); err != nil {
			t.Fatal(err)
		}
		cb, err = contribute("two")
		if err != nil {
			t.Fatal(err)
		}
		if cb.Path != "a.txt" {
			t.Fatalf("%s: the peer's own file was not replaced but renamed to %s", takeover.name, cb.Path)
		}
		takeover.do()
		if _, err := s.CompleteContribution(peer, sh, cb.ID); err == nil {
			t.Errorf("%s: the replace landed", takeover.name)
		}
		if got, _ := os.ReadFile(filepath.Join(root, "Drop", "a.txt")); string(got) != "operator's" {
			t.Errorf("%s: the operator's file was overwritten: %q", takeover.name, got)
		}
	}
}

// An origin's rename is followed only onto a free name: a name differing
// only in case is the same file on a case-insensitive disk, and another
// file — never to be overwritten — on a case-sensitive one.
func TestFollowedRenameDoesNotOverwrite(t *testing.T) {
	t.Parallel()

	f := newOriginFixture(t, DetailFull)
	f.sub.Mode = ModeContribute
	if err := f.st.UpdateFileFedSubscription(f.sub); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(f.root, "Peers", "north", "Photos")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("pushed"), 0o644)
	os.WriteFile(filepath.Join(dir, "A.txt"), []byte("other"), 0o644)
	a, _ := os.Stat(filepath.Join(dir, "a.txt"))
	b, _ := os.Stat(filepath.Join(dir, "A.txt"))
	if os.SameFile(a, b) {
		// Case-insensitive: one file, and following the rename is a
		// case-only rename of it.
		if _, err := f.s.followRename(f.sub, "Peers/north/Photos/a.txt", "A.txt"); err != nil {
			t.Fatalf("a case-only rename of the same file: %v", err)
		}
	} else {
		if _, err := f.s.followRename(f.sub, "Peers/north/Photos/a.txt", "A.txt"); err == nil {
			t.Fatal("the rename was followed onto another file")
		}
		if got, _ := os.ReadFile(filepath.Join(dir, "A.txt")); string(got) != "other" {
			t.Fatalf("the other file was overwritten: %q", got)
		}
	}
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("b"), 0o644)
	if _, err := f.s.followRename(f.sub, "Peers/north/Photos/b.txt", "A.txt"); err == nil {
		t.Fatal("the rename was followed onto another file")
	}
}

// A peer's catalog is the peer's to write: a thousand entries do not make
// a thousand Inbox notices.
func TestCatalogDoesNotFloodTheInbox(t *testing.T) {
	t.Parallel()

	f := newOriginFixture(t, DetailFull)
	if err := f.st.SetFedCapability(f.peer.ID, CapFilesShare, store.DirUse, ""); err != nil {
		t.Fatal(err)
	}
	var cat []CatalogEntry
	for i := 0; i < 1000; i++ {
		cat = append(cat, CatalogEntry{ID: int64(100 + i), Title: fmt.Sprintf("Share %d %s", i, strings.Repeat("x", 500)), Mode: ModeRead, Detail: DetailFull, UpdatedAt: time.Now().Format(time.RFC3339)})
	}
	f.answer = func(w http.ResponseWriter, r *http.Request) {
		body, _ := json.Marshal(map[string]any{"shares": cat})
		w.Write(body)
	}
	before, _ := f.st.CountActiveNotices()
	f.s.scanOffers(context.Background(), f.peer)
	after, _ := f.st.CountActiveNotices()
	if after-before > offerNoticesPerPeer {
		t.Fatalf("a catalog of 1000 made %d notices", after-before)
	}
	if after == before {
		t.Fatal("the offers made no notice at all")
	}
}

// A peer's thumbnail is a small JPEG: a few bytes declaring a vast image
// for the viewer's browser to decode are not cached or served.
func TestPeerThumbnailBombIsRefused(t *testing.T) {
	t.Parallel()

	f := newOriginFixture(t, DetailPreviews)
	if err := f.st.UpsertFileFedReplica(store.FileFedReplica{SubscriptionID: f.sub.ID, Rel: "x.jpg", Kind: "file", Size: 10, SHA256: sha([]byte("x"))}); err != nil {
		t.Fatal(err)
	}
	var small bytes.Buffer
	jpeg.Encode(&small, image.NewRGBA(image.Rect(0, 0, 8, 8)), nil)
	bomb := bytes.Clone(small.Bytes())
	i := bytes.Index(bomb, []byte{0xFF, 0xC0})
	if i < 0 {
		t.Fatal("no SOF0 in the encoded JPEG")
	}
	copy(bomb[i+5:], []byte{0xFF, 0xF0, 0xFF, 0xF0}) // 65520 × 65520
	var serve []byte
	f.answer = func(w http.ResponseWriter, r *http.Request) { w.Write(serve) }

	serve = bomb
	if rc, _, _, err := f.s.Thumb(context.Background(), "Peers/north/Photos/x.jpg", 320); err == nil {
		rc.Close()
		t.Fatal("a decompression bomb was taken as a thumbnail")
	}
	serve = small.Bytes()
	rc, _, _, err := f.s.Thumb(context.Background(), "Peers/north/Photos/x.jpg", 320)
	if err != nil {
		t.Fatalf("a plain thumbnail: %v", err)
	}
	rc.Close()
}

// A manifest that lists a folder, then drops it, takes back what it put
// there and nothing else: the subscriber's own files in that folder — its
// recorded contribution, a file it holds no row for, a file it keeps —
// stay, under any spelling of the folder's name, and so does the folder.
// What the origin's files leave goes to the trash, not away for good.
func TestPruneLeavesTheSubscribersFiles(t *testing.T) {
	t.Parallel()

	for _, spelling := range []string{"Notes", "notes"} {
		t.Run(spelling, func(t *testing.T) {
			f := newOriginFixture(t, DetailFull)
			f.sub.Mode = ModeContribute
			os.MkdirAll(filepath.Join(f.root, "Peers", "north", "Photos"), 0o755)
			self := f.s.Fed.InstanceID()
			blob := []byte("theirs")
			theirs := ManifestEntry{Path: spelling + "/theirs.txt", Kind: "file", Size: int64(len(blob)), SHA256: sha(blob), Mtime: "2026-11-02T10:00:00Z"}
			f.serveManifest([]ManifestEntry{{Path: spelling, Kind: "dir"}, theirs}, blob)
			if err := f.s.pull(context.Background(), f.sub, f.peer, nil); err != nil {
				t.Fatal(err)
			}
			// The folder lands under the manifest's spelling. On a
			// case-insensitive disk the subscriber's files go in under the
			// other spelling ("Notes" for "notes"), which is the same folder;
			// on a case-sensitive one there is only the one name.
			landed := filepath.Join(f.root, "Peers", "north", "Photos", spelling)
			if _, err := os.Stat(filepath.Join(landed, "theirs.txt")); err != nil {
				t.Fatalf("the origin's file did not land: %v", err)
			}
			name := spelling
			if f.s.caseInsensitiveAt("Peers/north/Photos") {
				name = "Notes"
			}
			dir := filepath.Join(f.root, "Peers", "north", "Photos", name)
			// The subscriber's own: one recorded as its contribution, one
			// with no row at all, each kept on this box so nothing pushes.
			os.WriteFile(filepath.Join(dir, "mine.txt"), []byte("mine"), 0o644)
			os.WriteFile(filepath.Join(dir, "draft.txt"), []byte("draft"), 0o644)
			fi, _ := os.Stat(filepath.Join(dir, "mine.txt"))
			f.st.UpsertFileFedReplica(store.FileFedReplica{SubscriptionID: f.sub.ID, Rel: name + "/mine.txt", Kind: "file",
				Size: 4, Mtime: fi.ModTime().UnixNano(), Hydrated: true, Origin: self, PushedAt: 1})
			f.st.AddFileShareExclude("Peers/north/Photos/" + name + "/mine.txt")
			f.st.AddFileShareExclude("Peers/north/Photos/" + name + "/draft.txt")

			f.serveManifest(nil, nil)
			if err := f.s.pull(context.Background(), f.sub, f.peer, nil); err != nil {
				t.Fatal(err)
			}
			for name, want := range map[string]string{"mine.txt": "mine", "draft.txt": "draft"} {
				if got, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(got) != want {
					t.Errorf("the subscriber's %s went with the origin's folder: %q %v", name, got, err)
				}
			}
			if _, err := os.Stat(filepath.Join(dir, "theirs.txt")); !os.IsNotExist(err) {
				t.Errorf("the origin's withdrawn file stayed: %v", err)
			}
			trash, _ := f.s.Files.ListTrash()
			found := false
			for _, it := range trash {
				found = found || it.Name == "theirs.txt"
			}
			if !found {
				t.Error("the withdrawn file was deleted, not trashed")
			}
		})
	}

	// A folder the origin placed and nobody added to goes once empty.
	f := newOriginFixture(t, DetailFull)
	f.serveManifest([]ManifestEntry{{Path: "Empty", Kind: "dir"}}, nil)
	f.s.pull(context.Background(), f.sub, f.peer, nil)
	f.serveManifest(nil, nil)
	f.s.pull(context.Background(), f.sub, f.peer, nil)
	if _, err := os.Stat(filepath.Join(f.root, "Peers", "north", "Photos", "Empty")); !os.IsNotExist(err) {
		t.Fatalf("an empty withdrawn folder stayed: %v", err)
	}
}

// The quota can't be talked down. A negative or absurd declared size is
// refused outright; sizes are summed without wrapping; and only what the
// pull would bring counts, so an entry claimed as the subscriber's own
// takes nothing off the total.
func TestQuotaCannotBeTalkedDown(t *testing.T) {
	t.Parallel()

	big := bytes.Repeat([]byte("x"), 4<<20)
	bigEntry := ManifestEntry{Path: "big.bin", Kind: "file", Size: int64(len(big)), SHA256: sha(big), Mtime: "2026-11-02T10:00:00Z"}
	for name, extra := range map[string][]ManifestEntry{
		"negative, as ours":  {{Path: "mine.bin", Kind: "file", Size: -(1 << 62), Origin: "self", Mtime: "2026-11-02T10:00:00Z"}},
		"negative":           {{Path: "neg.bin", Kind: "file", Size: -(4 << 20), Mtime: "2026-11-02T10:00:00Z"}},
		"wrapping":           {{Path: "a.bin", Kind: "file", Size: math.MaxInt64, Mtime: "2026-11-02T10:00:00Z"}, {Path: "b.bin", Kind: "file", Size: math.MaxInt64, Mtime: "2026-11-02T10:00:00Z"}},
		"huge, many of them": {{Path: "h1.bin", Kind: "file", Size: maxEntrySize, Mtime: "2026-11-02T10:00:00Z"}, {Path: "h2.bin", Kind: "file", Size: maxEntrySize, Mtime: "2026-11-02T10:00:00Z"}},
	} {
		t.Run(name, func(t *testing.T) {
			f := newOriginFixture(t, DetailFull)
			f.sub.QuotaBytes = 10
			for i := range extra {
				if extra[i].Origin == "self" {
					extra[i].Origin = f.s.Fed.InstanceID()
				}
			}
			f.serveManifest(append([]ManifestEntry{bigEntry}, extra...), big)
			f.s.pull(context.Background(), f.sub, f.peer, nil)
			if _, err := os.Stat(filepath.Join(f.root, "Peers", "north", "Photos", "big.bin")); !os.IsNotExist(err) {
				t.Fatalf("4 MiB landed under a 10-byte quota: %v", err)
			}
		})
	}
}

// On a case-insensitive disk a contribution to "SUB/x.txt" lands in the
// folder the disk calls "Sub". It is recorded under that name, so every
// other peer still sees it as the contributor's — a stub, never the
// origin's own — and a record made under another spelling before this
// still finds its file by identity.
func TestProvenanceSurvivesAnotherSpelling(t *testing.T) {
	t.Parallel()

	s, a, root := manifestFixture(t)
	os.MkdirAll(filepath.Join(root, "Photos", "Sub"), 0o755)
	os.WriteFile(filepath.Join(root, "Photos", "Sub", "probe"), []byte("p"), 0o644)
	if _, err := os.Stat(filepath.Join(root, "Photos", "SUB", "PROBE")); err != nil {
		t.Skip("this disk is case-sensitive")
	}
	os.Remove(filepath.Join(root, "Photos", "Sub", "probe"))
	b, err := s.Store.CreatePairedPeer("peer-2", "south", "", "key2")
	if err != nil {
		t.Fatal(err)
	}
	shA, shB := share(t, s, a, "Photos"), share(t, s, b, "Photos")
	shA.Mode = ModeContribute
	cb, err := s.BeginContribution(a, shA, "SUB/x.txt", 3, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendContribution(a, shA, cb.ID, 0, strings.NewReader("abc")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompleteContribution(a, shA, cb.ID); err != nil {
		t.Fatal(err)
	}
	if o, err := s.Store.FileFedOriginByPath("Photos/Sub/x.txt"); err != nil || o.PeerID != a.ID {
		t.Fatalf("provenance not recorded under the disk's spelling: %+v %v", o, err)
	}
	// A record from before, under the other spelling.
	os.WriteFile(filepath.Join(root, "Photos", "Sub", "y.txt"), []byte("yy"), 0o644)
	s.Store.UpsertFileFedOrigin(store.FileFedOrigin{Path: "Photos/SUB/y.txt", PeerID: a.ID, ShareID: shA.ID, InstanceID: a.InstanceID, Size: 2})

	for _, name := range []string{"x.txt", "y.txt"} {
		if err := s.CanServe(b, shB, "Sub/"+name); !errors.Is(err, ErrNotAuthor) {
			t.Errorf("%s went to another peer as the origin's own: %v", name, err)
		}
		if err := s.CanServe(a, shA, "Sub/"+name); err != nil {
			t.Errorf("%s to its own author: %v", name, err)
		}
	}
	m, err := s.buildManifest(shB)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"x.txt", "y.txt"} {
		if e := entry(m, "Sub/"+name); e.Origin != a.InstanceID {
			t.Errorf("%s listed without its author: %+v", name, e)
		}
	}
}

// A contributor withdrawing its file takes back only its own bytes: a file
// the operator has since edited stays, as the operator's, and an untouched
// one goes to the trash, not away for good.
func TestWithdrawalKeepsTheOperatorsEdit(t *testing.T) {
	t.Parallel()

	s, peer, root := manifestFixture(t)
	sh := share(t, s, peer, "Drop")
	sh.Mode = ModeContribute
	os.MkdirAll(filepath.Join(root, "Drop"), 0o755)
	for _, name := range []string{"edited.txt", "untouched.txt"} {
		cb, err := s.BeginContribution(peer, sh, name, 3, "", "")
		if err != nil {
			t.Fatal(err)
		}
		s.AppendContribution(peer, sh, cb.ID, 0, strings.NewReader("abc"))
		if _, err := s.CompleteContribution(peer, sh, cb.ID); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(root, "Drop", "edited.txt"), []byte("the operator's edit"), 0o644)
	for _, name := range []string{"edited.txt", "untouched.txt"} {
		if err := s.WithdrawContribution(peer, sh, name); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Store.FileFedOriginByPath("Drop/" + name); err == nil {
			t.Errorf("%s kept its provenance", name)
		}
	}
	if got, err := os.ReadFile(filepath.Join(root, "Drop", "edited.txt")); err != nil || string(got) != "the operator's edit" {
		t.Fatalf("the operator's edit went with the withdrawal: %q %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(root, "Drop", "untouched.txt")); !os.IsNotExist(err) {
		t.Fatalf("the untouched contribution stayed: %v", err)
	}
	trash, _ := s.Files.ListTrash()
	if len(trash) != 1 || trash[0].Name != "untouched.txt" {
		t.Fatalf("the withdrawn file is not in the trash: %+v", trash)
	}
}

// On a case-insensitive disk a file the subscriber saves under another
// spelling of a listed-only entry's name is the subscriber's: a changed
// listing doesn't remove it, an open doesn't overwrite it, and a write
// under any spelling of a replica's name meets the replica's guard.
func TestListedEntryNeverTouchesAnotherSpelling(t *testing.T) {
	t.Parallel()

	f := newOriginFixture(t, DetailPreviews)
	dir := filepath.Join(f.root, "Peers", "north", "Photos")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "probe"), []byte("p"), 0o644)
	if _, err := os.Stat(filepath.Join(dir, "PROBE")); err != nil {
		t.Skip("this disk is case-sensitive")
	}
	os.Remove(filepath.Join(dir, "probe"))
	blob := []byte("theirs")
	entry := ManifestEntry{Path: "a.txt", Kind: "file", Size: int64(len(blob)), SHA256: sha(blob), Mtime: "2026-11-02T10:00:00Z"}
	f.serveManifest([]ManifestEntry{entry}, blob)
	if err := f.s.pull(context.Background(), f.sub, f.peer, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Guard("peers/NORTH/photos/A.TXT"); err == nil {
		t.Fatal("a write under another spelling of a replica's name got past the guard")
	}
	// The subscriber's own file, under another spelling, kept on this box.
	os.WriteFile(filepath.Join(dir, "A.TXT"), []byte("mine"), 0o644)
	f.st.AddFileShareExclude("Peers/north/Photos/A.TXT")
	changed := entry
	changed.Size, changed.Mtime = 99, "2026-11-03T10:00:00Z"
	f.serveManifest([]ManifestEntry{changed}, blob)
	f.sub, _ = f.st.FileFedSubscriptionByID(f.sub.ID)
	f.s.pull(context.Background(), f.sub, f.peer, nil)
	if did, _ := f.s.Hydrate(context.Background(), "Peers/north/Photos/a.txt"); did {
		t.Error("an open fetched over the subscriber's file")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "A.TXT")); string(got) != "mine" {
		t.Fatalf("a changed listing replaced the subscriber's file: %q", got)
	}
	f.serveManifest(nil, nil)
	f.s.pull(context.Background(), f.sub, f.peer, nil)
	if got, err := os.ReadFile(filepath.Join(dir, "A.TXT")); err != nil || string(got) != "mine" {
		t.Fatalf("the subscriber's file under another spelling: %q %v", got, err)
	}
}

// A file the subscriber saves at an entry's name while that entry is
// arriving is kept: the download is not written over it, and the
// subscriber is told.
func TestDownloadNeverOverwritesAConcurrentSave(t *testing.T) {
	t.Parallel()

	f := newOriginFixture(t, DetailFull)
	dir := filepath.Join(f.root, "Peers", "north", "Photos")
	blob := []byte("theirs")
	f.serveManifest([]ManifestEntry{{Path: "x.txt", Kind: "file", Size: int64(len(blob)), SHA256: sha(blob), Mtime: "2026-11-02T10:00:00Z"}}, blob)
	inner := f.answer
	f.answer = func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/blob/") {
			// The subscriber saves its own file there now.
			os.MkdirAll(dir, 0o755)
			os.WriteFile(filepath.Join(dir, "x.txt"), []byte("mine"), 0o644)
		}
		inner(w, r)
	}
	f.s.pull(context.Background(), f.sub, f.peer, nil)
	if got, err := os.ReadFile(filepath.Join(dir, "x.txt")); err != nil || string(got) != "mine" {
		t.Fatalf("the download overwrote a file saved meanwhile: %q %v", got, err)
	}
	if _, err := f.st.FileFedReplica(f.sub.ID, "x.txt"); err == nil {
		t.Error("the sync still claims the subscriber's file")
	}
	found := false
	notices, _ := f.st.ActiveNotices()
	for _, n := range notices {
		found = found || n.Kind == NoticeConflict
	}
	if !found {
		t.Error("the subscriber was not told")
	}
}

// A manifest is read whole or not at all: an entry named twice (by case
// alone, where the disk can't tell the two apart), or a field that doesn't
// read — an mtime, a digest — refuses it before anything is placed.
func TestManifestWithDuplicatesOrUnreadableFieldsIsRefused(t *testing.T) {
	t.Parallel()

	blob := []byte("theirs")
	ok := ManifestEntry{Path: "a.txt", Kind: "file", Size: int64(len(blob)), SHA256: sha(blob), Mtime: "2026-11-02T10:00:00Z"}
	cases := map[string][]ManifestEntry{
		"twice":             {ok, ok},
		"file and folder":   {ok, {Path: "a.txt", Kind: "dir"}},
		"unreadable mtime":  {{Path: "a.txt", Kind: "file", Size: 6, SHA256: sha(blob), Mtime: "yesterday"}},
		"no mtime":          {{Path: "a.txt", Kind: "file", Size: 6, SHA256: sha(blob)}},
		"unreadable digest": {{Path: "a.txt", Kind: "file", Size: 6, SHA256: "not-a-digest", Mtime: "2026-11-02T10:00:00Z"}},
	}
	for name, entries := range cases {
		t.Run(name, func(t *testing.T) {
			f := newOriginFixture(t, DetailFull)
			f.serveManifest(entries, blob)
			if err := f.s.pull(context.Background(), f.sub, f.peer, nil); err == nil {
				t.Fatal("the manifest was applied")
			}
			if _, err := os.Stat(filepath.Join(f.root, "Peers", "north", "Photos", "a.txt")); !os.IsNotExist(err) {
				t.Fatalf("something was placed: %v", err)
			}
		})
	}
	t.Run("twice by case", func(t *testing.T) {
		f := newOriginFixture(t, DetailFull)
		os.MkdirAll(filepath.Join(f.root, "Peers", "north", "Photos"), 0o755)
		if !f.s.caseInsensitiveAt("Peers/north/Photos") {
			t.Skip("this disk tells a.txt and A.TXT apart")
		}
		upper := ok
		upper.Path = "A.TXT"
		f.serveManifest([]ManifestEntry{ok, upper}, blob)
		if err := f.s.pull(context.Background(), f.sub, f.peer, nil); err == nil {
			t.Fatal("two entries for one file on this disk were applied")
		}
	})
}

// A file this sync placed stays this sync's: recorded as the disk has it,
// it is neither "the subscriber's own" on the next pull nor kept past
// unsubscribing — so it can't slip out of the quota, outlive the copy, or
// be pushed on as the subscriber's contribution.
func TestPlacedFileStaysTheSyncs(t *testing.T) {
	t.Parallel()

	f := newOriginFixture(t, DetailFull)
	blob := []byte("theirs")
	f.serveManifest([]ManifestEntry{{Path: "a.txt", Kind: "file", Size: int64(len(blob)), SHA256: sha(blob), Mtime: "2026-11-02T10:00:00Z"}}, blob)
	for i := 0; i < 2; i++ {
		f.sub, _ = f.st.FileFedSubscriptionByID(f.sub.ID)
		f.sub.ManifestETag = ""
		if err := f.s.pull(context.Background(), f.sub, f.peer, nil); err != nil {
			t.Fatal(err)
		}
		r, err := f.st.FileFedReplica(f.sub.ID, "a.txt")
		if err != nil {
			t.Fatalf("pull %d: the sync no longer claims the file it placed: %v", i+1, err)
		}
		fi, _ := os.Stat(filepath.Join(f.root, "Peers", "north", "Photos", "a.txt"))
		if fi == nil || r.Size != fi.Size() || r.Mtime != fi.ModTime().UnixNano() {
			t.Fatalf("pull %d: the row isn't the disk's: %+v", i+1, r)
		}
	}
	if err := f.s.Unsubscribe(f.sub.ID, false, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.root, "Peers", "north", "Photos", "a.txt")); !os.IsNotExist(err) {
		t.Fatalf("the placed file outlived unsubscribing: %v", err)
	}
}

// Files the subscriber kept make one notice per peer, however many, and it
// says whose file was kept and that the peer's version was not written.
func TestKeptFileNoticesAreOnePerPeer(t *testing.T) {
	t.Parallel()

	f := newOriginFixture(t, DetailFull)
	dir := filepath.Join(f.root, "Peers", "north", "Photos")
	blob := []byte("theirs")
	var entries []ManifestEntry
	for i := 0; i < 20; i++ {
		entries = append(entries, ManifestEntry{Path: fmt.Sprintf("f%02d.txt", i), Kind: "file", Size: int64(len(blob)), SHA256: sha(blob), Mtime: "2026-11-02T10:00:00Z"})
	}
	f.serveManifest(entries, blob)
	inner := f.answer
	f.answer = func(w http.ResponseWriter, r *http.Request) {
		if i := strings.Index(r.URL.Path, "/blob/"); i >= 0 {
			os.MkdirAll(dir, 0o755)
			os.WriteFile(filepath.Join(dir, r.URL.Path[i+len("/blob/"):]), []byte("saved here"), 0o644)
		}
		inner(w, r)
	}
	before, _ := f.st.CountActiveNotices()
	f.s.pull(context.Background(), f.sub, f.peer, nil)
	after, _ := f.st.CountActiveNotices()
	if after-before != 1 {
		t.Fatalf("%d kept files made %d notices", len(entries), after-before)
	}
	notices, _ := f.st.ActiveNotices()
	for _, n := range notices {
		if n.Kind == NoticeConflict && (!strings.Contains(n.Body, "not written") || !strings.Contains(n.Title, "Kept 20 files")) {
			t.Fatalf("the notice doesn't say what happened: %s / %s", n.Title, n.Body)
		}
	}
}

// The manifest build reads each folder once, whatever the number of
// contributions recorded: 50,000 of them across shares cost seconds at
// most, not the minutes a lookup per record per folder came to.
func TestManifestBuildIsLinearInContributions(t *testing.T) {
	s, peer, root := manifestFixture(t)
	sh := share(t, s, peer, "Drop")
	other := share(t, s, peer, "Other")
	os.MkdirAll(filepath.Join(root, "Drop"), 0o755)
	os.MkdirAll(filepath.Join(root, "Other"), 0o755)
	for i := 0; i < 50000; i++ {
		shareOf, dir := sh, "Drop"
		if i%2 == 1 {
			shareOf, dir = other, "Other"
		}
		// Records under another spelling than the disk's, each in its own
		// folder: the costliest lookup there is.
		if err := s.Store.UpsertFileFedOrigin(store.FileFedOrigin{Path: fmt.Sprintf("%s/D%05d/F.TXT", strings.ToUpper(dir), i), PeerID: peer.ID, ShareID: shareOf.ID,
			InstanceID: peer.InstanceID, Size: 1}); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	if _, err := s.buildManifest(sh); err != nil {
		t.Fatal(err)
	}
	d := time.Since(start)
	t.Logf("manifest build with 50,000 contributions recorded: %s", d)
	if d > 10*time.Second {
		t.Fatalf("a manifest build with 50,000 contributions recorded took %s", d)
	}
}

// Neither one peer nor one share may hold more contributions than its cap:
// each is a record every manifest build reads.
func TestContributionCountsAreCapped(t *testing.T) {
	old := maxContribsPerPeer
	maxContribsPerPeer = 2
	t.Cleanup(func() { maxContribsPerPeer = old })
	s, peer, root := manifestFixture(t)
	sh := share(t, s, peer, "Drop")
	sh.Mode = ModeContribute
	os.MkdirAll(filepath.Join(root, "Drop"), 0o755)
	for i := 0; i < 2; i++ {
		cb, err := s.BeginContribution(peer, sh, fmt.Sprintf("%d.txt", i), 1, "", "")
		if err != nil {
			t.Fatal(err)
		}
		s.AppendContribution(peer, sh, cb.ID, 0, strings.NewReader("x"))
		if _, err := s.CompleteContribution(peer, sh, cb.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.BeginContribution(peer, sh, "2.txt", 1, "", ""); !errors.Is(err, ErrTooManyFiles) {
		t.Fatalf("a contribution past the cap: %v", err)
	}
}

// A file the subscriber saves over a replica — as editors save, through a
// new file renamed into place — is the subscriber's even at the replica's
// exact size and time: the replica was recorded by its device and inode
// too, and the save is another inode. The origin withdrawing the file
// then leaves the subscriber's save alone.
func TestSaveAtTheSameSizeAndTimeIsTheSubscribers(t *testing.T) {
	t.Parallel()

	f := newOriginFixture(t, DetailFull)
	f.sub.Mode = ModeContribute
	blob := []byte("theirs")
	f.serveManifest([]ManifestEntry{{Path: "a.txt", Kind: "file", Size: int64(len(blob)), SHA256: sha(blob), Mtime: "2026-11-02T10:00:00Z"}}, blob)
	os.MkdirAll(filepath.Join(f.root, "Peers", "north", "Photos"), 0o755)
	if err := f.s.pull(context.Background(), f.sub, f.peer, nil); err != nil {
		t.Fatal(err)
	}
	r, err := f.st.FileFedReplica(f.sub.ID, "a.txt")
	if err != nil || r.Ino == 0 {
		t.Fatalf("the placed file's inode was not recorded: %+v %v", r, err)
	}
	// The subscriber's save: same length, the time set back to the old one.
	local := filepath.Join(f.root, "Peers", "north", "Photos", "a.txt")
	fi, _ := os.Stat(local)
	tmp := local + ".save"
	os.WriteFile(tmp, []byte("MINE!!"), 0o644)
	os.Chtimes(tmp, fi.ModTime(), fi.ModTime())
	if err := os.Rename(tmp, local); err != nil {
		t.Fatal(err)
	}
	f.serveManifest(nil, nil)
	f.sub, _ = f.st.FileFedSubscriptionByID(f.sub.ID)
	f.sub.ManifestETag = ""
	f.s.pull(context.Background(), f.sub, f.peer, nil)
	if got, err := os.ReadFile(local); err != nil || string(got) != "MINE!!" {
		t.Fatalf("the subscriber's save went with the origin's withdrawal: %q %v", got, err)
	}
}

// The count caps hold against sessions opened before any completes: open
// sessions count as contributions on their way, the cap is checked again
// when each lands, a peer may hold only so many open at once, and a
// session left idle stops holding its place.
func TestOpenSessionsCountAgainstTheCaps(t *testing.T) {
	oldMax, oldOpen := maxContribsPerPeer, maxOpenContribsPerPeer
	maxContribsPerPeer, maxOpenContribsPerPeer = 2, 4
	t.Cleanup(func() { maxContribsPerPeer, maxOpenContribsPerPeer = oldMax, oldOpen })
	s, peer, root := manifestFixture(t)
	sh := share(t, s, peer, "Drop")
	sh.Mode = ModeContribute
	os.MkdirAll(filepath.Join(root, "Drop"), 0o755)

	var opened []ContribBegin
	var refused []error
	for i := 0; i < 20; i++ {
		cb, err := s.BeginContribution(peer, sh, fmt.Sprintf("%d.txt", i), 1, "", "")
		if err != nil {
			refused = append(refused, err)
			continue
		}
		opened = append(opened, cb)
	}
	if len(opened) != 2 || !errors.Is(refused[0], ErrTooManyFiles) {
		t.Fatalf("opened %d sessions under a cap of 2 (first refusal: %v)", len(opened), refused[0])
	}
	landed := 0
	for _, cb := range opened {
		s.AppendContribution(peer, sh, cb.ID, 0, strings.NewReader("x"))
		if _, err := s.CompleteContribution(peer, sh, cb.ID); err == nil {
			landed++
		}
	}
	if landed != 2 {
		t.Fatalf("%d landed", landed)
	}
	if n, _, _ := s.Store.CountFileFedOrigins(peer.ID, sh.ID); n != 2 {
		t.Fatalf("%d contributions recorded under a cap of 2", n)
	}

	// Complete re-checks: a session admitted, then the cap filled behind it.
	s2, peer2, root2 := manifestFixture(t)
	sh2 := share(t, s2, peer2, "Drop")
	sh2.Mode = ModeContribute
	os.MkdirAll(filepath.Join(root2, "Drop"), 0o755)
	cb, err := s2.BeginContribution(peer2, sh2, "late.txt", 1, "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"Drop/x", "Drop/y"} {
		s2.Store.UpsertFileFedOrigin(store.FileFedOrigin{Path: p, PeerID: peer2.ID, ShareID: sh2.ID, InstanceID: peer2.InstanceID, Size: 1})
	}
	s2.AppendContribution(peer2, sh2, cb.ID, 0, strings.NewReader("x"))
	if _, err := s2.CompleteContribution(peer2, sh2, cb.ID); !errors.Is(err, ErrTooManyFiles) {
		t.Fatalf("landing past the cap: %v", err)
	}

	// The open cap, and a stale session giving its place back.
	maxContribsPerPeer = 1000
	s3, peer3, root3 := manifestFixture(t)
	sh3 := share(t, s3, peer3, "Drop")
	sh3.Mode = ModeContribute
	os.MkdirAll(filepath.Join(root3, "Drop"), 0o755)
	var first ContribBegin
	for i := 0; i < maxOpenContribsPerPeer; i++ {
		cb, err := s3.BeginContribution(peer3, sh3, fmt.Sprintf("o%d.txt", i), 1, "", "")
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = cb
		}
	}
	if _, err := s3.BeginContribution(peer3, sh3, "one-too-many.txt", 1, "", ""); !errors.Is(err, ErrTooManyOpen) {
		t.Fatalf("past the open cap: %v", err)
	}
	s3.contribMu.Lock()
	cs := s3.contribs[first.ID]
	cs.touched = time.Now().Add(-contribIdleTTL - time.Minute)
	s3.contribs[first.ID] = cs
	s3.contribMu.Unlock()
	if _, err := s3.BeginContribution(peer3, sh3, "after-stale.txt", 1, "", ""); err != nil {
		t.Fatalf("a stale session still held its place: %v", err)
	}
}

// A replace is a session like any other on the peer's open cap: re-sending
// its own files can't open more at once than any other upload.
func TestReplaceSessionsCountOnTheOpenCap(t *testing.T) {
	oldOpen := maxOpenContribsPerPeer
	maxOpenContribsPerPeer = 4
	t.Cleanup(func() { maxOpenContribsPerPeer = oldOpen })
	s, peer, root := manifestFixture(t)
	sh := share(t, s, peer, "Drop")
	sh.Mode = ModeContribute
	os.MkdirAll(filepath.Join(root, "Drop"), 0o755)
	for i := 0; i < 6; i++ {
		cb, err := s.BeginContribution(peer, sh, fmt.Sprintf("%d.txt", i), 1, "", "")
		if err != nil {
			t.Fatal(err)
		}
		s.AppendContribution(peer, sh, cb.ID, 0, strings.NewReader("x"))
		if _, err := s.CompleteContribution(peer, sh, cb.ID); err != nil {
			t.Fatal(err)
		}
	}
	opened := 0
	var last error
	for i := 0; i < 6; i++ {
		cb, err := s.BeginContribution(peer, sh, fmt.Sprintf("%d.txt", i), 1, "", "")
		if err != nil {
			last = err
			continue
		}
		if !s.contribs[cb.ID].replaces {
			t.Fatalf("%d.txt was not begun as a replace", i)
		}
		opened++
	}
	if opened != 4 || !errors.Is(last, ErrTooManyOpen) {
		t.Fatalf("opened %d replace sessions under an open cap of 4 (last refusal: %v)", opened, last)
	}
}

// On a disk that folds Unicode forms together (APFS; ZFS formD; casefold
// ext4), "café" written precomposed (NFC) and decomposed (NFD) is one
// name. A manifest can't use the two forms as two entries — to land one
// over the other, or to take over a file another entry holds: the disk is
// asked whether the name already leads to a file this subscription holds
// or just placed, and the manifest is refused.
func TestUnicodeFormsAreOneName(t *testing.T) {
	t.Parallel()

	nfc, nfd := "café.txt", "café.txt"
	f := newOriginFixture(t, DetailFull)
	dir := filepath.Join(f.root, "Peers", "north", "Photos")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, nfc), []byte("p"), 0o644)
	if _, err := os.Stat(filepath.Join(dir, nfd)); err != nil {
		t.Skip("this disk keeps the two forms apart")
	}
	os.Remove(filepath.Join(dir, nfc))

	first, second := []byte("first"), []byte("second!")
	blobs := map[string][]byte{nfc: first, nfd: second}
	entries := []ManifestEntry{
		{Path: nfc, Kind: "file", Size: int64(len(first)), SHA256: sha(first), Mtime: "2026-11-02T10:00:00Z"},
		{Path: nfd, Kind: "file", Size: int64(len(second)), SHA256: sha(second), Mtime: "2026-11-02T10:00:00Z"},
	}
	serve := func(entries []ManifestEntry) {
		f.answer = func(w http.ResponseWriter, r *http.Request) {
			if i := strings.Index(r.URL.Path, "/blob/"); i >= 0 {
				w.Write(blobs[r.URL.Path[i+len("/blob/"):]])
				return
			}
			body, _ := json.Marshal(Manifest{Revision: 1, Mode: ModeRead, Detail: DetailFull, Apps: AppsDenied, Entries: entries, ETag: "e1"})
			w.Write(body)
		}
	}
	// Both forms in one manifest.
	serve(entries)
	if err := f.s.pull(context.Background(), f.sub, f.peer, nil); !errors.Is(err, errSameFileTwice) {
		t.Fatalf("a manifest naming one file in two forms: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, nfc))
	if string(got) != "first" && string(got) != "second!" {
		t.Fatalf("the file is neither entry's: %q", got)
	}
	if n, _ := f.st.CountActiveNotices(); n != 0 {
		t.Fatal("a duplicate was reported as the subscriber's file")
	}

	// One form placed; a later manifest names the other to take it over.
	f2 := newOriginFixture(t, DetailFull)
	f = f2
	dir = filepath.Join(f.root, "Peers", "north", "Photos")
	serve(entries[:1])
	if err := f.s.pull(context.Background(), f.sub, f.peer, nil); err != nil {
		t.Fatal(err)
	}
	serve(entries)
	f.sub, _ = f.st.FileFedSubscriptionByID(f.sub.ID)
	f.sub.ManifestETag = ""
	if err := f.s.pull(context.Background(), f.sub, f.peer, nil); !errors.Is(err, errSameFileTwice) {
		t.Fatalf("a manifest adding the other form of a held name: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, nfc)); string(got) != "first" {
		t.Fatalf("the held file was overwritten through the other form: %q", got)
	}
}
