// The MCP surface: /mcp, outside /api like /app/v1, so session auth and the
// Origin guard never apply. The credential is an MCP bearer token — one per
// connected client, named, created and revoked from Settings, stored as a
// digest with the same discipline as sessions and app tokens. The tools
// themselves live in internal/mcpserver; this file owns who gets in, under
// what name, and how the tokens come to exist.
package server

import (
	"errors"
	"net/http"
	"strings"
	"unicode"

	"gitea.lan/matthew/fog/internal/auth"
	"gitea.lan/matthew/fog/internal/mcpserver"
	"gitea.lan/matthew/fog/internal/store"
)

// maxMCPTokenName keeps a name readable in the token list and short enough
// that the audit actor column stays scannable.
const maxMCPTokenName = 40

// mcpAuthed gates /mcp on any live MCP token. 401 (not 403): the token
// either matches or it doesn't. Rate limited before the digest work so a
// brute-force loop pays for itself in 429s — keyed by source IP, not one
// shared bucket, so a brute-forcer throttles only itself and never the
// legitimate clients (or one busy client another). The matched token's name
// rides the context on to the tools, which audit under it.
func (s *Server) mcpAuthed(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.limitStranger(w, r, "mcp", "rate limited") {
			return
		}
		header := r.Header.Get("Authorization")
		token := strings.TrimPrefix(header, "Bearer ")
		if token == "" || token == header {
			writeErr(w, http.StatusUnauthorized, "MCP bearer token required")
			return
		}
		// Lookup is by digest, like sessions and app tokens: the presented
		// token is hashed and matched by index, so no plaintext is compared
		// and no row is examined unless it already hashes the same.
		tok, err := s.store.MCPTokenByDigest(auth.DigestToken(token))
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "unrecognized token")
			return
		}
		next.ServeHTTP(w, r.WithContext(mcpserver.WithToken(r.Context(), tok)))
	})
}

func (s *Server) handleListMCPTokens(w http.ResponseWriter, r *http.Request) {
	tokens, err := s.store.ListMCPTokens()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": tokens})
}

// handleCreateMCPToken mints a token for one client and returns the
// plaintext this once. Nothing is replaced: every client gets its own, so
// connecting a second assistant never logs the first one out.
func (s *Server) handleCreateMCPToken(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name            string `json:"name"`
		CanPostChat     bool   `json:"canPostChat"`
		CanReadAllFiles bool   `json:"canReadAllFiles"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	name := strings.TrimSpace(body.Name)
	if msg := validateMCPTokenName(name); msg != "" {
		badRequest(w, msg)
		return
	}
	token, digest := auth.NewToken()
	tok, err := s.store.CreateMCPToken(name, digest, body.CanPostChat, body.CanReadAllFiles)
	if errors.Is(err, store.ErrNameTaken) {
		conflict(w, "a token named "+name+" already exists")
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	// The rights ride the audit entry: a broad token is the one an operator
	// reviewing the log needs to be able to find.
	s.audit(r, "mcp.token.create", name, map[string]any{"canPostChat": tok.CanPostChat, "canReadAllFiles": tok.CanReadAllFiles})
	writeJSON(w, http.StatusOK, map[string]any{
		"id":              tok.ID,
		"name":            tok.Name,
		"canPostChat":     tok.CanPostChat,
		"canReadAllFiles": tok.CanReadAllFiles,
		"createdAt":       tok.CreatedAt,
		"token":           token,
	})
}

func (s *Server) handleDeleteMCPToken(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	// Read the name first: after the delete there is nothing left to name it
	// in the audit entry.
	name := ""
	if tok, err := s.store.MCPTokenByID(id); err == nil {
		name = tok.Name
	}
	if err := s.store.DeleteMCPToken(id); errors.Is(err, store.ErrNotFound) {
		notFound(w, "no such token")
		return
	} else if err != nil {
		writeError(w, err)
		return
	}
	s.audit(r, "mcp.token.revoke", name, nil)
	writeOK(w)
}

// validateMCPTokenName returns the operator-facing reason a name is
// unusable, or "" when it's fine. Names are labels, not identifiers, so the
// rule is only: present, short, and printable on one line.
func validateMCPTokenName(name string) string {
	switch {
	case name == "":
		return "name the client this token is for, e.g. claude-code"
	case len(name) > maxMCPTokenName:
		return "name is too long (max 40 characters)"
	}
	for _, r := range name {
		if !unicode.IsPrint(r) {
			return "name must be printable text"
		}
	}
	return ""
}
