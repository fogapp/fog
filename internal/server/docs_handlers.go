package server

import (
	"errors"
	"net/http"

	"gitea.lan/matthew/fog/internal/build"
)

// The app's own prose: the markdown files in its repo root, read from the
// checkout the last deploy built. Image apps and never-deployed git apps
// have none, and say so with an empty list rather than an error.

func (s *Server) repoBuilder() *build.Builder {
	if s.engine == nil {
		return nil
	}
	return s.engine.Builder
}

// GET /api/apps/{name}/docs — the markdown files in the repo root, README first.
func (s *Server) handleAppDocs(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, ok := s.appFromPath(w, r); !ok {
		return
	}
	docs := []build.RepoDoc{}
	if b := s.repoBuilder(); b != nil {
		docs = b.RepoDocs(name)
	}
	writeJSON(w, http.StatusOK, docs)
}

// GET /api/apps/{name}/docs/{file} — one of those files, capped in size.
func (s *Server) handleAppDoc(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, ok := s.appFromPath(w, r); !ok {
		return
	}
	b := s.repoBuilder()
	if b == nil {
		notFound(w, "no such doc")
		return
	}
	doc, err := b.ReadRepoDoc(name, r.PathValue("file"))
	if errors.Is(err, build.ErrNoDoc) {
		notFound(w, "no such doc")
		return
	} else if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, doc)
}
