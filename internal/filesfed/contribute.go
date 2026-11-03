package filesfed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gitea.lan/matthew/fog/internal/fed"
	"gitea.lan/matthew/fog/internal/files"
	"gitea.lan/matthew/fog/internal/store"
)

// Contribute mode: a drop box with one origin per file. A
// peer subscribed to a contribute share adds files by writing into its
// replica folder; each lands at the origin through a signed session (the
// upload protocol's verbs) with the author recorded in file_fed_origins.
// The origin's operator reads, mounts and backs it up, makes a copy to
// edit, or rejects it — never edits it in place; the author edits at
// home, and the edit is a new contribution superseding the last. Other
// subscribers see the file as a stub unless they are peered with its
// author, from whom the bytes then come: bytes cross only over the
// author's own peerings.

var (
	ErrNotContribute   = errors.New("this share does not take contributions")
	ErrNotContribution = errors.New("not a peer's contribution")
	ErrKeptHere        = errors.New("that entry is kept on this box")
	ErrNotAuthor       = errors.New("this file crosses only over its author's own peering")
	ErrStub            = errors.New("only a listing of this file is here; its author is not a peer")
	ErrQuotaExceeded   = errors.New("this share's contribution quota is full")
	ErrTooManyFiles    = errors.New("too many contributed files: this share, or this peer, holds as many as it may")
	ErrTooManyOpen     = errors.New("too many contributions open at once from this peer; finish or abort some first")
)

// contribSession binds an upload session to the peer and share it came
// through; a peer completes only what it began.
type contribSession struct {
	peerID  int64
	shareID int64
	// shareCreated pins the share the session was begun under: a share id
	// can be reused once its row is gone, and a session must not carry over
	// to a new share that happens to get the old one's number.
	shareCreated string
	inner        string
	// size is the declared upload size, held so an in-flight session counts
	// against its share's contribute quota — otherwise concurrent Begins each
	// see only committed bytes and each fit under a cap they jointly blow.
	size int64
	// startedAt bounds how long a reservation holds quota: a session this old
	// has been abandoned (a dropped upload), so it stops counting rather than
	// blocking the share's cap forever.
	startedAt time.Time
	// touched is the last chunk's arrival; a session idle past
	// contribIdleTTL has lost its upload part to the files sweep, so its
	// entry is dropped rather than kept forever.
	touched time.Time
	// replaces: the session was begun as a replace of the peer's own
	// earlier contribution, which it may overwrite only while that file is
	// still the peer's (stillTheirs, checked again at landing).
	replaces bool
}

// stillTheirs reports whether the file at rel is the peer's contribution
// as it landed: recorded as theirs, not withdrawn, spelled as the record
// spells it, and holding the bytes they sent. Anything else there — the
// operator's file, an edit the operator made, another peer's — is not
// theirs to overwrite.
func (s *Service) stillTheirs(peer *store.FedPeer, rel string) bool {
	o, err := s.Store.FileFedOriginByPath(rel)
	if err != nil || o.PeerID != peer.ID || o.Withdrawn || o.SHA256 == "" {
		return false
	}
	if !s.spelledAsOnDisk(rel) {
		return false
	}
	sha, err := s.Files.HashNow(rel)
	return err == nil && strings.EqualFold(sha, o.SHA256)
}

// contribReserveTTL is how long an in-flight contribution keeps its bytes
// reserved against the quota. Generous — a large file over a slow link is
// legitimate — but finite, so an abandoned session can't hold a share hostage.
const contribReserveTTL = 6 * time.Hour

// Contributions are counted as well as sized: each is a row every manifest
// build reads, so neither one peer nor one share may hold more than these.
// (Variables so a test can lower them.)
var (
	maxContribsPerPeer  = 10000
	maxContribsPerShare = 25000
	// maxOpenContribsPerPeer bounds the sessions one peer has open at once.
	maxOpenContribsPerPeer = 16
)

// contribIdleTTL is how long a session may sit without a chunk before it
// is stale: it is ended on the next Begin, its upload part discarded, and no
// longer counts as open. Measured from the last chunk, not the start, so a
// slow upload that is still moving keeps its session. (A dropped upload is
// re-begun, never resumed across sessions.)
const contribIdleTTL = 30 * time.Minute

// ContribBegin is the origin's answer to a new contribution: the session
// and the name the file will carry (renamed on a collision).
type ContribBegin struct {
	ID     string `json:"id"`
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Offset int64  `json:"offset"`
}

// ---- origin side ----

// BeginContribution opens a session for one file from the peer. A name
// the origin (or another peer) already holds is not taken over: the later
// arrival is renamed "name (from dave).ext"; the peer's own earlier
// contribution is superseded (the old bytes go to the trash).
func (s *Service) BeginContribution(peer *store.FedPeer, sh *store.FileFedShare, inner string, size int64, sha, mtime string) (ContribBegin, error) {
	if sh.Mode != ModeContribute {
		return ContribBegin{}, ErrNotContribute
	}
	inner, err := files.CleanRel(inner)
	if err != nil || inner == "" {
		return ContribBegin{}, files.ErrBadName
	}
	rel := files.Join(sh.Path, inner)
	if blocked, _ := s.floorSet().blocks(rel); blocked {
		return ContribBegin{}, ErrKeptHere
	}
	conflict := files.ConflictRename
	if s.Files.Exists(rel) {
		if s.stillTheirs(peer, rel) {
			conflict = files.ConflictReplace
		} else {
			dir, name := files.Split(rel)
			ext := path.Ext(name)
			stem := strings.TrimSuffix(name, ext)
			if ext != "" && (stem == "" || strings.HasSuffix(stem, ".")) {
				stem, ext = name, ""
			}
			rel = files.Join(dir, stem+" (from "+peer.DisplayName()+")"+ext)
			inner = strings.TrimPrefix(rel, sh.Path+"/")
		}
	}
	// Check the quota and open + reserve the session under one lock — and read
	// the committed total inside it too — so a concurrent contribution can't
	// pass the check against a stale snapshot and push the share past its cap.
	s.contribMu.Lock()
	defer s.contribMu.Unlock()
	if s.contribs == nil {
		s.contribs = map[string]contribSession{}
	}

	// A re-begin of the same file (a resume after a dropped connection) mints a
	// fresh upload id, so supersede the peer's prior reservation for this file
	// — otherwise it is counted alongside the new one, double-charging the
	// quota and rejecting a resume that actually fits. Sessions abandoned
	// long enough for their upload part to be swept go too.
	idle := time.Now().Add(-contribIdleTTL)
	for id, cs := range s.contribs {
		if (cs.shareID == sh.ID && cs.peerID == peer.ID && cs.inner == inner) || cs.touched.Before(idle) {
			delete(s.contribs, id)
			s.Files.AbortUpload(id)
		}
	}

	// Counts, under the lock: the peer's open sessions against its cap, every
	// session included; and what has landed and what is open — every open
	// session is a contribution on its way — against the count caps, which
	// a replace of the peer's own file adds nothing to.
	// Every session counts against the peer's open cap, a replace too.
	open := 0
	for _, cs := range s.contribs {
		if cs.peerID == peer.ID {
			open++
		}
	}
	if open >= maxOpenContribsPerPeer {
		return ContribBegin{}, ErrTooManyOpen
	}
	if conflict != files.ConflictReplace {
		perPeer, perShare, err := s.Store.CountFileFedOrigins(peer.ID, sh.ID)
		if err != nil {
			return ContribBegin{}, err
		}
		for _, cs := range s.contribs {
			if cs.replaces {
				continue
			}
			if cs.peerID == peer.ID {
				perPeer++
			}
			if cs.shareID == sh.ID {
				perShare++
			}
		}
		if perPeer >= maxContribsPerPeer || perShare >= maxContribsPerShare {
			return ContribBegin{}, ErrTooManyFiles
		}
	}

	if sh.ContributeQuotaBytes > 0 {
		committed, cerr := s.Store.ContributedBytesForShare(sh.ID)
		if cerr != nil {
			return ContribBegin{}, cerr
		}
		// A replace supersedes an earlier file, freeing its bytes, so they must
		// not count against the quota twice.
		if conflict == files.ConflictReplace {
			if o, oerr := s.Store.FileFedOriginByPath(rel); oerr == nil {
				committed -= o.Size
			}
		}
		inflight := int64(0)
		cutoff := time.Now().Add(-contribReserveTTL)
		for _, cs := range s.contribs {
			if cs.shareID == sh.ID && !cs.startedAt.Before(cutoff) {
				inflight += cs.size
			}
		}
		if committed+inflight+size > sh.ContributeQuotaBytes {
			return ContribBegin{}, ErrQuotaExceeded
		}
	}
	u, err := s.Files.BeginUploadOwned(rel, size, sha, mtime, conflict)
	if err != nil {
		return ContribBegin{}, err
	}
	now := time.Now()
	s.contribs[u.ID] = contribSession{peerID: peer.ID, shareID: sh.ID, shareCreated: sh.CreatedAt, inner: inner, size: size, startedAt: now, touched: now, replaces: conflict == files.ConflictReplace}
	return ContribBegin{ID: u.ID, Path: inner, Size: size, Offset: u.Offset}, nil
}

// contribFor finds the peer's session id under share sh, as the share is
// now. A session is begun under the share's lens and stays bound to it: if
// the share has since stopped taking contributions, or the file's path has
// since been put on the floor (keep on this box), the session ends here —
// its upload discarded — rather than landing a file the origin no longer
// accepts.
func (s *Service) contribFor(peer *store.FedPeer, sh *store.FileFedShare, id string) (contribSession, error) {
	s.contribMu.Lock()
	cs, ok := s.contribs[id]
	if !ok || cs.peerID != peer.ID || cs.shareID != sh.ID || cs.shareCreated != sh.CreatedAt {
		s.contribMu.Unlock()
		return contribSession{}, store.ErrNotFound
	}
	if sh.Mode != ModeContribute {
		delete(s.contribs, id)
		s.contribMu.Unlock()
		s.Files.AbortUpload(id)
		return contribSession{}, ErrNotContribute
	}
	cs.touched = time.Now()
	s.contribs[id] = cs
	s.contribMu.Unlock()
	if blocked, _ := s.floorSet().blocks(files.Join(sh.Path, cs.inner)); blocked {
		s.contribMu.Lock()
		delete(s.contribs, id)
		s.contribMu.Unlock()
		s.Files.AbortUpload(id)
		return contribSession{}, ErrKeptHere
	}
	return cs, nil
}

// AppendContribution takes one chunk.
func (s *Service) AppendContribution(peer *store.FedPeer, sh *store.FileFedShare, id string, offset int64, body io.Reader) (files.Upload, error) {
	if _, err := s.contribFor(peer, sh, id); err != nil {
		return files.Upload{}, err
	}
	return s.Files.AppendUpload(id, offset, body)
}

// CompleteContribution lands the file and records its author. The entry
// returned is what the peer's next manifest will say about it.
func (s *Service) CompleteContribution(peer *store.FedPeer, sh *store.FileFedShare, id string) (ManifestEntry, error) {
	cs, err := s.contribFor(peer, sh, id)
	if err != nil {
		return ManifestEntry{}, err
	}
	// The quota as it stands now, against what is committed now. A
	// reservation stops counting after contribReserveTTL so an abandoned
	// session can't hold the share hostage, which also means a session kept
	// alive past it was never counted by later Begins; landing is where the
	// cap holds regardless.
	if sh.ContributeQuotaBytes > 0 {
		committed, err := s.Store.ContributedBytesForShare(sh.ID)
		if err != nil {
			return ManifestEntry{}, err
		}
		if o, err := s.Store.FileFedOriginByPath(files.Join(sh.Path, cs.inner)); err == nil && o.PeerID == peer.ID {
			committed -= o.Size // a replace frees what it supersedes
		}
		if committed+cs.size > sh.ContributeQuotaBytes {
			s.contribMu.Lock()
			delete(s.contribs, id)
			s.contribMu.Unlock()
			s.Files.AbortUpload(id)
			return ManifestEntry{}, ErrQuotaExceeded
		}
	}
	// The count caps again, at landing: what has landed now, this one
	// included.
	if !cs.replaces {
		if perPeer, perShare, err := s.Store.CountFileFedOrigins(peer.ID, sh.ID); err != nil {
			return ManifestEntry{}, err
		} else if perPeer >= maxContribsPerPeer || perShare >= maxContribsPerShare {
			s.contribMu.Lock()
			delete(s.contribs, id)
			s.contribMu.Unlock()
			s.Files.AbortUpload(id)
			return ManifestEntry{}, ErrTooManyFiles
		}
	}
	// A replace lands only over the file it was begun to replace: if that
	// was withdrawn, turned away, or since overwritten by someone else, the
	// session ends here instead of overwriting what is there now.
	if cs.replaces && !s.stillTheirs(peer, files.Join(sh.Path, cs.inner)) {
		s.contribMu.Lock()
		delete(s.contribs, id)
		s.contribMu.Unlock()
		s.Files.AbortUpload(id)
		return ManifestEntry{}, ErrNotContribution
	}
	n, err := s.Files.CompleteUpload(id)
	if err != nil {
		return ManifestEntry{}, err
	}
	// Record the origin (which makes the bytes "committed") BEFORE dropping the
	// in-flight reservation, so a concurrent quota check never sees this file in
	// neither total. The brief overlap where it counts in both errs toward
	// refusing, which is the safe direction.
	// Provenance is recorded under the name the disk gives the file: on a
	// case-insensitive disk "PHOTOS/x.txt" lands in "Photos", and a record
	// under the other spelling would leave the file the origin's own to
	// every lookup by its real name.
	landed := n.Path
	if canon, ok := s.canonicalSpelling(n.Path); ok {
		landed = canon
	}
	s.Store.UpsertFileFedOrigin(store.FileFedOrigin{Path: landed, PeerID: peer.ID, ShareID: sh.ID, InstanceID: peer.InstanceID, SHA256: n.SHA256, Size: n.Size})
	s.contribMu.Lock()
	delete(s.contribs, id)
	s.contribMu.Unlock()
	s.invalidateManifest(sh.ID)
	s.onChange(n.Path)
	s.Log.Info("files fed contribution", "path", n.Path, "peer", peer.DisplayName())
	return ManifestEntry{Path: strings.TrimPrefix(n.Path, sh.Path+"/"), Kind: "file", Size: n.Size, Mtime: n.Modified, SHA256: n.SHA256, Mime: n.Mime, Origin: peer.InstanceID, OriginShare: sh.ID}, nil
}

// AbortContribution discards a session.
func (s *Service) AbortContribution(peer *store.FedPeer, sh *store.FileFedShare, id string) error {
	if _, err := s.contribFor(peer, sh, id); err != nil {
		return err
	}
	s.contribMu.Lock()
	delete(s.contribs, id)
	s.contribMu.Unlock()
	return s.Files.AbortUpload(id)
}

// WithdrawContribution takes the peer's own file away from this origin:
// they deleted it at home. Only while it is still theirs — the bytes they
// sent (stillTheirs) — and to the trash, so the operator can take it back
// as their own; a file the operator has since edited is the operator's,
// and stays, with only its provenance dropped.
func (s *Service) WithdrawContribution(peer *store.FedPeer, sh *store.FileFedShare, inner string) error {
	inner, err := files.CleanRel(inner)
	if err != nil || inner == "" {
		return files.ErrBadName
	}
	rel := files.Join(sh.Path, inner)
	o, err := s.originFor(rel)
	if err != nil || o.PeerID != peer.ID {
		return ErrNotContribution
	}
	rel = o.Path
	if s.stillTheirs(peer, rel) {
		if _, err := s.Files.TrashOwned(rel); err != nil && !errors.Is(err, files.ErrNotFound) {
			return err
		}
	}
	s.Store.DeleteFileFedOrigin(rel)
	s.invalidateManifest(sh.ID)
	s.onChange(rel)
	return nil
}

// Reject is the operator turning a contribution away (or removing a
// withdrawn stub). The author learns from its next manifest — nudged now.
func (s *Service) Reject(ctx context.Context, rel string) error {
	o, err := s.Store.FileFedOriginByPath(rel)
	if err != nil {
		return ErrNotContribution
	}
	if err := s.Files.RemovePermanently(rel); err != nil && !errors.Is(err, files.ErrNotFound) {
		return err
	}
	s.Store.DeleteFileFedOrigin(rel)
	s.invalidateManifest(o.ShareID)
	s.onChange(rel)
	if peer, err := s.Store.FedPeerByID(o.PeerID); err == nil && !o.Withdrawn {
		go s.sendEvent(context.WithoutCancel(ctx), peer, []int64{o.ShareID}, "changed")
	}
	return nil
}

// withdrawToStubs turns every contribution from a peer into a stub: the
// bytes leave with the relationship, the listing stays so other
// subscribers keep seeing what was there. shareIDs limits it; nil is all.
func (s *Service) withdrawToStubs(peerID int64, shareIDs []int64) {
	origins, err := s.Store.ListFileFedOrigins()
	if err != nil {
		return
	}
	for _, o := range origins {
		if o.PeerID != peerID || o.Withdrawn {
			continue
		}
		if len(shareIDs) > 0 {
			hit := false
			for _, id := range shareIDs {
				hit = hit || id == o.ShareID
			}
			if !hit {
				continue
			}
		}
		// A contribution the operator has since edited is the operator's:
		// it stays as it is, and only its provenance goes.
		if peer, err := s.Store.FedPeerByID(o.PeerID); err != nil || !s.stillTheirs(peer, o.Path) {
			s.Store.DeleteFileFedOrigin(o.Path)
			s.invalidateManifest(o.ShareID)
			continue
		}
		if err := s.Files.Dehydrate(o.Path); err != nil && !errors.Is(err, files.ErrNotFound) {
			s.Log.Warn("files fed withdraw", "path", o.Path, "err", err)
			continue
		}
		s.Store.SetFileFedOriginWithdrawn(o.Path, true)
		s.invalidateManifest(o.ShareID)
		s.onChange(o.Path)
	}
}

// crossable says whether one entry's bytes may go to this peer: the
// origin's own files always; a contribution only to its author (a stub
// to everyone else — they fill it over their own peering with the
// author).
func (s *Service) crossable(peer *store.FedPeer, rel string) error {
	o, err := s.originFor(rel)
	if err != nil {
		return nil
	}
	if o.Withdrawn {
		return files.ErrNotFound
	}
	if o.InstanceID != peer.InstanceID {
		return ErrNotAuthor
	}
	return nil
}

// ---- receiving side: pushing our own files up ----

// contribute pushes what the operator put into a contribute-mode replica
// folder: new files, edits of our own earlier contributions, and — for
// our files that vanished locally — a withdrawal at the origin.
func (s *Service) contribute(ctx context.Context, sub *store.FileFedSubscription, peer *store.FedPeer) error {
	self := s.Fed.InstanceID()
	if sub.Mode != ModeContribute || self == "" {
		return nil
	}
	have, err := s.Store.ListFileFedReplicas(sub.ID)
	if err != nil {
		return err
	}
	// Withdrawals first: our rows whose file is gone.
	for inner, r := range have {
		if r.Origin != self || r.Kind != "file" {
			continue
		}
		if s.Files.Exists(files.Join(sub.Folder, inner)) {
			continue
		}
		if err := s.withdraw(ctx, sub, peer, inner); err != nil {
			return err
		}
		s.Store.DeleteFileFedReplica(sub.ID, inner)
		delete(have, inner)
	}
	var first error
	kept, err := s.Store.ListFileShareExcludes()
	if err != nil {
		return err
	}
	err = s.Files.WalkTree(sub.Folder, func(rel string, fi os.FileInfo) error {
		if rel == sub.Folder || !fi.Mode().IsRegular() {
			return nil
		}
		// Keep on this box means what it says in a contribute folder too:
		// the file never goes up, however the folder is shared.
		if isKept(kept, rel) {
			return nil
		}
		inner := strings.TrimPrefix(rel, sub.Folder+"/")
		r, had := have[inner]
		if had && r.Origin != self {
			return nil // theirs, or a stub
		}
		if had && r.Size == fi.Size() && r.Mtime == fi.ModTime().UnixNano() && !r.Rejected {
			return nil // ours, already up
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.push(ctx, sub, peer, inner); err != nil {
			s.Log.Warn("files fed contribute", "path", rel, "err", err)
			if first == nil {
				first = fmt.Errorf("contribute %s: %w", inner, err)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return first
}

func (s *Service) withdraw(ctx context.Context, sub *store.FileFedSubscription, peer *store.FedPeer, inner string) error {
	p := fmt.Sprintf("/fed/v1/files/shares/%d/contributions?path=%s", sub.RemoteShareID, url.QueryEscape(inner))
	if err := s.signedJSON(ctx, peer, "DELETE", p, nil, nil, nil); err != nil {
		if isRemote404(err) {
			return nil // already gone there, as the origin itself says
		}
		return err
	}
	return nil
}

// push sends one file up in chunks and records the row; a collision
// rename at the origin is followed locally so the two listings agree.
func (s *Service) push(ctx context.Context, sub *store.FileFedSubscription, peer *store.FedPeer, inner string) error {
	rel := files.Join(sub.Folder, inner)
	sha, err := s.Files.HashNow(rel)
	if err != nil {
		return err
	}
	f, n, err := s.Files.OpenFile(rel)
	if err != nil {
		return err
	}
	// f is reopened below when the origin renames the file, so the deferred
	// close must read the variable at return, not capture the first handle.
	defer func() { f.Close() }()
	begin, _ := json.Marshal(map[string]any{"path": inner, "size": n.Size, "sha256": sha, "mtime": n.Modified})
	base := fmt.Sprintf("/fed/v1/files/shares/%d/contributions", sub.RemoteShareID)
	var cb ContribBegin
	if err := s.signedJSON(ctx, peer, "POST", base, begin, nil, &cb); err != nil {
		return err
	}
	if cb.Path != inner {
		// The origin renamed it (their name, or another peer's): follow.
		newRel, err := s.followRename(sub, rel, cb.Path)
		if err != nil {
			s.Fed.SignedDo(ctx, peer, "DELETE", base+"/"+cb.ID, "", nil)
			return err
		}
		rel, inner = newRel, cb.Path
		f.Close()
		if f, n, err = s.Files.OpenFile(rel); err != nil {
			return err
		}
	}
	lim := s.limiterFor(peer)
	buf := make([]byte, 4<<20)
	offset := cb.Offset
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return err
		}
	}
	for offset < n.Size {
		k, err := io.ReadFull(f, buf)
		if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
			return err
		}
		if k == 0 {
			break
		}
		if err := lim.wait(ctx, k); err != nil {
			return err
		}
		var u files.Upload
		if err := s.signedJSON(ctx, peer, "PATCH", base+"/"+cb.ID, buf[:k], http.Header{"Upload-Offset": []string{strconv.FormatInt(offset, 10)}}, &u); err != nil {
			return err
		}
		offset = u.Offset
	}
	var e ManifestEntry
	if err := s.signedJSON(ctx, peer, "POST", base+"/"+cb.ID+"/complete", nil, nil, &e); err != nil {
		return err
	}
	if e.Path != inner {
		if newRel, err := s.followRename(sub, rel, e.Path); err == nil {
			rel, inner = newRel, e.Path
		}
	}
	row, _ := s.Files.Node(rel)
	s.Store.UpsertFileFedReplica(store.FileFedReplica{SubscriptionID: sub.ID, Rel: inner, Kind: "file", SHA256: sha, Size: row.Size, Mtime: row.ModifiedNanos(), Hydrated: true, Origin: s.Fed.InstanceID(), PushedAt: time.Now().Unix()})
	s.Store.SetFileFedReplicaContribution(sub.ID, inner, time.Now().Unix(), false)
	s.Log.Info("files fed contributed", "path", rel, "peer", peer.DisplayName())
	return nil
}

// followRename moves our local file to the name the origin gave it, which
// must be a plain path inside the share, in the same folder.
func (s *Service) followRename(sub *store.FileFedSubscription, rel, newInner string) (string, error) {
	if err := sharePathOK(sub, newInner); err != nil {
		return "", err
	}
	newRel := files.Join(sub.Folder, newInner)
	if newRel == rel {
		return rel, nil
	}
	_, newName := files.Split(newRel)
	oldDir, _ := files.Split(rel)
	newDir, _ := files.Split(newRel)
	if oldDir != newDir {
		return "", fmt.Errorf("the origin moved %s to another folder", rel)
	}
	// The new name must be free. A name differing only in case passes the
	// files layer's collision check (it is a case-only rename on a
	// case-insensitive disk), so it is checked here by identity: whatever
	// answers to the new name must be this very file, or nothing.
	sameName := false
	if there, err := s.Files.Lstat(newRel); err == nil {
		here, herr := s.Files.Lstat(rel)
		if herr != nil || !sameFile(here, there) {
			return "", fmt.Errorf("the origin renamed %s onto another file", rel)
		}
		sameName = true
	}
	if _, err := s.Files.Rename(rel, newName); err != nil {
		// Where the new name already reaches this very file — a disk that
		// folds case or Unicode forms — renaming may change nothing at all
		// (POSIX: a rename onto the same file does nothing), and the file is
		// as reachable by the origin's name as by ours. That is following it.
		if sameName {
			return newRel, nil
		}
		return "", err
	}
	return newRel, nil
}

// signedJSON does a signed request with an optional body and decodes the
// JSON answer, which must carry the origin's signature for this request: an
// answer here moves local files (a collision rename is followed), records
// what was contributed, and settles a withdrawal, so whoever answers at the
// origin's address must not be able to make it up.
func (s *Service) signedJSON(ctx context.Context, peer *store.FedPeer, method, p string, body []byte, hdr http.Header, out any) error {
	ct := ""
	if len(body) > 0 {
		ct = "application/octet-stream"
		if method == "POST" && body[0] == '{' {
			ct = "application/json"
		}
	}
	resp, err := s.Fed.SignedDoHeaders(ctx, peer, method, p, ct, body, hdr)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20+1))
	if err != nil {
		return err
	}
	if len(raw) > 1<<20 {
		return fmt.Errorf("%s's answer to %s is too large", peer.DisplayName(), p)
	}
	// SignedDoHeaders verified the answer as the origin's, for this request,
	// and opened it.
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// ---- receiving side: third parties' contributions ----

// fillStub tries to fetch a third party's contribution over our own
// peering with its author; without one the entry stays a stub.
func (s *Service) fillStub(ctx context.Context, sub *store.FileFedSubscription, origin *store.FedPeer, e ManifestEntry, rel string) (bool, error) {
	author, err := s.Store.FedPeerByInstanceID(e.Origin)
	if err != nil || author.URL == "" || author.UnpairedAt != "" {
		return false, nil
	}
	// The origin vouches first: its share to us, its lens, its ticket.
	var tk struct {
		Ticket string      `json:"ticket"`
		For    RelayTicket `json:"for"`
	}
	tp := fmt.Sprintf("/fed/v1/files/shares/%d/relay/%s", sub.RemoteShareID, escapePath(e.Path))
	if err := s.signedJSON(ctx, origin, "GET", tp, nil, nil, &tk); err != nil {
		var re *fed.RemoteError
		if errors.As(err, &re) && re.Verified && (re.Status == http.StatusNotFound || re.Status == http.StatusForbidden) {
			return false, nil
		}
		return false, err
	}
	if tk.For.Author != author.InstanceID || tk.For.Path == "" {
		return false, nil
	}
	p := fmt.Sprintf("/fed/v1/files/contributions/%s/%d/blob/%s", origin.InstanceID, tk.For.Share, escapePath(tk.For.Path))
	resp, err := s.Fed.SignedDoHeaders(ctx, author, "GET", p, "", nil, http.Header{HeaderRelayTicket: {tk.Ticket}})
	if err != nil {
		var re *fed.RemoteError
		if errors.As(err, &re) && (re.Status == http.StatusNotFound || re.Status == http.StatusForbidden) {
			return false, nil
		}
		return false, err
	}
	defer resp.Body.Close()
	// The digest the answer's signed head declared; the body is checked
	// against it as it is read, and Materialize checks it again before the
	// file is placed.
	declared := fed.DeclaredDigest(resp)
	if declared == "" {
		return false, errors.New("the answer declared no digest")
	}
	mt, _ := time.Parse(time.RFC3339, e.Mtime)
	// The origin's manifest declared the size, and the quota counted it:
	// the author sends no more than that.
	capped := &sizeCap{r: resp.Body, max: e.Size}
	prev, perr := s.Store.FileFedReplica(sub.ID, e.Path)
	var old store.FileFedReplica
	if perr == nil {
		old = *prev
	}
	mine := func(fi fs.FileInfo) bool { return perr == nil && placedMatches(fi, old) }
	s.beginPlacing(sub.ID, old, perr == nil, e, mt)
	if _, err := s.Files.MaterializeOwned(rel, &throttledReader{ctx: ctx, r: capped, t: s.limiterFor(author)}, mt, declared, mine); err != nil {
		s.abandonPlacing(sub.ID, old, perr == nil, e.Path)
		if errors.Is(err, files.ErrDigestMismatch) {
			return false, errors.New("downloaded bytes do not match the author's signed digest")
		}
		return false, err
	}
	if capped.n > e.Size {
		return false, errOversize
	}
	s.addSubscriptionFetched(sub.ID, capped.n)
	return true, nil
}

// ServeOwnContribution answers a peer asking, over its peering with us,
// for a file we contributed to a third fog's share: found by the origin's
// instance and share id, and only if the file is ours there.
func (s *Service) ServeOwnContribution(originInstance string, remoteShareID int64, inner string) (*os.File, files.Node, string, error) {
	origin, err := s.Store.FedPeerByInstanceID(originInstance)
	if err != nil {
		return nil, files.Node{}, "", files.ErrNotFound
	}
	subs, err := s.Store.ListFileFedSubscriptions(origin.ID)
	if err != nil {
		return nil, files.Node{}, "", err
	}
	for _, sub := range subs {
		if sub.RemoteShareID != remoteShareID {
			continue
		}
		// Only a live contribution: still contributing to that share, the
		// file pushed and kept there, and not since marked keep on this box.
		if sub.Mode != ModeContribute || sub.Status == StatusRevoked || sub.Status == StatusPaused {
			return nil, files.Node{}, "", files.ErrNotFound
		}
		r, err := s.Store.FileFedReplica(sub.ID, inner)
		if err != nil || r.Origin != s.Fed.InstanceID() || r.Rejected || r.PushedAt == 0 {
			return nil, files.Node{}, "", files.ErrNotFound
		}
		rel := files.Join(sub.Folder, inner)
		if kept, err := s.Store.ListFileShareExcludes(); err != nil || isKept(kept, rel) {
			return nil, files.Node{}, "", files.ErrNotFound
		}
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
	return nil, files.Node{}, "", files.ErrNotFound
}

// isKept reports whether rel is under a path the operator marked keep on
// this box.
func isKept(kept []string, rel string) bool {
	for _, k := range kept {
		if underFold(rel, k) {
			return true
		}
	}
	return false
}

// sameFile compares two descriptions by device and inode.
func sameFile(a, b fs.FileInfo) bool {
	sa, ok1 := a.Sys().(*syscall.Stat_t)
	sb, ok2 := b.Sys().(*syscall.Stat_t)
	if !ok1 || !ok2 {
		return os.SameFile(a, b)
	}
	return sa.Dev == sb.Dev && sa.Ino == sb.Ino
}
