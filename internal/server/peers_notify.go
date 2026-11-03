// The push half of the app-facing federation surface. GET
// /app/v1/connect/peers answers "who can this app federate with"; on its
// own that is a question an app only thinks to ask when a human is looking
// at a settings page, so a household paired here stays invisible over there
// until someone reloads. This tells the app to ask again.
//
// The notice deliberately carries no peer data. It says "the directory
// changed" and nothing more: the GET stays the single authority, two
// notices racing cannot write a stale list into an app, and an app that
// missed one while it was redeploying is corrected by its own next poll
// instead of sitting on half a truth. The contract still
// holds — an app that never implements the callback loses nothing but
// latency.
package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"gitea.lan/matthew/fog/internal/appcall"
)

// AppDirectoryCallbackPath is the optional fog-connect/1 endpoint an app
// may serve (docs/connect-contract.md). Apps that don't answer 404 — a
// no-op, not a failure.
const AppDirectoryCallbackPath = "/fog/connect/peers/changed"

// Reasons carried in the notice. Purely diagnostic: an app re-reads the
// directory either way, but "why" is what makes its log line readable.
const (
	DirChangePaired      = "peer.paired"
	DirChangeUnpaired    = "peer.unpaired"
	DirChangeUpdated     = "peer.updated"
	DirChangeAdvertised  = "peer.directory"
	dirNotifyCallTimeout = 10 * time.Second
)

// directoryFingerprint reduces the app-visible directory to what an app
// could actually act on: who is listed, what they are called, and which
// apps they advertise. Online and lastSeenAt are excluded on purpose —
// they are clock-derived and move on every heartbeat, and a push every
// thirty seconds costs an app more than a status dot that greys over a
// poll late.
func directoryFingerprint(peers []appPeerView) string {
	h := sha256.New()
	for _, p := range peers {
		fmt.Fprintf(h, "%s\x1f%s\x1f%s\x1e", p.Name, p.DisplayName, p.InstanceID)
		for _, a := range p.Apps {
			fmt.Fprintf(h, "%s\x1f%s\x1f%s\x1f%s\x1e",
				a.App, a.Title, a.URL, strings.Join(a.Contracts, ","))
		}
		h.Write([]byte("\x1d"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// notifyAppsDirectoryChanged fans the notice out in the background. Call it
// anywhere the visible set *might* have moved — pairing in either role,
// unpairing, a rename or relabel, a capability toggle, the heartbeat that
// refreshes a peer's advertised apps. The fingerprint gate below turns the
// calls that changed nothing an app can see into no-ops, so callers never
// have to work out whether their edit mattered.
func (s *Server) notifyAppsDirectoryChanged(reason string) {
	s.goSafe("federation directory notify", func() { s.pushDirectoryChange(reason) })
}

func (s *Server) pushDirectoryChange(reason string) {
	peers, err := s.appPeerViews()
	if err != nil {
		s.log.Warn("federation: directory notify", "err", err)
		return
	}
	if !s.dirNotify.changed(directoryFingerprint(peers)) {
		return
	}

	list, err := s.store.AppsWithContract(ContractConnectV1)
	if err != nil {
		return
	}
	body := map[string]string{
		"reason":    reason,
		"changedAt": time.Now().UTC().Format(time.RFC3339),
	}
	s.fanOutApps(s.bgContext(), list, dirNotifyCallTimeout, func(ctx context.Context, t *appcall.Target) {
		// Debug, not Warn: the endpoint is optional, so "app answered
		// 404" is the expected shape of an app that never opted in.
		if _, err := t.Call(ctx, "POST", AppDirectoryCallbackPath, body); err != nil {
			s.log.Debug("federation: directory notify", "app", t.App.Name, "err", err)
		}
	})
}
