// The app page's Updates tab: what this app has run, what is newer, and the
// two verbs that change either — flip auto-updates, or go back to a version
// it ran before.
package server

import (
	"errors"
	"net/http"
	"strconv"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/updates"
)

// defaultVersionRows is what the tab lists: enough to reach past a bad
// update to the last version that worked, without turning a rollback
// question into a changelog.
const defaultVersionRows = 5

type appVersionRow struct {
	Ref   string `json:"ref"`
	Short string `json:"short"`
	Image string `json:"image,omitempty"`
	Rev   int64  `json:"rev"`
	Actor string `json:"actor,omitempty"`
	// At is when this version last went live, FirstAt when it first did.
	At      string `json:"at"`
	FirstAt string `json:"firstAt,omitempty"`
	Deploys int    `json:"deploys,omitempty"`
	Current bool   `json:"current"`
}

type appVersionsResponse struct {
	Source string `json:"source" ts:"\"image\" | \"git\" | \"\""`         // image | git
	Policy string `json:"policy" ts:"\"manual\" | \"notify\" | \"auto\""` // manual | notify | auto
	// Current is the ref of the version running now, as far as fog saw it
	// deploy. Empty for an app whose last deploy predates this record.
	Current  string          `json:"current,omitempty"`
	Versions []appVersionRow `json:"versions"`
	// The last check's findings, verbatim from app_updates — the same facts
	// the update badge is drawn from, so the tab and the badge cannot
	// disagree.
	Available       string `json:"available,omitempty"`
	UpdateAvailable bool   `json:"updateAvailable"`
	Comparable      bool   `json:"comparable"`
	CheckedAt       string `json:"checkedAt,omitempty"`
}

// handleAppVersions answers "where has this app been, and is there anything
// newer" in one request. Every row is a deploy fog performed; nothing here
// is inferred from a tag, which by now points somewhere else.
func (s *Server) handleAppVersions(w http.ResponseWriter, r *http.Request) {
	app, ok := s.appFromPath(w, r)
	if !ok {
		return
	}
	limit := defaultVersionRows
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 50 {
			limit = n
		}
	}
	resp := appVersionsResponse{Policy: "manual", Versions: []appVersionRow{}}
	if spec, err := apps.LatestSpec(s.store, app.ID); err == nil {
		resp.Source = spec.Source.Type
		if spec.UpdatePolicy != "" {
			resp.Policy = spec.UpdatePolicy
		}
	}
	if cur, err := s.store.CurrentAppVersion(app.ID); err == nil {
		resp.Current = cur.Ref
	}
	if u, err := s.store.AppUpdateFor(app.ID); err == nil {
		resp.Available = apps.ShortRef(u.RemoteDigest)
		resp.UpdateAvailable = u.Available
		resp.Comparable = u.LocalDigest != "" && u.RemoteDigest != ""
		resp.CheckedAt = u.CheckedAt
	}
	list, err := s.store.ListAppVersions(app.ID, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not read this app's versions")
		return
	}
	for _, v := range list {
		resp.Versions = append(resp.Versions, appVersionRow{
			Ref: v.Ref, Short: apps.ShortRef(v.Ref), Image: v.Image, Rev: v.Rev, Actor: v.Actor,
			At: v.CreatedAt, FirstAt: v.FirstAt, Deploys: v.Deploys, Current: v.Ref == resp.Current,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

type rollbackReq struct {
	Ref string `json:"ref"`
}

// handleAppRollback redeploys the app on a version it has run before. Like
// an update it answers as soon as the work starts — the deploy outlives the
// request — and refuses to stack on a deploy already in flight.
func (s *Server) handleAppRollback(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req rollbackReq
	if !readJSON(w, r, &req) {
		return
	}
	if req.Ref == "" {
		badRequest(w, "which version to go back to")
		return
	}
	app, ok := s.appFromPath(w, r)
	if !ok {
		return
	}
	// Check the version here as well as in Rollback: a ref this app never
	// ran is the caller's mistake, and saying so in the response beats
	// accepting the request and failing invisibly in a goroutine.
	if _, err := s.store.AppVersionByRef(app.ID, req.Ref); err != nil {
		badRequest(w, "fog has no record of "+name+" running that version")
		return
	}
	if s.engine.Deploying(name) {
		conflict(w, updates.ErrUpdateInProgress.Error())
		return
	}
	actor := s.actor(r)
	s.auditAs(actor, "app.rollback", name, map[string]any{"ref": req.Ref})
	s.goSafe("rollback", func() {
		ctx, cancel := s.deployCtx()
		defer cancel()
		var cancelled *apps.CancelledError
		if err := s.updates.Rollback(ctx, name, req.Ref, actor); errors.As(err, &cancelled) {
			s.log.Info("rollback", "app", name, "ref", req.Ref, "result", err)
		} else if err != nil {
			s.log.Error("rollback", "app", name, "ref", req.Ref, "err", err)
			s.updates.Notify("Rollback failed: "+name, err.Error(), false)
		} else {
			s.updates.Notify("Rolled back: "+name, "now running "+apps.ShortRef(req.Ref), true)
		}
	})
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}

type updatePolicyReq struct {
	Policy string `json:"policy"` // manual | notify | auto
}

// handleAppUpdatePolicy sets how this app handles new versions. The policy
// lives in the app's spec, so changing it writes a revision like any other
// config change — the flip shows up in History with who did it, rather than
// happening to the app from nowhere.
//
// No deploy: the checker reads the policy from the newest revision, so it
// takes effect at the next check without disturbing what is running.
func (s *Server) handleAppUpdatePolicy(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req updatePolicyReq
	if !readJSON(w, r, &req) {
		return
	}
	switch req.Policy {
	case "manual", "notify", "auto":
	default:
		badRequest(w, "policy must be manual, notify, or auto")
		return
	}
	app, ok := s.appFromPath(w, r)
	if !ok {
		return
	}
	spec, err := apps.LatestSpec(s.store, app.ID)
	if err != nil {
		badRequest(w, "could not read this app's configuration")
		return
	}
	current := spec.UpdatePolicy
	if current == "" {
		current = "manual"
	}
	if current == req.Policy {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "policy": req.Policy})
		return
	}
	// "manual" is the spec's zero value; storing it as the empty string keeps
	// a spec that means the default looking like the default.
	spec.UpdatePolicy = req.Policy
	if req.Policy == "manual" {
		spec.UpdatePolicy = ""
	}
	// The policy lives in the spec, so saving it re-validates the whole
	// spec. A stored spec this fogd no longer considers valid fails here
	// with a complaint about some unrelated field — say which change was
	// refused, or "route port 0 out of range" arrives as the answer to
	// flicking a switch labelled "Update automatically".
	if err := s.engine.UpdateSpec(r.Context(), name, spec, nil, "updates: "+current+" → "+req.Policy, s.actor(r)); err != nil {
		badRequest(w, "couldn't save the update policy — "+name+"'s configuration was rejected: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "policy": req.Policy})
}
