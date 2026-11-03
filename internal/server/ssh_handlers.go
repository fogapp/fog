package server

// Settings › People & access › SSH: authorize public keys for root. See
// internal/sshaccess for how the keys reach sshd without fogd editing
// root's own authorized_keys.

import (
	"errors"
	"net/http"

	"gitea.lan/matthew/fog/internal/sshaccess"
)

func (s *Server) handleSSHAccess(w http.ResponseWriter, r *http.Request) {
	st, err := s.sshAccess.Status(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// handleAddSSHKey saves the key, then makes sure sshd reads fog's file. A
// key that saved but isn't wired is reported as exactly that — wireError
// beside a status that says Wired=false — never as working access.
func (s *Server) handleAddSSHKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key  string `json:"key"`
		Name string `json:"name"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	key, err := s.sshAccess.Add(req.Key, req.Name)
	switch {
	case errors.Is(err, sshaccess.ErrInvalid):
		badRequest(w, err.Error())
		return
	case errors.Is(err, sshaccess.ErrExists):
		conflict(w, err.Error())
		return
	case err != nil:
		writeError(w, err)
		return
	}
	s.audit(r, "system.ssh.key.add", key.Fingerprint, map[string]any{"comment": key.Comment})

	wireErr := s.sshAccess.Wire(r.Context())
	if wireErr != nil {
		s.log.Warn("ssh access: wiring sshd", "err", wireErr)
	}
	st, err := s.sshAccess.Status(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	resp := map[string]any{"status": st, "key": key}
	if wireErr != nil {
		resp["wireError"] = wireErr.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

// The fingerprint rides in the query: SHA256 fingerprints contain "/" and
// "+", which a path segment would mangle.
func (s *Server) handleRemoveSSHKey(w http.ResponseWriter, r *http.Request) {
	fp := r.URL.Query().Get("fingerprint")
	if fp == "" {
		badRequest(w, "fingerprint is required")
		return
	}
	key, err := s.sshAccess.Remove(fp)
	if errors.Is(err, sshaccess.ErrNotFound) {
		notFound(w, err.Error())
		return
	} else if err != nil {
		writeError(w, err)
		return
	}
	s.audit(r, "system.ssh.key.remove", key.Fingerprint, map[string]any{"comment": key.Comment})
	writeOK(w)
}
