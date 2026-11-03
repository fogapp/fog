package server

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"gitea.lan/matthew/fog/internal/nas"
)

// Host filesystem browser backing the path picker in the UI. Host paths in a
// spec (volumes, backup dir) are only correct when they sit on the intended
// filesystem with ownership the app's uid can use — a bare text input shows
// none of that, so this endpoint returns the metadata alongside the names.

// fsMaxEntries bounds a listing; a media library can hold tens of thousands
// of entries and the picker only needs enough to navigate.
const fsMaxEntries = 500

type fsEntry struct {
	Name       string `json:"name"`
	Path       string `json:"path"`
	Dir        bool   `json:"dir"`
	SizeBytes  int64  `json:"sizeBytes,omitempty"` // files only
	Owner      string `json:"owner,omitempty"`     // "uid:gid"
	Mode       string `json:"mode,omitempty"`      // "drwxr-xr-x"
	MountPoint bool   `json:"mountPoint,omitempty"`
}

type fsListResponse struct {
	Path       string `json:"path"`
	Parent     string `json:"parent,omitempty"`
	Owner      string `json:"owner"`
	Mode       string `json:"mode"`
	MountPoint bool   `json:"mountPoint"`
	// FsMount is where the filesystem holding Path is mounted — "/" means the
	// path lands on the system disk, not a share.
	FsMount    string    `json:"fsMount"`
	TotalBytes uint64    `json:"totalBytes,omitempty"`
	FreeBytes  uint64    `json:"freeBytes,omitempty"`
	Entries    []fsEntry `json:"entries"`
	Truncated  int       `json:"truncated,omitempty"` // entries beyond the cap
}

// ownerOf formats "uid:gid" from a stat, or "" where the platform hides it.
func ownerOf(info fs.FileInfo) string {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return fmt.Sprintf("%d:%d", st.Uid, st.Gid)
	}
	return ""
}

// fsMountOf finds the longest mount point that is a prefix of path. Falls
// back to "/" (also the right answer when /proc/mounts is unavailable).
func fsMountOf(path string, mounts map[string]bool) string {
	best := "/"
	for m := range mounts {
		if m == "/" || m == path || strings.HasPrefix(path, m+"/") {
			if len(m) > len(best) {
				best = m
			}
		}
	}
	return best
}

// cleanAbs validates and normalizes a browse path from the client.
func cleanAbs(p string) (string, error) {
	if p == "" {
		return "/", nil
	}
	if !filepath.IsAbs(p) {
		return "", errors.New("path must be absolute")
	}
	return filepath.Clean(p), nil
}

func (s *Server) handleFsList(w http.ResponseWriter, r *http.Request) {
	path, err := cleanAbs(r.URL.Query().Get("path"))
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			badRequest(w, "no such directory: "+path)
		} else {
			writeError(w, err)
		}
		return
	}
	if !info.IsDir() {
		badRequest(w, "not a directory: "+path)
		return
	}

	mounts := nas.MountedPoints()
	resp := fsListResponse{
		Path:       path,
		Owner:      ownerOf(info),
		Mode:       info.Mode().String(),
		MountPoint: mounts[path],
		FsMount:    fsMountOf(path, mounts),
		Entries:    []fsEntry{},
	}
	if path != "/" {
		resp.Parent = filepath.Dir(path)
	}
	if total, free, err := diskUsage(path); err == nil {
		resp.TotalBytes, resp.FreeBytes = total, free
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		writeError(w, err)
		return
	}
	// Directories first (they're what the picker selects), then files as
	// context that this is the directory the user thinks it is.
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir() != entries[j].IsDir() {
			return entries[i].IsDir()
		}
		return strings.ToLower(entries[i].Name()) < strings.ToLower(entries[j].Name())
	})
	for _, e := range entries {
		if len(resp.Entries) >= fsMaxEntries {
			resp.Truncated = len(entries) - fsMaxEntries
			break
		}
		full := filepath.Join(path, e.Name())
		// Stat (not Lstat): a symlink to a directory is browsable and usable
		// as a host path, so present it as what it points at.
		fi, err := os.Stat(full)
		if err != nil {
			continue // dangling symlink or vanished mid-listing
		}
		ent := fsEntry{
			Name:  e.Name(),
			Path:  full,
			Dir:   fi.IsDir(),
			Owner: ownerOf(fi),
			Mode:  fi.Mode().String(),
		}
		if fi.IsDir() {
			ent.MountPoint = mounts[full]
		} else {
			ent.SizeBytes = fi.Size()
		}
		resp.Entries = append(resp.Entries, ent)
	}
	writeJSON(w, http.StatusOK, resp)
}

type fsMkdirReq struct {
	Path  string `json:"path"`
	Owner string `json:"owner"` // optional "uid:gid", applied to created dirs only
}

func (s *Server) handleFsMkdir(w http.ResponseWriter, r *http.Request) {
	var req fsMkdirReq
	if !readJSON(w, r, &req) {
		return
	}
	path, err := cleanAbs(req.Path)
	if err != nil || path == "/" {
		badRequest(w, "path must be an absolute directory path")
		return
	}
	var uid, gid int
	if req.Owner != "" {
		u, g, ok := strings.Cut(req.Owner, ":")
		var err1, err2 error
		uid, err1 = strconv.Atoi(u)
		if ok {
			gid, err2 = strconv.Atoi(g)
		} else {
			gid = uid
		}
		if err1 != nil || err2 != nil || uid < 0 || gid < 0 {
			badRequest(w, "owner must be numeric uid:gid")
			return
		}
	}

	// Track which dirs MkdirAll creates so a requested owner lands on the
	// new dirs and never on pre-existing parents.
	var created []string
	for p := path; p != "/"; p = filepath.Dir(p) {
		if _, err := os.Stat(p); err == nil {
			break
		}
		created = append(created, p)
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		writeError(w, err)
		return
	}
	if req.Owner != "" {
		for _, p := range created {
			if err := os.Chown(p, uid, gid); err != nil {
				writeError(w, err)
				return
			}
		}
	}
	s.audit(r, "fs.mkdir", path, map[string]any{"owner": req.Owner})

	info, err := os.Stat(path)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, fsEntry{
		Name:  filepath.Base(path),
		Path:  path,
		Dir:   true,
		Owner: ownerOf(info),
		Mode:  info.Mode().String(),
	})
}
