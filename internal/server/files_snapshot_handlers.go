package server

// Files backups: the snapshots, browsing one (time travel), restoring
// from one, and the per-entry exclusions.

import (
	"errors"
	"mime"
	"net/http"
	"strconv"
	"time"

	"gitea.lan/matthew/fog/internal/files"
)

func (s *Server) handleFilesSnapshots(w http.ResponseWriter, r *http.Request) {
	list, err := s.files.ListSnapshots()
	if err != nil {
		filesError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshots": list, "status": s.files.BackupStatus(nowUTC())})
}

func (s *Server) handleFilesSnapshotRun(w http.ResponseWriter, r *http.Request) {
	job, err := s.files.StartSnapshot("Drive snapshot")
	if err != nil {
		if errors.Is(err, files.ErrSnapshotRunning) {
			conflict(w, err.Error())
			return
		}
		badRequest(w, err.Error())
		return
	}
	s.audit(r, "files.snapshot", "", map[string]any{"trigger": "manual"})
	writeJSON(w, http.StatusAccepted, job)
}

func snapshotID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		notFound(w, "no such snapshot")
		return 0, false
	}
	return id, true
}

func (s *Server) handleFilesSnapshotList(w http.ResponseWriter, r *http.Request) {
	id, ok := snapshotID(w, r)
	if !ok {
		return
	}
	rel, ok := relParam(w, r, "path")
	if !ok {
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	l, err := s.files.SnapshotList(id, rel, files.ListOpts{Sort: files.Sort(q.Get("sort")), Desc: q.Get("dir") == "desc", Cursor: q.Get("cursor"), Limit: limit})
	if err != nil {
		filesError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, l)
}

func (s *Server) handleFilesSnapshotRaw(w http.ResponseWriter, r *http.Request) {
	id, ok := snapshotID(w, r)
	if !ok {
		return
	}
	rel, err := files.CleanRel(r.PathValue("path"))
	if err != nil {
		filesError(w, err)
		return
	}
	f, n, err := s.files.SnapshotOpenFile(id, rel)
	if err != nil {
		filesError(w, err)
		return
	}
	defer f.Close()
	disposition := "inline"
	if r.URL.Query().Get("dl") == "1" {
		disposition = "attachment"
	}
	w.Header().Set("Content-Type", n.Mime)
	w.Header().Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": n.Name}))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox")
	w.Header().Set("Cache-Control", "private, no-cache")
	info, err := f.Stat()
	if err != nil {
		writeError(w, err)
		return
	}
	http.ServeContent(w, r, n.Name, info.ModTime(), f)
}

type filesRestoreReq struct {
	Paths      []string `json:"paths"`
	To         *string  `json:"to"`
	OnConflict string   `json:"onConflict"`
}

func (s *Server) handleFilesSnapshotRestore(w http.ResponseWriter, r *http.Request) {
	id, ok := snapshotID(w, r)
	if !ok {
		return
	}
	var req filesRestoreReq
	if !readJSON(w, r, &req) {
		return
	}
	var rels []string
	for _, p := range req.Paths {
		rel, err := files.CleanRel(p)
		if err != nil {
			filesError(w, err)
			return
		}
		rels = append(rels, rel)
	}
	if len(rels) == 0 {
		badRequest(w, "paths is required")
		return
	}
	to := ""
	if req.To != nil {
		var err error
		if to, err = files.CleanRel(*req.To); err != nil {
			filesError(w, err)
			return
		}
	}
	c, ok := conflictParam(w, req.OnConflict)
	if !ok {
		return
	}
	job, err := s.files.RestoreSnapshot(id, rels, to, req.To != nil, c)
	if err != nil {
		filesMutError(w, err)
		return
	}
	s.audit(r, "files.restore.snapshot", strconv.FormatInt(id, 10), map[string]any{"paths": joinPaths(rels)})
	writeJSON(w, http.StatusAccepted, job)
}

func (s *Server) handleFilesSnapshotDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := snapshotID(w, r)
	if !ok {
		return
	}
	if err := s.files.DeleteSnapshot(id); err != nil {
		if errors.Is(err, files.ErrLastSnapshot) {
			conflict(w, err.Error())
			return
		}
		filesError(w, err)
		return
	}
	s.audit(r, "files.snapshot.delete", strconv.FormatInt(id, 10), nil)
	writeOK(w)
}

func (s *Server) handleFilesExcludes(w http.ResponseWriter, r *http.Request) {
	list, err := s.files.ListExcluded()
	if err != nil {
		filesError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": list})
}

func (s *Server) handleFilesExcludeSet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path     string `json:"path"`
		Excluded bool   `json:"excluded"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	rel, err := files.CleanRel(req.Path)
	if err != nil {
		filesError(w, err)
		return
	}
	if err := s.files.SetExcluded(rel, req.Excluded); err != nil {
		filesError(w, err)
		return
	}
	verb := "files.exclude"
	if !req.Excluded {
		verb = "files.include"
	}
	s.audit(r, verb, rel, nil)
	d, err := s.files.Stat(rel)
	if err != nil {
		writeOK(w)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func nowUTC() time.Time { return time.Now().UTC() }
