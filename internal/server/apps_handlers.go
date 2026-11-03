package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/backup"
	"gitea.lan/matthew/fog/internal/catalog"
	"gitea.lan/matthew/fog/internal/dockerx"
	"gitea.lan/matthew/fog/internal/hostcpu"
	"gitea.lan/matthew/fog/internal/proxy"
	"gitea.lan/matthew/fog/internal/selfupdate"
	"gitea.lan/matthew/fog/internal/store"
	"gitea.lan/matthew/fog/internal/tunnel"
	"gitea.lan/matthew/fog/internal/updates"
	"gitea.lan/matthew/fog/internal/vpn"
)

// SettingCatalogEnabled stores a JSON array of visible catalog ids; unset
// means the default set (Manor + Hub).
const SettingCatalogEnabled = "catalog_enabled"

// deployLogTailBytes is how much of each historical deploy log travels with
// the app page. Enough to hold a failing build's error and the steps around
// it; the UI flags anything longer as trimmed.
const deployLogTailBytes = 8000

func (s *Server) enabledCatalogIDs() []string {
	raw, _ := s.store.GetSetting(SettingCatalogEnabled)
	if raw == "" {
		return catalog.DefaultEnabled
	}
	var ids []string
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		return catalog.DefaultEnabled
	}
	return ids
}

// catalogEntries is the catalog with the household's own apps pointed at
// this fog's source settings (catalog/source.go). Entries with no source
// are still here, with an empty repository; the store leaves them out.
func (s *Server) catalogEntries() []catalog.Entry {
	base, _ := s.store.GetSetting(catalog.SettingSourceBase)
	overrides, _ := s.store.GetSetting(catalog.SettingSourceOverrides)
	return catalog.Resolve(catalog.Entries(), catalog.ParseSources(base, overrides))
}

func (s *Server) handleCatalog(w http.ResponseWriter, r *http.Request) {
	// Each entry is preflighted against this machine so the store can say
	// "not here, and why" before an install is attempted. A first-party app
	// this fog has no source for isn't offered at all; the App store
	// settings page says how many are waiting on one.
	host := hostcpu.Detect()
	entries := []catalog.Entry{}
	unsourced := 0
	for _, e := range s.catalogEntries() {
		if !catalog.Available(e) {
			unsourced++
			continue
		}
		e.Blocker = e.Preflight(host)
		entries = append(entries, e)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entries":   entries,
		"enabled":   s.enabledCatalogIDs(),
		"unsourced": unsourced,
	})
}

func (s *Server) handleListApps(w http.ResponseWriter, r *http.Request) {
	withStats := r.URL.Query().Get("stats") == "1"
	statuses, err := s.engine.Statuses(r.Context(), withStats)
	if err != nil {
		writeError(w, err)
		return
	}
	if statuses == nil {
		statuses = []apps.AppStatus{}
	}
	writeJSON(w, http.StatusOK, statuses)
}

type createAppReq struct {
	Name    string            `json:"name"`
	Spec    apps.AppSpec      `json:"spec"`
	Secrets map[string]string `json:"secrets"`
	// RestoreFrom seeds the new instance from a fog-backup/1 artifact: fog
	// stages it into the app's first managed volume before the first deploy,
	// and the app's release phase restores it before serving a byte.
	RestoreFrom *restoreFromReq `json:"restoreFrom,omitempty"`
	// Review creates the app held, as an imported manifest is: nothing
	// deploys until an operator has read its review and confirmed it. The
	// page that turns a pasted compose file into a spec always asks for it
	// — that spec is somebody else's, whatever the form shows.
	Review bool `json:"review,omitempty"`
}

type restoreFromReq struct {
	BackupID        int64  `json:"backupId,omitempty"`
	Path            string `json:"path,omitempty"`
	ConfirmIdentity bool   `json:"confirmIdentity,omitempty"`
}

// resolveRestoreArtifact turns a restoreFrom request into an on-disk
// artifact path, validating before the app row exists so a bad request
// can't leave a half-created app behind.
func (s *Server) resolveRestoreArtifact(rf *restoreFromReq) (string, error) {
	artifact := rf.Path
	if rf.BackupID != 0 {
		b, err := s.store.BackupByID(rf.BackupID)
		if err != nil {
			return "", fmt.Errorf("no such backup %d", rf.BackupID)
		}
		if b.Format != backup.ArtifactFormat {
			return "", fmt.Errorf("backup %d is a legacy volume snapshot — only fog-backup/1 artifacts can seed a new instance", rf.BackupID)
		}
		artifact = filepath.Join(b.Path, "artifact.tar")
	}
	if artifact == "" {
		return "", fmt.Errorf("restoreFrom needs a backupId or a path")
	}
	if _, err := os.Stat(artifact); err != nil {
		return "", fmt.Errorf("artifact not found at %s", artifact)
	}
	return artifact, nil
}

func (s *Server) handleCreateApp(w http.ResponseWriter, r *http.Request) {
	var req createAppReq
	if !readJSON(w, r, &req) {
		return
	}
	artifact := ""
	if req.RestoreFrom != nil {
		var err error
		if artifact, err = s.resolveRestoreArtifact(req.RestoreFrom); err != nil {
			badRequest(w, err.Error())
			return
		}
		if !req.Spec.DelegatedBackups() {
			badRequest(w, "restoreFrom needs a spec with backup.mode: delegated — the app's own boot guard performs the restore")
			return
		}
		// Preflight before the app exists. The secrets come from this request
		// rather than the vault — they are what the app will be created with,
		// and a mismatch here is the difference between an install that
		// restores and one whose first boot loops on a restore it can never
		// finish.
		// No identity yet: the app does not exist, so nothing has declared
		// what software it is. Preflight warns and the app's own boot
		// preflight refuses a mismatch.
		pf := backup.PreflightArtifact(artifact, req.Name, "", func(key string) string {
			if v := req.Secrets[key]; v != "" {
				return v
			}
			return req.Spec.Env[key]
		}, req.RestoreFrom.ConfirmIdentity, false)
		if !pf.OK() {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
				"error":  "the artifact cannot be restored into this app: " + pf.Err().Error(),
				"errors": pf.Errors,
			})
			return
		}
	}
	// The same check the store shows, enforced: an install that would only
	// crash-loop is refused with the reason rather than started.
	if why := catalog.PreflightSpec(req.Spec, hostcpu.Detect()); why != "" {
		writeErr(w, http.StatusUnprocessableEntity, why)
		return
	}
	actor := s.actor(r)
	// Mount what it needs of the apps it uses before the first revision,
	// so the install is wired from its very first deploy.
	s.wireSpec(req.Name, &req.Spec)
	if req.Review {
		if err := s.engine.CreateForReview(r.Context(), req.Name, &req.Spec, req.Secrets, actor); err != nil {
			badRequest(w, err.Error())
			return
		}
		s.auditAs(actor, "app.import", req.Name, map[string]any{"from": "compose"})
		writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "name": req.Name, "reviewPending": true})
		return
	}
	if err := s.engine.Create(r.Context(), req.Name, &req.Spec, req.Secrets, actor); err != nil {
		badRequest(w, err.Error())
		return
	}
	// The operator's own install: what it acknowledges, they approve.
	if err := s.engine.ApproveSpec(req.Name, &req.Spec, actor); err != nil {
		writeError(w, err)
		return
	}
	s.goSafe("wiring", func() { s.wireConsumersOf(s.bgContext(), req.Name, actor) })
	if artifact != "" {
		if err := s.backup.StageArtifact(req.Name, &req.Spec, artifact, req.RestoreFrom.ConfirmIdentity, false, "fog-create"); err != nil {
			writeError(w, fmt.Errorf("app created but staging the restore failed: %w", err))
			return
		}
		s.cleanupUpload(req.RestoreFrom.Path)
		s.auditAs(actor, "backup.restore.staged", req.Name, nil)
	}
	// Image apps deploy immediately — nothing else is needed. Git apps land
	// in the setup flow first: the deploy key must reach the git host before
	// any deploy can succeed.
	if req.Spec.Source.Type == "image" {
		s.deployAsync("initial deploy", req.Name, 0, actor)
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "name": req.Name})
}

func (s *Server) handleRegenerateDeployKey(w http.ResponseWriter, r *http.Request) {
	gi, err := s.engine.RegenerateDeployKey(r.PathValue("name"), s.actor(r))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, gi)
}

// handleTestConnection: fast git reachability check for the setup flow.
func (s *Server) handleTestConnection(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.engine.TestConnection(r.Context(), r.PathValue("name")))
}

// handleHealthCheck probes the app's health path once and reports the whole
// response. An optional {"path"} tries a path before it is saved.
func (s *Server) handleHealthCheck(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
	}
	if !readOptionalJSON(w, r, &body) {
		return
	}
	writeJSON(w, http.StatusOK, s.engine.CheckHealth(r.Context(), r.PathValue("name"), strings.TrimSpace(body.Path)))
}

// handleCancelDeploy calls off the app's in-flight deploy, update, or
// rollback — the way out of one that has hung (a snapshot the app never
// finishes, a build that stalls). It only signals; the operation unwinds on
// its own goroutine, and deploy-status reads as running until it has.
func (s *Server) handleCancelDeploy(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	stage := s.engine.DeployStage(name)
	actor := s.actor(r)
	if err := s.engine.CancelOp(name, actor); err != nil {
		conflict(w, err.Error())
		return
	}
	s.auditAs(actor, "app.deploy.cancel", name, map[string]any{"stage": stage})
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}

// handleDeployStatus is polled by the UI: in-flight progress plus the last
// finished deploy with a plain-English diagnosis when it failed.
func (s *Server) handleDeployStatus(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	app, ok := s.appFromPath(w, r)
	if !ok {
		return
	}
	resp := deployStatusResponse{Current: s.engine.CurrentDeploy(name)}
	// This is polled every second or two, and a build log runs to 256 KB:
	// read only the tail it shows. One byte past the 4000 lets LogTail see
	// whether the cut landed on a line boundary, exactly as on the full log.
	const tail = 4000
	if deploys, err := s.engine.DeploySummaries(app.ID, 1, tail+1); err == nil && len(deploys) > 0 {
		last := deploys[0]
		entry := &deployStatusLast{
			FinishedAt: last.FinishedAt,
			LogTail:    apps.LogTail(last.Log, tail),
			Rev:        last.Rev,
			Status:     last.Status,
		}
		if last.Status == "failed" {
			// Diagnosis matches against the whole log, so a failure
			// still pays for reading it.
			log := last.Log
			if last.LogTruncated {
				log, _ = s.engine.DeployLog(app.ID, last.ID)
			}
			entry.Diagnosis = apps.Diagnose(log)
		}
		resp.Last = entry
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleLiveDeploys lists every app with a deploy, update, or rollback in
// flight, for the shell's deploy tray: its stage, and what the tray needs
// to say how far along it is — how long its deploy has run and how long the
// app's deploys usually take. Without both the tray shows the stage alone.
func (s *Server) handleLiveDeploys(w http.ResponseWriter, r *http.Request) {
	out := []liveDeployView{}
	for _, name := range s.engine.DeployingApps() {
		stage := s.engine.DeployStage(name)
		if stage == "" {
			continue // finished between the two reads
		}
		v := liveDeployView{App: name, Stage: stage}
		if app, err := s.store.AppByName(name); err == nil {
			started, typical, err := s.store.DeployTiming(app.ID)
			if err == nil {
				// Elapsed, not the start: the server's clock measures it, so a
				// browser whose clock is off still draws the right bar.
				if t, perr := time.Parse(time.RFC3339, started); perr == nil {
					v.ElapsedMs = max(time.Since(t).Milliseconds(), 1)
				}
				v.TypicalMs = typical.Milliseconds()
			}
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, liveDeploysResponse{Deploys: out})
}

type liveDeploysResponse struct {
	Deploys []liveDeployView `json:"deploys"`
}

// liveDeployView is one app's in-flight operation. ElapsedMs is absent until
// the operation reaches its deploy (an update snapshots and pulls first);
// TypicalMs is 0 before the app's first successful deploy.
type liveDeployView struct {
	App       string `json:"app"`
	Stage     string `json:"stage"`
	ElapsedMs int64  `json:"elapsedMs,omitempty"`
	TypicalMs int64  `json:"typicalMs,omitempty"`
}

// deployStatusResponse is the deploy in flight, and the last one that
// finished, absent before the app's first deploy.
type deployStatusResponse struct {
	Current apps.DeploySnapshot `json:"current"`
	Last    *deployStatusLast   `json:"last,omitempty"`
}

// deployStatusLast's fields are in alphabetical order, as they came out
// when this was built as a map. FinishedAt is "" while that deploy runs.
type deployStatusLast struct {
	Diagnosis  *apps.Diagnosis `json:"diagnosis,omitempty"`
	FinishedAt string          `json:"finishedAt"`
	LogTail    string          `json:"logTail"`
	Rev        int64           `json:"rev"`
	Status     string          `json:"status"`
}

func (s *Server) handleGetApp(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	app, ok := s.appFromPath(w, r)
	if !ok {
		return
	}
	// A corrupt revision row must be a loud 500, not a silently empty spec
	// the UI would happily render (and could save back over the real one).
	rev, spec, err := apps.SpecAt(s.store, app.ID, 0)
	if err != nil {
		writeError(w, fmt.Errorf("app %s: %w", name, err))
		return
	}
	allKeys, _ := s.store.SecretKeys(app.ID)
	secretKeys := []string{}
	for _, k := range allKeys {
		// "_"-prefixed secrets are Fog-internal (deploy keys, pg passwords).
		if !strings.HasPrefix(k, "_") {
			secretKeys = append(secretKeys, k)
		}
	}
	// Summaries, not full rows: neither the revision specs nor whole build
	// logs are read by the history view that consumes these.
	revisions, _ := s.store.ListRevisionSummaries(app.ID, 50)
	deploys, _ := s.engine.DeploySummaries(app.ID, 20, deployLogTailBytes)
	if revisions == nil {
		revisions = []store.AppRevision{}
	}
	if deploys == nil {
		deploys = []store.Deploy{}
	}
	resp := map[string]any{
		"app":        app,
		"spec":       spec,
		"latestRev":  rev.Rev,
		"secretKeys": secretKeys,
		"revisions":  revisions,
		"deploys":    deploys,
	}
	// An imported app no deploy may touch until its review is confirmed.
	if held, err := s.store.AppReviewHold(app.ID); err == nil && held {
		resp["reviewPending"] = true
	}
	// What the app told fog about its federation (Project J): the contracts it
	// implements, what a host change costs, and any contract fog can't honour.
	// Only present when the app declares something, so the UI can stay quiet
	// for the apps that don't federate.
	if facts, err := s.store.AppFederationFacts(app.ID); err == nil &&
		(len(facts.Contracts) > 0 || facts.HostChange != "" || len(facts.UnknownContracts) > 0) {
		resp["federation"] = facts
	}
	if spec.Source.Type == "git" {
		if gi, err := s.engine.GitInfo(app.ID, app.Name); err == nil {
			resp["git"] = gi
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleWebhook reacts to a git push. Unauthenticated by session — the
// per-app token in the path is the credential. What happens is governed by
// the app's update policy: "auto" deploys immediately (push-to-deploy);
// otherwise the push just refreshes update state so the UI shows the Update
// button within seconds instead of waiting for the periodic check.
func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	token := r.PathValue("token")
	app, err := s.store.AppByName(name)
	if err != nil || !s.engine.CheckWebhookToken(app.ID, token) {
		notFound(w, "not found")
		return
	}
	spec, err := apps.LatestSpec(s.store, app.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	if spec.UpdatePolicy == "auto" {
		s.auditAs("webhook", "app.deploy.webhook", name, nil)
		s.deployAsync("webhook deploy", name, 0, "webhook")
		writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
		return
	}
	s.auditAs("webhook", "app.update-check.webhook", name, nil)
	s.goSafe("webhook update check", func() {
		ctx, cancel := s.deployCtx()
		defer cancel()
		available, err := s.updates.CheckApp(ctx, app, spec)
		if err != nil {
			s.log.Error("webhook update check", "app", name, "err", err)
			return
		}
		if available && spec.UpdatePolicy == "notify" {
			s.updates.Notify("Update available: "+name, "a new commit was pushed to "+spec.Source.Repo, true)
		}
	})
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}

type updateAppReq struct {
	Spec    apps.AppSpec      `json:"spec"`
	Secrets map[string]string `json:"secrets"`
	Comment string            `json:"comment"`
	Deploy  bool              `json:"deploy"`
}

func (s *Server) handleUpdateApp(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req updateAppReq
	if !readJSON(w, r, &req) {
		return
	}
	actor := s.actor(r)
	if err := s.engine.UpdateSpec(r.Context(), name, &req.Spec, req.Secrets, req.Comment, actor); err != nil {
		badRequest(w, err.Error())
		return
	}
	// The operator's own save: what it acknowledges is what they approve
	// (apps/approvals.go). No other writer of a revision does this.
	if err := s.engine.ApproveSpec(name, &req.Spec, actor); err != nil {
		writeError(w, err)
		return
	}
	if req.Deploy {
		s.deployAsync("deploy after spec update", name, 0, actor)
	}
	writeOK(w)
}

type appActionReq struct {
	Action string `json:"action"` // start | stop | restart | deploy | rollback
	Rev    int64  `json:"rev"`    // for deploy/rollback; 0 = latest
}

func (s *Server) handleAppAction(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req appActionReq
	if !readJSON(w, r, &req) {
		return
	}
	actor := s.actor(r)
	var err error
	switch req.Action {
	case "start":
		err = s.engine.Start(r.Context(), name, actor)
	case "stop":
		err = s.engine.Stop(r.Context(), name, actor)
	case "restart":
		err = s.engine.Restart(r.Context(), name, actor)
	case "deploy", "rollback":
		// Already updating/deploying: say so instead of quietly stacking a
		// second run behind the first, which reads as "started" while doing
		// nothing yet. Same guard as MCP appDeploy and handleApplyUpdate.
		if s.engine.Deploying(name) {
			conflict(w, updates.ErrUpdateInProgress.Error())
			return
		}
		// An imported app waiting for review deploys only through its
		// confirmation; say so here rather than fail in the background.
		if held, _ := s.engine.ReviewPending(name); held {
			conflict(w, (&apps.ErrReviewPending{App: name}).Error())
			return
		}
		s.deployAsync("deploy", name, req.Rev, actor)
		writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
		return
	default:
		badRequest(w, "unknown action")
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeOK(w)
}

func (s *Server) handleDeleteApp(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	deleteData := r.URL.Query().Get("data") == "1"
	actor := s.actor(r)
	// Read before the rows cascade away; revoke once the app is gone.
	var minted []store.Integration
	if a, err := s.store.AppByName(name); err == nil {
		minted, _ = s.store.ListIntegrations(a.ID)
	}
	// And the apps connected to it, to disconnect once it is gone: left
	// behind, their connections would be to whatever app takes its name.
	consumers, _ := s.store.IntegrationsWithProvider(name)
	// Peers' access to it, too: what it minted for them through Connect is
	// revoked while it still runs. Those credentials live in the app's own
	// data, so keeping the data when revocation failed would hand them to an
	// app later installed under this name: that takes the operator saying so
	// (keepUnrevoked=1); otherwise the delete stops and says why.
	// No credential is minted for it from here on, and every peer it ever
	// minted for — paired now or not — is owed a revocation, kept until the
	// app holding this name answers it (connect_ledger.go).
	unfreeze := s.freezeMinting(name)
	defer unfreeze()
	if a, err := s.store.AppByName(name); err == nil {
		if owed := s.revokeForRemoval(r.Context(), a); len(owed) > 0 && !deleteData && r.URL.Query().Get("keepUnrevoked") != "1" {
			var who []string
			for _, o := range owed {
				who = append(who, o.Instance)
			}
			writeJSON(w, http.StatusConflict, map[string]string{"code": "connect_revoke_failed",
				"error": name + " has not revoked the access it gave households (" + strings.Join(who, ", ") + "). Delete its data too, " +
					"or confirm keeping it anyway: an app installed later under this name would accept that access until it is revoked."})
			return
		}
	}
	if err := s.engine.Delete(r.Context(), name, deleteData, actor); err != nil {
		writeError(w, err)
		return
	}
	if deleteData {
		s.forgiveOwed(name) // the credentials went with the data
	}
	s.forgetAppForPeers(name)
	s.goSafe("revoke integrations", func() { s.integrations().RevokeAll(s.bgContext(), minted) })
	s.forgetProvider(r.Context(), name, consumers, actor)
	writeOK(w)
}

// forgetProvider disconnects every app connected to a deleted provider:
// the env and secrets it handed them go, and so do the connections.
func (s *Server) forgetProvider(ctx context.Context, provider string, rows []store.Integration, actor string) {
	done := map[int64]bool{}
	for _, in := range rows {
		if done[in.AppID] {
			continue
		}
		done[in.AppID] = true
		consumer, err := s.store.AppByID(in.AppID)
		if err != nil {
			continue
		}
		if _, err := s.integrations().Disconnect(ctx, consumer, provider, true, actor); err != nil {
			s.log.Warn("disconnecting from a deleted provider", "app", consumer.Name, "provider", provider, "err", err)
		}
	}
}

// handleAppLogs streams container logs as chunked text; the frontend consumes
// it with a streaming fetch.
// The main container's lifecycle facts and a day of health probes, for the
// app page. 404 only when the app itself does not exist: an app with no
// container is a found=false answer, not a missing resource.
func (s *Server) handleAppContainer(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, ok := s.appFromPath(w, r); !ok {
		return
	}
	facts, err := s.engine.Container(r.Context(), name)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, facts)
}

func (s *Server) handleAppLogs(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	follow := r.URL.Query().Get("follow") == "1"
	tail := r.URL.Query().Get("tail")
	if tail == "" {
		tail = "200"
	} else if _, err := strconv.Atoi(tail); err != nil {
		badRequest(w, "tail must be a number")
		return
	}

	// Main container unless ?process= names a worker/sidecar — same selection
	// as exec, so an app with a pg sidecar never shows the sidecar's logs on
	// Docker list-order luck.
	target, err := s.docker.ProcessContainer(r.Context(), name, r.URL.Query().Get("process"))
	if errors.Is(err, dockerx.ErrNoProcess) {
		notFound(w, err.Error())
		return
	}
	if err != nil {
		notFound(w, dockerx.ErrNoContainer.Error())
		return
	}
	// ?timestamps=1 prefixes each line with Docker's stamp — the app page
	// needs it to interleave fogd's own actions with the container's output.
	rc, err := s.docker.Logs(r.Context(), target.ID, follow, tail, r.URL.Query().Get("timestamps") == "1")
	if err != nil {
		writeError(w, err)
		return
	}
	defer rc.Close()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	fl, _ := w.(http.Flusher)
	buf := make([]byte, 4096)
	for {
		n, err := rc.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
		}
		if err != nil {
			if err != io.EOF {
				s.log.Debug("log stream ended", "app", name, "err", err)
			}
			return
		}
	}
}

// ---- system secrets (DNS token etc.) ----

type systemSecretReq struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

func (s *Server) handlePutSystemSecret(w http.ResponseWriter, r *http.Request) {
	var req systemSecretReq
	if !readJSON(w, r, &req) {
		return
	}
	allowed := map[string]bool{proxy.SettingDNSToken: true, selfupdate.SecretUpdateBasicAuth: true, tunnel.SecretToken: true, vpn.SecretAuthKey: true}
	if !allowed[req.Key] {
		badRequest(w, "unknown system secret")
		return
	}
	// The update token is bound to the source it is saved for, and only to
	// an https one (selfupdate.authorize sends it nowhere else).
	tokenOrigin := ""
	if req.Key == selfupdate.SecretUpdateBasicAuth && req.Value != "" {
		src := selfupdate.EffectiveSource(s.store)
		if err := selfupdate.CheckTokenSource(src); err != nil {
			badRequest(w, err.Error())
			return
		}
		tokenOrigin = selfupdate.SourceOrigin(src)
	}
	if req.Value == "" {
		s.store.DeleteSecret(0, req.Key)
	} else {
		if err := s.secrets().Put(0, req.Key, req.Value); err != nil {
			writeError(w, err)
			return
		}
	}
	if req.Key == selfupdate.SecretUpdateBasicAuth {
		s.store.SetSetting(selfupdate.SettingUpdateTokenOrigin, tokenOrigin)
	}
	s.audit(r, "system.secret.update", req.Key, nil)
	s.goSafe("proxy apply", s.applyProxy)
	writeOK(w)
}

func (s *Server) handleGetSystemSecrets(w http.ResponseWriter, r *http.Request) {
	all, err := s.store.SecretKeys(0)
	if err != nil {
		writeError(w, err)
		return
	}
	keys := []string{}
	for _, k := range all {
		// "_"-prefixed secrets are Fog-internal (the proxy forward secret).
		if !strings.HasPrefix(k, "_") {
			keys = append(keys, k)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": keys})
}

func (s *Server) handleProxyApply(w http.ResponseWriter, r *http.Request) {
	if err := s.proxy.Apply(r.Context()); err != nil {
		writeError(w, err)
		return
	}
	s.audit(r, "proxy.apply", "", nil)
	writeOK(w)
}

func (s *Server) applyProxy() {
	if s.proxy == nil {
		return
	}
	if err := s.proxy.Apply(s.bgContext()); err != nil {
		s.log.Error("proxy apply", "err", err)
	}
}
