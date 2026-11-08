package filesfed

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"math/rand/v2"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"gitea.lan/matthew/fog/internal/backoff"
	"gitea.lan/matthew/fog/internal/fed"
	"gitea.lan/matthew/fog/internal/files"
	"gitea.lan/matthew/fog/internal/fsx"
	"gitea.lan/matthew/fog/internal/jobs"
	"gitea.lan/matthew/fog/internal/loop"
	"gitea.lan/matthew/fog/internal/store"
)

// The receiving half. Nothing arrives until the operator subscribes to an
// entry in a peer's catalog; subscribing records the impact sentence as
// accepted. Pull is the truth: the manifest (ETag'd, so an unchanged share
// is one cheap request) is diffed against the replica table, changed
// files are downloaded into staging, verified against the digest the
// origin signed, and renamed into place; vanished ones are removed. A
// replica folder is read-only through every verb and never re-shared.

const (
	// pullEvery is the unnudged cadence; a nudge pulls within nudgeSoon.
	pullEvery = 15 * time.Minute
	nudgeSoon = 30 * time.Second
	// syncsAtOnce bounds the scheduler's concurrent pulls; bootSpread is
	// the window a restart's first pulls are spread across.
	syncsAtOnce = 2
	bootSpread  = 2 * time.Minute
	// backoffBase doubles per consecutive failure up to backoffCap.
	backoffBase = 15 * time.Minute
	backoffCap  = 24 * time.Hour

	StatusActive       = "active"
	StatusPaused       = "paused"
	StatusRevoked      = "revoked"
	StatusError        = "error"
	StatusOfferChanged = "offer_changed"

	// A failing sync and a widened offer are states the scanner re-derives
	// every sweep (issues.checkFilesSync), so they are issues, not notices:
	// they auto-resolve when the peer recovers or the operator acknowledges,
	// instead of waiting to be dismissed by hand next to the identical issue
	// row. What stays here is what no sweep can re-derive.
	NoticeRevoked  = "files-fed-revoked"
	NoticeQuota    = "files-fed-quota"
	NoticeOffer    = "files-fed-offer"
	NoticeConflict = "files-fed-conflict"
	NoticeRejected = "files-fed-rejected"

	// PeersFolder is where replicas land: <root>/Peers/<peer>/<title>.
	PeersFolder = "Peers"
)

var (
	ErrSyncBusy      = errors.New("this subscription is syncing")
	ErrNotActive     = errors.New("this subscription is not active")
	ErrOfferChanged  = errors.New("the peer widened this share; acknowledge the new terms to continue")
	ErrDetailCeiling = errors.New("the origin offers less detail than that")
	ErrMounted       = errors.New("an app mounts this copy")
)

// CatalogView is a peer's catalog with what we already hold.
type CatalogView struct {
	Peer   string         `json:"peer"`
	Shares []CatalogEntry `json:"shares"`
}

// FetchCatalog asks the peer for what it shares with us.
func (s *Service) FetchCatalog(ctx context.Context, peer *store.FedPeer) ([]CatalogEntry, error) {
	var out struct {
		Shares []CatalogEntry `json:"shares"`
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if _, err := s.Fed.SignedGetJSON(ctx, peer, "/fed/v1/files/catalog", "", &out); err != nil {
		return nil, err
	}
	if out.Shares == nil {
		out.Shares = []CatalogEntry{}
	}
	return out.Shares, nil
}

// folderFor picks a free folder for a subscription: Peers/<peer>/<title>,
// then "<title> 2", …
func (s *Service) folderFor(peer *store.FedPeer, title string) string {
	safe := func(v string) string {
		v = strings.Map(func(r rune) rune {
			if r == '/' || r == 0 {
				return '-'
			}
			return r
		}, strings.TrimSpace(v))
		if v == "" || v == "." || v == ".." {
			v = "share"
		}
		return v
	}
	base := files.Join(files.Join(PeersFolder, safe(peer.DisplayName())), safe(title))
	cand := base
	for i := 2; s.Files.Exists(cand) || s.folderTaken(cand); i++ {
		cand = base + " " + strconv.Itoa(i)
	}
	return cand
}

func (s *Service) folderTaken(rel string) bool {
	subs, _ := s.Store.ListFileFedSubscriptions(0)
	for _, sub := range subs {
		if sub.Folder == rel {
			return true
		}
	}
	return false
}

// Subscribe takes an offer: the folder is created, the impact accepted,
// and the first pull runs on the caller's goroutine budget (a job).
func (s *Service) Subscribe(ctx context.Context, peer *store.FedPeer, remoteShareID int64, detailWanted string) (*store.FileFedSubscription, error) {
	cat, err := s.FetchCatalog(ctx, peer)
	if err != nil {
		return nil, err
	}
	var offer *CatalogEntry
	for i := range cat {
		if cat[i].ID == remoteShareID {
			offer = &cat[i]
		}
	}
	if offer == nil {
		return nil, ErrNotShared
	}
	if detailWanted == "" {
		detailWanted = offer.Detail
	}
	if rank(detailWanted) > rank(offer.Detail) {
		return nil, ErrDetailCeiling
	}
	if detailWanted == DetailListing {
		detailWanted = DetailPreviews // a listing is what previews are without thumbnails
	}
	folder := s.folderFor(peer, offer.Title)
	if offer.Apps != AppsAllowed {
		if apps := s.appsReaching(folder); len(apps) > 0 {
			return nil, fmt.Errorf("%w (%s mounts a folder that would contain Files/%s)", ErrReachedByApps, strings.Join(apps, ", "), folder)
		}
	}
	if err := s.Files.EnsureDir(folder); err != nil {
		return nil, err
	}
	sub := &store.FileFedSubscription{
		PeerID: peer.ID, RemoteShareID: remoteShareID, Title: offer.Title, Folder: folder, Mode: offer.Mode,
		DetailOffered: offer.Detail, DetailWanted: detailWanted, AppsAllowed: offer.Apps == AppsAllowed,
		AcceptedImpact: offer.Impact, CurrentImpact: offer.Impact, RevisionSeen: offer.Revision,
	}
	out, err := s.createSubscription(sub)
	if err != nil {
		s.Files.RemoveEmptyDir(folder) // just made: only if nothing came in
		return nil, err
	}
	s.Store.DismissNoticeByKind(NoticeOffer, offerKey(peer.ID, remoteShareID))
	s.Files.Jobs.Start("sync", "Fetch "+offer.Title+" from "+peer.DisplayName(), func(ctx context.Context, p *jobs.Progress) error {
		return s.Sync(ctx, out.ID, p)
	})
	return out, nil
}

func rank(detail string) int {
	return map[string]int{DetailListing: 0, DetailPreviews: 1, DetailFull: 2}[detail]
}

func offerKey(peerID, shareID int64) string {
	return fmt.Sprintf("%d:%d", peerID, shareID)
}

// Acknowledge accepts a widened offer and resumes.
func (s *Service) Acknowledge(id int64) error {
	sub, err := s.Store.FileFedSubscriptionByID(id)
	if err != nil {
		return err
	}
	sub.AcceptedImpact = sub.CurrentImpact
	if sub.Status == StatusOfferChanged {
		sub.Status = StatusActive
	}
	sub.NextAttemptAt = ""
	if err := s.updateSubscription(sub); err != nil {
		return err
	}
	s.SyncSoon(id)
	return nil
}

// SetStatus pauses or resumes.
func (s *Service) SetPaused(id int64, paused bool) error {
	sub, err := s.Store.FileFedSubscriptionByID(id)
	if err != nil {
		return err
	}
	if sub.Status == StatusRevoked {
		return ErrNotActive
	}
	if paused {
		sub.Status = StatusPaused
	} else if sub.Status == StatusPaused {
		sub.Status = StatusActive
		sub.NextAttemptAt = ""
		sub.ConsecutiveFailures = 0
	}
	if err := s.updateSubscription(sub); err != nil {
		return err
	}
	if !paused {
		s.SyncSoon(id)
	}
	return nil
}

// Update rewrites the operator's knobs: backup and quota.
func (s *Service) UpdateSubscription(id int64, backup *bool, quota *int64, detail *string) (*store.FileFedSubscription, error) {
	sub, err := s.Store.FileFedSubscriptionByID(id)
	if err != nil {
		return nil, err
	}
	if backup != nil {
		sub.Backup = *backup
	}
	detailChanged := false
	if detail != nil {
		d := *detail
		if d == DetailListing {
			d = DetailPreviews
		}
		if rank(d) > rank(sub.DetailOffered) {
			return nil, ErrDetailCeiling
		}
		if d != DetailFull && d != DetailPreviews {
			return nil, ErrDetailCeiling
		}
		detailChanged = d != sub.DetailWanted
		sub.DetailWanted = d
		if detailChanged {
			sub.ManifestETag = "" // the next pull must diff, not 304
		}
	}
	if quota != nil && *quota >= 0 {
		sub.QuotaBytes = *quota
		if sub.Status == StatusPaused && sub.LastError == "quota" {
			sub.Status = StatusActive
			sub.LastError = ""
		}
	}
	if err := s.updateSubscription(sub); err != nil {
		return nil, err
	}
	if detailChanged {
		if sub.DetailWanted == DetailPreviews {
			// Free the space now; pinned copies stay.
			if err := s.dehydrateAll(sub); err != nil {
				return nil, err
			}
		} else if sub.Status == StatusActive || sub.Status == StatusError {
			// Fill in: a job so the operator can watch it land.
			s.SyncNow(sub.ID)
		}
		s.Files.Kick()
	}
	return sub, nil
}

// Unsubscribe removes the copy (mode "remove") or keeps the files as our
// own (mode "adopt": replica rows dropped, the folder becomes ordinary and
// shareable — an audited act). An app mounting the folder refuses "remove"
// unless forced.
func (s *Service) Unsubscribe(id int64, adopt bool, force bool) error {
	sub, err := s.Store.FileFedSubscriptionByID(id)
	if err != nil {
		return err
	}
	if !adopt && !force {
		if apps := s.MountedBy(sub.Folder); len(apps) > 0 {
			return fmt.Errorf("%w: %s", ErrMounted, strings.Join(apps, ", "))
		}
	}
	s.stopSync(id)
	// The copy is dealt with while its rows still say what the sync placed;
	// the subscription and its rows go after.
	if !adopt {
		s.removeCopy(sub, true)
	} else {
		// What was only listed cannot be adopted: nothing could fetch it.
		s.dropDehydrated(sub)
	}
	if err := s.deleteSubscription(id); err != nil {
		return err
	}
	for _, kind := range []string{NoticeRevoked, NoticeQuota} {
		s.Store.DismissNoticeByKind(kind, strconv.FormatInt(id, 10))
	}
	if adopt {
		s.notifyApps(sub.Folder, "adopted")
	}
	s.Files.Kick()
	return nil
}

// PeerUnpaired tears down everything from a peer: every copy of theirs
// leaves (content leaves when the relationship ends), every share to them
// is told "revoked" while the channel still exists, and contributions from
// them at this origin lose their provenance rows (the files stay ours to
// hold). Called before the peer row is deleted.
func (s *Service) PeerUnpaired(ctx context.Context, peer *store.FedPeer) {
	if shares, err := s.Store.ListFileFedShares(peer.ID); err == nil && len(shares) > 0 {
		var ids []int64
		for _, sh := range shares {
			ids = append(ids, sh.ID)
		}
		go s.sendEvent(context.WithoutCancel(ctx), peer, ids, "revoked")
	}
	// Their contributions here become stubs; ours there are withdrawn.
	s.withdrawToStubs(peer.ID, nil)
	if subs, err := s.Store.ListFileFedSubscriptions(peer.ID); err == nil {
		var contributed []int64
		for _, sub := range subs {
			if sub.Mode == ModeContribute {
				contributed = append(contributed, sub.RemoteShareID)
			}
		}
		if len(contributed) > 0 {
			go s.sendEvent(context.WithoutCancel(ctx), peer, contributed, "withdrawn")
		}
		for _, sub := range subs {
			s.removeCopy(&sub, true)
			s.deleteSubscription(sub.ID)
			s.stopSync(sub.ID)
			for _, kind := range []string{NoticeRevoked, NoticeQuota} {
				s.Store.DismissNoticeByKind(kind, strconv.FormatInt(sub.ID, 10))
			}
		}
	}
	// The peer-level Peers/<label> folder, if it is now empty, goes too.
	parent := files.Join(PeersFolder, peer.DisplayName())
	s.Files.RemoveEmptyDir(parent)
}

// ---- events ----

func (s *Service) handleEvent(peer *store.FedPeer, ev Event) {
	if ev.Event == "withdrawn" {
		// The author is leaving: their contributions here become stubs.
		s.withdrawToStubs(peer.ID, ev.ShareIDs)
		return
	}
	subs, err := s.Store.ListFileFedSubscriptions(peer.ID)
	if err != nil {
		return
	}
	for _, sub := range subs {
		hit := len(ev.ShareIDs) == 0
		for _, id := range ev.ShareIDs {
			if id == sub.RemoteShareID {
				hit = true
			}
		}
		if !hit {
			continue
		}
		switch ev.Event {
		case "revoked":
			s.markRevoked(&sub)
		default:
			s.SyncSoon(sub.ID)
		}
	}
	if ev.Event == "changed" && len(ev.ShareIDs) > 0 {
		// A new share in their catalog? Look, so the offer lands in the Inbox.
		go s.scanOffers(context.Background(), peer)
	}
}

func (s *Service) markRevoked(sub *store.FileFedSubscription) {
	if sub.Status == StatusRevoked {
		return
	}
	sub.Status = StatusRevoked
	sub.LastError = ""
	s.updateSubscription(sub)
	s.notifyApps(sub.Folder, "revoked")
	name := s.peerName(sub.PeerID, "the peer")
	s.Store.CreateNotice(NoticeRevoked, strconv.FormatInt(sub.ID, 10),
		fmt.Sprintf("%s stopped sharing %s", name, sub.Title),
		fmt.Sprintf("Your copy at Files/%s stays until you decide: remove it, or keep the files as your own. Open the peer on the Connect page.", sub.Folder))
}

// scanOffers files an Inbox notice for each catalog entry not yet subscribed.
func (s *Service) scanOffers(ctx context.Context, peer *store.FedPeer) {
	if _, ok, _ := s.Store.FedCapabilityConfig(peer.ID, CapFilesShare, store.DirUse); !ok {
		return
	}
	cat, err := s.FetchCatalog(ctx, peer)
	if err != nil {
		return
	}
	subs, _ := s.Store.ListFileFedSubscriptions(peer.ID)
	have := map[int64]bool{}
	for _, sub := range subs {
		have[sub.RemoteShareID] = true
	}
	var open []CatalogEntry
	for _, c := range cat {
		if !have[c.ID] {
			open = append(open, c)
		}
	}
	// A peer's catalog is the peer's to write, so it does not get one
	// Inbox notice per entry: past a few, the offers are one notice.
	if len(open) > offerNoticesPerPeer {
		s.Store.CreateNotice(NoticeOffer, offerKey(peer.ID, 0),
			fmt.Sprintf("%s shares %d folders with you", peer.DisplayName(), len(open)),
			"Subscribe from the peer's card on the Connect page to keep a copy in Files.")
		return
	}
	for _, c := range open {
		s.Store.CreateNotice(NoticeOffer, offerKey(peer.ID, c.ID),
			fmt.Sprintf("%s shares %s with you", peer.DisplayName(), clip(c.Title, 80)),
			clip(c.Impact, 400)+" Subscribe from the peer's card on the Connect page to keep a copy in Files.")
	}
}

// offerNoticesPerPeer is how many offers from one peer get a notice each.
const offerNoticesPerPeer = 3

// clip shortens a peer-written string for a notice.
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// ---- the pull ----

// SyncSoon asks for a pull within nudgeSoon.
func (s *Service) SyncSoon(id int64) {
	s.syncMu.Lock()
	if s.soon == nil {
		s.soon = map[int64]time.Time{}
	}
	s.soon[id] = time.Now().Add(nudgeSoon)
	s.syncMu.Unlock()
}

type runningSync struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// stopSync cancels the subscription's pull in flight, if any, and waits
// for it to let go. A copy is removed only after this: a pull still placing
// files would otherwise put the folder straight back — unpair "removed"
// every copy and one came back seconds later. The wait is bounded; a pull
// stuck past it is reported, and finds its subscription gone the next time
// it looks.
func (s *Service) stopSync(id int64) {
	s.syncMu.Lock()
	run := s.syncing[id]
	s.syncMu.Unlock()
	if run == nil {
		return
	}
	run.cancel()
	select {
	case <-run.done:
	case <-time.After(30 * time.Second):
		s.Log.Warn("files federation: a pull did not stop in time; removing its copy anyway", "subscription", id)
	}
}

// SyncNow runs a pull as a job, refusing to stack.
func (s *Service) SyncNow(id int64) (jobs.Job, error) {
	sub, err := s.Store.FileFedSubscriptionByID(id)
	if err != nil {
		return jobs.Job{}, err
	}
	s.syncMu.Lock()
	busy := s.syncing[id] != nil
	s.syncMu.Unlock()
	if busy {
		return jobs.Job{}, ErrSyncBusy
	}
	return s.Files.Jobs.Start("sync", "Sync "+sub.Title, func(ctx context.Context, p *jobs.Progress) error {
		return s.Sync(ctx, id, p)
	}), nil
}

// Sync is one pull. p may be nil.
func (s *Service) Sync(ctx context.Context, id int64, p *jobs.Progress) error {
	s.syncMu.Lock()
	if s.syncing[id] != nil {
		s.syncMu.Unlock()
		return ErrSyncBusy
	}
	ctx, cancel := context.WithCancel(ctx)
	run := &runningSync{cancel: cancel, done: make(chan struct{})}
	s.syncing[id] = run
	s.syncMu.Unlock()
	defer func() {
		cancel()
		s.syncMu.Lock()
		delete(s.syncing, id)
		delete(s.soon, id)
		s.syncMu.Unlock()
		close(run.done)
	}()
	sub, err := s.Store.FileFedSubscriptionByID(id)
	if err != nil {
		return err
	}
	if sub.Status == StatusPaused || sub.Status == StatusRevoked {
		return ErrNotActive
	}
	peer, err := s.Store.FedPeerByID(sub.PeerID)
	if err != nil {
		return err
	}
	err = s.pull(ctx, sub, peer, p)
	sub2, gerr := s.Store.FileFedSubscriptionByID(id)
	if gerr != nil {
		return err
	}
	sub = sub2
	if err != nil {
		if errors.Is(err, ErrOfferChanged) || errors.Is(err, ErrNotActive) {
			return err
		}
		if errors.Is(err, errShareGone) {
			s.markRevoked(sub)
			return err
		}
		sub.ConsecutiveFailures++
		sub.LastError = err.Error()
		wait := backoff.Exp(backoffBase, backoffCap, sub.ConsecutiveFailures-1)
		sub.NextAttemptAt = time.Now().UTC().Add(wait).Format(time.RFC3339)
		if sub.Status == StatusActive {
			sub.Status = StatusError
		}
		s.updateSubscription(sub)
		return err
	}
	sub.ConsecutiveFailures = 0
	sub.LastError = ""
	sub.NextAttemptAt = ""
	sub.LastSyncedAt = time.Now().UTC().Format(time.RFC3339)
	if sub.Status == StatusError {
		sub.Status = StatusActive
	}
	s.updateSubscription(sub)
	return nil
}

// isRemote404 reports a 404 the peer provably wrote for this request: a
// refusal signed by its key. A 404 from anything else — a proxy, a stranger
// now holding the address — says nothing about the peer's shares.
func isRemote404(err error) bool {
	var re *fed.RemoteError
	return errors.As(err, &re) && re.Verified && re.Status == http.StatusNotFound
}

// sharePathOK reports whether inner — a path an origin named — is a plain
// path inside sub's folder: canonical (no "..", ".", empty or doubled
// segments, no leading slash), carrying no reserved name, and still under
// the folder once joined. A share's paths are relative to the share; the
// subscriber alone decides where the share lives.
func sharePathOK(sub *store.FileFedSubscription, inner string) error {
	clean, err := files.CleanRel(inner)
	if err != nil || clean == "" || clean != inner {
		return fmt.Errorf("the origin named a path outside the share: %q", inner)
	}
	joined, err := files.CleanRel(files.Join(sub.Folder, inner))
	if err != nil || !strings.HasPrefix(joined, sub.Folder+"/") {
		return fmt.Errorf("the origin named a path outside the share: %q", inner)
	}
	return nil
}

// caseInsensitiveAt reports whether the disk under rel tells names apart
// only by more than case: the folder, looked up by another spelling, is
// the same folder.
func (s *Service) caseInsensitiveAt(rel string) bool {
	here, err := s.Files.Lstat(rel)
	if err != nil {
		return false
	}
	swapped := strings.Map(func(r rune) rune {
		if unicode.IsUpper(r) {
			return unicode.ToLower(r)
		}
		return unicode.ToUpper(r)
	}, rel)
	if swapped == rel {
		return false
	}
	there, err := s.Files.Lstat(swapped)
	return err == nil && sameFile(here, there)
}

// maxEntrySize is the largest size a manifest entry may declare: 16 TiB,
// past any file a household shares, and small enough that no manifest's
// sizes can wrap a sum.
const maxEntrySize = 1 << 44

// addSize adds without wrapping: a total past what int64 holds stays at
// its largest value.
func addSize(a, b int64) int64 {
	if b > 0 && a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

// checkManifestPaths holds every entry of a manifest to sharePathOK, and
// to the two kinds there are.
func checkManifestPaths(sub *store.FileFedSubscription, m *Manifest, foldCase bool) error {
	seen := make(map[string]bool, len(m.Entries))
	for _, e := range m.Entries {
		if e.Kind != "file" && e.Kind != "dir" {
			return fmt.Errorf("the origin's manifest names an entry of kind %q", e.Kind)
		}
		// One entry per name — per name as this disk tells names apart.
		key := e.Path
		if foldCase {
			key = strings.ToLower(key)
		}
		if seen[key] {
			return fmt.Errorf("the origin's manifest names %s twice", e.Path)
		}
		seen[key] = true
		// Every field reads as what it says, or none of the manifest does.
		if e.Kind == "file" || e.Mtime != "" {
			if _, err := time.Parse(time.RFC3339, e.Mtime); err != nil {
				return fmt.Errorf("the origin's manifest gives %s an unreadable time %q", e.Path, e.Mtime)
			}
		}
		if e.SHA256 != "" && (len(e.SHA256) != 64 || strings.Trim(e.SHA256, "0123456789abcdef") != "") {
			return fmt.Errorf("the origin's manifest gives %s an unreadable digest", e.Path)
		}
		if e.Size < 0 || e.Size > maxEntrySize {
			return fmt.Errorf("the origin's manifest gives %s a size of %d bytes", e.Path, e.Size)
		}
		if err := sharePathOK(sub, e.Path); err != nil {
			return err
		}
	}
	return nil
}

// errShareGone marks a pull that ended because the origin said, provably,
// that the share is gone: its manifest answered a signed 404, or the origin
// signed that it no longer knows this instance. Only the manifest's answer
// counts — a blob that vanished between the manifest and its fetch is an
// ordinary failure, retried, not the end of the share.
var errShareGone = errors.New("the origin no longer shares this")

// pull does the work: manifest, consent check, diff, transfer, prune.
func (s *Service) pull(ctx context.Context, sub *store.FileFedSubscription, peer *store.FedPeer, p *jobs.Progress) error {
	// Ours go up before theirs come down: a rename at the origin is then
	// already in the manifest we diff against, and the manifest is fresh.
	if sub.Mode == ModeContribute {
		if err := s.contribute(ctx, sub, peer); err != nil {
			return err
		}
	}
	var m Manifest
	mpath := fmt.Sprintf("/fed/v1/files/shares/%d/manifest", sub.RemoteShareID)
	etag, err := s.Fed.SignedGetJSON(ctx, peer, mpath, sub.ManifestETag, &m)
	if errors.Is(err, fed.ErrNotModified) {
		return nil
	}
	if fed.Unpaired(err) || isRemote404(err) {
		return fmt.Errorf("%w: %w", errShareGone, err)
	}
	if err != nil {
		return err
	}
	// Every path a manifest names is the origin's word about where a file
	// goes under this subscription's folder, and nothing more: one that
	// isn't a plain path inside the share refuses the whole manifest before
	// anything is placed, fetched or pruned.
	if err := checkManifestPaths(sub, &m, s.caseInsensitiveAt(sub.Folder)); err != nil {
		return err
	}
	// The consent check: a widened lens waits for the operator.
	if m.Impact != "" && m.Impact != sub.CurrentImpact {
		sub.CurrentImpact = m.Impact
	}
	widenedNow := rank(m.Detail) > rank(sub.DetailOffered) || (sub.Mode == ModeRead && m.Mode == ModeContribute) || (!sub.AppsAllowed && m.Apps == AppsAllowed)
	sub.DetailOffered, sub.Mode, sub.AppsAllowed = m.Detail, m.Mode, m.Apps == AppsAllowed
	sub.RevisionSeen = m.Revision
	if widenedNow && sub.CurrentImpact != sub.AcceptedImpact {
		sub.Status = StatusOfferChanged
		s.updateSubscription(sub)
		return ErrOfferChanged
	}
	if sub.Status == StatusOfferChanged {
		sub.Status = StatusActive
	}
	sub.AcceptedImpact = sub.CurrentImpact
	s.updateSubscription(sub)
	// The lens as it now reads, against the app volumes in place now.
	if !s.guardAppsLens(sub) {
		return ErrNotActive
	}

	have, err := s.Store.ListFileFedReplicas(sub.ID)
	if err != nil {
		return err
	}
	kept, err := s.Store.ListFileShareExcludes()
	if err != nil {
		return err
	}
	self := s.Fed.InstanceID()
	// Quota: refuse to fill the disk. What counts is what this pull would
	// bring here — the origin's files, not entries that are ours by our own
	// records nor stubs — summed so no size can wrap the total (a size is
	// never negative or past maxEntrySize: checkManifestPaths).
	var total int64
	for _, e := range m.Entries {
		if e.Kind != "file" || e.Stub || (e.Origin != "" && e.Origin == self) {
			continue
		}
		if old, had := have[e.Path]; (had && old.Origin == self) || (!had && s.Files.Exists(files.Join(sub.Folder, e.Path))) {
			continue
		}
		total = addSize(total, e.Size)
	}
	if sub.QuotaBytes > 0 && total > sub.QuotaBytes && sub.DetailWanted == DetailFull {
		sub.Status = StatusPaused
		sub.LastError = "quota"
		s.updateSubscription(sub)
		s.Store.CreateNotice(NoticeQuota, strconv.FormatInt(sub.ID, 10),
			fmt.Sprintf("%s is over its quota", sub.Title),
			fmt.Sprintf("The share now holds %s, more than the %s you allowed. Raise the quota or switch to previews on the Connect page.", fsx.HumanBytes(total), fsx.HumanBytes(sub.QuotaBytes)))
		return nil
	}
	if p != nil {
		p.Set("comparing", 0, total)
	}
	want := map[string]ManifestEntry{}
	for _, e := range m.Entries {
		want[e.Path] = e
	}
	// Directories first, shallowest first.
	dirs := []ManifestEntry{}
	fileEntries := []ManifestEntry{}
	for _, e := range m.Entries {
		if e.Kind == "dir" {
			dirs = append(dirs, e)
		} else {
			fileEntries = append(fileEntries, e)
		}
	}
	sort.Slice(dirs, func(i, j int) bool { return strings.Count(dirs[i].Path, "/") < strings.Count(dirs[j].Path, "/") })
	// Placement is batched: the index refreshes each touched folder once
	// and the replica rows land in one transaction, at the end or on the
	// way out of a failed pass — a placed entry is always recorded, so a
	// later pull can prune it.
	batch, err := s.Files.NewBatch()
	if err != nil {
		return err
	}
	var rows []store.FileFedReplica
	defer func() {
		if err := s.Store.UpsertFileFedReplicas(rows); err != nil {
			s.Log.Warn("files fed: record replicas", "subscription", sub.ID, "err", err)
		}
		if err := batch.Flush(); err != nil {
			s.Log.Warn("files fed: index", "subscription", sub.ID, "err", err)
		}
	}()
	// One name, one entry — as the disk tells names apart. Two entries whose
	// names differ only in a form the disk folds together (Unicode NFC and
	// NFD on APFS, ZFS formD, casefold ext4, CIFS) lead to one file; the disk
	// is asked, not a table of forms: an entry whose name already leads to a
	// file another entry holds or this pull placed is a duplicate, and the
	// manifest is refused rather than one entry let over another.
	ids := newPlacedIDs(have)
	for _, e := range m.Entries {
		if err := ids.check(s, files.Join(sub.Folder, e.Path), e.Path); err != nil {
			return err
		}
	}
	for _, d := range dirs {
		rel := files.Join(sub.Folder, d.Path)
		if err := ids.check(s, rel, d.Path); err != nil {
			return err
		}
		// A folder already here that we hold no row for is the operator's
		// (or was made by another spelling of its name): the manifest may
		// put entries in it, never claim it.
		if _, had := have[d.Path]; !had && s.Files.Exists(rel) {
			continue
		}
		if err := batch.EnsureDir(rel); err != nil {
			return err
		}
		ids.placed(s, rel, d.Path)
		rows = append(rows, store.FileFedReplica{SubscriptionID: sub.ID, Rel: d.Path, Kind: "dir", Hydrated: true})
	}
	// Files: decide per entry, then transfer what needs bytes.
	var fetch []ManifestEntry
	var files_, bytes int64
	for _, e := range fileEntries {
		if err := ctx.Err(); err != nil {
			return err
		}
		files_++
		bytes = addSize(bytes, e.Size)
		rel := files.Join(sub.Folder, e.Path)
		old, had := have[e.Path]
		mt, _ := time.Parse(time.RFC3339, e.Mtime)
		if e.Origin != "" && e.Origin == self {
			continue // ours: the local file is the truth
		}
		// Whether a path is ours is decided from our own records, never
		// from the manifest: an origin that lists our contribution with
		// its own (or no) author must not be able to overwrite or
		// dehydrate it. Ours is a row we recorded as the author, or bytes
		// here we hold no replica row for — put in this folder by the
		// operator and not yet pushed, or kept on this box.
		if had && old.Origin == self {
			continue
		}
		if !had && s.Files.Exists(rel) {
			continue
		}
		// What this sync placed may since have been replaced by the
		// subscriber's own file — on a case-insensitive disk, saved under
		// another spelling of the name. That file is theirs now: never
		// dehydrated, overwritten or turned into a stub.
		if had && !s.placedHere(rel, old) {
			s.Store.DeleteFileFedReplica(sub.ID, e.Path)
			delete(have, e.Path)
			continue
		}
		// A row from before the file's device and inode were recorded, for
		// a file still as placed: recorded now.
		if had && old.Hydrated && old.Ino == 0 && s.Files.Exists(rel) {
			s.recordPlaced(&old, rel)
			s.Store.UpsertFileFedReplica(old)
			have[e.Path] = old
		}
		if e.Stub || (e.Origin != "" && e.Origin != peer.InstanceID) {
			// A third party's contribution: a stub, filled only over our
			// own peering with its author.
			if had && old.Hydrated && s.Files.Exists(rel) && old.SHA256 == e.SHA256 && !e.Stub {
				continue
			}
			if err := batch.PlaceDehydrated(rel, e.Size, mt, e.SHA256, e.Mime); err != nil {
				return err
			}
			row := store.FileFedReplica{SubscriptionID: sub.ID, Rel: e.Path, Kind: "file", SHA256: e.SHA256, Size: e.Size, Mtime: mt.UnixNano(), Hydrated: false, Origin: e.Origin, OriginShare: e.OriginShare, Stub: true}
			if !e.Stub && sub.DetailWanted == DetailFull {
				if filled, err := s.fillStub(ctx, sub, peer, e, rel); err != nil {
					s.Log.Debug("files fed stub", "path", rel, "err", err)
				} else if filled {
					row.Hydrated, row.Stub = true, false
					// Recorded as it is on the disk now.
					s.recordPlaced(&row, rel)
				}
			}
			rows = append(rows, row)
			continue
		}
		same := had && old.Kind == "file" && old.Size == e.Size && old.Mtime == mt.UnixNano() && (e.SHA256 == "" || old.SHA256 == e.SHA256)
		if same && old.Hydrated && !s.Files.Exists(rel) {
			same = false // the copy went missing underneath us
		}
		if m.Detail == DetailListing {
			// The origin narrowed this share to a listing: no bytes may come,
			// and none may stay — narrowing scrubs on the next pull, pinned
			// or not. The entry stays listed.
			if same && !old.Hydrated {
				continue
			}
			if had && old.Hydrated && s.Files.Exists(rel) {
				if err := s.Files.Dehydrate(rel); err != nil && !errors.Is(err, files.ErrNotFound) {
					return err
				}
			}
			if !same {
				if err := batch.PlaceDehydrated(rel, e.Size, mt, e.SHA256, e.Mime); err != nil {
					return err
				}
			}
			rows = append(rows, store.FileFedReplica{SubscriptionID: sub.ID, Rel: e.Path, Kind: "file", SHA256: e.SHA256, Size: e.Size, Mtime: mt.UnixNano(), Hydrated: false, Origin: e.Origin})
			continue
		}
		if sub.DetailWanted == DetailFull {
			if same && old.Hydrated {
				continue
			}
			fetch = append(fetch, e)
			continue
		}
		// Previews (or a listing): every entry is listed, bytes only for
		// pinned ones. An unchanged on-demand copy stays as the cache it
		// is; a changed one is listed again and fetched on the next open.
		if same {
			if !old.Hydrated {
				if n, err := s.Files.Node(rel); err != nil || !n.Dehydrated {
					// The index lost the row (a rebuild): list it again.
					if err := batch.PlaceDehydrated(rel, e.Size, mt, e.SHA256, e.Mime); err != nil {
						return err
					}
				}
			}
			continue
		}
		if had && old.Hydrated && old.Pinned {
			fetch = append(fetch, e)
			continue
		}
		if err := batch.PlaceDehydrated(rel, e.Size, mt, e.SHA256, e.Mime); err != nil {
			return err
		}
		rows = append(rows, store.FileFedReplica{SubscriptionID: sub.ID, Rel: e.Path, Kind: "file", SHA256: e.SHA256, Size: e.Size, Mtime: mt.UnixNano(), Hydrated: false, Origin: e.Origin})
	}
	// Prune what the origin no longer has before fetching what it does: a
	// fetch that fails must not leave what the origin withdrew in place —
	// narrowing a share's scope is honoured on the next pull, not the next
	// pull whose every download succeeded. Deepest first so folders empty.
	// What goes is only ever what this sync placed (prune), file by file,
	// to the trash; a folder goes only once nothing is left in it.
	var gone []string
	// Our own contributions may come back under the disk's spelling at a
	// case-insensitive origin ("SUB/x.txt" listed as "sub/x.txt"): still
	// listed, not turned away.
	oursListed := map[string]bool{}
	for _, e := range m.Entries {
		if e.Origin != "" && e.Origin == self {
			oursListed[strings.ToLower(e.Path)] = true
		}
	}
	for rel, r := range have {
		if _, ok := want[rel]; ok {
			continue
		}
		if r.Origin != "" && r.Origin == self && oursListed[strings.ToLower(rel)] {
			continue
		}
		gone = append(gone, rel)
	}
	sort.Slice(gone, func(i, j int) bool { return strings.Count(gone[i], "/") > strings.Count(gone[j], "/") })
	for _, rel := range gone {
		if r := have[rel]; r.Origin != "" && r.Origin == self {
			// Our contribution is no longer listed: the origin turned it
			// away. (A push lands before the manifest is fetched, and the
			// origin rebuilds its manifest on completion, so a fresh push
			// is always in the manifest it precedes.)
			if r.PushedAt > 0 && !r.Rejected {
				s.Store.SetFileFedReplicaContribution(sub.ID, rel, r.PushedAt, true)
				s.Store.CreateNotice(NoticeRejected, strconv.FormatInt(sub.ID, 10)+":"+rel,
					fmt.Sprintf("%s did not keep %s", peer.DisplayName(), path.Base(rel)),
					fmt.Sprintf("Your file in %s was turned away at %s's fog. It is still yours here, in Files/%s.", sub.Title, peer.DisplayName(), sub.Folder))
			}
			continue
		}
		s.prune(sub, have[rel], kept, true)
		s.Store.DeleteFileFedReplica(sub.ID, rel)
	}
	if len(fetch) > 0 {
		if err := s.transferAll(ctx, sub, peer, batch, fetch, have, p, ids); err != nil {
			return err
		}
	}
	if err := s.Store.UpsertFileFedReplicas(rows); err != nil {
		return err
	}
	rows = nil
	if err := batch.Flush(); err != nil {
		return err
	}
	sub.ManifestETag = etag
	sub.Files, sub.Bytes = files_, bytes
	s.updateSubscription(sub)
	s.Files.Kick()
	if len(fetch) > 0 || len(gone) > 0 {
		s.notifyApps(sub.Folder, "synced")
	}
	return nil
}

// materializer places verified bytes: the Files manager for one file, or a
// sync's batch.
type materializer interface {
	MaterializeOwned(rel string, src io.Reader, mtime time.Time, wantSHA string, mine func(fs.FileInfo) bool) (string, error)
}

// placedHere reports whether what is at rel is what this sync recorded
// putting there under r: nothing (a listed-only entry, or not yet placed),
// or the bytes of a hydrated row, by their size and time. Anything else is
// the subscriber's.
func (s *Service) placedHere(rel string, r store.FileFedReplica) bool {
	fi, err := s.Files.Lstat(rel)
	if err != nil {
		return errors.Is(err, files.ErrNotFound)
	}
	return placedMatches(fi, r)
}

// placedMatches is whether fi describes the bytes row r placed: as they
// were recorded from the disk when placed, or — a row still marked as
// being placed — as the copy being placed would be.
func placedMatches(fi fs.FileInfo, r store.FileFedReplica) bool {
	if r.Kind != "file" || !fi.Mode().IsRegular() {
		return false
	}
	if size, mt, ok := placingIdentity(r.SHA256); ok && fi.Size() == size && fi.ModTime().UnixNano() == mt {
		return true
	}
	if !r.Hydrated || fi.Size() != r.Size || fi.ModTime().UnixNano() != r.Mtime {
		return false
	}
	// The file itself: the device and inode it landed as, once recorded.
	if r.Ino != 0 {
		dev, ino := fileIdentity(fi)
		return dev == r.Dev && ino == r.Ino
	}
	return true
}

// fileIdentity is the device and inode fi describes (0, 0 where the
// platform doesn't say).
func fileIdentity(fi fs.FileInfo) (dev, ino uint64) {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Dev), uint64(st.Ino)
	}
	return 0, 0
}

// recordPlaced sets r's identity to what the disk has at rel now: size,
// time, device and inode, as placed.
func (s *Service) recordPlaced(r *store.FileFedReplica, rel string) {
	if fi, err := s.Files.Lstat(rel); err == nil {
		r.Size, r.Mtime = fi.Size(), fi.ModTime().UnixNano()
		r.Dev, r.Ino = fileIdentity(fi)
	}
}

// mineFor is the placement check for an entry this sync holds row r for
// (had says whether it does): only bytes it placed may be replaced.
func mineFor(r store.FileFedReplica, had bool) func(fs.FileInfo) bool {
	return func(fi fs.FileInfo) bool { return had && placedMatches(fi, r) }
}

// errOversize: the bytes coming ran past the size the entry declared.
var errOversize = errors.New("the origin sent more bytes than the entry declared")

// sizeCap passes at most max bytes and fails the read that would pass one
// more, so a download stops where its declared size says — the quota is
// checked against declared sizes, and a peer must not be able to declare
// one byte and stream a disk's worth. n is what actually passed.
type sizeCap struct {
	r      io.Reader
	max, n int64
}

func (c *sizeCap) Read(b []byte) (int, error) {
	if c.n > c.max {
		return 0, errOversize
	}
	if room := c.max - c.n + 1; int64(len(b)) > room {
		b = b[:room]
	}
	k, err := c.r.Read(b)
	c.n += int64(k)
	if c.n > c.max {
		return 0, errOversize
	}
	return k, err
}

// fetchBlob downloads one file into place, verifying the origin's signed
// digest and the manifest's, if it had one, and the entry's size: it
// returns the digest and the bytes that actually arrived, never more than
// e.Size.
func (s *Service) fetchBlob(ctx context.Context, dst materializer, peer *store.FedPeer, shareID int64, e ManifestEntry, rel string, p *jobs.Progress, lim *throttle, mine func(fs.FileInfo) bool) (string, int64, error) {
	bpath := fmt.Sprintf("/fed/v1/files/shares/%d/blob/%s", shareID, escapePath(e.Path))
	resp, err := s.Fed.SignedDo(ctx, peer, "GET", bpath, "", nil)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	// The digest the answer's signed head declared; the body is checked
	// against it as it is read, and Materialize checks it again before the
	// file is placed.
	declared := fed.DeclaredDigest(resp)
	if declared == "" {
		return "", 0, errors.New("the answer declared no digest")
	}
	if e.SHA256 != "" && !strings.EqualFold(e.SHA256, declared) {
		return "", 0, errors.New("the origin declared a different digest than its manifest")
	}
	capped := &sizeCap{r: resp.Body, max: e.Size}
	var r io.Reader = capped
	if lim != nil {
		r = &throttledReader{ctx: ctx, r: r, t: lim}
	}
	if p != nil {
		r = &progressReader{r: r, p: p}
	}
	mt, _ := time.Parse(time.RFC3339, e.Mtime)
	// Materialize verifies before the rename, so a corrupt download never
	// replaces the good copy already in place.
	got, err := dst.MaterializeOwned(rel, r, mt, declared, mine)
	if errors.Is(err, files.ErrDigestMismatch) {
		return "", 0, errors.New("downloaded bytes do not match the signed digest")
	}
	if err == nil && capped.n > e.Size {
		err = errOversize // belt and braces: a reader that swallowed the error
	}
	return got, capped.n, err
}

// transferAll fetches a batch, transfersAtOnce at a time, under the peer's
// bandwidth cap; the first failure cancels the rest and is the error.
func (s *Service) transferAll(ctx context.Context, sub *store.FileFedSubscription, peer *store.FedPeer, dst materializer, entries []ManifestEntry, have map[string]store.FileFedReplica, p *jobs.Progress, ids *placedIDs) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var conflicts placeConflicts
	lim := s.limiterFor(peer)
	sem := make(chan struct{}, transfersAtOnce)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var first error
	for _, e := range entries {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(e ManifestEntry) {
			defer wg.Done()
			defer func() { <-sem }()
			rel := files.Join(sub.Folder, e.Path)
			if p != nil {
				p.Current(e.Path)
			}
			old, had := have[e.Path]
			mt, _ := time.Parse(time.RFC3339, e.Mtime)
			fail := func(err error) {
				mu.Lock()
				if first == nil {
					first = err
				}
				mu.Unlock()
				cancel()
			}
			if err := ids.check(s, rel, e.Path); err != nil {
				fail(err)
				return
			}
			// In flight until what it placed is recorded (ids.placed) or it
			// gives up, so an entry that finds its name taken can wait for
			// that before deciding whose the name is.
			finished := ids.start()
			defer finished()
			// Marked as being placed first, so a copy that lands but isn't
			// yet recorded (a crash between) is still known as this sync's.
			s.beginPlacing(sub.ID, old, had, e, mt)
			sha, _, err := s.fetchBlob(ctx, dst, peer, sub.RemoteShareID, e, rel, p, lim, mineFor(old, had))
			if errors.Is(err, files.ErrNotPlaced) {
				s.abandonPlacing(sub.ID, old, had, e.Path)
				// Another entry of this manifest may have got there first
				// under another form of the name, and not yet said so: the
				// placements under way are let finish first.
				finished()
				if ids.settle(ctx) != nil {
					return
				}
				// One that did is a duplicate, not a file of the
				// subscriber's.
				if derr := ids.check(s, rel, e.Path); derr != nil {
					fail(derr)
					return
				}
				// What holds the name isn't what this sync placed: a file
				// saved there here. It stays, and is no longer this sync's.
				s.Store.DeleteFileFedReplica(sub.ID, e.Path)
				conflicts.add(rel, err)
				return
			}
			if err != nil {
				s.abandonPlacing(sub.ID, old, had, e.Path)
				mu.Lock()
				if first == nil && ctx.Err() == nil {
					first = fmt.Errorf("fetch %s: %w", e.Path, err)
				}
				mu.Unlock()
				cancel()
				return
			}
			// Recorded as it is on the disk now — never as the origin said.
			row := store.FileFedReplica{SubscriptionID: sub.ID, Rel: e.Path, Kind: "file", SHA256: sha, Hydrated: true, Origin: e.Origin, Pinned: have[e.Path].Pinned}
			s.recordPlaced(&row, rel)
			ids.placed(s, rel, e.Path)
			finished()
			n := row.Size
			if err := s.Store.UpsertFileFedReplica(row); err != nil {
				// The bytes are in place but unrecorded: the next pull sees
				// no replica row and fetches the file again.
				mu.Lock()
				if first == nil {
					first = fmt.Errorf("record %s: %w", e.Path, err)
				}
				mu.Unlock()
				cancel()
				return
			}
			if err := s.addSubscriptionFetched(sub.ID, n); err != nil {
				s.Log.Warn("files fed: fetched-bytes count", "subscription", sub.ID, "err", err)
			}
		}(e)
	}
	wg.Wait()
	conflicts.notify(s, sub, peer)
	if first != nil {
		return first
	}
	return ctx.Err()
}

// placedIDs maps the files this subscription holds — recorded rows, and
// whatever this pull places — by device and inode to the entry each is,
// so the disk can say when two names are one file.
type placedIDs struct {
	mu sync.Mutex
	by map[[2]uint64]string
	// inflight are the placements under way: each channel closes once
	// what it placed is in by, or it placed nothing.
	inflight map[int]chan struct{}
	next     int
}

func newPlacedIDs(have map[string]store.FileFedReplica) *placedIDs {
	p := &placedIDs{by: map[[2]uint64]string{}, inflight: map[int]chan struct{}{}}
	for rel, r := range have {
		if r.Ino != 0 {
			p.by[[2]uint64{r.Dev, r.Ino}] = rel
		}
	}
	return p
}

// errSameFileTwice: two entries name one file on this disk.
var errSameFileTwice = errors.New("the origin's manifest names one file twice")

// check refuses entry when its name already leads, on this disk, to a file
// held or placed under another entry.
func (p *placedIDs) check(s *Service, rel, entry string) error {
	fi, err := s.Files.Lstat(rel)
	if err != nil {
		return nil
	}
	dev, ino := fileIdentity(fi)
	if ino == 0 {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if other, ok := p.by[[2]uint64{dev, ino}]; ok && other != entry {
		return fmt.Errorf("%w: %q and %q are one name on this disk", errSameFileTwice, other, entry)
	}
	return nil
}

// start marks a placement under way; the returned func ends it, and may
// be called more than once.
func (p *placedIDs) start() func() {
	p.mu.Lock()
	defer p.mu.Unlock()
	id, done := p.next, make(chan struct{})
	p.next++
	p.inflight[id] = done
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			delete(p.inflight, id)
			p.mu.Unlock()
			close(done)
		})
	}
}

// settle waits for the placements under way now to end — not for any
// begun later — so a check after it sees whatever they placed.
func (p *placedIDs) settle(ctx context.Context) error {
	p.mu.Lock()
	waits := make([]chan struct{}, 0, len(p.inflight))
	for _, c := range p.inflight {
		waits = append(waits, c)
	}
	p.mu.Unlock()
	for _, c := range waits {
		select {
		case <-c:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// placed notes what entry now holds at rel.
func (p *placedIDs) placed(s *Service, rel, entry string) {
	fi, err := s.Files.Lstat(rel)
	if err != nil {
		return
	}
	if dev, ino := fileIdentity(fi); ino != 0 {
		p.mu.Lock()
		p.by[[2]uint64{dev, ino}] = entry
		p.mu.Unlock()
	}
}

// placeConflicts gathers one pull's kept files, for one notice.
type placeConflicts struct {
	mu    sync.Mutex
	paths []string
	lost  []string // put-back failures: where the file is now
}

func (c *placeConflicts) add(rel string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.paths = append(c.paths, rel)
	if errors.Is(err, files.ErrPutBackFailed) {
		c.lost = append(c.lost, err.Error())
	}
}

// notify files at most one notice per peer: which file was kept and whose,
// how many in all, and — if putting one back failed — exactly that.
func (c *placeConflicts) notify(s *Service, sub *store.FileFedSubscription, peer *store.FedPeer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.paths) == 0 {
		return
	}
	title := fmt.Sprintf("Kept a file of this box's in %s", sub.Title)
	body := fmt.Sprintf("Files/%s was saved or changed on this box after %s's copy was placed there, so it was kept as it is and %s's version was not written. It is no longer part of the copy.", c.paths[0], peer.DisplayName(), peer.DisplayName())
	if len(c.paths) > 1 {
		title = fmt.Sprintf("Kept %d files of this box's in %s", len(c.paths), sub.Title)
		body = fmt.Sprintf("%d files in Files/%s, among them %s, were saved or changed on this box after %s's copies were placed there, so they were kept as they are and %s's versions were not written. They are no longer part of the copy.", len(c.paths), sub.Folder, c.paths[0], peer.DisplayName(), peer.DisplayName())
	}
	if len(c.lost) > 0 {
		body += " Putting one back failed: " + strings.Join(c.lost, "; ")
	}
	s.Store.CreateNotice(NoticeConflict, strconv.FormatInt(peer.ID, 10), title, body)
}

// placingPrefix marks a row whose bytes are being placed: the size and
// time the new copy will have, so a copy that landed before a crash is
// still known as this sync's.
const placingPrefix = "placing:"

// beginPlacing marks e's row as being placed. Its recorded identity stays
// what it was — what is at the name now is checked against that.
func (s *Service) beginPlacing(subID int64, old store.FileFedReplica, had bool, e ManifestEntry, mt time.Time) {
	r := old
	if !had {
		r = store.FileFedReplica{SubscriptionID: subID, Rel: e.Path, Kind: "file", Origin: e.Origin}
	}
	r.SHA256 = fmt.Sprintf("%s%d:%d:%s", placingPrefix, e.Size, mt.UnixNano(), e.SHA256)
	s.Store.UpsertFileFedReplica(r)
}

// abandonPlacing puts back the row a failed placing marked.
func (s *Service) abandonPlacing(subID int64, old store.FileFedReplica, had bool, rel string) {
	if had {
		s.Store.UpsertFileFedReplica(old)
	} else {
		s.Store.DeleteFileFedReplica(subID, rel)
	}
}

// placingIdentity reads a placing mark: the size and time the copy being
// placed will have.
func placingIdentity(sha string) (size, mtime int64, ok bool) {
	rest, found := strings.CutPrefix(sha, placingPrefix)
	if !found {
		return 0, 0, false
	}
	parts := strings.SplitN(rest, ":", 3)
	if len(parts) != 3 {
		return 0, 0, false
	}
	size, err1 := strconv.ParseInt(parts[0], 10, 64)
	mtime, err2 := strconv.ParseInt(parts[1], 10, 64)
	return size, mtime, err1 == nil && err2 == nil
}

type progressReader struct {
	r io.Reader
	p *jobs.Progress
}

func (pr *progressReader) Read(b []byte) (int, error) {
	n, err := pr.r.Read(b)
	pr.p.Add(int64(n), 0)
	return n, err
}

func escapePath(rel string) string {
	segs := strings.Split(rel, "/")
	for i, sg := range segs {
		segs[i] = strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(sg, "%", "%25"), "?", "%3F"), "#", "%23")
	}
	return strings.Join(segs, "/")
}

// ---- scheduler ----

// SyncLoop pulls every active subscription on the cadence, sooner when
// nudged, and honours backoff. Also the guard and backup-skip hooks live
// with it (registered once).
//
// At most syncsAtOnce pulls run at a time; a subscription due while they
// are all busy waits for a later tick. A subscription first seen (every
// one, at boot) is placed at a random point in the first bootSpread, so a
// restart doesn't pull everything in the same instant.
func (s *Service) SyncLoop(ctx context.Context) {
	last := map[int64]time.Time{}
	sem := make(chan struct{}, syncsAtOnce)
	loop.Every(ctx, 10*time.Second, 10*time.Second, func(ctx context.Context) {
		now := time.Now()
		subs, err := s.Store.ListFileFedSubscriptions(0)
		if err != nil {
			return
		}
		live := make(map[int64]bool, len(subs))
		for _, sub := range subs {
			live[sub.ID] = true
		}
		for id := range last {
			if !live[id] {
				delete(last, id)
			}
		}
		s.syncMu.Lock()
		soon := map[int64]time.Time{}
		for k, v := range s.soon {
			if !live[k] {
				delete(s.soon, k) // a nudge for a subscription since removed
				continue
			}
			soon[k] = v
		}
		s.syncMu.Unlock()
		for _, sub := range subs {
			if sub.Status != StatusActive && sub.Status != StatusError {
				continue
			}
			if _, seen := last[sub.ID]; !seen {
				last[sub.ID] = now.Add(-pullEvery + rand.N(bootSpread))
			}
			due := false
			if at, ok := soon[sub.ID]; ok && now.After(at) {
				due = true
			}
			if sub.NextAttemptAt != "" {
				if t, err := time.Parse(time.RFC3339, sub.NextAttemptAt); err == nil && now.Before(t) {
					continue
				}
			}
			if now.Sub(last[sub.ID]) > pullEvery {
				due = true
			}
			if !due {
				continue
			}
			select {
			case sem <- struct{}{}:
			default:
				continue // all busy: still due next tick
			}
			last[sub.ID] = now
			id := sub.ID
			go func() {
				defer func() { <-sem }()
				if err := s.Sync(ctx, id, nil); err != nil && !errors.Is(err, ErrSyncBusy) && !errors.Is(err, ErrOfferChanged) && !errors.Is(err, ErrNotActive) {
					s.Log.Warn("files sync", "subscription", id, "err", err)
				}
			}()
		}
	})
}

// Guard refuses writes inside any replica folder.
func (s *Service) Guard(rel string) error {
	// Origin side: a contributed file is its author's to change, under
	// any spelling of its name.
	if o, err := s.originFor(rel); err == nil {
		name := s.peerName(o.PeerID, "a peer")
		if o.Withdrawn {
			return fmt.Errorf("added by %s, who has since unpaired; only its listing is here — reject it to remove the entry", name)
		}
		return fmt.Errorf("added by %s — theirs to change; make a copy to edit, or reject it", name)
	}
	subs, err := s.subscriptions()
	if err != nil {
		return nil
	}
	// The write is judged by the name the disk gives what it reaches: on a
	// case-insensitive disk "peers/North/photos/A.JPG" is the replica
	// "Peers/north/Photos/a.jpg", and must meet the same rule.
	check := rel
	if s.Files != nil {
		if c, _ := s.canonicalSpelling(rel); c != "" {
			check = c
		}
	}
	for _, sub := range subs {
		if !underFold(check, sub.Folder) {
			continue
		}
		name := s.peerName(sub.PeerID, "a peer")
		if sub.Mode == ModeContribute && !strings.EqualFold(check, sub.Folder) && sub.Status != StatusRevoked {
			// A drop box: new entries and our own are ours to write.
			inner := check
			if len(check) > len(sub.Folder) {
				inner = check[len(sub.Folder)+1:]
			}
			r, err := s.Store.FileFedReplica(sub.ID, inner)
			if err != nil || (r.Origin != "" && r.Origin == s.Fed.InstanceID()) {
				return nil
			}
			if r.Origin != "" {
				return fmt.Errorf("added to %s's %q by %s — theirs to change", name, sub.Title, r.Origin)
			}
		}
		return fmt.Errorf("this is a copy of %s's %q — change it on their fog, or drop the subscription to remove this copy", name, sub.Title)
	}
	return nil
}

// BackupSkip leaves un-backed-up replicas out of the snapshot.
func (s *Service) BackupSkip(rel string) bool {
	subs, err := s.subscriptions()
	if err != nil {
		return false
	}
	for _, sub := range subs {
		if !sub.Backup && under(rel, sub.Folder) {
			return true
		}
	}
	return false
}

// prune takes away one entry the origin stopped listing — only ever what
// this sync put there, never the subscriber's own. A row says the sync
// placed it; it is removed only if it is still what was placed: a file
// whose size and time are the ones recorded (anything else was changed
// here, and is the subscriber's now), not kept on this box, to the trash so
// a peer's word can be undone; a folder only once it is empty, so whatever
// the subscriber put in it stays, and the folder with it. A listed-only
// entry has no bytes and just leaves the listing.
func (s *Service) prune(sub *store.FileFedSubscription, r store.FileFedReplica, kept []string, toTrash bool) {
	rel := files.Join(sub.Folder, r.Rel)
	if isKept(kept, rel) {
		return
	}
	fi, err := s.Files.Lstat(rel)
	if err != nil {
		if errors.Is(err, files.ErrNotFound) && r.Kind == "file" {
			s.Files.RemovePermanently(rel) // listed only: drops the index row
		}
		return
	}
	switch {
	case r.Kind == "dir" && fi.IsDir():
		if err := s.Files.RemoveEmptyDir(rel); err != nil && !errors.Is(err, files.ErrNotFound) {
			s.Log.Debug("files fed: folder kept, not empty", "path", rel)
		}
	case placedMatches(fi, r):
		var err error
		if toTrash {
			_, err = s.Files.TrashOwned(rel)
		} else {
			err = s.Files.RemovePermanently(rel)
		}
		if err != nil {
			s.Log.Warn("files fed: prune", "path", rel, "err", err)
			return
		}
		// The folders made on the way to it, as they empty (never the
		// subscription's own, and never one with anything left in it).
		for dir, _ := files.Split(rel); dir != sub.Folder && under(dir, sub.Folder); dir, _ = files.Split(dir) {
			if s.Files.RemoveEmptyDir(dir) != nil {
				break
			}
		}
	}
}

// removeCopy takes away a subscription's copy: everything this sync placed
// in its folder, entry by entry under prune's rules — only what is still
// as placed, never the subscriber's own files or what they keep — then the
// folder, once empty. toTrash sends the files to the trash; without it they
// are deleted (a copy an app must not reach, which the trash would keep on
// the disk). The rows go with it.
func (s *Service) removeCopy(sub *store.FileFedSubscription, toTrash bool) {
	rows, err := s.Store.ListFileFedReplicas(sub.ID)
	if err != nil {
		return
	}
	kept, _ := s.Store.ListFileShareExcludes()
	self := s.Fed.InstanceID()
	order := make([]string, 0, len(rows))
	for rel := range rows {
		order = append(order, rel)
	}
	sort.Slice(order, func(i, j int) bool { return strings.Count(order[i], "/") > strings.Count(order[j], "/") })
	for _, rel := range order {
		if r := rows[rel]; !(r.Origin != "" && r.Origin == self) {
			s.prune(sub, r, kept, toTrash)
		}
		s.Store.DeleteFileFedReplica(sub.ID, rel)
	}
	s.Files.RemoveEmptyDir(sub.Folder)
}
