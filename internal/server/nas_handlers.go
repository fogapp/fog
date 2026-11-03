package server

import (
	"errors"
	"net/http"
	"strings"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/nas"
	"gitea.lan/matthew/fog/internal/store"
)

func (s *Server) handleNasList(w http.ResponseWriter, r *http.Request) {
	statuses, err := s.nas.Statuses()
	if err != nil {
		writeError(w, err)
		return
	}
	if statuses == nil {
		statuses = []nas.ShareStatus{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"shares":        statuses,
		"helpers":       nas.HelpersInstalled(),
		"mountsBlocked": nas.MountsBlocked(),
	})
}

type nasCreateReq struct {
	Name       string `json:"name"`
	Proto      string `json:"proto"`
	Server     string `json:"server"`
	Remote     string `json:"remote"`
	MountPoint string `json:"mountPoint"`
	Options    string `json:"options"`
	Username   string `json:"username"`
	Password   string `json:"password"`
}

func (s *Server) handleNasCreate(w http.ResponseWriter, r *http.Request) {
	var req nasCreateReq
	if !readJSON(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if err := apps.ValidateName(req.Name); err != nil {
		badRequest(w, "share name: "+err.Error())
		return
	}
	sh := store.NasShare{
		Name:       req.Name,
		Proto:      req.Proto,
		Server:     strings.TrimSpace(req.Server),
		Remote:     strings.TrimSpace(req.Remote),
		MountPoint: strings.TrimSpace(req.MountPoint),
		Options:    strings.TrimSpace(req.Options),
		Username:   strings.TrimSpace(req.Username),
		Enabled:    true,
	}
	if sh.MountPoint == "" {
		sh.MountPoint = nas.MountRoot + "/" + sh.Name
	}
	if err := nas.Validate(sh); err != nil {
		badRequest(w, err.Error())
		return
	}
	if req.Password != "" {
		if err := s.secrets().Put(0, nas.PasswordSecret(sh.Name), req.Password); err != nil {
			writeError(w, err)
			return
		}
	}
	if _, err := s.store.CreateNasShare(sh); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			badRequest(w, "a share with that name or mount point already exists")
		} else {
			writeError(w, err)
		}
		return
	}
	s.audit(r, "nas.add", sh.Name, map[string]any{"proto": sh.Proto, "server": sh.Server, "remote": sh.Remote})
	// Mount now — a definitive success/failure beats an optimistic 200. The
	// share stays stored either way so the operator can retry from the UI.
	if err := s.nas.Mount(r.Context(), sh); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeOK(w)
}

// handleNasUpdate edits a share in place — the path a share takes when its
// server moves or is renumbered. The name is fixed (it's the identity apps
// and the vault key hang off); everything about how to reach the share can
// change without losing the stored password or the share's history.
func (s *Server) handleNasUpdate(w http.ResponseWriter, r *http.Request) {
	sh, ok := s.nasShareFromPath(w, r)
	if !ok {
		return
	}
	var req nasCreateReq
	if !readJSON(w, r, &req) {
		return
	}
	upd := sh
	upd.Proto = strings.TrimSpace(req.Proto)
	upd.Server = strings.TrimSpace(req.Server)
	upd.Remote = strings.TrimSpace(req.Remote)
	upd.Options = strings.TrimSpace(req.Options)
	upd.Username = strings.TrimSpace(req.Username)
	if mp := strings.TrimSpace(req.MountPoint); mp != "" {
		upd.MountPoint = mp
	}
	if err := nas.Validate(upd); err != nil {
		badRequest(w, err.Error())
		return
	}
	// The old mount (possibly to the old server) must come down before the
	// row changes underneath it. A dead mount that won't answer a normal
	// unmount gets force-detached — that is exactly the state an edit exists
	// to fix.
	if err := s.nas.Unmount(r.Context(), sh); err != nil {
		if err := s.nas.ForceUnmount(r.Context(), sh); err != nil {
			writeError(w, err)
			return
		}
	}
	if err := s.store.UpdateNasShare(sh.Name, upd); err != nil {
		writeError(w, err)
		return
	}
	if req.Password != "" {
		if err := s.secrets().Put(0, nas.PasswordSecret(sh.Name), req.Password); err != nil {
			writeError(w, err)
			return
		}
	}
	s.audit(r, "nas.update", sh.Name, map[string]any{"proto": upd.Proto, "server": upd.Server, "remote": upd.Remote})
	if !sh.Enabled {
		writeOK(w)
		return
	}
	if err := s.nas.Mount(r.Context(), upd); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeOK(w)
}

func (s *Server) handleNasMount(w http.ResponseWriter, r *http.Request) {
	sh, ok := s.nasShareFromPath(w, r)
	if !ok {
		return
	}
	if err := s.nas.Mount(r.Context(), sh); err != nil {
		writeError(w, err)
		return
	}
	s.store.SetNasShareEnabled(sh.Name, true)
	s.audit(r, "nas.mount", sh.Name, nil)
	writeOK(w)
}

func (s *Server) handleNasUnmount(w http.ResponseWriter, r *http.Request) {
	sh, ok := s.nasShareFromPath(w, r)
	if !ok {
		return
	}
	if err := s.nas.Unmount(r.Context(), sh); err != nil {
		writeError(w, err)
		return
	}
	// Unmounting by hand means "don't bring this back at boot".
	s.store.SetNasShareEnabled(sh.Name, false)
	s.audit(r, "nas.unmount", sh.Name, nil)
	writeOK(w)
}

func (s *Server) handleNasDelete(w http.ResponseWriter, r *http.Request) {
	sh, ok := s.nasShareFromPath(w, r)
	if !ok {
		return
	}
	if err := s.nas.Unmount(r.Context(), sh); err != nil {
		// A busy mount blocks deletion — removing the row while data is
		// flowing would strand a live mount fog no longer knows about. But a
		// mount whose server is gone fails unmount too, and blocking there
		// would make a dead share undeletable; ?force=1 detaches it lazily.
		if r.URL.Query().Get("force") != "1" {
			conflict(w, err.Error())
			return
		}
		if err := s.nas.ForceUnmount(r.Context(), sh); err != nil {
			writeError(w, err)
			return
		}
	}
	if err := s.store.DeleteNasShare(sh.Name); err != nil {
		writeError(w, err)
		return
	}
	s.store.DeleteSecret(0, nas.PasswordSecret(sh.Name))
	s.audit(r, "nas.remove", sh.Name, nil)
	writeOK(w)
}

// nasShareFromPath loads the share named in the path, answering 404 for a
// name fog doesn't know and a logged 500 for anything else.
func (s *Server) nasShareFromPath(w http.ResponseWriter, r *http.Request) (store.NasShare, bool) {
	sh, err := s.store.GetNasShare(r.PathValue("name"))
	if errors.Is(err, store.ErrNotFound) {
		notFound(w, "share not found")
		return store.NasShare{}, false
	}
	if err != nil {
		writeError(w, err)
		return store.NasShare{}, false
	}
	return sh, true
}
