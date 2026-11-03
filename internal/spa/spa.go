// Package spa serves a built single-page frontend: fogd's own UI
// (internal/ui) and the installer wizard (internal/setupui). It lives apart
// from both so neither binary carries the other's embedded frontend.
package spa

import (
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// FromDist serves the Vite build under dist/app when one was built in, and
// otherwise the tracked placeholder at dist/index.html, which says so
// instead of rendering blank.
func FromDist(dist fs.FS) http.Handler {
	if app, err := fs.Sub(dist, "dist/app"); err == nil {
		if _, err := fs.Stat(app, "index.html"); err == nil {
			return Handler(app)
		}
	}
	placeholder, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err) // embed guarantees dist exists at compile time
	}
	return Handler(placeholder)
}

// Handler serves files out of root, and index.html for any path that isn't
// one, so client-side routing survives a refresh or a pasted deep link.
//
// Hashed assets can be cached forever; index.html must not be — including
// when it arrives as the fallback for a deep link, or a browser that cached
// it there would keep loading the previous release's asset names.
func Handler(root fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(root))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if p == "" {
			p = "index.html"
		}
		if _, err := fs.Stat(root, p); err != nil {
			// SPA fallback — serve index.html and let the router handle it.
			w.Header().Set("Cache-Control", "no-cache")
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/"
			fileServer.ServeHTTP(w, r2)
			return
		}
		if strings.HasPrefix(p, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		fileServer.ServeHTTP(w, r)
	})
}
