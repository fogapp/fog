package server

// Files across peers: the signed /fed/v1/files plane (catalog, manifest,
// blobs, thumbnails, events) and the session-authed management of shares
// and the keep-on-this-box floor. Every fed handler follows the flare
// envelope's shape: fedAuthed, then the files.share grant, then a signed
// answer.

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"gitea.lan/matthew/fog/internal/fed"
	"gitea.lan/matthew/fog/internal/files"
	"gitea.lan/matthew/fog/internal/filesfed"
	"gitea.lan/matthew/fog/internal/store"
)

// fedFilesPeer authenticates the caller and requires the files.share grant.
func (s *Server) fedFilesPeer(w http.ResponseWriter, r *http.Request, maxBody int64) (*store.FedPeer, []byte, bool) {
	peer, body, ok := s.fedAuthed(w, r, maxBody)
	if !ok {
		return nil, nil, false
	}
	if _, granted, _ := s.store.FedCapabilityConfig(peer.ID, filesfed.CapFilesShare, store.DirGrant); !granted {
		forbidden(w, "this peer is not granted files.share here")
		return nil, nil, false
	}
	return peer, body, true
}

func (s *Server) handleFedFilesCatalog(w http.ResponseWriter, r *http.Request) {
	peer, _, ok := s.fedFilesPeer(w, r, 1<<16)
	if !ok {
		return
	}
	cat, err := s.filesFed.Catalog(peer)
	if err != nil {
		writeError(w, err)
		return
	}
	s.store.TouchFedPeerSeen(peer.ID)
	s.writeFedSignedJSON(w, r, map[string]any{"shares": cat, "contract": filesfed.Contract})
}

func (s *Server) fedShare(w http.ResponseWriter, r *http.Request, peer *store.FedPeer) (*store.FileFedShare, bool) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return nil, false
	}
	sh, err := s.filesFed.ShareForPeer(peer, id)
	if err != nil {
		notFound(w, "no such share")
		return nil, false
	}
	return sh, true
}

func (s *Server) handleFedFilesManifest(w http.ResponseWriter, r *http.Request) {
	peer, _, ok := s.fedFilesPeer(w, r, 1<<16)
	if !ok {
		return
	}
	sh, ok := s.fedShare(w, r, peer)
	if !ok {
		return
	}
	m, err := s.filesFed.Manifest(sh)
	if err != nil {
		writeError(w, err)
		return
	}
	s.store.TouchFedPeerSeen(peer.ID)
	if s.writeFedNotModified(w, r, m.ETag) {
		return
	}
	s.writeFedSignedJSON(w, r, m)
}

// handleFedFilesBlob streams one shared file. The response is signed over
// the file's digest, declared in Fog-Content-SHA256, so a Range-resumed
// download can still be verified once whole.
func (s *Server) handleFedFilesBlob(w http.ResponseWriter, r *http.Request) {
	peer, _, ok := s.fedFilesPeer(w, r, 1<<16)
	if !ok {
		return
	}
	sh, ok := s.fedShare(w, r, peer)
	if !ok {
		return
	}
	if sh.Detail == filesfed.DetailListing {
		forbidden(w, "this share is a listing; its files do not cross")
		return
	}
	inner, err := files.CleanRel(r.PathValue("path"))
	if err != nil || inner == "" {
		notFound(w, "no such file")
		return
	}
	f, _, sha, err := s.filesFed.OpenBlob(peer, sh, inner)
	if errors.Is(err, filesfed.ErrNotAuthor) {
		forbidden(w, err.Error())
		return
	}
	if err != nil {
		notFound(w, "no such file")
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	if r.Header.Get("Range") == "" {
		s.store.AddFileFedShareServed(sh.ID, info.Size())
	}
	s.store.TouchFedPeerSeen(peer.ID)
	s.serveFedFile(w, r, f, info.Size(), sha)
}

// handleFedFilesRelay issues the ticket a subscriber presents to a
// contribution's author: this origin's word that the caller's share carries
// that file, with its bytes.
func (s *Server) handleFedFilesRelay(w http.ResponseWriter, r *http.Request) {
	peer, _, ok := s.fedFilesPeer(w, r, 1<<16)
	if !ok {
		return
	}
	sh, ok := s.fedShare(w, r, peer)
	if !ok {
		return
	}
	inner, err := files.CleanRel(r.PathValue("path"))
	if err != nil || inner == "" {
		notFound(w, "no such file")
		return
	}
	ticket, t, err := s.filesFed.IssueRelayTicket(peer, sh, inner)
	if errors.Is(err, filesfed.ErrNoRelay) {
		notFound(w, err.Error())
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	s.writeFedSignedJSON(w, r, map[string]any{"ticket": ticket, "for": t})
}

func (s *Server) handleFedFilesThumb(w http.ResponseWriter, r *http.Request) {
	peer, _, ok := s.fedFilesPeer(w, r, 1<<16)
	if !ok {
		return
	}
	sh, ok := s.fedShare(w, r, peer)
	if !ok {
		return
	}
	if sh.Detail == filesfed.DetailListing {
		forbidden(w, "this share is a listing; no previews")
		return
	}
	inner, err := files.CleanRel(r.PathValue("path"))
	if err != nil || inner == "" {
		notFound(w, "no such file")
		return
	}
	if err := s.filesFed.CanServe(peer, sh, inner); errors.Is(err, files.ErrNotFound) {
		notFound(w, "no such file")
		return
	} else if err != nil {
		forbidden(w, "no thumbnail")
		return
	}
	width, _ := strconv.Atoi(r.URL.Query().Get("w"))
	if width <= 0 {
		width = 320
	}
	f, _, mod, err := s.files.Thumbnail(r.Context(), files.Join(sh.Path, inner), width)
	if err != nil {
		notFound(w, "no thumbnail")
		return
	}
	defer f.Close()
	body, err := io.ReadAll(f)
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Last-Modified", mod.UTC().Format(http.TimeFormat))
	w.Write(body)
}

// handleFedFilesEvents takes a peer's nudge: "changed" pulls sooner,
// "revoked" marks the subscription. Neither carries content.
func (s *Server) handleFedFilesEvents(w http.ResponseWriter, r *http.Request) {
	// A nudge acts on the files channel (SyncSoon, scanOffers), so the caller
	// must be in a files.share relationship — but in EITHER direction. An
	// origin nudging one of its subscribers holds files.share here as a `use`,
	// not a grant (the grant lives on the origin's side), so requiring a grant
	// like the other files routes silently 403s every origin→subscriber
	// "changed"/"revoked" notice, leaving subscribers to converge only on their
	// next scheduled poll. Accept grant or use, for events alone.
	peer, body, ok := s.fedAuthed(w, r, 1<<16)
	if !ok {
		return
	}
	_, granted, _ := s.store.FedCapabilityConfig(peer.ID, filesfed.CapFilesShare, store.DirGrant)
	_, used, _ := s.store.FedCapabilityConfig(peer.ID, filesfed.CapFilesShare, store.DirUse)
	if !granted && !used {
		forbidden(w, "this peer has no files.share relationship here")
		return
	}
	var ev filesfed.Event
	if !fedDecode(w, body, &ev) {
		return
	}
	s.store.TouchFedPeerSeen(peer.ID)
	s.goSafe("files event", func() { s.filesFed.HandleEvent(peer, ev) })
	s.writeFedSignedJSON(w, r, map[string]any{"ok": true})
}

// ---- session plane: shares and the floor ----

type peerShareReq struct {
	PeerID               int64    `json:"peerId"`
	Path                 string   `json:"path"`
	Title                string   `json:"title"`
	Mode                 string   `json:"mode"`
	Scope                string   `json:"scope"`
	Detail               string   `json:"detail"`
	Since                string   `json:"since"`
	Apps                 string   `json:"apps"`
	ContributeQuotaBytes int64    `json:"contributeQuotaBytes"`
	Picks                []string `json:"picks"`
	Filters              []string `json:"filters"`
}

func (r peerShareReq) share() *store.FileFedShare {
	return &store.FileFedShare{PeerID: r.PeerID, Path: r.Path, Title: r.Title, Mode: r.Mode, Scope: r.Scope, Detail: r.Detail, Since: strings.TrimSpace(r.Since), Apps: r.Apps, ContributeQuotaBytes: r.ContributeQuotaBytes, Picks: r.Picks, Filters: r.Filters}
}

func filesFedError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, filesfed.ErrBadLens), errors.Is(err, filesfed.ErrNoPeer):
		badRequest(w, err.Error())
	case errors.Is(err, filesfed.ErrIsReplica):
		conflict(w, err.Error())
	case errors.Is(err, store.ErrAlreadyExists):
		conflict(w, "that folder is already shared with this peer")
	case errors.Is(err, store.ErrNotFound), errors.Is(err, filesfed.ErrNotShared):
		notFound(w, "no such share")
	default:
		filesError(w, err)
	}
}

func (s *Server) handleListPeerShares(w http.ResponseWriter, r *http.Request) {
	peerID, _ := strconv.ParseInt(r.URL.Query().Get("peer"), 10, 64)
	list, err := s.filesFed.ListShares(peerID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"shares": list})
}

func (s *Server) handleCreatePeerShare(w http.ResponseWriter, r *http.Request) {
	var req peerShareReq
	if !readJSON(w, r, &req) {
		return
	}
	sh, err := s.filesFed.CreateShare(req.share())
	if err != nil {
		filesFedError(w, err)
		return
	}
	s.audit(r, "files.share.peer", sh.Path, map[string]any{"peerId": sh.PeerID})
	writeJSON(w, http.StatusCreated, s.filesFed.View(*sh))
}

func (s *Server) handleUpdatePeerShare(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	var req peerShareReq
	if !readJSON(w, r, &req) {
		return
	}
	sh := req.share()
	sh.ID = id
	out, err := s.filesFed.UpdateShare(sh)
	if err != nil {
		filesFedError(w, err)
		return
	}
	s.audit(r, "files.share.peer.update", out.Path, map[string]any{"peerId": out.PeerID})
	writeJSON(w, http.StatusOK, s.filesFed.View(*out))
}

func (s *Server) handleDeletePeerShare(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	sh, err := s.store.FileFedShareByID(id)
	if err != nil {
		notFound(w, "no such share")
		return
	}
	if err := s.filesFed.DeleteShare(r.Context(), id); err != nil {
		filesFedError(w, err)
		return
	}
	s.audit(r, "files.share.peer.revoke", sh.Path, map[string]any{"peerId": sh.PeerID})
	writeOK(w)
}

func (s *Server) handleFilesKeep(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
		Keep bool   `json:"keep"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	rel, err := files.CleanRel(req.Path)
	if err != nil {
		filesError(w, err)
		return
	}
	if err := s.filesFed.SetKept(rel, req.Keep); err != nil {
		filesError(w, err)
		return
	}
	verb := "files.keep"
	if !req.Keep {
		verb = "files.unkeep"
	}
	s.audit(r, verb, rel, nil)
	d, err := s.files.Stat(rel)
	if err != nil {
		writeOK(w)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

var _ = fed.HeaderContentSHA256
