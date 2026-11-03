// Package setupui embeds the built installer wizard. Vite builds into
// dist/app (gitignored, wiped by every build); the tracked placeholder at
// dist/index.html keeps the Go tree compiling — and honestly explaining
// itself — when no frontend build has run. Same pattern as internal/ui.
package setupui

import (
	"embed"
	"net/http"

	"gitea.lan/matthew/fog/internal/spa"
)

//go:embed all:dist
var distFS embed.FS

func Handler() http.Handler { return spa.FromDist(distFS) }
