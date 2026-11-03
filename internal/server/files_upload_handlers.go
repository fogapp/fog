package server

// Resumable uploads into Files: a session names the destination and size,
// chunks arrive at an offset, completion verifies and renames into place.
// See internal/files/uploads.go for why it is not one multipart POST.

import (
	"errors"
	"net/http"
	"strconv"

	"gitea.lan/matthew/fog/internal/files"
)

// uploadChunkMax bounds one PATCH. The client sends 8 MiB; anything a
// proxy would choke on is refused here before it is read.
const uploadChunkMax = 64 << 20

type filesUploadReq struct {
	Path       string `json:"path"`
	Size       int64  `json:"size"`
	SHA256     string `json:"sha256"`
	Mtime      string `json:"mtime"`
	OnConflict string `json:"onConflict"`
}

func (s *Server) handleFilesUploadBegin(w http.ResponseWriter, r *http.Request) {
	var req filesUploadReq
	if !readJSON(w, r, &req) {
		return
	}
	rel, err := files.CleanRel(req.Path)
	if err != nil {
		filesError(w, err)
		return
	}
	c, ok := conflictParam(w, req.OnConflict)
	if !ok {
		return
	}
	u, err := s.files.BeginUpload(rel, req.Size, req.SHA256, req.Mtime, c)
	if err != nil {
		filesMutError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, u)
}

func (s *Server) handleFilesUploadStatus(w http.ResponseWriter, r *http.Request) {
	u, err := s.files.UploadStatus(r.PathValue("id"))
	if err != nil {
		filesError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, u)
}

func (s *Server) handleFilesUploadChunk(w http.ResponseWriter, r *http.Request) {
	offset, err := strconv.ParseInt(r.Header.Get("Upload-Offset"), 10, 64)
	if err != nil || offset < 0 {
		badRequest(w, "Upload-Offset header is required")
		return
	}
	body := http.MaxBytesReader(w, r.Body, uploadChunkMax)
	u, err := s.files.AppendUpload(r.PathValue("id"), offset, body)
	if err != nil {
		var eo *files.ErrOffset
		if errors.As(err, &eo) {
			// The one 409 the client handles without asking anyone: it
			// simply continues from the offset the server has.
			writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "offset": eo.Offset})
			return
		}
		filesError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, u)
}

func (s *Server) handleFilesUploadComplete(w http.ResponseWriter, r *http.Request) {
	n, err := s.files.CompleteUpload(r.PathValue("id"))
	if err != nil {
		switch {
		case errors.Is(err, files.ErrUploadIncomplete):
			writeErr(w, http.StatusPreconditionFailed, err.Error())
		case errors.Is(err, files.ErrUploadCorrupt):
			writeErr(w, http.StatusUnprocessableEntity, err.Error())
		default:
			filesMutError(w, err)
		}
		return
	}
	s.audit(r, "files.upload", n.Path, map[string]any{"size": n.Size})
	writeJSON(w, http.StatusOK, n)
}

func (s *Server) handleFilesUploadAbort(w http.ResponseWriter, r *http.Request) {
	if err := s.files.AbortUpload(r.PathValue("id")); err != nil {
		filesError(w, err)
		return
	}
	writeOK(w)
}
