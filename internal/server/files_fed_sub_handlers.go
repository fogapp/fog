package server

// Files across peers, receiving side: a peer's catalog, subscriptions and
// their verbs (acknowledge, pause, resume, sync now, unsubscribe or adopt).

import (
	"errors"
	"net/http"
	"strconv"

	"gitea.lan/matthew/fog/internal/files"
	"gitea.lan/matthew/fog/internal/filesfed"
	"gitea.lan/matthew/fog/internal/store"
)

func (s *Server) handlePeerFilesCatalog(w http.ResponseWriter, r *http.Request) {
	peer, ok := s.peerFromPath(w, r)
	if !ok {
		return
	}
	cat, err := s.filesFed.FetchCatalog(r.Context(), peer)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "could not read "+peer.DisplayName()+"'s catalog: "+err.Error())
		return
	}
	subs, _ := s.store.ListFileFedSubscriptions(peer.ID)
	s.filesFed.StampSubscriptionViews(subs)
	writeJSON(w, http.StatusOK, map[string]any{"peer": peer.DisplayName(), "shares": cat, "subscriptions": subs})
}

func (s *Server) handleListFileSubscriptions(w http.ResponseWriter, r *http.Request) {
	peerID, _ := strconv.ParseInt(r.URL.Query().Get("peer"), 10, 64)
	subs, err := s.store.ListFileFedSubscriptions(peerID)
	if err != nil {
		writeError(w, err)
		return
	}
	s.filesFed.StampSubscriptionViews(subs)
	writeJSON(w, http.StatusOK, map[string]any{"subscriptions": subs})
}

func subError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, filesfed.ErrNotShared):
		notFound(w, "no such share")
	case errors.Is(err, filesfed.ErrDetailCeiling), errors.Is(err, filesfed.ErrNotActive):
		badRequest(w, err.Error())
	case errors.Is(err, filesfed.ErrSyncBusy), errors.Is(err, filesfed.ErrMounted), errors.Is(err, store.ErrAlreadyExists):
		conflict(w, err.Error())
	case errors.Is(err, filesfed.ErrOfferChanged):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "code": "offer_changed"})
	default:
		filesError(w, err)
	}
}

func (s *Server) handleSubscribeFiles(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PeerID        int64  `json:"peerId"`
		RemoteShareID int64  `json:"remoteShareId"`
		Detail        string `json:"detail"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	peer, err := s.store.FedPeerByID(req.PeerID)
	if err != nil {
		notFound(w, "peer not found")
		return
	}
	// Subscribing is also opting in to consuming their shares: the use row.
	s.store.SetFedCapability(peer.ID, filesfed.CapFilesShare, store.DirUse, "{}")
	sub, err := s.filesFed.Subscribe(r.Context(), peer, req.RemoteShareID, req.Detail)
	if err != nil {
		subError(w, err)
		return
	}
	s.audit(r, "files.subscribe", sub.Folder, map[string]any{"peer": peer.Name})
	writeJSON(w, http.StatusCreated, sub)
}

func (s *Server) subParam(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return 0, false
	}
	return id, true
}

func (s *Server) handleFileSubscriptionAction(w http.ResponseWriter, r *http.Request) {
	id, ok := s.subParam(w, r)
	if !ok {
		return
	}
	var err error
	action := r.PathValue("action")
	switch action {
	case "sync":
		var job any
		job, err = s.filesFed.SyncNow(id)
		if err == nil {
			writeJSON(w, http.StatusAccepted, job)
			return
		}
	case "acknowledge":
		err = s.filesFed.Acknowledge(id)
	case "pause":
		err = s.filesFed.SetPaused(id, true)
	case "resume":
		err = s.filesFed.SetPaused(id, false)
	default:
		badRequest(w, "unknown action")
		return
	}
	if err != nil {
		subError(w, err)
		return
	}
	s.audit(r, "files.subscription."+action, strconv.FormatInt(id, 10), nil)
	sub, err := s.store.FileFedSubscriptionByID(id)
	if err != nil {
		subError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sub)
}

func (s *Server) handleUpdateFileSubscription(w http.ResponseWriter, r *http.Request) {
	id, ok := s.subParam(w, r)
	if !ok {
		return
	}
	var req struct {
		Backup     *bool   `json:"backup"`
		QuotaBytes *int64  `json:"quotaBytes"`
		Detail     *string `json:"detail"` // full | previews — what this box keeps
	}
	if !readJSON(w, r, &req) {
		return
	}
	sub, err := s.filesFed.UpdateSubscription(id, req.Backup, req.QuotaBytes, req.Detail)
	if err != nil {
		subError(w, err)
		return
	}
	s.audit(r, "files.subscription.update", strconv.FormatInt(id, 10), nil)
	writeJSON(w, http.StatusOK, sub)
}

func (s *Server) handleUnsubscribeFiles(w http.ResponseWriter, r *http.Request) {
	id, ok := s.subParam(w, r)
	if !ok {
		return
	}
	sub, err := s.store.FileFedSubscriptionByID(id)
	if err != nil {
		subError(w, err)
		return
	}
	adopt := r.URL.Query().Get("mode") == "adopt"
	force := r.URL.Query().Get("force") == "1"
	if err := s.filesFed.Unsubscribe(id, adopt, force); err != nil {
		subError(w, err)
		return
	}
	verb := "files.unsubscribe"
	if adopt {
		verb = "files.adopt"
	}
	s.audit(r, verb, sub.Folder, nil)
	writeOK(w)
}

// handleFilesHydrate pins a peer's entry offline (fetching it) or lets it
// go (freeing the space on a previews subscription). A folder pins every
// file in it.
func (s *Server) handleFilesHydrate(w http.ResponseWriter, r *http.Request) {
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
	if err := s.filesFed.SetPinned(r.Context(), rel, req.Keep); err != nil {
		hydrateError(w, err)
		return
	}
	verb := "files.keep-offline"
	if !req.Keep {
		verb = "files.free-space"
	}
	s.audit(r, verb, rel, nil)
	d, err := s.files.Stat(rel)
	if err != nil {
		filesError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func hydrateError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, filesfed.ErrNotReplica):
		badRequest(w, err.Error())
	case errors.Is(err, filesfed.ErrListingOnly), errors.Is(err, filesfed.ErrRevoked), errors.Is(err, filesfed.ErrStub):
		forbidden(w, err.Error())
	case errors.Is(err, store.ErrNotFound):
		notFound(w, "no such file")
	default:
		writeErr(w, http.StatusBadGateway, "could not fetch from the peer: "+err.Error())
	}
}
