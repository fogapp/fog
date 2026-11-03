// Files: fog's own filesystem root, browsed and edited from the Files
// page. Every path here is root-relative and resolved through *os.Root in
// internal/files, so a client cannot name anything outside the root. Reads
// stream (Range requests, streamed zips); long operations are jobs the UI
// polls.
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"os"
	"strconv"
	"strings"

	"gitea.lan/matthew/fog/internal/files"
	"gitea.lan/matthew/fog/internal/jobs"
)

// filesError maps package errors onto the house envelope with the status
// the UI keys on: 404 for a path that is not there, 409 for a name that is
// taken, 400 for a path that could never be valid.
func filesError(w http.ResponseWriter, err error) {
	var status int
	switch {
	case errors.Is(err, files.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, files.ErrExists):
		status = http.StatusConflict
	case errors.Is(err, files.ErrEscapes), errors.Is(err, files.ErrReserved), errors.Is(err, files.ErrBadName),
		errors.Is(err, files.ErrNameTooLong), errors.Is(err, files.ErrNotDirectory), errors.Is(err, files.ErrIsDirectory),
		errors.Is(err, files.ErrSymlink):
		status = http.StatusBadRequest
	case errors.Is(err, files.ErrNoSpace):
		status = http.StatusInsufficientStorage
	case errors.Is(err, files.ErrNotConfigured):
		status = http.StatusServiceUnavailable
	case errors.Is(err, files.ErrRootHasContent):
		status = http.StatusConflict
	case errors.Is(err, os.ErrPermission), errors.Is(err, files.ErrReadOnly):
		status = http.StatusForbidden
	default:
		status = http.StatusInternalServerError
	}
	writeErr(w, status, err.Error())
}

// relParam reads and normalizes a root-relative path from the query.
func relParam(w http.ResponseWriter, r *http.Request, key string) (string, bool) {
	rel, err := files.CleanRel(r.URL.Query().Get(key))
	if err != nil {
		filesError(w, err)
		return "", false
	}
	return rel, true
}

func (s *Server) handleFilesStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.files.Status())
}

func (s *Server) handleFilesReindex(w http.ResponseWriter, r *http.Request) {
	s.goSafe("files walk", func() {
		if err := s.files.Walk(r.Context()); err != nil && !errors.Is(err, files.ErrWalking) {
			s.log.Warn("files reindex", "err", err)
		}
	})
	writeJSON(w, http.StatusAccepted, map[string]any{"started": true})
}

type filesRootMoveReq struct {
	To   string `json:"to"`
	Mode string `json:"mode"` // move | adopt
}

func (s *Server) handleFilesRootMove(w http.ResponseWriter, r *http.Request) {
	var req filesRootMoveReq
	if !readJSON(w, r, &req) {
		return
	}
	job, err := s.files.MoveRoot(strings.TrimSpace(req.To), req.Mode)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	s.audit(r, "files.root.move", req.To, map[string]any{"mode": req.Mode})
	writeJSON(w, http.StatusAccepted, job)
}

type filesPlacementReq struct {
	Folder string `json:"folder" ts:"\"Apps\" | \"Content\" | \"Backups\""`
	// To is a host path; empty puts the folder back inside the Drive root,
	// or the backup directory back at fog's default.
	To   string `json:"to"`
	Mode string `json:"mode"` // move | adopt
}

// handleFilesPlacement starts the job that moves Apps or Content to another
// disk, or back into the Drive root — or that moves the backup directory,
// which the same settings section places.
func (s *Server) handleFilesPlacement(w http.ResponseWriter, r *http.Request) {
	var req filesPlacementReq
	if !readJSON(w, r, &req) {
		return
	}
	move := s.files.MovePlacement
	if req.Folder == files.BackupsFolder {
		move = func(_, to, mode string) (jobs.Job, error) { return s.files.MoveBackups(to, mode) }
	}
	job, err := move(req.Folder, strings.TrimSpace(req.To), req.Mode)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	s.audit(r, "files.placement", req.Folder, map[string]any{"to": req.To, "mode": req.Mode})
	writeJSON(w, http.StatusAccepted, job)
}

func (s *Server) handleFilesList(w http.ResponseWriter, r *http.Request) {
	rel, ok := relParam(w, r, "path")
	if !ok {
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	opts := files.ListOpts{Sort: files.Sort(q.Get("sort")), Desc: q.Get("dir") == "desc", Cursor: q.Get("cursor"), Limit: limit}
	l, err := s.files.List(rel, opts)
	if err != nil {
		filesError(w, err)
		return
	}
	// The page polls; an unchanged directory should cost a 304, not a body.
	etag := listingETag(l)
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-cache")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSON(w, http.StatusOK, l)
}

func listingETag(l files.Listing) string {
	h := uint64(14695981039346656037)
	add := func(s string) {
		for i := 0; i < len(s); i++ {
			h ^= uint64(s[i])
			h *= 1099511628211
		}
		h ^= 0xff
		h *= 1099511628211
	}
	add(l.Path)
	add(l.Dir.Modified)
	add(strconv.Itoa(l.Total))
	add(string(l.Sort))
	add(strconv.FormatBool(l.Desc))
	add(l.Next)
	for _, e := range l.Entries {
		add(e.Name)
		add(e.Kind)
		add(strconv.FormatInt(e.Size, 10))
		add(e.Modified)
		add(e.SHA256)
		add(e.WalkedAt)
		// Federation facts change without the file changing.
		add(strconv.FormatBool(e.Kept))
		add(strings.Join(e.SharedWith, ","))
		add(strings.Join(e.MountedBy, ","))
		add(strconv.FormatBool(e.Excluded))
		add(strconv.FormatBool(e.Dehydrated))
		if e.Peer != nil {
			// Every fact on the mark: hydration, pinning, ownership, stubs.
			if b, err := json.Marshal(e.Peer); err == nil {
				add(string(b))
			}
		}
	}
	add(strings.Join(l.Dir.SharedWith, ","))
	add(strconv.FormatBool(l.Dir.Kept))
	return fmt.Sprintf(`"%x"`, h)
}

func (s *Server) handleFilesStat(w http.ResponseWriter, r *http.Request) {
	rel, ok := relParam(w, r, "path")
	if !ok {
		return
	}
	d, err := s.files.Stat(rel)
	if err != nil {
		filesError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) handleFilesSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	under, ok := relParam(w, r, "under")
	if !ok {
		return
	}
	kind := q.Get("kind")
	if kind != "" && kind != "file" && kind != "dir" {
		badRequest(w, "kind must be file or dir")
		return
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	out, err := s.files.Search(q.Get("q"), under, kind, limit)
	if err != nil {
		filesError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": out})
}

// handleFilesRaw serves one file's bytes. http.ServeContent gives Range
// (video scrubbing, resumed downloads), If-Modified-Since and ETag
// handling for free. Two headers matter for safety: the response is
// sandboxed so an uploaded HTML or SVG file cannot run script on fog's
// origin with the operator's session, and the type is declared, never
// sniffed.
func (s *Server) handleFilesRaw(w http.ResponseWriter, r *http.Request) {
	rel, err := files.CleanRel(r.PathValue("path"))
	if err != nil {
		filesError(w, err)
		return
	}
	f, n, err := s.files.OpenFile(rel)
	if errors.Is(err, files.ErrNotFound) && s.filesFed != nil {
		// Listed here, held by a peer: opening is what fetches it.
		did, herr := s.filesFed.Hydrate(r.Context(), rel)
		if herr != nil {
			hydrateError(w, herr)
			return
		}
		if did {
			f, n, err = s.files.OpenFile(rel)
		}
	}
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
	if n.SHA256 != "" {
		w.Header().Set("ETag", `"`+n.SHA256+`"`)
	}
	info, err := f.Stat()
	if err != nil {
		writeError(w, err)
		return
	}
	http.ServeContent(w, r, n.Name, info.ModTime(), f)
}

// handleFilesThumb serves a cached JPEG thumbnail, made on first request.
// No thumbnail (a PDF, a video without ffmpeg) is a 404 the page turns
// into an icon; the ETag is the content digest, so a browser never
// refetches a thumbnail for bytes it has already seen.
func (s *Server) handleFilesThumb(w http.ResponseWriter, r *http.Request) {
	rel, err := files.CleanRel(r.PathValue("path"))
	if err != nil {
		filesError(w, err)
		return
	}
	width, _ := strconv.Atoi(r.URL.Query().Get("w"))
	if width <= 0 {
		width = 320
	}
	f, sha, mod, err := s.files.Thumbnail(r.Context(), rel, width)
	if errors.Is(err, files.ErrNotFound) && s.filesFed != nil {
		// A peer's entry not held here: the origin renders, we cache.
		f, sha, mod, err = s.filesFed.Thumb(r.Context(), rel, width)
	}
	if err != nil {
		if errors.Is(err, files.ErrNotFound) {
			notFound(w, "no such file")
		} else {
			notFound(w, err.Error())
		}
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "image/jpeg")
	if sha != "" {
		w.Header().Set("Cache-Control", "private, max-age=86400")
		w.Header().Set("ETag", `"`+sha+`-`+strconv.Itoa(width)+`"`)
	} else {
		// A peer's file whose digest hasn't arrived: nothing identifies
		// this render, so nothing may reuse it.
		w.Header().Set("Cache-Control", "private, no-store")
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, "thumb.jpg", mod, f)
}

// handleFilesZip streams a zip of one or more entries. The archive is
// built straight onto the response — no temp file, no size limit — so the
// download starts instantly; the cost is that a failure mid-stream shows
// up as a truncated file, which the UI notes.
func (s *Server) handleFilesZip(w http.ResponseWriter, r *http.Request) {
	var rels []string
	for _, p := range r.URL.Query()["path"] {
		rel, err := files.CleanRel(p)
		if err != nil {
			filesError(w, err)
			return
		}
		rels = append(rels, rel)
	}
	if len(rels) == 0 {
		badRequest(w, "path is required")
		return
	}
	name := "files.zip"
	if len(rels) == 1 {
		if rels[0] == "" {
			name = "Drive.zip"
		} else {
			_, base := files.Split(rels[0])
			name = base + ".zip"
		}
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if err := s.files.WriteZip(r.Context(), w, rels); err != nil {
		// Headers are gone; the best that can be done is to stop.
		s.log.Warn("files zip", "err", err)
	}
}

func (s *Server) handleFilesJobs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"jobs": s.files.Jobs.List()})
}

func (s *Server) handleFilesJob(w http.ResponseWriter, r *http.Request) {
	job, ok := s.files.Jobs.Get(r.PathValue("id"))
	if !ok {
		notFound(w, "no such job")
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) handleFilesJobCancel(w http.ResponseWriter, r *http.Request) {
	if !s.files.Jobs.Cancel(r.PathValue("id")) {
		notFound(w, "no such job")
		return
	}
	writeOK(w)
}
