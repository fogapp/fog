package server

// The Files verbs: folders, names, moves, copies, and the trash. Every one
// audits, and a name collision comes back as 409 carrying the entry in the
// way so the page can offer Replace / Keep both / Skip at once.

import (
	"errors"
	"net/http"

	"gitea.lan/matthew/fog/internal/files"
)

func filesMutError(w http.ResponseWriter, err error) {
	var c *files.ConflictError
	if errors.As(err, &c) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": c.Error(), "conflict": c})
		return
	}
	if errors.Is(err, files.ErrMounted) {
		conflict(w, err.Error())
		return
	}
	filesError(w, err)
}

// cleanRels normalizes a list of client paths, refusing the root.
func cleanRels(w http.ResponseWriter, raw []string) ([]string, bool) {
	if len(raw) == 0 {
		badRequest(w, "paths is required")
		return nil, false
	}
	out := make([]string, 0, len(raw))
	for _, p := range raw {
		rel, err := files.CleanRel(p)
		if err != nil {
			filesError(w, err)
			return nil, false
		}
		if rel == "" {
			badRequest(w, "the root itself cannot be moved, copied, or trashed")
			return nil, false
		}
		out = append(out, rel)
	}
	return out, true
}

func conflictParam(w http.ResponseWriter, v string) (files.Conflict, bool) {
	switch files.Conflict(v) {
	case files.ConflictAsk, files.ConflictRename, files.ConflictReplace, files.ConflictSkip:
		return files.Conflict(v), true
	}
	badRequest(w, "onConflict must be rename, replace, or skip")
	return "", false
}

func (s *Server) handleFilesMkdir(w http.ResponseWriter, r *http.Request) {
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
	n, err := s.files.Mkdir(rel)
	if err != nil {
		filesMutError(w, err)
		return
	}
	s.audit(r, "files.mkdir", rel, nil)
	writeJSON(w, http.StatusCreated, n)
}

func (s *Server) handleFilesRename(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
		Name string `json:"name"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	rel, err := files.CleanRel(req.Path)
	if err != nil {
		filesError(w, err)
		return
	}
	n, err := s.files.Rename(rel, req.Name)
	if err != nil {
		filesMutError(w, err)
		return
	}
	s.audit(r, "files.rename", rel, map[string]any{"to": n.Path})
	writeJSON(w, http.StatusOK, n)
}

type filesMoveReq struct {
	Paths      []string `json:"paths"`
	To         string   `json:"to"`
	OnConflict string   `json:"onConflict"`
}

func (s *Server) handleFilesMove(w http.ResponseWriter, r *http.Request) {
	var req filesMoveReq
	if !readJSON(w, r, &req) {
		return
	}
	rels, ok := cleanRels(w, req.Paths)
	if !ok {
		return
	}
	to, err := files.CleanRel(req.To)
	if err != nil {
		filesError(w, err)
		return
	}
	c, ok := conflictParam(w, req.OnConflict)
	if !ok {
		return
	}
	res, err := s.files.Move(rels, to, c)
	if err != nil {
		filesMutError(w, err)
		return
	}
	s.audit(r, "files.move", to, map[string]any{"paths": joinPaths(rels)})
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleFilesCopy(w http.ResponseWriter, r *http.Request) {
	var req filesMoveReq
	if !readJSON(w, r, &req) {
		return
	}
	rels, ok := cleanRels(w, req.Paths)
	if !ok {
		return
	}
	to, err := files.CleanRel(req.To)
	if err != nil {
		filesError(w, err)
		return
	}
	c, ok := conflictParam(w, req.OnConflict)
	if !ok {
		return
	}
	job, err := s.files.Copy(rels, to, c)
	if err != nil {
		filesMutError(w, err)
		return
	}
	s.audit(r, "files.copy", to, map[string]any{"paths": joinPaths(rels)})
	writeJSON(w, http.StatusAccepted, job)
}

func (s *Server) handleFilesTrash(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Paths []string `json:"paths"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	rels, ok := cleanRels(w, req.Paths)
	if !ok {
		return
	}
	items, err := s.files.Trash(rels)
	if err != nil {
		// Entries before the failing one are already in the trash; the
		// page re-reads the folder, so only the error needs saying.
		filesMutError(w, err)
		return
	}
	s.audit(r, "files.trash", joinPaths(rels), nil)
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleFilesTrashList(w http.ResponseWriter, r *http.Request) {
	items, err := s.files.ListTrash()
	if err != nil {
		filesError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleFilesTrashRestore(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []string `json:"ids"`
	}
	if !readJSON(w, r, &req) || len(req.IDs) == 0 {
		if len(req.IDs) == 0 {
			badRequest(w, "ids is required")
		}
		return
	}
	paths, err := s.files.RestoreTrash(req.IDs)
	if err != nil {
		filesMutError(w, err)
		return
	}
	s.audit(r, "files.restore", joinPaths(paths), nil)
	writeJSON(w, http.StatusOK, map[string]any{"paths": paths})
}

func (s *Server) handleFilesTrashPurge(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.files.PurgeTrash(id); err != nil {
		filesError(w, err)
		return
	}
	s.audit(r, "files.purge", id, nil)
	writeOK(w)
}

func (s *Server) handleFilesTrashEmpty(w http.ResponseWriter, r *http.Request) {
	n, err := s.files.EmptyTrash()
	if err != nil {
		filesError(w, err)
		return
	}
	s.audit(r, "files.trash.empty", "", nil)
	writeJSON(w, http.StatusOK, map[string]any{"purged": n})
}

func joinPaths(ps []string) string {
	out := ""
	for i, p := range ps {
		if i > 0 {
			out += ", "
		}
		out += p
		if len(out) > 500 {
			return out[:500] + "…"
		}
	}
	return out
}
