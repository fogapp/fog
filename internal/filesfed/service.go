// Package filesfed is Files across peers (docs/files-contract.md):
// the origin side — shares, the lens that decides what a peer sees, the
// floor nothing sees past, the manifest and blobs served over the signed
// channel, and the nudges — and the receiving side (sync.go) — catalogs,
// subscriptions, and the replica folders that land in the tree.
//
// The invariants are hub's, applied to files: a share is per peer and is
// the only thing that names what crosses; every file has one origin and a
// replica is never re-shared; a narrowed lens scrubs remote copies; and an
// entry marked "keep on this box" never leaves.
package filesfed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"gitea.lan/matthew/fog/internal/fed"
	"gitea.lan/matthew/fog/internal/files"
	"gitea.lan/matthew/fog/internal/loop"
	"gitea.lan/matthew/fog/internal/store"
	"golang.org/x/sync/singleflight"
)

// Capability, contract, and lens vocabularies.
const (
	CapFilesShare = "files.share"
	Contract      = "fog-files/1"

	ModeRead       = "read"
	ModeContribute = "contribute"

	ScopeAll      = "all"
	ScopePicked   = "picked"
	ScopeFiltered = "filtered"

	DetailFull     = "full"
	DetailPreviews = "previews"
	DetailListing  = "listing"

	AppsAllowed = "allowed"
	AppsDenied  = "denied"

	// nudgeDebounce bounds how often a changing folder pokes its peers.
	nudgeDebounce = 30 * time.Second
	// manifestTTL bounds how long a share's manifest is cached. Changes
	// fog sees drop it at once (invalidateManifest); the TTL catches what
	// it doesn't, such as a subscription added under the share.
	manifestTTL = 60 * time.Second
)

// manifestHashBudget is how many bytes of not-yet-hashed files one
// manifest build hashes inline, so a handful of new photos arrive with
// their digests (and thumbnails) at once; the rest go to the background
// hasher. A variable for tests.
var manifestHashBudget int64 = 32 << 20

// manifestBuilding runs as each manifest build starts; tests count and
// hold builds with it.
var manifestBuilding = func(shareID int64) {}

var (
	ErrBadLens   = errors.New("invalid share lens")
	ErrIsReplica = errors.New("a copy of a peer's share cannot be shared onward")
	ErrNoPeer    = errors.New("no such peer")
	ErrNotShared = errors.New("not shared with this peer")
)

// Service is the one instance of files-across-peers, both directions.
type Service struct {
	Files *files.Manager
	Fed   *fed.Service
	Store *store.Store
	Log   *slog.Logger
	// Notify raises an operator notification (ntfy); optional.
	Notify func(title, msg string, ok bool)
	// AppNotify tells apps mounting a folder that it changed under them
	// (docs/files-contract.md); wired by the server. reason is one of
	// changed | synced | hydrated | revoked | adopted.
	AppNotify func(rel, reason string)

	nudgeMu    sync.Mutex
	nudgeDue   bool
	manifestMu sync.Mutex
	manifests  map[int64]*cachedManifest
	// manifestGen counts each share's invalidations, so a build that
	// started before a change neither stores its result nor is joined by
	// a caller who arrives after it.
	manifestGen    map[int64]uint64
	manifestFlight singleflight.Group

	syncMu sync.Mutex
	// syncing is the pull in flight per subscription: how to stop it, and
	// when it has stopped (stopSync).
	syncing map[int64]*runningSync
	soon    map[int64]time.Time

	// hydrateMu guards the single-flight table for on-demand fetches and
	// the per-peer bandwidth buckets.
	hydrateMu sync.Mutex
	hydrating map[string]chan struct{}
	throttles map[int64]*throttle

	// contribMu guards the open contribution sessions (origin side).
	contribMu sync.Mutex
	contribs  map[string]contribSession

	// subsSnap is the in-memory copy of the subscriptions (subsnap.go).
	subsSnap subsSnapshot
}

type cachedManifest struct {
	m       *Manifest
	builtAt time.Time
	rev     int64
	path    string
}

// New wires the service into the Files manager's hooks.
func New(fm *files.Manager, fs *fed.Service, st *store.Store, log *slog.Logger) *Service {
	s := &Service{Files: fm, Fed: fs, Store: st, Log: log, manifests: map[int64]*cachedManifest{}, manifestGen: map[int64]uint64{}, syncing: map[int64]*runningSync{}}
	fm.OnChange = s.onChange
	fm.Annotators = append(fm.Annotators, s.annotate)
	fm.Guards = append(fm.Guards, s.Guard)
	fm.BackupSkip = s.BackupSkip
	return s
}

// ---- lens ----

// ValidateShare checks a lens for legality and normalizes it.
func ValidateShare(sh *store.FileFedShare) error {
	sh.Title = strings.TrimSpace(sh.Title)
	if sh.Title == "" {
		_, sh.Title = files.Split(sh.Path)
		if sh.Title == "" {
			sh.Title = "Files"
		}
	}
	if len(sh.Title) > 120 {
		return fmt.Errorf("%w: title too long", ErrBadLens)
	}
	if sh.Mode == "" {
		sh.Mode = ModeRead
	}
	if sh.Scope == "" {
		sh.Scope = ScopeAll
	}
	if sh.Detail == "" {
		sh.Detail = DetailFull
	}
	if sh.Apps == "" {
		sh.Apps = AppsAllowed
	}
	// Mode and scope are the shared consent-grammar vocabulary, so the
	// shared list decides; detail and apps are Files' own words.
	if !fed.LensValid("mode", sh.Mode) {
		return fmt.Errorf("%w: mode must be read or contribute", ErrBadLens)
	}
	if !fed.LensValid("scope", sh.Scope) {
		return fmt.Errorf("%w: scope must be all, picked, or filtered", ErrBadLens)
	}
	switch sh.Detail {
	case DetailFull, DetailPreviews, DetailListing:
	default:
		return fmt.Errorf("%w: detail must be full, previews, or listing", ErrBadLens)
	}
	switch sh.Apps {
	case AppsAllowed, AppsDenied:
	default:
		return fmt.Errorf("%w: apps must be allowed or denied", ErrBadLens)
	}
	if sh.Mode == ModeContribute && sh.Scope != ScopeAll {
		return fmt.Errorf("%w: a contribute share covers its whole folder", ErrBadLens)
	}
	if sh.Since != "" {
		if _, err := time.Parse(time.RFC3339, sh.Since); err != nil {
			return fmt.Errorf("%w: since must be an RFC3339 time", ErrBadLens)
		}
	}
	if sh.Scope == ScopePicked && len(sh.Picks) == 0 {
		return fmt.Errorf("%w: a picked share needs at least one pick", ErrBadLens)
	}
	if sh.Scope != ScopePicked {
		sh.Picks = []string{}
	}
	if sh.Scope != ScopeFiltered {
		sh.Filters = []string{}
	} else if len(sh.Filters) == 0 {
		return fmt.Errorf("%w: a filtered share needs at least one filter", ErrBadLens)
	}
	for i, p := range sh.Picks {
		rel, err := files.CleanRel(p)
		if err != nil || rel == "" {
			return fmt.Errorf("%w: pick %q", ErrBadLens, p)
		}
		sh.Picks[i] = rel
	}
	for _, f := range sh.Filters {
		kind, arg, ok := strings.Cut(f, ":")
		if !ok || arg == "" {
			return fmt.Errorf("%w: filter %q must be kind:<category> or glob:<pattern>", ErrBadLens, f)
		}
		switch kind {
		case "kind":
			switch arg {
			case "image", "video", "audio", "document", "archive", "text":
			default:
				return fmt.Errorf("%w: unknown kind %q", ErrBadLens, arg)
			}
		case "glob":
			if _, err := path.Match(arg, "x"); err != nil {
				return fmt.Errorf("%w: bad glob %q", ErrBadLens, arg)
			}
		default:
			return fmt.Errorf("%w: filter %q", ErrBadLens, f)
		}
	}
	return nil
}

// lensChanged reports whether an update moves the lens (revision bumps)
// rather than only its picks.
func lensChanged(old, new *store.FileFedShare) bool {
	if old.Mode != new.Mode || old.Scope != new.Scope || old.Detail != new.Detail || old.Since != new.Since || old.Apps != new.Apps {
		return true
	}
	if strings.Join(old.Filters, "\x00") != strings.Join(new.Filters, "\x00") {
		return true
	}
	return false
}

// ---- the floor ----

type floor struct {
	kept    []string // keep-on-this-box paths (root-relative)
	folders []string // subscription folders: replicas, never re-shared
}

func (s *Service) floorSet() floor {
	var f floor
	f.kept, _ = s.Store.ListFileShareExcludes()
	subs, _ := s.subscriptions()
	for _, sub := range subs {
		f.folders = append(f.folders, sub.Folder)
	}
	return f
}

// under is files.IsUnder except at the drive root: here an empty dir holds
// nothing but itself, which the lens checks (a share's root inner path is
// "") rely on.
func under(rel, dir string) bool {
	return rel == dir || strings.HasPrefix(rel, dir+"/")
}

// underFold is under without regard to case: on a case-insensitive disk
// "Taxes/a.pdf" and "taxes/A.PDF" are one file, so what the floor holds
// back is held back however a peer spells it. (On a case-sensitive disk
// this holds back a little more than it must, never less.)
func underFold(rel, dir string) bool {
	return under(strings.ToLower(rel), strings.ToLower(dir))
}

// blocks reports whether rel (root-relative) is on the floor, and why.
func (f floor) blocks(rel string) (bool, string) {
	for _, k := range f.kept {
		if underFold(rel, k) {
			return true, "kept"
		}
	}
	for _, d := range f.folders {
		if underFold(rel, d) || underFold(d, rel) {
			return true, "replica"
		}
	}
	return false, ""
}

func (f floor) keptCount(sharePath string) int {
	n := 0
	for _, k := range f.kept {
		if under(k, sharePath) {
			n++
		}
	}
	return n
}

// ---- shares ----

// CreateShare validates the lens, refuses replicas, and records the share.
func (s *Service) CreateShare(sh *store.FileFedShare) (*store.FileFedShare, error) {
	rel, err := files.CleanRel(sh.Path)
	if err != nil {
		return nil, err
	}
	sh.Path = rel
	if err := ValidateShare(sh); err != nil {
		return nil, err
	}
	if _, err := s.Store.FedPeerByID(sh.PeerID); err != nil {
		return nil, ErrNoPeer
	}
	info, err := s.Files.Lstat(rel)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, files.ErrNotDirectory
	}
	if blocked, why := s.floorSet().blocks(rel); blocked && why == "replica" {
		return nil, ErrIsReplica
	}
	out, err := s.Store.CreateFileFedShare(sh)
	if err != nil {
		return nil, err
	}
	// A share is only reachable if the peer holds the files.share grant.
	// Writing it here — not only in the settings UI — keeps a share made
	// through the API, MCP, or any other client from being one the peer is
	// silently refused. The grant config is always "{}"; only the use side
	// carries config, so this never clobbers a meaningful value.
	if _, granted, _ := s.Store.FedCapabilityConfig(sh.PeerID, CapFilesShare, store.DirGrant); !granted {
		if err := s.Store.SetFedCapability(sh.PeerID, CapFilesShare, store.DirGrant, "{}"); err != nil {
			return nil, err
		}
	}
	s.scheduleNudge()
	return out, nil
}

// UpdateShare rewrites a lens; a change to the lens bumps the revision.
func (s *Service) UpdateShare(sh *store.FileFedShare) (*store.FileFedShare, error) {
	old, err := s.Store.FileFedShareByID(sh.ID)
	if err != nil {
		return nil, err
	}
	sh.Path, sh.PeerID = old.Path, old.PeerID
	if err := ValidateShare(sh); err != nil {
		return nil, err
	}
	if err := s.Store.UpdateFileFedShare(sh, lensChanged(old, sh)); err != nil {
		return nil, err
	}
	s.invalidateManifest(sh.ID)
	s.scheduleNudge()
	return s.Store.FileFedShareByID(sh.ID)
}

// DeleteShare stops sharing: the peer is told "revoked" now, best effort.
//
// What that peer contributed through the share turns to stubs first, as on
// unpair. Its provenance rows go with the share, and a contribution's bytes
// left here without one would read as this household's own file — served
// in full to every other peer the folder is shared with, past "bytes cross
// only over the author's own peerings".
func (s *Service) DeleteShare(ctx context.Context, id int64) error {
	sh, err := s.Store.FileFedShareByID(id)
	if err != nil {
		return err
	}
	s.withdrawToStubs(sh.PeerID, []int64{id})
	if err := s.Store.DeleteFileFedShare(id); err != nil {
		return err
	}
	s.invalidateManifest(id)
	if peer, err := s.Store.FedPeerByID(sh.PeerID); err == nil {
		// Best effort, off the request path: a peer that is away learns on
		// its next pull when the manifest is gone.
		go s.sendEvent(context.WithoutCancel(ctx), peer, []int64{id}, "revoked")
	}
	return nil
}

// GrantWithdrawn is the operator turning the files.share grant off for a
// peer: every share to it stops, exactly as if each were stopped by hand —
// the peer told "revoked", contributions through them turned to stubs, the
// rows gone. Leaving the rows would refuse the peer for now and hand every
// one of them back the moment any new share re-wrote the grant.
func (s *Service) GrantWithdrawn(ctx context.Context, peer *store.FedPeer) error {
	shares, err := s.Store.ListFileFedShares(peer.ID)
	if err != nil {
		return err
	}
	for _, sh := range shares {
		if err := s.DeleteShare(ctx, sh.ID); err != nil {
			return err
		}
	}
	return nil
}

// ShareView is a share with the facts the page shows beside it.
type ShareView struct {
	store.FileFedShare
	Peer   string `json:"peer"`
	Impact string `json:"impact"`
	Files  int64  `json:"files"`
	Bytes  int64  `json:"bytes"`
}

// ListShares returns every share (or one peer's) with impact sentences.
func (s *Service) ListShares(peerID int64) ([]ShareView, error) {
	shares, err := s.Store.ListFileFedShares(peerID)
	if err != nil {
		return nil, err
	}
	out := make([]ShareView, 0, len(shares))
	for _, sh := range shares {
		out = append(out, s.view(sh))
	}
	return out, nil
}

// View is one share with its impact sentence and counts.
func (s *Service) View(sh store.FileFedShare) ShareView { return s.view(sh) }

// peerName is a peer's display name, or fallback when it is no longer
// paired.
func (s *Service) peerName(id int64, fallback string) string {
	if p, err := s.Store.FedPeerByID(id); err == nil {
		return p.DisplayName()
	}
	return fallback
}

func (s *Service) view(sh store.FileFedShare) ShareView {
	v := ShareView{FileFedShare: sh, Peer: s.peerName(sh.PeerID, "")}
	if m, err := s.Manifest(&sh); err == nil {
		v.Files, v.Bytes = m.files, m.bytes
	}
	v.Impact = s.impact(&sh, v.Peer, v.Files, s.floorSet())
	return v
}

// impact renders the lens as one true sentence — the consent artifact.
// fl is the floor the caller already read, for the kept count.
func (s *Service) impact(sh *store.FileFedShare, peer string, n int64, fl floor) string {
	if peer == "" {
		peer = "the peer"
	}
	var b strings.Builder
	what := "the files"
	if n > 0 {
		what = fmt.Sprintf("%d file%s", n, plural(n))
	}
	switch sh.Scope {
	case ScopePicked:
		what = fmt.Sprintf("%d hand-picked entr%s", len(sh.Picks), map[bool]string{true: "y", false: "ies"}[len(sh.Picks) == 1])
	case ScopeFiltered:
		what = "only " + describeFilters(sh.Filters)
		if n > 0 {
			what = fmt.Sprintf("%d file%s — %s —", n, plural(n), describeFilters(sh.Filters))
		}
	}
	detail := map[string]string{
		DetailFull:     "full copies",
		DetailPreviews: "previews (names, sizes and thumbnails; a file is fetched when opened)",
		DetailListing:  "a listing (names, sizes and dates only)",
	}[sh.Detail]
	fmt.Fprintf(&b, "%s sees %s in %s as %s", peer, what, sh.Title, detail)
	if sh.Since != "" {
		if t, err := time.Parse(time.RFC3339, sh.Since); err == nil {
			fmt.Fprintf(&b, ", going forward from %s", t.Format("2 Jan 2006"))
		}
	}
	if sh.Apps == AppsAllowed {
		fmt.Fprintf(&b, " — and may mount them for apps on %s's fog.", peer)
	} else {
		b.WriteString(" — for people, not for apps there.")
	}
	if sh.Mode == ModeContribute {
		b.WriteString(" They can add files of their own; each side keeps its own.")
	} else {
		b.WriteString(" They cannot add files.")
	}
	if k := fl.keptCount(sh.Path); k > 0 {
		fmt.Fprintf(&b, " Never shared: %d entr%s marked keep on this box.", k, map[bool]string{true: "y", false: "ies"}[k == 1])
	}
	return b.String()
}

func plural(n int64) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func describeFilters(fs []string) string {
	var parts []string
	for _, f := range fs {
		kind, arg, _ := strings.Cut(f, ":")
		if kind == "kind" {
			parts = append(parts, map[string]string{"image": "photos", "video": "videos", "audio": "audio", "document": "documents", "archive": "archives", "text": "text files"}[arg])
		} else {
			parts = append(parts, "entries matching "+arg)
		}
	}
	return strings.Join(parts, " and ")
}

// ---- the manifest ----

// ManifestEntry is one entry as the peer sees it: paths relative to the
// share folder, never the origin's.
type ManifestEntry struct {
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	Size   int64  `json:"size,omitempty"`
	Mtime  string `json:"mtime"`
	SHA256 string `json:"sha256,omitempty"`
	Mime   string `json:"mime,omitempty"`
	// Origin is the author instance for a contributed file at this origin;
	// empty for the origin's own files.
	Origin string `json:"origin,omitempty"`
	// OriginShare is the share the author contributed through at this
	// origin: with the origin's instance id, how the author's fog finds
	// the file when a third subscriber asks it over their own peering.
	OriginShare int64 `json:"originShare,omitempty"`
	// Stub: the author unpaired; the listing remains, the bytes do not.
	Stub bool `json:"stub,omitempty"`
}

type Manifest struct {
	Revision int64           `json:"revision"`
	Mode     string          `json:"mode"`
	Detail   string          `json:"detail"`
	Apps     string          `json:"apps"`
	Impact   string          `json:"impact"`
	Entries  []ManifestEntry `json:"entries"`
	ETag     string          `json:"etag"`

	files, bytes int64
}

func (s *Service) invalidateManifest(ids ...int64) {
	s.manifestMu.Lock()
	for _, id := range ids {
		delete(s.manifests, id)
		s.manifestGen[id]++
	}
	s.manifestMu.Unlock()
}

// Manifest applies the lens to the folder and returns what crosses.
//
// The cache does not look at the share's dirty flag: every change that
// sets it drops the cached manifest itself (onChange, UpdateShare), while
// the flag stays set until the next nudge goes out — up to the debounce
// window, during which every catalog and share page would otherwise
// rebuild from a full walk. Revision and path are still compared because
// a caller may hold a share row read before an edit, or before a rename
// above the share (which moves the row without a change inside the
// folder). Concurrent callers share one build.
func (s *Service) Manifest(sh *store.FileFedShare) (*Manifest, error) {
	s.manifestMu.Lock()
	if c, ok := s.manifests[sh.ID]; ok && c.rev == sh.Revision && c.path == sh.Path && time.Since(c.builtAt) < manifestTTL {
		s.manifestMu.Unlock()
		return c.m, nil
	}
	gen := s.manifestGen[sh.ID]
	s.manifestMu.Unlock()
	key := fmt.Sprintf("%d/%d/%d/%s", sh.ID, gen, sh.Revision, sh.Path)
	v, err, _ := s.manifestFlight.Do(key, func() (any, error) {
		manifestBuilding(sh.ID)
		m, err := s.buildManifest(sh)
		if err != nil {
			return nil, err
		}
		s.manifestMu.Lock()
		if s.manifestGen[sh.ID] == gen {
			s.manifests[sh.ID] = &cachedManifest{m: m, builtAt: time.Now(), rev: sh.Revision, path: sh.Path}
		}
		s.manifestMu.Unlock()
		return m, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*Manifest), nil
}

func (s *Service) buildManifest(sh *store.FileFedShare) (*Manifest, error) {
	fl := s.floorSet()
	origins := map[string]store.FileFedOrigin{}
	var stubs []store.FileFedOrigin
	if list, err := s.Store.ListFileFedOrigins(); err == nil {
		listings := map[string][]string{}
		for _, o := range list {
			// Only this share's: another share's contributions are no part
			// of this manifest.
			if !underFold(o.Path, sh.Path) {
				continue
			}
			// Keyed by the path as the disk spells it, which is how the
			// walk below names the file: looked up with one directory read
			// per folder per build, however many records there are.
			key := o.Path
			if canon, ok := s.canonicalSpellingIn(o.Path, listings); ok {
				key = canon
			}
			origins[key] = o
			if o.Withdrawn && under(o.Path, sh.Path) && o.Path != sh.Path {
				stubs = append(stubs, o)
			}
		}
	}
	var since time.Time
	if sh.Since != "" {
		since, _ = time.Parse(time.RFC3339, sh.Since)
	}
	m := &Manifest{Revision: sh.Revision, Mode: sh.Mode, Detail: sh.Detail, Apps: sh.Apps, Entries: []ManifestEntry{}}
	h := sha256.New()
	fmt.Fprintf(h, "rev=%d\n", sh.Revision)
	indexed := map[string]map[string]files.IndexedFile{} // parent dir → its files' digests
	var unhashed []string                                // dirs with a file lacking one
	queued := map[string]bool{}                          // unhashed as a set
	hashBudget := manifestHashBudget
	err := s.Files.WalkTree(sh.Path, func(rel string, fi fs.FileInfo) error {
		if rel == sh.Path {
			return nil
		}
		if blocked, _ := fl.blocks(rel); blocked {
			if fi.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		inner := strings.TrimPrefix(rel, sh.Path+"/")
		if fi.IsDir() {
			if !s.dirInLens(sh, inner) {
				return fs.SkipDir
			}
			m.Entries = append(m.Entries, ManifestEntry{Path: inner, Kind: "dir", Mtime: fi.ModTime().UTC().Format(time.RFC3339)})
			return nil
		}
		if !fi.Mode().IsRegular() {
			return nil
		}
		if !s.fileInLens(sh, inner, fi, since) {
			return nil
		}
		e := ManifestEntry{Path: inner, Kind: "file", Size: fi.Size(), Mtime: fi.ModTime().UTC().Format(time.RFC3339), Mime: files.MimeFor(fi.Name()), Origin: origins[rel].InstanceID, OriginShare: origins[rel].ShareID}
		if sh.Detail != DetailListing {
			// The digest is what the receiver verifies against and keys its
			// thumbnail cache on. One the index doesn't have yet is hashed
			// here only within a small budget — a large file (or a folder of
			// new ones) would otherwise hash inside the peer's request — and
			// otherwise queued: the entry ships without one, the receiver
			// takes the digest the blob is signed with, and a later manifest
			// carries it.
			parent, name := files.Split(rel)
			known, ok := indexed[parent]
			if !ok {
				known, _ = s.Files.IndexedDir(parent)
				indexed[parent] = known
			}
			if e.SHA256 = known[name].HashFor(fi); e.SHA256 == "" && fi.Size() <= hashBudget {
				if sha, err := s.Files.HashNow(rel); err == nil {
					e.SHA256 = sha
					hashBudget -= fi.Size()
				}
			}
			if e.SHA256 == "" && !queued[parent] {
				queued[parent] = true
				unhashed = append(unhashed, parent)
			}
		}
		m.Entries = append(m.Entries, e)
		m.files++
		m.bytes += fi.Size()
		fmt.Fprintf(h, "%s\x00%d\x00%s\x00%s\n", e.Path, e.Size, e.Mtime, e.SHA256)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(unhashed) > 0 {
		go s.Files.QueueHashes(unhashed)
	}
	// Withdrawn contributions: listed, not fetchable, by anyone.
	for _, o := range stubs {
		if blocked, _ := fl.blocks(o.Path); blocked {
			continue
		}
		inner := strings.TrimPrefix(o.Path, sh.Path+"/")
		if !s.dirInLens(sh, path.Dir(inner)) && path.Dir(inner) != "." {
			continue
		}
		// A stub is an entry like any other: the lens — picks, filters,
		// since — decides whether this peer sees it at all.
		at, _ := time.Parse(time.RFC3339, o.ContributedAt)
		if !s.nameInLens(sh, inner, path.Base(inner), at, since) {
			continue
		}
		e := ManifestEntry{Path: inner, Kind: "file", Size: o.Size, Mtime: o.ContributedAt, Mime: files.MimeFor(path.Base(inner)), Origin: o.InstanceID, OriginShare: o.ShareID, Stub: true}
		m.Entries = append(m.Entries, e)
		m.files++
		fmt.Fprintf(h, "%s\x00%d\x00stub\n", e.Path, e.Size)
	}
	// A filter or a since date narrows which files cross; the folder tree
	// around the ones that don't is not theirs to see either. Only folders
	// leading to an entry that crosses are listed.
	if sh.Scope == ScopeFiltered || !since.IsZero() {
		m.Entries = foldersLeadingToFiles(m.Entries)
	}
	sort.Slice(m.Entries, func(i, j int) bool { return m.Entries[i].Path < m.Entries[j].Path })
	m.ETag = `"` + hex.EncodeToString(h.Sum(nil))[:32] + `"`
	if peer, err := s.Store.FedPeerByID(sh.PeerID); err == nil {
		m.Impact = s.impact(sh, peer.DisplayName(), m.files, fl)
	}
	return m, nil
}

// foldersLeadingToFiles drops every folder entry that no file entry is
// under.
func foldersLeadingToFiles(entries []ManifestEntry) []ManifestEntry {
	lead := map[string]bool{}
	for _, e := range entries {
		if e.Kind == "dir" {
			continue
		}
		for d := path.Dir(e.Path); d != "." && d != "/" && !lead[d]; d = path.Dir(d) {
			lead[d] = true
		}
	}
	out := entries[:0]
	for _, e := range entries {
		if e.Kind != "dir" || lead[e.Path] {
			out = append(out, e)
		}
	}
	return out
}

// dirInLens: for a picked share, a directory is kept if it is a pick, is
// inside one, or leads to one; otherwise every directory is kept.
func (s *Service) dirInLens(sh *store.FileFedShare, inner string) bool {
	if sh.Scope != ScopePicked {
		return true
	}
	for _, p := range sh.Picks {
		if under(inner, p) || under(p, inner) {
			return true
		}
	}
	return false
}

func (s *Service) fileInLens(sh *store.FileFedShare, inner string, fi fs.FileInfo, since time.Time) bool {
	return s.nameInLens(sh, inner, fi.Name(), fi.ModTime(), since)
}

// nameInLens is fileInLens for an entry known by name and time alone — a
// withdrawn contribution's stub has no bytes to stat.
func (s *Service) nameInLens(sh *store.FileFedShare, inner, name string, mtime, since time.Time) bool {
	if !since.IsZero() && mtime.Before(since) {
		return false
	}
	switch sh.Scope {
	case ScopePicked:
		for _, p := range sh.Picks {
			if under(inner, p) {
				return true
			}
		}
		return false
	case ScopeFiltered:
		for _, f := range sh.Filters {
			kind, arg, _ := strings.Cut(f, ":")
			if kind == "glob" {
				if ok, _ := path.Match(arg, name); ok {
					return true
				}
			} else if mimeCategory(files.MimeFor(name)) == arg {
				return true
			}
		}
		return false
	}
	return true
}

func mimeCategory(mime string) string {
	switch {
	case strings.HasPrefix(mime, "image/"):
		return "image"
	case strings.HasPrefix(mime, "video/"):
		return "video"
	case strings.HasPrefix(mime, "audio/"):
		return "audio"
	case strings.HasPrefix(mime, "text/"):
		return "text"
	}
	switch mime {
	case "application/pdf", "application/epub+zip", "application/msword", "application/vnd.oasis.opendocument.text",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		"application/vnd.openxmlformats-officedocument.presentationml.presentation":
		return "document"
	case "application/zip", "application/x-tar", "application/gzip", "application/x-7z-compressed", "application/vnd.rar", "application/x-xz", "application/zstd":
		return "archive"
	}
	return "other"
}

// InLens reports whether one entry (inside the share folder) crosses.
func (s *Service) InLens(sh *store.FileFedShare, inner string) (fs.FileInfo, bool) {
	rel := files.Join(sh.Path, inner)
	fi, err := s.Files.Lstat(rel)
	if err != nil || !fi.Mode().IsRegular() {
		return nil, false
	}
	// Only the name the file has: on a case-insensitive disk another
	// spelling opens the same file, but would miss every check made by
	// name — a contribution's author, a picked folder, a filter.
	if !s.spelledAsOnDisk(rel) {
		return nil, false
	}
	if blocked, _ := s.floorSet().blocks(rel); blocked {
		return nil, false
	}
	// Every ancestor must be in the lens too (a picked share).
	for d := path.Dir(inner); d != "." && d != ""; d = path.Dir(d) {
		if !s.dirInLens(sh, d) {
			return nil, false
		}
	}
	var since time.Time
	if sh.Since != "" {
		since, _ = time.Parse(time.RFC3339, sh.Since)
	}
	if !s.fileInLens(sh, inner, fi, since) {
		return nil, false
	}
	return fi, true
}

// spelledAsOnDisk reports whether every component of rel is spelled as
// the directory holding it spells it. The index (exact, by name) answers
// for what it has seen; a miss is settled by reading the directories.
func (s *Service) spelledAsOnDisk(rel string) bool {
	if n, err := s.Files.Node(rel); err == nil && n.Path == rel {
		return true
	}
	canon, exists := s.canonicalSpelling(rel)
	return exists && canon == rel
}

// canonicalSpelling is rel as the disk spells it: each component that
// exists takes the name its directory holds for it — exactly, or else the
// one name equal to it but for case (what a case-insensitive disk opens);
// from the first that doesn't exist, the rest is rel's own. exists says
// whether the whole path does.
func (s *Service) canonicalSpelling(rel string) (string, bool) {
	return s.canonicalSpellingIn(rel, nil)
}

// canonicalSpellingIn is canonicalSpelling reading each directory at most
// once into listings — one manifest build's worth of lookups costs one read
// per folder, not one per lookup.
func (s *Service) canonicalSpellingIn(rel string, listings map[string][]string) (string, bool) {
	parts := strings.Split(rel, "/")
	dir := ""
	for i, name := range parts {
		names, cached := listings[dir]
		if !cached {
			// By descriptor, no link followed: a folder an app swapped for
			// a link spells nothing.
			var err error
			if names, err = s.Files.DirNames(dir); err != nil {
				names = nil
			}
			if listings != nil {
				listings[dir] = names
			}
		}
		if names == nil {
			return strings.Join(parts, "/"), false
		}
		found := slices.Contains(names, name)
		if !found {
			for _, n := range names {
				if strings.EqualFold(n, name) {
					parts[i], found = n, true
					break
				}
			}
		}
		if !found {
			return strings.Join(parts, "/"), false
		}
		dir = files.Join(dir, parts[i])
	}
	return strings.Join(parts, "/"), true
}

// originFor finds the contribution record for the file at rel: by its
// path, or — a record made under another spelling of the same file on a
// case-insensitive disk — by the file's identity, so another spelling
// never strips a file of its author.
func (s *Service) originFor(rel string) (*store.FileFedOrigin, error) {
	if o, err := s.Store.FileFedOriginByPath(rel); err == nil {
		return o, nil
	}
	if s.Files == nil {
		return nil, store.ErrNotFound // no drive: no file to be the same as
	}
	here, err := s.Files.Lstat(rel)
	if err != nil {
		return nil, store.ErrNotFound
	}
	list, err := s.Store.ListFileFedOrigins()
	if err != nil {
		return nil, err
	}
	for i := range list {
		o := list[i]
		if !strings.EqualFold(o.Path, rel) || o.Path == rel {
			continue
		}
		if there, err := s.Files.Lstat(o.Path); err == nil && sameFile(here, there) {
			return &o, nil
		}
	}
	return nil, store.ErrNotFound
}

// CanServe says whether one file of a share may go to this peer, in any
// form — its bytes or a preview: it has to be inside the share's lens
// (files.ErrNotFound when not), and crossable to this peer.
func (s *Service) CanServe(peer *store.FedPeer, sh *store.FileFedShare, inner string) error {
	if _, ok := s.InLens(sh, inner); !ok {
		return files.ErrNotFound
	}
	return s.crossable(peer, files.Join(sh.Path, inner))
}

// OpenBlob opens one shared file for serving with its digest.
func (s *Service) OpenBlob(peer *store.FedPeer, sh *store.FileFedShare, inner string) (*os.File, files.Node, string, error) {
	if err := s.CanServe(peer, sh, inner); err != nil {
		return nil, files.Node{}, "", err
	}
	rel := files.Join(sh.Path, inner)
	sha, err := s.Files.HashNow(rel)
	if err != nil {
		return nil, files.Node{}, "", err
	}
	f, n, err := s.Files.OpenFile(rel)
	if err != nil {
		return nil, files.Node{}, "", err
	}
	return f, n, sha, nil
}

// ---- catalog ----

// CatalogEntry is a share as its peer sees it: no paths, just the offer.
type CatalogEntry struct {
	ID        int64  `json:"id"`
	Title     string `json:"title"`
	Mode      string `json:"mode" ts:"\"read\" | \"contribute\""`
	Detail    string `json:"detail" ts:"\"full\" | \"previews\" | \"listing\""`
	Apps      string `json:"apps" ts:"\"allowed\" | \"denied\""`
	Since     string `json:"since,omitempty"`
	Revision  int64  `json:"revision"`
	Impact    string `json:"impact"`
	Files     int64  `json:"files"`
	Bytes     int64  `json:"bytes"`
	UpdatedAt string `json:"updatedAt"`
}

func (s *Service) Catalog(peer *store.FedPeer) ([]CatalogEntry, error) {
	shares, err := s.Store.ListFileFedShares(peer.ID)
	if err != nil {
		return nil, err
	}
	out := make([]CatalogEntry, 0, len(shares))
	for _, sh := range shares {
		e := CatalogEntry{ID: sh.ID, Title: sh.Title, Mode: sh.Mode, Detail: sh.Detail, Apps: sh.Apps, Since: sh.Since, Revision: sh.Revision, UpdatedAt: sh.UpdatedAt}
		if m, err := s.Manifest(&sh); err == nil {
			e.Files, e.Bytes, e.Impact = m.files, m.bytes, m.Impact
		} else {
			e.Impact = s.impact(&sh, peer.DisplayName(), 0, s.floorSet())
		}
		out = append(out, e)
	}
	return out, nil
}

// ShareForPeer resolves a share id the peer presented, refusing another
// peer's share.
func (s *Service) ShareForPeer(peer *store.FedPeer, id int64) (*store.FileFedShare, error) {
	sh, err := s.Store.FileFedShareByID(id)
	if err != nil || sh.PeerID != peer.ID {
		return nil, ErrNotShared
	}
	return sh, nil
}

// ---- change tracking and nudges ----

func (s *Service) onChange(rel string) {
	if sub, _, ok := s.subscriptionFor(rel); ok && sub.Mode == ModeContribute && sub.Status == StatusActive {
		s.SyncSoon(sub.ID)
	}
	s.notifyApps(rel, "changed")
	if ids, err := s.Store.MarkFileFedSharesDirty(rel); err == nil && len(ids) > 0 {
		s.invalidateManifest(ids...)
		s.scheduleNudge()
	}
}

func (s *Service) scheduleNudge() {
	s.nudgeMu.Lock()
	s.nudgeDue = true
	s.nudgeMu.Unlock()
}

// Event is the nudge body.
type Event struct {
	ShareIDs []int64 `json:"shareIds"`
	Event    string  `json:"event"` // changed | revoked
}

func (s *Service) sendEvent(ctx context.Context, peer *store.FedPeer, ids []int64, event string) {
	if peer.URL == "" || peer.UnpairedAt != "" {
		return
	}
	body, _ := json.Marshal(Event{ShareIDs: ids, Event: event})
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if _, err := s.Fed.SignedPost(ctx, peer, "/fed/v1/files/events", "application/json", body); err != nil {
		s.Log.Debug("files nudge", "peer", peer.Name, "event", event, "err", err)
	}
}

// FlushNudges delivers pending "changed" nudges, once per debounce window.
func (s *Service) FlushNudges(ctx context.Context) {
	s.nudgeMu.Lock()
	due := s.nudgeDue
	s.nudgeDue = false
	s.nudgeMu.Unlock()
	if !due {
		return
	}
	byPeer, err := s.Store.TakeDirtyFileFedShares()
	if err != nil {
		return
	}
	for peerID, ids := range byPeer {
		peer, err := s.Store.FedPeerByID(peerID)
		if err != nil {
			continue
		}
		if _, granted, _ := s.Store.FedCapabilityConfig(peer.ID, CapFilesShare, store.DirGrant); !granted {
			continue
		}
		s.sendEvent(ctx, peer, ids, "changed")
	}
}

// ---- annotations on listings ----

func (s *Service) annotate(nodes []files.Node) {
	if len(nodes) == 0 {
		return
	}
	kept, _ := s.Store.ListFileShareExcludes()
	shares, _ := s.Store.ListFileFedShares(0)
	subs, _ := s.subscriptions()
	origins, _ := s.Store.ListFileFedOrigins()
	peerLabel := map[int64]string{}
	peerInstance := map[int64]string{}
	// Replica rows for just the files on this page, one query per
	// subscription they fall in — not every row the subscription holds.
	subOf := make([]int, len(nodes)) // index into subs, -1 for none
	wanted := map[int][]string{}
	for i, n := range nodes {
		subOf[i] = -1
		for j, sub := range subs {
			if under(n.Path, sub.Folder) {
				subOf[i] = j
				if n.Kind == "file" && n.Path != sub.Folder {
					wanted[j] = append(wanted[j], strings.TrimPrefix(n.Path, sub.Folder+"/"))
				}
				break
			}
		}
	}
	replicas := map[int64]map[string]store.FileFedReplica{}
	for j, rels := range wanted {
		replicas[subs[j].ID], _ = s.Store.FileFedReplicasFor(subs[j].ID, rels)
	}
	self := s.Fed.InstanceID()
	authorName := map[string]string{} // instance → display name; "" when not a peer here
	authorLabel := func(instance string, via string) string {
		name, ok := authorName[instance]
		if !ok {
			if p, err := s.Store.FedPeerByInstanceID(instance); err == nil {
				name = p.DisplayName()
			}
			authorName[instance] = name
		}
		if name != "" {
			return name
		}
		return "someone " + via + " knows"
	}
	label := func(id int64) string {
		if l, ok := peerLabel[id]; ok {
			return l
		}
		if p, err := s.Store.FedPeerByID(id); err == nil {
			peerLabel[id], peerInstance[id] = p.DisplayName(), p.InstanceID
		} else {
			peerLabel[id] = "?"
		}
		return peerLabel[id]
	}
	for i := range nodes {
		n := &nodes[i]
		for _, k := range kept {
			if under(n.Path, k) {
				n.Kept = true
				if k != n.Path {
					n.KeptBy = k
				}
				break
			}
		}
		for _, sh := range shares {
			if under(n.Path, sh.Path) {
				l := label(sh.PeerID)
				dup := false
				for _, x := range n.SharedWith {
					if x == l {
						dup = true
					}
				}
				if !dup {
					n.SharedWith = append(n.SharedWith, l)
				}
			}
		}
		if j := subOf[i]; j >= 0 {
			sub := subs[j]
			n.Peer = &files.PeerMark{Peer: label(sub.PeerID), Instance: peerInstance[sub.PeerID], Kind: "replica", ReadOnly: true, Subscription: sub.ID, Revoked: sub.Status == StatusRevoked, Backup: sub.Backup, Detail: sub.DetailWanted, Mode: sub.Mode}
			if n.Kind == "file" && n.Path != sub.Folder {
				if r, ok := replicas[sub.ID][strings.TrimPrefix(n.Path, sub.Folder+"/")]; ok {
					hyd := r.Hydrated
					n.Peer.Hydrated = &hyd
					n.Peer.Pinned = r.Pinned
					if r.Origin != "" && r.Origin == self {
						n.Peer.Mine, n.Peer.ReadOnly, n.Peer.Rejected = true, false, r.Rejected
					} else if r.Origin != "" && r.Origin != peerInstance[sub.PeerID] {
						n.Peer.Author = authorLabel(r.Origin, n.Peer.Peer)
						n.Peer.Stub = r.Stub && !r.Hydrated
					}
				} else if sub.Mode == ModeContribute {
					// Not yet pushed: ours, on its way up.
					n.Peer.Mine, n.Peer.ReadOnly = true, false
				}
			} else if n.Kind == "dir" && sub.Mode == ModeContribute {
				n.Peer.ReadOnly = false
			}
		}
		if n.Peer == nil {
			for _, o := range origins {
				if o.Path == n.Path {
					n.Peer = &files.PeerMark{Peer: label(o.PeerID), Instance: o.InstanceID, Kind: "contribution", ReadOnly: true, Stub: o.Withdrawn}
					break
				}
			}
		}
	}
}

// Loop flushes nudges on the debounce cadence.
func (s *Service) Loop(ctx context.Context) {
	loop.Every(ctx, nudgeDebounce, nudgeDebounce, s.FlushNudges)
}

// SetKept marks or unmarks an entry as keep on this box; a marked entry
// leaves every manifest at once and peers are nudged.
func (s *Service) SetKept(rel string, kept bool) error {
	if rel == "" {
		return files.ErrBadName
	}
	var err error
	if kept {
		err = s.Store.AddFileShareExclude(rel)
	} else {
		err = s.Store.RemoveFileShareExclude(rel)
	}
	if err != nil {
		return err
	}
	s.onChange(rel)
	return nil
}

// HandleEvent is a peer's nudge. The receiving half (sync.go) pulls on
// "changed" and marks the subscription on "revoked".
func (s *Service) HandleEvent(peer *store.FedPeer, ev Event) {
	s.handleEvent(peer, ev)
}

// ---- apps ----

// notifyApps forwards a folder change to the app-side notice, if wired.
func (s *Service) notifyApps(rel, reason string) {
	if s.AppNotify != nil {
		s.AppNotify(rel, reason)
	}
}

// MountedBy names the apps mounting rel (or something inside it).
func (s *Service) MountedBy(rel string) []string { return s.Files.AppsMounting(rel) }

// MountPolicy rules on an app volume's host path at deploy time: a copy of
// a peer's share mounts read-only (the one-origin invariant holds inside
// the container too), and only when the lens allows apps and the copy is
// full — a previews subscription has nothing for a container to read.
func (s *Service) MountPolicy(hostPath string) (bool, error) {
	root := s.Files.Root()
	if root == "" {
		return false, nil
	}
	abs := filepath.Clean(hostPath)
	if !files.IsUnder(abs, root) {
		return false, nil
	}
	rel := strings.TrimPrefix(strings.TrimPrefix(abs, root), "/")
	sub, _, ok := s.subscriptionFor(rel)
	if !ok {
		if rel == "" || under(rel, PeersFolder) {
			// The drive, the Peers folder, or a peer's folder: a mount of it
			// carries every copy inside, so each of those is held to its own
			// lens — one shared for people and not for apps refuses the
			// whole mount, rather than riding in under its parent.
			subs, err := s.subscriptions()
			if err != nil {
				return false, err
			}
			for _, inner := range subs {
				if rel != "" && !under(inner.Folder, rel) {
					continue
				}
				if !inner.AppsAllowed {
					return false, fmt.Errorf("Files/%s holds %q from %s, shared for people, not for apps here; mount a folder that doesn't contain it",
						rel, inner.Title, s.peerName(inner.PeerID, "a peer"))
				}
			}
			if rel == "" {
				return false, nil
			}
			return true, nil
		}
		return false, nil
	}
	peer := s.peerName(sub.PeerID, "the peer")
	if !sub.AppsAllowed {
		return false, fmt.Errorf("%s shares %q for people, not for apps here", peer, sub.Title)
	}
	if sub.DetailWanted != DetailFull {
		return false, fmt.Errorf("%q from %s is a previews subscription; keep a full copy (Connect page) to mount it for apps", sub.Title, peer)
	}
	return true, nil
}

// StampSubscriptionViews fills the view-only fields on subscriptions.
func (s *Service) StampSubscriptionViews(subs []store.FileFedSubscription) {
	names := map[int64]string{}
	// The mounts once for the whole list: reading them parses every spec.
	mounts := s.Files.Mounts()
	for i := range subs {
		sub := &subs[i]
		if _, ok := names[sub.PeerID]; !ok {
			names[sub.PeerID] = s.peerName(sub.PeerID, "")
		}
		sub.Peer = names[sub.PeerID]
		sub.MountedBy = mounts.Apps(sub.Folder)
	}
}
