// Flare triage API: what the Errors tab, the fleet inbox, and the dashboard
// badges read and act on. Session-authed like the rest of /api — the
// unauthenticated ingest surface lives in flare_handlers.go.
package server

import (
	"encoding/json"
	"net/http"
	"time"

	"gitea.lan/matthew/fog/internal/store"
)

// errorGroupWithApp decorates a group with display names for cross-app
// views; per-app endpoints return bare groups. Origin is the peer that
// relayed the group, empty for local ones.
type errorGroupWithApp struct {
	store.ErrorGroup
	App    string `json:"app"`
	Origin string `json:"origin,omitempty"`
}

// handleFleetErrors is the fleet inbox: open and regressed groups across
// every app — local and peer-relayed — newest first.
func (s *Server) handleFleetErrors(w http.ResponseWriter, r *http.Request) {
	groups, err := s.store.ListOpenErrorGroupsAll(200)
	if err != nil {
		writeError(w, err)
		return
	}
	peers, err := s.store.ListFedPeers()
	if err != nil {
		writeError(w, err)
		return
	}
	peerNames := map[int64]string{}
	for _, p := range peers {
		// The origin shown on a relayed error follows the label, so
		// renaming a peer renames it everywhere a human reads it.
		peerNames[p.ID] = p.DisplayName()
	}
	out := make([]errorGroupWithApp, 0, len(groups))
	for _, g := range groups {
		out = append(out, errorGroupWithApp{ErrorGroup: g, App: g.AppName, Origin: peerNames[g.PeerID]})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleListAppErrors(w http.ResponseWriter, r *http.Request) {
	app, ok := s.appFromPath(w, r)
	if !ok {
		return
	}
	status := r.URL.Query().Get("status")
	switch status {
	case "", "open", "regressed", "resolved", "ignored":
	default:
		badRequest(w, "unknown status filter")
		return
	}
	groups, err := s.store.ListErrorGroups(app.ID, status, 200)
	if err != nil {
		writeError(w, err)
		return
	}
	if groups == nil {
		groups = []store.ErrorGroup{}
	}
	writeJSON(w, http.StatusOK, groups)
}

// errorGroupFor resolves {name}/{id} and enforces that the group belongs to
// the named app — a group id under the wrong app is a 404, same as a
// missing one.
func (s *Server) errorGroupFor(w http.ResponseWriter, r *http.Request) (*store.App, *store.ErrorGroup, bool) {
	app, ok := s.appFromPath(w, r)
	if !ok {
		return nil, nil, false
	}
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return nil, nil, false
	}
	g, err := s.store.ErrorGroupByID(id)
	if err != nil || g.AppID != app.ID {
		notFound(w, "error group not found")
		return nil, nil, false
	}
	return app, g, true
}

// errorGroupByID resolves {id} without app scoping — the global endpoints
// the fleet inbox uses, which must reach peer groups too.
func (s *Server) errorGroupByID(w http.ResponseWriter, r *http.Request) (*store.ErrorGroup, bool) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return nil, false
	}
	g, err := s.store.ErrorGroupByID(id)
	if err != nil {
		notFound(w, "error group not found")
		return nil, false
	}
	return g, true
}

func (s *Server) handleGetErrorGroup(w http.ResponseWriter, r *http.Request) {
	if _, g, ok := s.errorGroupFor(w, r); ok {
		s.writeGroupDetail(w, g)
	}
}

func (s *Server) handleGetErrorGroupGlobal(w http.ResponseWriter, r *http.Request) {
	if g, ok := s.errorGroupByID(w, r); ok {
		s.writeGroupDetail(w, g)
	}
}

func (s *Server) writeGroupDetail(w http.ResponseWriter, g *store.ErrorGroup) {
	events, err := s.store.ListErrorEvents(g.ID, 50)
	if err != nil {
		writeError(w, err)
		return
	}
	if events == nil {
		events = []store.ErrorEvent{}
	}
	out := map[string]any{"group": g, "events": events}
	// Peer groups carry the peer's release SHA. When this instance runs the
	// same catalog app, surface what IT runs — a peer group first seen on a
	// SHA already deployed here usually means "wait for their update poll,"
	// not "write code." Only stated when both sides are actually known.
	if g.PeerID != 0 && g.LastRelease != "" {
		if app, err := s.store.AppByName(g.AppName); err == nil && app.DeployedCommit != "" {
			out["localRelease"] = app.DeployedCommit
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetErrorEvent(w http.ResponseWriter, r *http.Request) {
	if _, g, ok := s.errorGroupFor(w, r); ok {
		s.writeGroupEvent(w, r, g)
	}
}

func (s *Server) handleGetErrorEventGlobal(w http.ResponseWriter, r *http.Request) {
	if g, ok := s.errorGroupByID(w, r); ok {
		s.writeGroupEvent(w, r, g)
	}
}

func (s *Server) writeGroupEvent(w http.ResponseWriter, r *http.Request, g *store.ErrorGroup) {
	eid, ok := pathInt64(w, r, "eid")
	if !ok {
		return
	}
	e, err := s.store.ErrorEventByID(eid)
	if err != nil || e.GroupID != g.ID {
		notFound(w, "event not found")
		return
	}
	// The stored payload is JSON already; hand it over raw instead of
	// re-encoding it as one giant string the UI would have to parse again.
	writeJSON(w, http.StatusOK, map[string]any{
		"id": e.ID, "eventId": e.EventID, "receivedAt": e.ReceivedAt,
		"release": e.Release, "environment": e.Environment, "transaction": e.Txn,
		"payload": json.RawMessage(e.Payload),
	})
}

func (s *Server) handleResolveErrorGroup(w http.ResponseWriter, r *http.Request) {
	if _, g, ok := s.errorGroupFor(w, r); ok {
		s.doResolve(w, r, g)
	}
}

func (s *Server) handleResolveErrorGroupGlobal(w http.ResponseWriter, r *http.Request) {
	if g, ok := s.errorGroupByID(w, r); ok {
		s.doResolve(w, r, g)
	}
}

func (s *Server) doResolve(w http.ResponseWriter, r *http.Request, g *store.ErrorGroup) {
	var req struct {
		// NextDeploy pins the resolution to a release: further events from
		// that release stay resolved (the fix isn't out yet), and only a
		// later one regresses the group. For local groups the pin is the
		// app's deployed commit; for peer groups it's the release the peer
		// last reported — the best knowledge of their fleet we have.
		NextDeploy bool `json:"nextDeploy"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	release := ""
	if req.NextDeploy {
		if g.PeerID != 0 {
			release = g.LastRelease
		} else if app, err := s.store.AppByID(g.AppID); err == nil {
			release = app.DeployedCommit
		}
	}
	if err := s.store.ResolveErrorGroup(g.ID, release); err != nil {
		writeError(w, err)
		return
	}
	s.audit(r, "app.errors.resolve", g.AppName, map[string]any{"group": g.ID, "nextDeploy": req.NextDeploy})
	g, _ = s.store.ErrorGroupByID(g.ID)
	writeJSON(w, http.StatusOK, g)
}

func (s *Server) handleIgnoreErrorGroup(w http.ResponseWriter, r *http.Request) {
	if _, g, ok := s.errorGroupFor(w, r); ok {
		s.doIgnore(w, r, g)
	}
}

func (s *Server) handleIgnoreErrorGroupGlobal(w http.ResponseWriter, r *http.Request) {
	if g, ok := s.errorGroupByID(w, r); ok {
		s.doIgnore(w, r, g)
	}
}

func (s *Server) doIgnore(w http.ResponseWriter, r *http.Request, g *store.ErrorGroup) {
	var req struct {
		ForHours   int   `json:"forHours"`   // 0 = no time limit
		UntilCount int64 `json:"untilCount"` // 0 = no count threshold
	}
	if !readJSON(w, r, &req) {
		return
	}
	until := ""
	if req.ForHours > 0 {
		until = time.Now().UTC().Add(time.Duration(req.ForHours) * time.Hour).Format(time.RFC3339)
	}
	if err := s.store.IgnoreErrorGroup(g.ID, until, req.UntilCount); err != nil {
		writeError(w, err)
		return
	}
	s.audit(r, "app.errors.ignore", g.AppName, map[string]any{"group": g.ID, "forHours": req.ForHours, "untilCount": req.UntilCount})
	g, _ = s.store.ErrorGroupByID(g.ID)
	writeJSON(w, http.StatusOK, g)
}

func (s *Server) handleUnresolveErrorGroup(w http.ResponseWriter, r *http.Request) {
	if _, g, ok := s.errorGroupFor(w, r); ok {
		s.doUnresolve(w, r, g)
	}
}

func (s *Server) handleUnresolveErrorGroupGlobal(w http.ResponseWriter, r *http.Request) {
	if g, ok := s.errorGroupByID(w, r); ok {
		s.doUnresolve(w, r, g)
	}
}

func (s *Server) doUnresolve(w http.ResponseWriter, r *http.Request, g *store.ErrorGroup) {
	if err := s.store.UnresolveErrorGroup(g.ID); err != nil {
		writeError(w, err)
		return
	}
	s.audit(r, "app.errors.unresolve", g.AppName, map[string]any{"group": g.ID})
	g, _ = s.store.ErrorGroupByID(g.ID)
	writeJSON(w, http.StatusOK, g)
}
