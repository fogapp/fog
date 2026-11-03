// The app-facing surface: /app/v1, outside /api like /ingest and /fed, so
// session auth and the Origin guard never apply. Auth is the app's own
// FOG_API_TOKEN (digest lookup, session discipline). This is how managed
// apps see the federation — and, by design, an app that
// never calls it loses nothing: fog is a convenience, not a dependency.
package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"gitea.lan/matthew/fog/internal/auth"
	"gitea.lan/matthew/fog/internal/fed"
	"gitea.lan/matthew/fog/internal/store"
)

// appAuthed resolves the bearer token to a managed app, rate limited per
// app. 401 (not 403): the token either identifies an app or it doesn't.
func (s *Server) appAuthed(w http.ResponseWriter, r *http.Request) (*store.App, bool) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token == "" || token == r.Header.Get("Authorization") {
		writeErr(w, http.StatusUnauthorized, "FOG_API_TOKEN required")
		return nil, false
	}
	app, err := s.store.AppByAPITokenDigest(auth.DigestToken(token))
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "unrecognized token")
		return nil, false
	}
	if !limit(w, s.appAPILimiter, "app:"+app.Name, "rate limited") {
		return nil, false
	}
	return app, true
}

// appPeerView is one federated peer as an app sees it: where, whether it's
// alive, and which of its apps are exposed. No keys, no capabilities, no
// URLs fogd wouldn't want re-shared — the minimum discovery needs.
//
// Name and DisplayName are deliberately two fields rather than one. Name is
// the addressing key — Connect resolves a peer by it, so it stays the
// stable slug this instance paired under, whatever the operator renames the
// peer to. DisplayName is what an app should put in front of a human: the
// operator's label when they set one, the same name otherwise. An app that
// shows Name shows a peer by a name its operator has already replaced.
type appPeerView struct {
	Name        string             `json:"name"`
	DisplayName string             `json:"displayName"`
	InstanceID  string             `json:"instanceId"`
	Online      bool               `json:"online"`
	LastSeenAt  string             `json:"lastSeenAt,omitempty"`
	Apps        []fed.DirectoryApp `json:"apps"`
}

// handleAppConnectPeers lists the peers this instance has chosen to
// expose to its apps (the use:apps.directory toggle), with each peer's
// advertised app directory from its last heartbeat. An unfederated fog
// returns an empty list, so apps degrade to standalone behavior with zero
// special-casing.
func (s *Server) handleAppConnectPeers(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.appAuthed(w, r); !ok {
		return
	}
	out, err := s.appPeerViews()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"peers": out})
}

// handleAppConnectReachability answers an app's "can the internet dial
// this household?" with the very object the Connect page shows the
// operator — computeReachability, shared, so the two can never disagree. An
// app that delivers to the public (a feed's outbox, a mail relay) reads this
// to say so honestly in its own UI and to pause public delivery instead of
// burning attempts against a box that only dials out.
func (s *Server) handleAppConnectReachability(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.appAuthed(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.computeReachability())
}

// appVisiblePeers is the set of peers the operator exposed to apps
// (use:apps.directory). Apps see these and only these — in the directory,
// and when connecting — so every one of those asks here.
func (s *Server) appVisiblePeers() (map[int64]bool, error) {
	rows, err := s.store.FedCapabilityConfigs(store.CapAppsDirectory, store.DirUse)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]bool, len(rows))
	for id := range rows {
		out[id] = true
	}
	return out, nil
}

// peerVisibleToApps is appVisiblePeers for one peer.
func (s *Server) peerVisibleToApps(p *store.FedPeer) bool {
	_, visible, _ := s.store.FedCapabilityConfig(p.ID, store.CapAppsDirectory, store.DirUse)
	return visible
}

// appPeerViews builds that list. Split out because the change notification
// (peers_notify.go) has to answer the same question — "what would an app
// see right now" — and a second definition of visible would eventually
// disagree with this one, pushing apps about changes they cannot see or
// staying quiet about ones they can.
func (s *Server) appPeerViews() ([]appPeerView, error) {
	peers, err := s.store.ListFedPeers()
	if err != nil {
		return nil, err
	}
	visible, err := s.appVisiblePeers()
	if err != nil {
		return nil, err
	}
	out := []appPeerView{}
	for _, p := range peers {
		if !visible[p.ID] {
			continue
		}
		var dir []fed.DirectoryApp
		json.Unmarshal(p.Directory, &dir)
		if dir == nil {
			dir = []fed.DirectoryApp{}
		}
		out = append(out, appPeerView{
			Name:        p.Name,
			DisplayName: p.DisplayName(),
			InstanceID:  p.InstanceID,
			Online:      fed.Online(&p, time.Now()),
			LastSeenAt:  p.LastSeenAt,
			Apps:        dir,
		})
	}
	return out, nil
}
