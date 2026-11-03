package server

// Contribute mode over the signed plane: a peer's file arrives through a
// session (begin, chunks, complete, abort), is withdrawn by its author, or
// is fetched by a third party over the author's own peering. Plus the
// operator's reject on the session plane.

import (
	"bytes"
	"errors"
	"net/http"
	"strconv"

	"gitea.lan/matthew/fog/internal/files"
	"gitea.lan/matthew/fog/internal/filesfed"
	"gitea.lan/matthew/fog/internal/store"
)

// contribChunkMax bounds one signed chunk; the body is read whole to
// verify the signature, so this is also the memory bound.
const contribChunkMax = 8 << 20

func (s *Server) handleFedContributionBegin(w http.ResponseWriter, r *http.Request) {
	peer, body, ok := s.fedFilesPeer(w, r, 1<<16)
	if !ok {
		return
	}
	sh, ok := s.fedShare(w, r, peer)
	if !ok {
		return
	}
	var req struct {
		Path   string `json:"path"`
		Size   int64  `json:"size"`
		SHA256 string `json:"sha256"`
		Mtime  string `json:"mtime"`
	}
	if !fedDecode(w, body, &req) {
		return
	}
	cb, err := s.filesFed.BeginContribution(peer, sh, req.Path, req.Size, req.SHA256, req.Mtime)
	if err != nil {
		contribError(w, err)
		return
	}
	s.store.TouchFedPeerSeen(peer.ID)
	s.writeFedSignedJSON(w, r, cb)
}

func (s *Server) handleFedContributionChunk(w http.ResponseWriter, r *http.Request) {
	peer, body, ok := s.fedFilesPeer(w, r, contribChunkMax)
	if !ok {
		return
	}
	sh, ok := s.fedShare(w, r, peer)
	if !ok {
		return
	}
	offset, err := strconv.ParseInt(r.Header.Get("Upload-Offset"), 10, 64)
	if err != nil || offset < 0 {
		badRequest(w, "Upload-Offset header is required")
		return
	}
	u, err := s.filesFed.AppendContribution(peer, sh, r.PathValue("uid"), offset, bytes.NewReader(body))
	if err != nil {
		var eo *files.ErrOffset
		if errors.As(err, &eo) {
			writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "offset": eo.Offset})
			return
		}
		contribError(w, err)
		return
	}
	s.writeFedSignedJSON(w, r, u)
}

func (s *Server) handleFedContributionComplete(w http.ResponseWriter, r *http.Request) {
	peer, _, ok := s.fedFilesPeer(w, r, 1<<16)
	if !ok {
		return
	}
	sh, ok := s.fedShare(w, r, peer)
	if !ok {
		return
	}
	e, err := s.filesFed.CompleteContribution(peer, sh, r.PathValue("uid"))
	if err != nil {
		contribError(w, err)
		return
	}
	s.auditAs(store.FederationActor(peer.Name), "files.contribution", files.Join(sh.Path, e.Path), nil)
	s.writeFedSignedJSON(w, r, e)
}

func (s *Server) handleFedContributionAbort(w http.ResponseWriter, r *http.Request) {
	peer, _, ok := s.fedFilesPeer(w, r, 1<<16)
	if !ok {
		return
	}
	sh, ok := s.fedShare(w, r, peer)
	if !ok {
		return
	}
	if err := s.filesFed.AbortContribution(peer, sh, r.PathValue("uid")); err != nil {
		contribError(w, err)
		return
	}
	s.writeFedSignedJSON(w, r, map[string]any{"ok": true})
}

func (s *Server) handleFedContributionWithdraw(w http.ResponseWriter, r *http.Request) {
	peer, _, ok := s.fedFilesPeer(w, r, 1<<16)
	if !ok {
		return
	}
	sh, ok := s.fedShare(w, r, peer)
	if !ok {
		return
	}
	if err := s.filesFed.WithdrawContribution(peer, sh, r.URL.Query().Get("path")); err != nil {
		contribError(w, err)
		return
	}
	s.auditAs(store.FederationActor(peer.Name), "files.contribution.withdraw", sh.Path, nil)
	s.writeFedSignedJSON(w, r, map[string]any{"ok": true})
}

// handleFedContributionBlob serves, to a peer of ours, a file we
// contributed to a third fog's share — the fill-through of a stub.
func (s *Server) handleFedContributionBlob(w http.ResponseWriter, r *http.Request) {
	peer, _, ok := s.fedFilesPeer(w, r, 1<<16)
	if !ok {
		return
	}
	shareID, ok := pathInt64(w, r, "share")
	if !ok {
		return
	}
	inner, err := files.CleanRel(r.PathValue("path"))
	if err != nil || inner == "" {
		notFound(w, "no such file")
		return
	}
	// Only against the origin's ticket for this caller and this file: the
	// origin's lens decides who sees it, not merely our grant to the caller.
	if err := s.filesFed.CheckRelayTicket(peer, r.PathValue("origin"), shareID, inner, r.Header.Get(filesfed.HeaderRelayTicket)); err != nil {
		forbidden(w, err.Error())
		return
	}
	f, _, sha, err := s.filesFed.ServeOwnContribution(r.PathValue("origin"), shareID, inner)
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
	s.store.TouchFedPeerSeen(peer.ID)
	s.serveFedFile(w, r, f, info.Size(), sha)
}

func contribError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, filesfed.ErrNotContribute), errors.Is(err, filesfed.ErrKeptHere), errors.Is(err, filesfed.ErrNotContribution):
		forbidden(w, err.Error())
	case errors.Is(err, filesfed.ErrQuotaExceeded), errors.Is(err, filesfed.ErrTooManyFiles):
		writeErr(w, http.StatusRequestEntityTooLarge, err.Error())
	case errors.Is(err, filesfed.ErrTooManyOpen):
		writeErr(w, http.StatusTooManyRequests, err.Error())
	case errors.Is(err, store.ErrNotFound):
		notFound(w, "no such session")
	default:
		filesMutError(w, err)
	}
}

// handleFilesReject is the operator turning a contribution away.
func (s *Server) handleFilesReject(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	rel, err := files.CleanRel(req.Path)
	if err != nil {
		filesError(w, err)
		return
	}
	if err := s.filesFed.Reject(r.Context(), rel); err != nil {
		contribError(w, err)
		return
	}
	s.audit(r, "files.contribution.reject", rel, nil)
	writeOK(w)
}
