// Import and export: compose translation on the way in, app manifests on the
// way out. Secrets never travel here — the recovery kit carries those.
package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"gitea.lan/matthew/fog/internal/apps"
	"gopkg.in/yaml.v3"
)

// ---- compose import ----

func (s *Server) handleImportCompose(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Yaml string `json:"yaml"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	services, err := apps.ImportCompose(req.Yaml)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"services": services})
}

// ---- manifest export / import ----

type manifest struct {
	Name string       `yaml:"name" json:"name"`
	Spec apps.AppSpec `yaml:"spec" json:"spec"`
}

// specYAML round-trips through JSON so manifests use the same camelCase
// field names as the API, with empty fields omitted.
func specYAML(m manifest) ([]byte, error) {
	j, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	var generic map[string]any
	if err := json.Unmarshal(j, &generic); err != nil {
		return nil, err
	}
	return yaml.Marshal(generic)
}

func yamlManifest(text string) (*manifest, error) {
	var generic map[string]any
	if err := yaml.Unmarshal([]byte(text), &generic); err != nil {
		return nil, err
	}
	j, err := json.Marshal(generic)
	if err != nil {
		return nil, err
	}
	m := &manifest{}
	return m, json.Unmarshal(j, m)
}

func (s *Server) handleExportApp(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	app, ok := s.appFromPath(w, r)
	if !ok {
		return
	}
	// A corrupt revision row must fail the export — an empty manifest with a
	// 200 would be archived as if it were the app.
	spec, err := apps.LatestSpec(s.store, app.ID)
	if err != nil {
		writeError(w, fmt.Errorf("app %s: %w", name, err))
		return
	}
	out, err := specYAML(manifest{Name: name, Spec: *spec})
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/yaml")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.fog.yaml"`, name))
	w.Write([]byte("# Fog app manifest — secrets are NOT included (names only).\n"))
	w.Write(out)
}

// handleExportAll streams every app manifest in one file — the "rebuild my
// server from one archive" export. Secrets travel via the recovery kit, not
// here.
func (s *Server) handleExportAll(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListApps()
	if err != nil {
		writeError(w, err)
		return
	}
	// Assemble before writing: once the attachment headers go out there is no
	// way left to say a manifest was bad, and a rebuild archive silently
	// missing one app is worse than no archive.
	var body bytes.Buffer
	body.WriteString("# Fog server export — import each app via 'Add app → Image or compose → Import a Fog manifest'.\n# Secrets are not included; restore them from the recovery kit or re-enter.\n")
	// One query for every app's newest spec. Raw rather than LatestSpecs,
	// which drops a corrupt row: here a corrupt row must fail the export.
	raw, err := s.store.LatestRevisionSpecs()
	if err != nil {
		writeError(w, err)
		return
	}
	for _, a := range list {
		specJSON, ok := raw[a.ID]
		if !ok {
			// An app created moments ago may have no revision yet — nothing to
			// export for it.
			continue
		}
		spec, err := apps.ParseSpec(specJSON)
		if err != nil {
			writeError(w, fmt.Errorf("app %s: %w", a.Name, err))
			return
		}
		out, err := specYAML(manifest{Name: a.Name, Spec: *spec})
		if err != nil {
			writeError(w, fmt.Errorf("app %s: %w", a.Name, err))
			return
		}
		body.WriteString("---\n")
		body.Write(out)
	}
	w.Header().Set("Content-Type", "application/yaml")
	w.Header().Set("Content-Disposition", `attachment; filename="fog-apps.yaml"`)
	w.Write(body.Bytes())
}

// handleImportManifest creates the app a manifest describes, held: it is
// not deployed, and no deploy will touch it, until an operator has read
// the review this answers with — every host mount, published port, grant
// and acknowledgement in it — and confirmed it (handleConfirmReview). A
// manifest is somebody else's spec, and a spec can bind the Docker socket.
func (s *Server) handleImportManifest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Yaml        string          `json:"yaml"`
		RestoreFrom *restoreFromReq `json:"restoreFrom,omitempty"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	m, err := yamlManifest(req.Yaml)
	if err != nil {
		badRequest(w, "not a valid manifest: "+err.Error())
		return
	}
	if m.Name == "" {
		badRequest(w, "manifest has no name")
		return
	}
	artifact := ""
	if req.RestoreFrom != nil {
		if artifact, err = s.resolveRestoreArtifact(req.RestoreFrom); err != nil {
			badRequest(w, err.Error())
			return
		}
	}
	actor := s.actor(r)
	s.wireSpec(m.Name, &m.Spec)
	if err := s.engine.CreateForReview(r.Context(), m.Name, &m.Spec, nil, actor); err != nil {
		badRequest(w, err.Error())
		return
	}
	if artifact != "" {
		if err := s.backup.StageArtifact(m.Name, &m.Spec, artifact, req.RestoreFrom.ConfirmIdentity, false, "fog-import"); err != nil {
			writeError(w, fmt.Errorf("app created but staging the restore failed: %w", err))
			return
		}
		s.cleanupUpload(req.RestoreFrom.Path)
		s.auditAs(actor, "backup.restore.staged", m.Name, nil)
	}
	s.auditAs(actor, "app.import", m.Name, nil)
	review, rev, err := s.engine.AppReview(m.Name)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, importResult{Name: m.Name, Rev: rev, Review: review, ReviewPending: true})
}

// importResult is what an import, or a look at a held app's review,
// answers: the review of one revision, and whether it still waits for a
// confirmation.
type importResult struct {
	Name          string      `json:"name"`
	Rev           int64       `json:"rev"`
	Review        apps.Review `json:"review"`
	ReviewPending bool        `json:"reviewPending"`
}

// handleAppReview lays out an app's latest revision for review — the page
// an imported app opens on, and the same layout for any app.
func (s *Server) handleAppReview(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, ok := s.appFromPath(w, r); !ok {
		return
	}
	review, rev, err := s.engine.AppReview(name)
	if err != nil {
		writeError(w, err)
		return
	}
	pending, err := s.engine.ReviewPending(name)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, importResult{Name: name, Rev: rev, Review: review, ReviewPending: pending})
}

// handleConfirmReview is the operator's explicit yes to a held app: it
// names the revision that was read, releases the hold, and (when asked)
// deploys that revision.
// Only here, from an operator's session — not MCP, not a webhook, not
// wiring — does a held app become deployable.
func (s *Server) handleConfirmReview(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, ok := s.appFromPath(w, r); !ok {
		return
	}
	var req struct {
		Rev int64 `json:"rev"`
		// Deploy starts the first deploy at once. A git app says no: its
		// deploy key has to reach the git host first (the setup panel).
		Deploy bool `json:"deploy"`
		// AdoptLeftovers is the operator's yes to the app taking over what
		// an earlier app of its name left behind (review.leftovers).
		AdoptLeftovers bool `json:"adoptLeftovers"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Rev <= 0 {
		badRequest(w, "name the revision you reviewed")
		return
	}
	actor := s.actor(r)
	if err := s.engine.ConfirmReview(name, req.Rev, req.AdoptLeftovers, actor); err != nil {
		if errors.Is(err, apps.ErrReviewStale) {
			conflict(w, err.Error())
			return
		}
		var left *apps.ErrLeftovers
		if errors.As(err, &left) {
			writeJSON(w, http.StatusConflict, map[string]any{"code": "leftover_data", "error": err.Error(), "leftovers": left.What})
			return
		}
		writeError(w, err)
		return
	}
	if req.Deploy {
		s.deployAsync("deploy after import review", name, req.Rev, actor)
	}
	s.goSafe("wiring", func() { s.wireConsumersOf(s.bgContext(), name, actor) })
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "deploying": req.Deploy})
}
