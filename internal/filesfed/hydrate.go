package filesfed

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"gitea.lan/matthew/fog/internal/fed"
	"gitea.lan/matthew/fog/internal/files"
	"gitea.lan/matthew/fog/internal/store"
)

// Detail on the receiving side. A previews subscription lists every entry
// the origin offers and holds none of them: the index row carries the
// origin's size, time and digest, listings show a cloud, and the bytes
// arrive when something opens the file — the page, a download, WebDAV, an
// app — or when the operator pins it ("keep offline"). What the page shows
// is exact; what the disk holds is what was asked for. Thumbnails for
// entries not held come from the origin, signed, into the same cache the
// local renderer uses.

var (
	ErrNotReplica  = errors.New("not a copy of a peer's file")
	ErrListingOnly = errors.New("the peer shares this as a listing; the file itself stays with them")
	ErrRevoked     = errors.New("the peer no longer shares this")
	ErrNoThumb     = errors.New("no thumbnail")

	// transfersAtOnce bounds concurrent blob fetches per pull.
	transfersAtOnce = 2
	// thumbMaxBytes bounds a thumbnail fetched from a peer.
	thumbMaxBytes = int64(4 << 20)
	// thumbMaxSide bounds a peer's thumbnail's dimensions.
	thumbMaxSide = 4096
)

// subscriptionFor finds the subscription whose folder holds rel and rel's
// path inside it ("" for the folder itself).
func (s *Service) subscriptionFor(rel string) (*store.FileFedSubscription, string, bool) {
	subs, err := s.subscriptions()
	if err != nil {
		return nil, "", false
	}
	for _, sub := range subs {
		if under(rel, sub.Folder) {
			return &sub, strings.TrimPrefix(strings.TrimPrefix(rel, sub.Folder), "/"), true
		}
	}
	return nil, "", false
}

// Hydrate fetches the bytes of one listed-only entry into place and
// reports whether it did. (false, nil) means rel is not such an entry —
// the caller's ordinary not-found handling applies. Concurrent opens of
// one file share a single fetch.
func (s *Service) Hydrate(ctx context.Context, rel string) (bool, error) {
	sub, inner, ok := s.subscriptionFor(rel)
	if !ok || inner == "" {
		return false, nil
	}
	rep, err := s.Store.FileFedReplica(sub.ID, inner)
	if err != nil || rep.Kind != "file" {
		return false, nil
	}
	if rep.Hydrated && s.Files.Exists(rel) {
		return false, nil
	}
	if sub.DetailOffered == DetailListing {
		return false, ErrListingOnly
	}
	if !sub.AppsAllowed && len(s.appsReaching(sub.Folder)) > 0 {
		return false, ErrReachedByApps
	}
	if sub.Status == StatusRevoked {
		return false, ErrRevoked
	}
	peer, err := s.Store.FedPeerByID(sub.PeerID)
	if err != nil {
		return false, err
	}
	s.hydrateMu.Lock()
	if s.hydrating == nil {
		s.hydrating = map[string]chan struct{}{}
	}
	if ch, busy := s.hydrating[rel]; busy {
		s.hydrateMu.Unlock()
		select {
		case <-ch:
			return s.Files.Exists(rel), nil
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	ch := make(chan struct{})
	s.hydrating[rel] = ch
	s.hydrateMu.Unlock()
	defer func() {
		s.hydrateMu.Lock()
		delete(s.hydrating, rel)
		s.hydrateMu.Unlock()
		close(ch)
	}()
	// The manifest's digest is a hint here, not a demand: the origin may
	// have rewritten the file since the last pull, and what it signs now
	// is what the operator asked to open.
	e := ManifestEntry{Path: inner, Kind: "file", Size: rep.Size, Mtime: time.Unix(0, rep.Mtime).UTC().Format(time.RFC3339), SHA256: rep.SHA256, Origin: rep.Origin, OriginShare: rep.OriginShare}
	if rep.Origin != "" && rep.Origin != peer.InstanceID && rep.Origin != s.Fed.InstanceID() {
		// A third party's contribution: only its author can send it.
		filled, err := s.fillStub(ctx, sub, peer, e, rel)
		if err != nil {
			return false, err
		}
		if !filled {
			return false, ErrStub
		}
		s.Store.SetFileFedReplicaState(sub.ID, inner, true, rep.Pinned)
		s.Files.Kick()
		return true, nil
	}
	e.SHA256 = ""
	// The size the last pull recorded is a ceiling: a file the origin has
	// since grown waits for the next pull, which checks it against the
	// quota, rather than streaming in unmeasured.
	// Nothing of the subscriber's may be at the name (on a case-insensitive
	// disk, a file saved under another spelling of it).
	if !s.placedHere(rel, *rep) {
		return false, fmt.Errorf("a file of yours is at %s; it was kept and nothing was fetched", path.Base(rel))
	}
	mt := time.Unix(0, rep.Mtime)
	s.beginPlacing(sub.ID, *rep, true, e, mt)
	sha, n, err := s.fetchBlob(ctx, s.Files, peer, sub.RemoteShareID, e, rel, nil, s.limiterFor(peer), mineFor(*rep, true))
	if errors.Is(err, files.ErrNotPlaced) {
		s.Store.DeleteFileFedReplica(sub.ID, inner)
		return false, fmt.Errorf("a file saved here is at %s, so it was kept and nothing was fetched: %w", path.Base(rel), err)
	}
	if err != nil {
		s.abandonPlacing(sub.ID, *rep, true, inner)
	}
	if errors.Is(err, errOversize) {
		s.SyncSoon(sub.ID)
		return false, fmt.Errorf("%s has grown since it was listed; it can be opened after the next sync", path.Base(rel))
	}
	if err != nil {
		return false, err
	}
	// Recorded as it is on the disk now.
	rep.SHA256, rep.Size, rep.Hydrated = sha, n, true
	s.recordPlaced(rep, rel)
	s.Store.UpsertFileFedReplica(*rep)
	s.Store.SetFileFedReplicaState(sub.ID, inner, true, rep.Pinned)
	s.addSubscriptionFetched(sub.ID, n)
	s.Files.Kick()
	s.notifyApps(rel, "hydrated")
	s.Log.Info("files fed hydrated", "path", rel, "peer", peer.DisplayName())
	return true, nil
}

// SetPinned keeps an entry (or every file in a folder) offline, or lets it
// go again. Pinning fetches; unpinning on a previews subscription frees the
// space, on a full one only clears the mark.
func (s *Service) SetPinned(ctx context.Context, rel string, pinned bool) error {
	sub, inner, ok := s.subscriptionFor(rel)
	if !ok {
		return ErrNotReplica
	}
	reps, err := s.Store.ListFileFedReplicas(sub.ID)
	if err != nil {
		return err
	}
	var targets []store.FileFedReplica
	for _, r := range reps {
		if r.Kind == "file" && files.IsUnder(r.Rel, inner) {
			targets = append(targets, r)
		}
	}
	if len(targets) == 0 {
		return ErrNotReplica
	}
	if pinned && sub.DetailOffered == DetailListing {
		return ErrListingOnly
	}
	var first error
	for _, r := range targets {
		full := files.Join(sub.Folder, r.Rel)
		if pinned {
			if _, err := s.Hydrate(ctx, full); err != nil {
				if first == nil {
					first = fmt.Errorf("%s: %w", r.Rel, err)
				}
				continue
			}
			s.Store.SetFileFedReplicaState(sub.ID, r.Rel, true, true)
			continue
		}
		hydrated := r.Hydrated
		if sub.DetailWanted != DetailFull && r.Hydrated {
			if err := s.Files.Dehydrate(full); err != nil && !errors.Is(err, files.ErrNotFound) {
				if first == nil {
					first = fmt.Errorf("%s: %w", r.Rel, err)
				}
				continue
			}
			hydrated = false
		}
		s.Store.SetFileFedReplicaState(sub.ID, r.Rel, hydrated, false)
	}
	s.Files.Kick()
	s.notifyApps(rel, "hydrated")
	return first
}

// dehydrateAll frees every unpinned copy in a subscription: the switch
// from a full copy to previews.
func (s *Service) dehydrateAll(sub *store.FileFedSubscription) error {
	reps, err := s.Store.ListFileFedReplicas(sub.ID)
	if err != nil {
		return err
	}
	var first error
	for _, r := range reps {
		if r.Kind != "file" || !r.Hydrated || r.Pinned {
			continue
		}
		if err := s.Files.Dehydrate(files.Join(sub.Folder, r.Rel)); err != nil && !errors.Is(err, files.ErrNotFound) {
			if first == nil {
				first = err
			}
			continue
		}
		s.Store.SetFileFedReplicaState(sub.ID, r.Rel, false, false)
	}
	return first
}

// dropDehydrated removes the listed-only rows of a subscription: on adopt
// they would otherwise outlive the thing that could fetch them.
func (s *Service) dropDehydrated(sub *store.FileFedSubscription) {
	reps, err := s.Store.ListFileFedReplicas(sub.ID)
	if err != nil {
		return
	}
	for _, r := range reps {
		rel := files.Join(sub.Folder, r.Rel)
		// Listed only: nothing on the disk, just the listing to drop. A
		// file there now is the subscriber's, and stays.
		if r.Kind == "file" && !r.Hydrated {
			if _, err := s.Files.Lstat(rel); errors.Is(err, files.ErrNotFound) {
				s.Files.RemovePermanently(rel)
			}
		}
	}
}

// Thumb serves a thumbnail for a file this box lists but does not hold:
// from the cache by the origin's digest, else fetched from the origin,
// signature-checked, and cached like a local render. The digest comes back
// as the thumbnail's identity; "" means it has none yet.
func (s *Service) Thumb(ctx context.Context, rel string, w int) (io.ReadSeekCloser, string, time.Time, error) {
	sub, inner, ok := s.subscriptionFor(rel)
	if !ok || inner == "" {
		return nil, "", time.Time{}, ErrNotReplica
	}
	rep, err := s.Store.FileFedReplica(sub.ID, inner)
	if err != nil || rep.Kind != "file" {
		return nil, "", time.Time{}, ErrNoThumb
	}
	mod := time.Unix(0, rep.Mtime)
	peer, err := s.Store.FedPeerByID(sub.PeerID)
	if err != nil {
		return nil, "", time.Time{}, err
	}
	// An entry listed before the origin had its digest has nothing to key
	// a cache on: its thumbnail is fetched each time, uncached, until a
	// pull brings the digest. The digest is the origin's claim, so the
	// cache key is scoped to the origin: a peer naming the digest of one of
	// this household's own files must not replace that file's thumbnail.
	key := remoteThumbKey(peer.InstanceID, rep.SHA256)
	if key != "" {
		if f, err := s.Files.CachedThumb(key, w); err == nil {
			return f, key, mod, nil
		}
	}
	if sub.DetailOffered == DetailListing || sub.Status == StatusRevoked {
		return nil, "", time.Time{}, ErrNoThumb
	}
	path := fmt.Sprintf("/fed/v1/files/shares/%d/thumb/%s", sub.RemoteShareID, escapePath(inner))
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := s.Fed.SignedDo(ctx, peer, "GET", path+"?w="+strconv.Itoa(files.SnapWidth(w)), "", nil)
	if err != nil {
		return nil, "", time.Time{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", time.Time{}, ErrNoThumb
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, thumbMaxBytes+1))
	if err != nil {
		return nil, "", time.Time{}, err
	}
	if int64(len(body)) > thumbMaxBytes {
		return nil, "", time.Time{}, ErrNoThumb
	}
	// A thumbnail is a small JPEG. Anything else — another format, or a
	// small file declaring a vast image for a browser to decode — is not
	// cached or served as one.
	if cfg, format, err := image.DecodeConfig(bytes.NewReader(body)); err != nil || format != "jpeg" ||
		cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > thumbMaxSide || cfg.Height > thumbMaxSide {
		return nil, "", time.Time{}, ErrNoThumb
	}
	// SignedDo handed back an answer already verified as the origin's, for
	// this request, and opened.
	if key != "" {
		s.Files.StoreThumb(key, w, body)
	}
	return bytesFile{bytes.NewReader(body)}, key, mod, nil
}

// remoteThumbKey is the thumbnail-cache key for a digest an origin
// asserted: a digest of its own, scoped to that origin, which no local
// file's content digest can collide with.
func remoteThumbKey(originInstance, sha string) string {
	if sha == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("fogfed-thumb\n" + originInstance + "\n" + sha))
	return hex.EncodeToString(sum[:])
}

type bytesFile struct{ *bytes.Reader }

func (bytesFile) Close() error { return nil }

// ---- the bandwidth cap ----

// A peer's cap lives in the config of our files.share "use" row:
// {"bandwidthBps": N}. Zero is uncapped. One bucket per peer, shared by
// every transfer from them, so two-at-a-time still adds up to the cap.
type throttle struct {
	mu  sync.Mutex
	bps int64
	lim *rate.Limiter // nil while uncapped
}

// set applies the current cap; the bucket holds at most a second of it.
func (t *throttle) set(bps int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if bps == t.bps {
		return
	}
	t.bps = bps
	switch {
	case bps <= 0:
		t.lim = nil
	case t.lim == nil:
		t.lim = rate.NewLimiter(rate.Limit(bps), int(bps))
	default:
		t.lim.SetLimit(rate.Limit(bps))
		t.lim.SetBurst(int(bps))
	}
}

// wait takes n bytes' worth from the bucket, sleeping as the cap requires
// but never past ctx, and without holding anything another transfer from
// the same peer needs: each takes its turn at the bucket, not at a lock.
func (t *throttle) wait(ctx context.Context, n int) error {
	t.mu.Lock()
	lim := t.lim
	t.mu.Unlock()
	for lim != nil && n > 0 {
		// WaitN refuses more than a burst at once; a chunk bigger than a
		// second's worth waits in second-sized pieces.
		k := min(n, lim.Burst())
		if err := lim.WaitN(ctx, k); err != nil {
			return err
		}
		n -= k
	}
	return nil
}

type throttledReader struct {
	ctx context.Context
	r   io.Reader
	t   *throttle
}

func (tr *throttledReader) Read(b []byte) (int, error) {
	n, err := tr.r.Read(b)
	if werr := tr.t.wait(tr.ctx, n); werr != nil && err == nil {
		err = werr
	}
	return n, err
}

// PeerBandwidth reads the cap for a peer, in bytes per second.
func (s *Service) PeerBandwidth(peerID int64) int64 {
	c, _, _ := fed.CapConfig[struct {
		BandwidthBps int64 `json:"bandwidthBps"`
	}](s.Store, peerID, CapFilesShare, store.DirUse)
	return c.BandwidthBps
}

func (s *Service) limiterFor(peer *store.FedPeer) *throttle {
	// Read outside hydrateMu: the lock also guards the single-flight table
	// every on-demand open goes through, and a query is no reason to hold
	// those up.
	bps := s.PeerBandwidth(peer.ID)
	s.hydrateMu.Lock()
	if s.throttles == nil {
		s.throttles = map[int64]*throttle{}
	}
	t := s.throttles[peer.ID]
	if t == nil {
		t = &throttle{}
		s.throttles[peer.ID] = t
	}
	s.hydrateMu.Unlock()
	t.set(bps)
	return t
}
