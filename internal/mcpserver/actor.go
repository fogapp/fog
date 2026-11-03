// Who a tool call is acting as. Each MCP token is named for the client
// holding it, and that name rides the request context from the auth layer
// down to the audit log — so "restart Jellyfin" reads as mcp:claude-code or
// mcp:openclaw, not as an anonymous "mcp" shared by every connected client.
package mcpserver

import (
	"context"

	"gitea.lan/matthew/fog/internal/store"
)

type actorCtxKey struct{}

// callerToken is what a tool may know about the token behind its call.
type callerToken struct {
	name            string
	canPostChat     bool
	canReadAllFiles bool
}

// ActorPrefix marks every MCP-made change in the audit log. Kept as a prefix
// (mcp:<token name>) so a filter on "mcp" still finds all of them.
const ActorPrefix = "mcp"

// WithToken labels ctx with the token that authenticated the request.
// server.mcpAuthed calls this; tools read it back with actorFor and
// canPostChat, so none has to look the token up again.
func WithToken(ctx context.Context, tok *store.MCPToken) context.Context {
	if tok == nil || tok.Name == "" {
		return ctx
	}
	return context.WithValue(ctx, actorCtxKey{}, callerToken{name: tok.Name, canPostChat: tok.CanPostChat, canReadAllFiles: tok.CanReadAllFiles})
}

func tokenFor(ctx context.Context) (callerToken, bool) {
	t, ok := ctx.Value(actorCtxKey{}).(callerToken)
	return t, ok
}

// actorFor is the audit identity for this call. An unlabelled context — a
// direct handler test, a caller that bypassed mcpAuthed — is still honestly
// "some MCP client", so it degrades to the bare prefix rather than lying
// about which one.
func actorFor(ctx context.Context) string {
	if t, ok := tokenFor(ctx); ok {
		return ActorPrefix + ":" + t.name
	}
	return ActorPrefix
}

// canPostChat reports whether the calling token was created with chat
// posting allowed; an unlabelled context never is.
func canPostChat(ctx context.Context) bool {
	t, ok := tokenFor(ctx)
	return ok && t.canPostChat
}

// canReadAllFiles reports whether the calling token's Drive tools reach
// Apps/ and dotfiles. An unlabelled context never does: the narrow reach is
// the default, and a caller that skipped mcpAuthed has proved nothing.
func canReadAllFiles(ctx context.Context) bool {
	t, ok := tokenFor(ctx)
	return ok && t.canReadAllFiles
}
