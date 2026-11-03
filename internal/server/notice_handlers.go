// Notices: the persistent operator inbox for things Fog cannot do itself.
package server

import (
	"net/http"

	"gitea.lan/matthew/fog/internal/store"
)

// ---- notices: the persistent operator inbox ----

func (s *Server) handleListNotices(w http.ResponseWriter, r *http.Request) {
	notices, err := s.store.ActiveNotices()
	if err != nil {
		writeError(w, err)
		return
	}
	if notices == nil {
		notices = []store.Notice{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"notices": notices})
}

func (s *Server) handleDismissNotice(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	if err := s.store.DismissNotice(id); err != nil {
		writeError(w, err)
		return
	}
	writeOK(w)
}
