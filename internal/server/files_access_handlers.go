package server

// Files from outside the browser: the WebDAV mount (app passwords), share
// links (public, tokened, optionally expiring and password-protected), and
// the management of both.

import (
	"errors"
	"html/template"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"gitea.lan/matthew/fog/internal/auth"
	"gitea.lan/matthew/fog/internal/files"
	"gitea.lan/matthew/fog/internal/fsx"
	"gitea.lan/matthew/fog/internal/store"
)

// ---- WebDAV ----

// davAuthed gates /dav with HTTP Basic: any username, the password is an
// app password from files_access_tokens. Rate limited per IP like every
// other token plane, and never the session cookie — a desktop client
// should hold a revocable credential of its own.
func (s *Server) davAuthed(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.limitStranger(w, r, "dav", "rate limited") {
			return
		}
		_, pw, ok := r.BasicAuth()
		if ok && pw != "" {
			if tok, err := s.store.FilesAccessTokenByDigest(auth.DigestToken(pw)); err == nil {
				r.Header.Set("X-Fog-Dav-Client", tok.Name)
				next.ServeHTTP(w, r)
				return
			}
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="fog files"`)
		http.Error(w, "an app password from Settings → Storage → Drive is required", http.StatusUnauthorized)
	})
}

// davHeaders gives what WebDAV serves the same protection as every other
// route that serves Drive's bytes on fog's origin (handleFilesRaw,
// handleShare): a file an app planted — an .html, an .svg — must not run
// as fog with the operator's session when a browser opens it under /dav/.
// The page is sandboxed (an opaque origin, no script), its type is the one
// fog declared (the webdav package is given it, see files.davInfo), never
// sniffed, and only what a browser previews safely is offered inline.
func davHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			h := w.Header()
			h.Set("Content-Security-Policy", "sandbox")
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("Cache-Control", "private, no-cache")
			if name := path.Base(r.URL.Path); name != "" && name != "/" && name != "." && !strings.HasSuffix(r.URL.Path, "/") {
				// Declared here: the webdav package hands a GET to
				// http.ServeContent, which sniffs when no type is set.
				ct := files.MimeFor(name)
				h.Set("Content-Type", ct)
				disposition := "attachment"
				if previewableMime(ct) {
					disposition = "inline"
				}
				h.Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": name}))
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleListFilesAccessTokens(w http.ResponseWriter, r *http.Request) {
	tokens, err := s.store.ListFilesAccessTokens()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": tokens, "url": s.davURL(r)})
}

// davURL is the address a desktop client connects to, as seen from this
// request (the base domain when served through Caddy, the LAN address
// otherwise).
func (s *Server) davURL(r *http.Request) string {
	return requestOrigin(r) + "/dav/"
}

func (s *Server) handleCreateFilesAccessToken(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	name := strings.TrimSpace(body.Name)
	if msg := validateMCPTokenName(name); msg != "" {
		badRequest(w, strings.Replace(msg, "client this token", "device this password", 1))
		return
	}
	token, digest := auth.NewToken()
	tok, err := s.store.CreateFilesAccessToken(name, digest)
	if errors.Is(err, store.ErrNameTaken) {
		conflict(w, "a password named "+name+" already exists")
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	s.audit(r, "files.access.create", name, nil)
	writeJSON(w, http.StatusOK, map[string]any{"id": tok.ID, "name": tok.Name, "createdAt": tok.CreatedAt, "password": token, "url": s.davURL(r)})
}

func (s *Server) handleDeleteFilesAccessToken(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	name := ""
	if tok, err := s.store.FilesAccessTokenByID(id); err == nil {
		name = tok.Name
	}
	if err := s.store.DeleteFilesAccessToken(id); errors.Is(err, store.ErrNotFound) {
		notFound(w, "no such password")
		return
	} else if err != nil {
		writeError(w, err)
		return
	}
	s.audit(r, "files.access.revoke", name, nil)
	writeOK(w)
}

// ---- shares: management ----

func (s *Server) handleListFileShares(w http.ResponseWriter, r *http.Request) {
	rel := ""
	if p := r.URL.Query().Get("path"); p != "" {
		var err error
		if rel, err = files.CleanRel(p); err != nil {
			filesError(w, err)
			return
		}
	}
	list, err := s.files.ListShares(rel)
	if err != nil {
		filesError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"shares": list})
}

type createShareReq struct {
	Path     string `json:"path"`
	Days     int    `json:"days"` // 0 = never expires
	Password string `json:"password"`
}

func (s *Server) handleCreateFileShare(w http.ResponseWriter, r *http.Request) {
	var req createShareReq
	if !readJSON(w, r, &req) {
		return
	}
	rel, err := files.CleanRel(req.Path)
	if err != nil {
		filesError(w, err)
		return
	}
	if req.Days < 0 || req.Days > 3650 {
		badRequest(w, "days must be between 0 (never) and 3650")
		return
	}
	if req.Password != "" && len(req.Password) < minSharePassword {
		badRequest(w, "a share password needs at least "+strconv.Itoa(minSharePassword)+" characters")
		return
	}
	token, share, err := s.files.CreateShare(rel, time.Duration(req.Days)*24*time.Hour, req.Password)
	if err != nil {
		filesError(w, err)
		return
	}
	s.audit(r, "files.share.create", rel, map[string]any{"days": req.Days})
	writeJSON(w, http.StatusCreated, map[string]any{"share": share, "url": s.shareURL(r, token)})
}

func (s *Server) shareURL(r *http.Request, token string) string {
	return requestOrigin(r) + "/s/" + token
}

func (s *Server) handleDeleteFileShare(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	path := ""
	if sh, err := s.store.FileShareByID(id); err == nil {
		path = sh.Path
	}
	if err := s.files.RevokeShare(id); errors.Is(err, store.ErrNotFound) {
		notFound(w, "no such share")
		return
	} else if err != nil {
		writeError(w, err)
		return
	}
	s.audit(r, "files.share.revoke", path, nil)
	writeOK(w)
}

// ---- shares: the public page ----

var sharePage = template.Must(template.New("share").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.Title}}</title>
<style>
body{margin:0;font:15px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;background:#f4f6fa;color:#12151c}
main{max-width:56rem;margin:2rem auto;padding:0 1rem}
h1{font-size:1.15rem;margin:0 0 .25rem}
.crumbs{font-size:.85rem;color:#545e72;margin-bottom:1rem}.crumbs a{color:#545e72}
table{width:100%;border-collapse:collapse;background:#fff;border:1px solid #d2d8e3;border-radius:.5rem;overflow:hidden}
th,td{text-align:left;padding:.5rem .75rem;border-bottom:1px solid #e6eaf1;font-size:.9rem}th{font-size:.75rem;text-transform:uppercase;letter-spacing:.05em;color:#545e72}
tr:last-child td{border-bottom:0}td.n{text-align:right;color:#545e72;white-space:nowrap}a{color:#1f3a5f;text-decoration:none}a:hover{text-decoration:underline}
.btn{display:inline-block;margin:.75rem 0;padding:.4rem .8rem;border:1px solid #d2d8e3;border-radius:.375rem;background:#fff;color:#12151c;font-size:.85rem}
footer{margin-top:2rem;font-size:.75rem;color:#8891a5}
</style></head><body><main>
<h1><bdi>{{.Title}}</bdi></h1>
<div class="crumbs">{{range .Crumbs}}<a href="{{.Href}}"><bdi>{{.Label}}</bdi></a> / {{end}}</div>
<a class="btn" href="{{.ZipHref}}">Download all as zip</a>
<table><thead><tr><th>Name</th><th>Size</th><th>Modified</th></tr></thead><tbody>
{{range .Entries}}<tr><td><a href="{{.Href}}"><bdi>{{.Name}}{{if .Dir}}/{{end}}</bdi></a></td><td class="n">{{.Size}}</td><td class="n">{{.Modified}}</td></tr>{{end}}
{{if not .Entries}}<tr><td colspan="3">This folder is empty.</td></tr>{{end}}
</tbody></table>
<footer>Shared from fog{{if .Expires}} · link expires {{.Expires}}{{end}}</footer>
</main></body></html>`))

type shareRow struct {
	Name, Href, Size, Modified string
	Dir                        bool
}

// handleShare serves /s/{token} and /s/{token}/{path...}: a file streams,
// a folder renders a listing (or a zip with ?zip=1). Password-protected
// shares ask with HTTP Basic (any username). No session, rate limited,
// and the same sandboxing headers as authenticated reads.
func (s *Server) handleShare(w http.ResponseWriter, r *http.Request) {
	if !s.limitStranger(w, r, "share", "rate limited") {
		return
	}
	sh, err := s.files.ResolveShare(r.PathValue("token"))
	if err != nil {
		if errors.Is(err, files.ErrShareExpired) {
			http.Error(w, "This link has expired.", http.StatusGone)
			return
		}
		http.Error(w, "No such link.", http.StatusNotFound)
		return
	}
	if sh.HasPassword && !s.sharePasswordOK(w, r, sh) { // signin_handlers_share.go
		return
	}
	sub, err := files.CleanRel(r.PathValue("path"))
	if err != nil {
		http.Error(w, "No such file.", http.StatusNotFound)
		return
	}
	target := sh.Path
	if sub != "" {
		target = sh.Path + "/" + sub
	}
	// The share is of one entry; nothing above it is reachable.
	if !files.IsUnder(target, sh.Path) {
		http.Error(w, "No such file.", http.StatusNotFound)
		return
	}
	d, err := s.files.Stat(target)
	if err != nil {
		http.Error(w, "No such file.", http.StatusNotFound)
		return
	}
	token := r.PathValue("token")
	base := "/s/" + token
	if d.Kind != "dir" {
		f, n, err := s.files.OpenFile(target)
		if err != nil {
			http.Error(w, "No such file.", http.StatusNotFound)
			return
		}
		defer f.Close()
		disposition := "inline"
		if r.URL.Query().Get("dl") == "1" || !previewableMime(n.Mime) {
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
		return
	}
	if r.URL.Query().Get("zip") == "1" {
		_, name := files.Split(target)
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name + ".zip"}))
		if err := s.files.WriteZip(r.Context(), w, []string{target}); err != nil {
			s.log.Warn("share zip", "err", err)
		}
		return
	}
	l, err := s.files.List(target, files.ListOpts{Limit: 2000})
	if err != nil {
		http.Error(w, "No such folder.", http.StatusNotFound)
		return
	}
	_, title := files.Split(sh.Path)
	// Names are an app's to choose: shown through files.DisplayName (no
	// control or bidi character acts), each isolated in <bdi>. Links and
	// downloads keep the real name.
	crumbs := []struct{ Label, Href string }{{files.DisplayName(title), base}}
	acc := ""
	for _, seg := range strings.Split(sub, "/") {
		if seg == "" {
			continue
		}
		acc = strings.TrimPrefix(acc+"/"+seg, "/")
		crumbs = append(crumbs, struct{ Label, Href string }{files.DisplayName(seg), base + "/" + encodePath(acc)})
	}
	rows := make([]shareRow, 0, len(l.Entries))
	for _, e := range l.Entries {
		if e.Kind == "other" {
			continue
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(e.Path, sh.Path), "/")
		row := shareRow{Name: files.DisplayName(e.Name), Href: base + "/" + encodePath(rel), Dir: e.Kind == "dir", Modified: e.Modified[:10]}
		if e.Kind == "file" {
			row.Size = fsx.HumanBytes(e.Size)
		}
		rows = append(rows, row)
	}
	expires := ""
	if sh.ExpiresAt != "" {
		expires = sh.ExpiresAt[:10]
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src 'self'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	zipHref := base + "?zip=1"
	if sub != "" {
		zipHref = base + "/" + encodePath(sub) + "?zip=1"
	}
	pageTitle := title
	if sub != "" {
		_, pageTitle = files.Split(sub)
	}
	sharePage.Execute(w, map[string]any{"Title": files.DisplayName(pageTitle), "Crumbs": crumbs, "Entries": rows, "ZipHref": zipHref, "Expires": expires})
}

func previewableMime(m string) bool {
	return strings.HasPrefix(m, "image/") || strings.HasPrefix(m, "video/") || strings.HasPrefix(m, "audio/") ||
		m == "application/pdf" || strings.HasPrefix(m, "text/")
}

func encodePath(rel string) string {
	segs := strings.Split(rel, "/")
	for i, s := range segs {
		segs[i] = strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(s, "%", "%25"), "?", "%3F"), "#", "%23")
	}
	return strings.Join(segs, "/")
}
