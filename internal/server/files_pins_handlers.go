package server

// Drive pins: a person's shortcuts to files and folders, listed under Drive
// in their sidebar. The store keeps the paths; each read checks the disk,
// so a pin whose entry was trashed or moved outside fog says so instead of
// linking to nothing.

import (
	"errors"
	"fmt"
	"net/http"

	"gitea.lan/matthew/fog/internal/files"
	"gitea.lan/matthew/fog/internal/store"
)

type filePinView struct {
	Path string `json:"path"`
	Name string `json:"name"`
	Kind string `json:"kind" ts:"\"file\" | \"dir\""`
	// Missing: nothing is at the path now — trashed (a restore brings it
	// back), or moved or deleted outside fog.
	Missing bool `json:"missing,omitempty"`
}

func (s *Server) pinPerson(w http.ResponseWriter, r *http.Request) (int64, bool) {
	pc := s.currentPerson(r)
	if pc == nil {
		writeErr(w, http.StatusUnauthorized, "sign in to pin")
		return 0, false
	}
	return pc.Person.ID, true
}

func (s *Server) handleFilePinsList(w http.ResponseWriter, r *http.Request) {
	pid, ok := s.pinPerson(w, r)
	if !ok {
		return
	}
	pins, err := s.store.ListFilePins(pid)
	if err != nil {
		writeError(w, err)
		return
	}
	out := make([]filePinView, 0, len(pins))
	for _, p := range pins {
		_, name := files.Split(p.Path)
		v := filePinView{Path: p.Path, Name: name, Kind: p.Kind}
		kind, err := s.files.KindOf(p.Path)
		switch {
		case errors.Is(err, files.ErrNotFound):
			v.Missing = true
		case err == nil && kind != "other":
			v.Kind = kind
		}
		// Any other error (the Drive root unavailable) is not evidence the
		// entry is gone; the pin keeps what it knew.
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"pins": out})
}

func (s *Server) handleFilePinAdd(w http.ResponseWriter, r *http.Request) {
	pid, ok := s.pinPerson(w, r)
	if !ok {
		return
	}
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
	if rel == "" {
		badRequest(w, "Drive itself is already in the sidebar")
		return
	}
	kind, err := s.files.KindOf(rel)
	if err != nil {
		filesError(w, err)
		return
	}
	if kind == "other" {
		badRequest(w, "only files and folders can be pinned")
		return
	}
	if err := s.store.AddFilePin(pid, rel, kind); err != nil {
		if errors.Is(err, store.ErrFilePinLimit) {
			conflict(w, fmt.Sprintf("You can pin up to %d items — unpin one to make room.", store.MaxFilePins))
			return
		}
		writeError(w, err)
		return
	}
	writeOK(w)
}

func (s *Server) handleFilePinRemove(w http.ResponseWriter, r *http.Request) {
	pid, ok := s.pinPerson(w, r)
	if !ok {
		return
	}
	rel, ok := relParam(w, r, "path")
	if !ok {
		return
	}
	if err := s.store.RemoveFilePin(pid, rel); err != nil {
		writeError(w, err)
		return
	}
	writeOK(w)
}
