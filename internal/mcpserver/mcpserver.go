// Package mcpserver is fogd's MCP (Model Context Protocol) surface: a
// streamable-HTTP server that lets an AI client manage the box — app
// statuses and lifecycle, logs, the issues inbox, update checks — through
// the same service layer the web UI uses. Read plus safe-manage only:
// nothing destructive (delete, restore, reboot, self-update, exec) is
// exposed here; those stay behind the UI.
//
// Auth is the operator's MCP bearer token, enforced by the HTTP layer
// (server.mcpAuthed) before a request ever reaches this handler. Two rights
// beyond that are per token and off unless the operator chose them at
// creation: posting to chat, and Drive reads that reach Apps/ and dotfiles
// (tools_files.go). Both ride the context (actor.go).
package mcpserver

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/chat"
	"gitea.lan/matthew/fog/internal/config"
	"gitea.lan/matthew/fog/internal/dockerx"
	"gitea.lan/matthew/fog/internal/files"
	"gitea.lan/matthew/fog/internal/issues"
	"gitea.lan/matthew/fog/internal/metrics"
	"gitea.lan/matthew/fog/internal/store"
	"gitea.lan/matthew/fog/internal/updates"
)

// Deps mirrors server.Deps for the subsystems the tools touch. Everything
// but Store and Log may be nil (tests, partial wiring); each tool guards
// what it needs and reports a plain error instead of panicking.
type Deps struct {
	Store   *store.Store
	Engine  *apps.Engine
	Docker  *dockerx.Client
	Issues  *issues.Scanner
	Updates *updates.Checker
	Metrics *metrics.Sampler
	// Files is fog's own file tree; nil means the files tools report
	// themselves unavailable.
	Files *files.Manager
	// Chat is the sealed room server; nil means the chat tools report
	// themselves unavailable.
	Chat *chat.Service
	Log  *slog.Logger
}

type server struct {
	Deps
}

// New builds the MCP server with all tools registered and returns it as an
// http.Handler ready to mount. Stateless + JSON responses: every call is a
// self-contained POST, so there are no long-lived streams to reason about
// during shutdown and no session table to leak.
func New(deps Deps) http.Handler {
	s := &server{deps}
	srv := mcp.NewServer(&mcp.Implementation{Name: "fog", Version: config.Version}, nil)
	s.addReadTools(srv)
	s.addManageTools(srv)
	s.addFilesTools(srv)
	s.addChatTools(srv)
	return mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, Logger: deps.Log},
	)
}

var (
	errNoEngine  = errors.New("the app engine is not available")
	errNoDocker  = errors.New("docker is not available")
	errNoIssues  = errors.New("the issues scanner is not running")
	errNoUpdates = errors.New("the update checker is not available")
)

// detach runs tool-initiated work that outlives the call (deploys, sweeps)
// with its own generous deadline and a recover wrapper — the panic story
// from server.goSafe, which this package can't reach.
func (s *server) detach(task string, fn func(ctx context.Context)) {
	go func() {
		defer func() {
			if v := recover(); v != nil {
				s.Log.Error("panic in mcp background task",
					"task", task, "panic", v, "stack", string(debug.Stack()))
			}
		}()
		// Same ceiling as the UI's detached deploys (deployCtx): generous
		// enough for slow image pulls, bounded so nothing leaks forever.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		fn(ctx)
	}()
}
