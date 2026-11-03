package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"gitea.lan/matthew/fog/internal/auth"
	"gitea.lan/matthew/fog/internal/backup"
	"gitea.lan/matthew/fog/internal/catalog"
	"gitea.lan/matthew/fog/internal/config"
	"gitea.lan/matthew/fog/internal/dockerx"
	"gitea.lan/matthew/fog/internal/files"
	"gitea.lan/matthew/fog/internal/flare"
	"gitea.lan/matthew/fog/internal/notify"
	"gitea.lan/matthew/fog/internal/offsite"
	"gitea.lan/matthew/fog/internal/people"
	"gitea.lan/matthew/fog/internal/proxy"
	"gitea.lan/matthew/fog/internal/selfupdate"
	"gitea.lan/matthew/fog/internal/store"
)

// ---- helpers ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		badRequest(w, "invalid JSON body")
		return false
	}
	return true
}

// readOptionalJSON is readJSON for endpoints whose body may be left off: an
// empty body leaves v as it is, anything else must decode.
func readOptionalJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil && !errors.Is(err, io.EOF) {
		badRequest(w, "invalid JSON body")
		return false
	}
	return true
}

// writeErr sends the {"error": msg} body the frontend's ApiError reads. A
// 5xx's message also goes into the request's log line: the client may never
// show it, and the log ring is what feedback reports and diagnosis read.
func writeErr(w http.ResponseWriter, status int, msg string) {
	if status >= 500 {
		recordServerError(w, msg)
	}
	writeJSON(w, status, map[string]string{"error": msg})
}

func unauthorized(w http.ResponseWriter) {
	writeErr(w, http.StatusUnauthorized, "authentication required")
}

func badRequest(w http.ResponseWriter, msg string) {
	writeErr(w, http.StatusBadRequest, msg)
}

func forbidden(w http.ResponseWriter, msg string) {
	writeErr(w, http.StatusForbidden, msg)
}

func notFound(w http.ResponseWriter, msg string) {
	writeErr(w, http.StatusNotFound, msg)
}

func conflict(w http.ResponseWriter, msg string) {
	writeErr(w, http.StatusConflict, msg)
}

// writeError picks the status for an error the handler didn't classify
// itself: a missing row is a 404, a duplicate a 409, and everything else a
// 500 that lands in the request's log line. A request the handler finds
// wrong is answered with badRequest where it is found. Package-specific
// mappers (filesError, chatError, …) stay where a domain has its own
// sentinels.
func writeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		notFound(w, err.Error())
	case errors.Is(err, store.ErrAlreadyExists):
		conflict(w, err.Error())
	case errors.Is(err, errSessionRate):
		writeErr(w, http.StatusTooManyRequests, err.Error()) // signin_handlers_limits.go
	default:
		writeErr(w, http.StatusInternalServerError, err.Error())
	}
}

// limit charges one attempt to key and, when lim refuses it, answers 429
// with the house body and a Retry-After of the limiter's window. Reports
// whether the request may go on.
func limit(w http.ResponseWriter, lim *auth.RateLimiter, key, msg string) bool {
	if lim.Allow(key) {
		return true
	}
	w.Header().Set("Retry-After", strconv.Itoa(int(lim.Window().Seconds())))
	writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": msg, "code": "rate_limited"})
	return false
}

// writeOK is the {"ok": true} answer of an action with nothing to return.
func writeOK(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// clientIP is the address a request came from, the key for per-IP rate
// limits. Through Caddy the TCP peer is always the proxy container, so Caddy
// names the client in a header and proves itself with the forward secret.
// Any other peer's headers are ignored. A LAN host can set X-Forwarded-For
// to anything, and trusting it would let one client rotate through buckets.
func (s *Server) clientIP(r *http.Request) string {
	peer := remoteIP(r)
	got := r.Header.Get(proxy.HeaderSecret)
	if got == "" || s.proxy == nil {
		return peer
	}
	want, err := s.proxy.ForwardSecret()
	if err != nil || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		return peer
	}
	if ip := net.ParseIP(strings.TrimSpace(r.Header.Get(proxy.HeaderClientIP))); ip != nil {
		return ip.String()
	}
	return peer
}

// ---- setup ----

func (s *Server) handleSetupStatus(w http.ResponseWriter, r *http.Request) {
	// The USB installer's wizard page polls this endpoint cross-origin to see
	// the installed system come up after reboot. It's unauthenticated liveness
	// info, so the open CORS header costs nothing and enables that handoff.
	w.Header().Set("Access-Control-Allow-Origin", "*")
	n, err := s.store.CountOperators()
	if err != nil {
		writeError(w, err)
		return
	}
	done, _ := s.store.GetSetting(store.SettingSetupDone)
	writeJSON(w, http.StatusOK, setupStatus{NeedsSetup: n == 0, SetupDone: done == "1", Version: config.Version})
}

// setupStatus is what /api/setup/status answers: whether the box still
// wants its first operator, and which fog it is.
type setupStatus struct {
	NeedsSetup bool   `json:"needsSetup"`
	SetupDone  bool   `json:"setupDone"`
	Version    string `json:"version"`
}

type setupReq struct {
	Username   string `json:"username"`
	Password   string `json:"password"`
	ServerName string `json:"serverName"`
	BaseDomain string `json:"baseDomain"`
}

// handleSetup creates the admin account. It is only valid while no user
// exists — afterwards the endpoint is permanently closed.
func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	n, err := s.store.CountOperators()
	if err != nil {
		writeError(w, err)
		return
	}
	if n > 0 {
		forbidden(w, "setup already completed")
		return
	}
	var req setupReq
	if !readJSON(w, r, &req) {
		return
	}
	// Every field is validated before the first write: once the person row
	// exists the n > 0 guard above closes this endpoint forever, so a
	// rejected request that had already created the operator would brick
	// first-run setup on a typo.
	req.Username = strings.TrimSpace(req.Username)
	if req.Username == "" {
		badRequest(w, "username is required")
		return
	}
	// The name typed here becomes a handle, and a handle is what everyone
	// signs in with — so it has to satisfy the same rules a member's does.
	handle, err := people.NormalizeHandle(req.Username)
	if err != nil {
		badRequest(w, "username "+err.Error())
		return
	}
	if err := auth.ValidatePassword(req.Password); err != nil {
		badRequest(w, err.Error())
		return
	}
	if req.BaseDomain != "" && !validDomain(req.BaseDomain) {
		badRequest(w, "base domain must be a bare domain like home.example.com")
		return
	}
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		writeError(w, err)
		return
	}
	personID, err := s.store.CreateFirstOperator(handle, req.Username, hash)
	if errors.Is(err, store.ErrSetupDone) {
		forbidden(w, "setup already completed")
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	if req.ServerName != "" {
		s.store.SetSetting(store.SettingServerName, strings.TrimSpace(req.ServerName))
	}
	if req.BaseDomain != "" {
		s.store.SetSetting(store.SettingBaseDomain, strings.ToLower(strings.TrimSpace(req.BaseDomain)))
	}
	s.store.SetSetting(store.SettingSetupDone, "1")
	s.auditAs(handle, "setup.complete", "", nil)

	token, _, err := s.mintPersonSession(r, personID, 0, "web", "pwd")
	if err != nil {
		writeError(w, err)
		return
	}
	s.setSessionCookie(w, r, token)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "username": handle})
}

func validDomain(d string) bool {
	d = strings.TrimSpace(strings.ToLower(d))
	if d == "" || len(d) > 253 || strings.Contains(d, "://") || strings.Contains(d, "/") || strings.Contains(d, " ") {
		return false
	}
	return strings.Contains(d, ".")
}

// handleSetupClaim turns the one-shot token minted by the USB installer into
// a browser session. The wizard page that drove the install holds the token;
// after the machine reboots into the installed system, the page navigates
// here (a top-level GET, so the SameSite=Lax cookie sticks) and the operator
// lands on the dashboard signed in as the account they typed into the
// installer — typed once, never again. Single use, deadline-bound, and only
// ever present on a preseeded first boot.
func (s *Server) handleSetupClaim(w http.ResponseWriter, r *http.Request) {
	deny := func() { http.Redirect(w, r, "/", http.StatusSeeOther) }
	if !s.strangerAllowed(r, "setup-claim") {
		deny()
		return
	}
	token := r.URL.Query().Get("token")
	digest, _ := s.store.GetSetting(store.SettingClaimDigest)
	if token == "" || digest == "" || auth.DigestToken(token) != digest {
		deny()
		return
	}
	// Burn the token before honoring it: whatever happens next, it was
	// single-shot.
	expiresRaw, _ := s.store.GetSetting(store.SettingClaimExpires)
	username, _ := s.store.GetSetting(store.SettingClaimUser)
	s.store.DeleteSetting(store.SettingClaimDigest)
	s.store.DeleteSetting(store.SettingClaimExpires)
	s.store.DeleteSetting(store.SettingClaimUser)
	exp, err := time.Parse(time.RFC3339, expiresRaw)
	if err != nil || time.Now().After(exp) {
		deny()
		return
	}
	handle, err := people.NormalizeHandle(username)
	if err != nil {
		deny()
		return
	}
	p, err := s.store.PersonByHandle(handle)
	if err != nil || p.Role != store.PersonRoleOperator || p.Status != store.PersonStatusActive {
		deny()
		return
	}
	sess, _, err := s.mintPersonSession(r, p.ID, 0, "web", "pwd")
	if err != nil {
		writeError(w, err)
		return
	}
	s.setSessionCookie(w, r, sess)
	s.auditAs(p.Handle, "auth.claim", "", map[string]any{"via": "installer handoff"})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// ---- auth ----

// handleMe answers who is signed in and what they may do.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	pc := s.currentPerson(r)
	writeJSON(w, http.StatusOK, meView{
		DisplayName: pc.Person.DisplayName,
		Handle:      pc.Person.Handle,
		Operator:    pc.Operator,
		Role:        pc.Person.Role,
	})
}

// meView is /api/auth/me. Operator is the role, and the only thing that
// decides which shell the UI mounts. Its fields are in alphabetical order,
// as they came out when this was built as a map.
type meView struct {
	DisplayName string `json:"displayName"`
	Handle      string `json:"handle"`
	Operator    bool   `json:"operator"`
	Role        string `json:"role" ts:"\"operator\" | \"member\" | \"guest\""`
}

// ---- system ----

func (s *Server) handleSystem(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	hostname, _ := os.Hostname()

	sys := systemStatus{
		Arch:      runtime.GOARCH,
		DataDir:   s.cfg.DataDir,
		Docker:    s.dockerStatus(ctx),
		Hostname:  hostname,
		OS:        runtime.GOOS,
		StartedAt: s.startedAt.UTC().Format(time.RFC3339),
		Version:   config.Version,
		Warnings:  s.cachedWarnings(),
	}
	if total, free, err := diskUsage(s.cfg.DataDir); err == nil {
		sys.Disk = &systemDisk{FreeBytes: free, TotalBytes: total}
	}
	writeJSON(w, http.StatusOK, sys)
}

// systemStatus is /api/system: the host fog runs on, polled by the
// dashboard and the footer. Its fields are in alphabetical order, as they
// came out when this was built as a map.
type systemStatus struct {
	Arch    string `json:"arch"`
	DataDir string `json:"dataDir"`
	// Disk is the filesystem holding the data directory, absent when it
	// can't be read.
	Disk      *systemDisk      `json:"disk,omitempty"`
	Docker    dockerStatusView `json:"docker"`
	Hostname  string           `json:"hostname"`
	OS        string           `json:"os"`
	StartedAt string           `json:"startedAt"`
	Version   string           `json:"version"`
	Warnings  []string         `json:"warnings"`
}

type systemDisk struct {
	FreeBytes  uint64 `json:"freeBytes"`
	TotalBytes uint64 `json:"totalBytes"`
}

// dockerStatusView is whether Docker answers, and what it said about
// itself when it did.
type dockerStatusView struct {
	Available bool             `json:"available"`
	Info      *dockerx.Info    `json:"info,omitempty"`
	Version   *dockerx.Version `json:"version,omitempty"`
}

// The dashboard polls /api/system every ten seconds and the footer keeps a
// copy alive, so anything expensive here runs continuously for the life of an
// open tab. Two things qualified: three Docker round-trips, and a pair of
// writability probes that each created and deleted a temp file. Both are
// cached — with different lifetimes, because they change on different
// timescales.
const (
	dockerStatusTTL = 30 * time.Second // Docker can stop or be installed
	warningsTTL     = 5 * time.Minute  // filesystem writability essentially never moves
)

func (s *Server) dockerStatus(ctx context.Context) dockerStatusView {
	return *s.dockerCache.get(ctx, dockerStatusTTL, s.probeDocker)
}

// probeDocker is three Docker round-trips, each bounded only by the
// client's 10s timeout.
func (s *Server) probeDocker(ctx context.Context) *dockerStatusView {
	d := &dockerStatusView{}
	if s.docker.Available(ctx) {
		d.Available = true
		if v, err := s.docker.Version(ctx); err == nil {
			d.Version = v
		}
		if i, err := s.docker.Info(ctx); err == nil {
			d.Info = i
		}
	}
	return d
}

// invalidateDockerStatus drops the cached view so the next poll re-checks.
// Called when something is known to have changed Docker's availability;
// without it the UI would keep reporting "not installed" for up to the TTL
// after an install finished.
func (s *Server) invalidateDockerStatus() { s.dockerCache.invalidate() }

func (s *Server) cachedWarnings() []string {
	return s.warningsCache.get(warningsTTL, s.environmentWarnings)
}

// environmentWarnings runs cheap self-checks for problems that would
// otherwise only surface as cryptic failures later (e.g. a service unit
// missing write access where self-update swaps the binary).
func (s *Server) environmentWarnings() []string {
	warnings := []string{}
	if !dirWritable(s.cfg.DataDir) {
		warnings = append(warnings, "Fog's data directory ("+s.cfg.DataDir+") is not writable — nothing can be saved or deployed.")
	}
	if exe, err := os.Executable(); err == nil {
		if dir := filepath.Dir(exe); !dirWritable(dir) {
			warnings = append(warnings,
				"Self-update can't write next to the fogd binary — "+dir+" is read-only for the service. "+
					"Fix: printf '[Service]\\nReadWritePaths="+dir+"\\n' | sudo tee /etc/systemd/system/fogd.service.d/override.conf && sudo systemctl daemon-reload && sudo systemctl restart fogd")
		}
	}
	return warnings
}

func dirWritable(dir string) bool {
	f, err := os.CreateTemp(dir, ".fog-writecheck-*")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return true
}

func (s *Server) handleDockerInstallStart(w http.ResponseWriter, r *http.Request) {
	if s.docker.Available(r.Context()) {
		badRequest(w, "Docker is already installed and running")
		return
	}
	// Background context: the install must outlive this request.
	if err := s.installer.Start(context.Background()); err != nil {
		badRequest(w, err.Error())
		return
	}
	// A new run arms the once-per-run invalidation below for its completion.
	s.dockerCache.installInvalidated.Store(false)
	s.audit(r, "docker.install.start", "", nil)
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}

func (s *Server) handleDockerInstallStatus(w http.ResponseWriter, r *http.Request) {
	st := s.installer.Status()
	if st.Done && !s.dockerCache.installInvalidated.Swap(true) {
		// The install just changed the answer /api/system caches; drop it so
		// the dashboard stops saying "Docker isn't available" the moment it
		// becomes available, rather than up to a TTL later. Once per run, not
		// per poll: this endpoint is polled, and repeating the invalidation
		// would defeat the status TTL for as long as anything watches.
		s.invalidateDockerStatus()
	}
	writeJSON(w, http.StatusOK, st)
}

// ---- settings ----

// SettingUIMode switches the frontend between the full control plane
// ("advanced", the default) and the stripped-down launcher ("basic").
const SettingUIMode = "ui_mode"

// editableSettings are the settings GET /api/settings answers and PUT
// writes, each with the check a written value must pass. The UI's Settings
// type is generated from these keys (internal/tools/tsgen); a comment on an
// entry is its documentation there.
var editableSettings = map[string]func(string) bool{
	SettingUIMode:            func(v string) bool { return v == "" || v == "basic" || v == "advanced" }, // "" | "basic" | "advanced"; empty means advanced
	store.SettingServerName:  func(v string) bool { return len(v) <= 100 },
	store.SettingBaseDomain:  func(v string) bool { return v == "" || validDomain(v) },
	proxy.SettingAcmeEmail:   func(v string) bool { return v == "" || proxy.ValidAcmeEmail(v) }, // a plain address: it lands in the Caddyfile
	proxy.SettingCaddyImage:  func(v string) bool { return len(v) <= 300 },
	notify.SettingNtfyServer: func(v string) bool { return v == "" || notify.ValidServer(v) == nil }, // http(s) URL; no query or fragment
	// Pushes for errors peers relay: "" | "on" | "off"; empty means on.
	flare.SettingPeerPush:   func(v string) bool { return v == "" || v == "on" || v == "off" },
	notify.SettingNtfyTopic: func(v string) bool { return v == "" || notify.ValidTopic(v) }, // letters, digits, - and _ (ntfy's grammar), at most 64
	backup.SettingBackupRetention: func(v string) bool {
		if v == "" {
			return true
		}
		n, err := strconv.Atoi(v)
		return err == nil && n > 0 && n <= 100
	},
	backup.SettingBackupHourUTC: func(v string) bool {
		if v == "" {
			return true
		}
		n, err := strconv.Atoi(v)
		return err == nil && n >= 0 && n <= 23
	},
	// Snapshot an app before updates and rollbacks: "" | "on" | "off";
	// empty means on.
	backup.SettingBackupBeforeUpdate: func(v string) bool { return v == "" || v == "on" || v == "off" },
	// The kit protects the vault master key — a weak passphrase defeats it.
	backup.SettingRecoveryKitPassphrase: func(v string) bool { return backup.CheckKitPassphrase(v) == nil },
	// Offsite retention tiers: counts of backups, not days.
	offsite.SettingKeepRecent:  offsite.ValidPolicySetting(offsite.SettingKeepRecent),  // newest N backups kept in the locker; "" = 30
	offsite.SettingKeepMonthly: offsite.ValidPolicySetting(offsite.SettingKeepMonthly), // first-of-month kept for M months; "" = 6
	offsite.SettingKeepYearly:  offsite.ValidPolicySetting(offsite.SettingKeepYearly),  // first-of-year kept for Y years; "" = 3
	// Shape only; the full check (writable, not one of fog's trees, share
	// mounted, current root empty) runs in handlePutSettings before storing.
	files.SettingRoot:        files.ValidRootSetting,                                            // "" = <data dir>/files
	files.SettingOwner:       files.ValidOwnerSetting,                                           // "uid:gid" applied to what Files creates; "" = 1000:1000
	files.SettingShowHidden:  files.ValidShowSetting,                                            // dotfiles: "" | "on" | "off"; empty means off
	files.SettingShowSystem:  files.ValidShowSetting,                                            // @eaDir, Thumbs.db…: "" | "on" | "off"; empty means off
	files.SettingFilesBackup: func(v string) bool { return v == "" || v == "on" || v == "off" }, // "" | "on" | "off"; empty means on
	// The resolver's own rules (selfupdate.ValidateSource); handlePutSettings
	// answers with its reason.
	selfupdate.SettingUpdateBaseURL: func(v string) bool { return v == "" || selfupdate.ValidateSource(v) == nil },
	// Clone URL prefix of the household's own apps, e.g.
	// "ssh://git@git.example.com/you/"; "" leaves them out of the store.
	catalog.SettingSourceBase: func(v string) bool { return catalog.ValidBase(v) == nil },
	// JSON object of first-party repository name to its full clone URL.
	catalog.SettingSourceOverrides: func(v string) bool { return catalog.ValidOverrides(v) == nil },
	SettingCatalogEnabled: func(v string) bool {
		if v == "" {
			return true
		}
		var ids []string
		return json.Unmarshal([]byte(v), &ids) == nil
	},
}

// settings that affect routing/TLS; changing them reapplies the proxy.
var proxySettings = map[string]bool{
	store.SettingBaseDomain: true,
	proxy.SettingAcmeEmail:  true,
	proxy.SettingCaddyImage: true,
}

// secretSettings are writable but never read back: GET answers "" for each
// and, beside it, <key>_set = "on" when one is stored — enough for the UI to
// say "a passphrase is saved" without the settings response (which every
// page fetches and the browser caches) carrying the thing that opens the
// recovery kit.
var secretSettings = map[string]bool{
	backup.SettingRecoveryKitPassphrase: true,
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	all, err := s.store.AllSettings()
	if err != nil {
		writeError(w, err)
		return
	}
	out := map[string]string{}
	for k := range editableSettings {
		if secretSettings[k] {
			out[k] = ""
			out[k+"_set"] = ""
			if all[k] != "" {
				out[k+"_set"] = "on"
			}
			continue
		}
		out[k] = all[k]
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	var req map[string]string
	if !readJSON(w, r, &req) {
		return
	}
	for k, v := range req {
		validate, ok := editableSettings[k]
		if !ok {
			badRequest(w, "unknown setting: "+k)
			return
		}
		if !validate(strings.TrimSpace(v)) {
			if k == selfupdate.SettingUpdateBaseURL {
				badRequest(w, "update source: "+selfupdate.ValidateSource(v).Error())
				return
			}
			if k == catalog.SettingSourceBase {
				badRequest(w, "app source: "+catalog.ValidBase(strings.TrimSpace(v)).Error())
				return
			}
			if k == catalog.SettingSourceOverrides {
				badRequest(w, "app source overrides: "+catalog.ValidOverrides(strings.TrimSpace(v)).Error())
				return
			}
			badRequest(w, "invalid value for "+k)
			return
		}
	}
	// The files root is the one setting whose value has to be tried, not
	// just parsed — and one that refuses when the current root holds
	// content (that is a move, done through Files, not a settings write).
	if v, ok := req[files.SettingRoot]; ok {
		if err := s.files.PrepareRootChange(strings.TrimSpace(v)); err != nil {
			if errors.Is(err, files.ErrRootHasContent) {
				writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "job": "root-move"})
			} else {
				badRequest(w, err.Error())
			}
			return
		}
	}
	actor := s.actor(r)
	touchProxy := false
	rootChanged := false
	passChanged := false
	oldSource := selfupdate.EffectiveSource(s.store)
	for k, v := range req {
		v := strings.TrimSpace(v)
		if k == files.SettingRoot {
			if cur, _ := s.store.GetSetting(k); cur != v {
				rootChanged = true
			}
		}
		// The recovery-kit passphrase is sealed with the vault, never written
		// to the settings table in the clear (backup.StoreKitPassphrase).
		if k == backup.SettingRecoveryKitPassphrase {
			if backup.KitPassphrase(s.store, s.vault) != v {
				passChanged = true
			}
			if err := backup.StoreKitPassphrase(s.store, s.vault, v); err != nil {
				writeError(w, err)
				return
			}
			s.auditAs(actor, "settings.update", k, nil)
			if v != "" {
				s.store.DismissNoticeByKind(backup.NoticeRecoveryKitUnscheduled, "")
			}
			continue
		}
		if err := s.store.SetSetting(k, v); err != nil {
			writeError(w, err)
			return
		}
		s.auditAs(actor, "settings.update", k, nil)
		if proxySettings[k] {
			touchProxy = true
		}
	}
	// The update token belongs to the host it was saved for: a source on
	// another host drops it, so no one can collect it by pointing the
	// source at a server of their own.
	if _, ok := req[selfupdate.SettingUpdateBaseURL]; ok && !selfupdate.TokenFollowsSource(oldSource, selfupdate.EffectiveSource(s.store)) {
		if keys, _ := s.store.SecretKeys(0); slices.Contains(keys, selfupdate.SecretUpdateBasicAuth) {
			s.store.DeleteSecret(0, selfupdate.SecretUpdateBasicAuth)
			s.auditAs(actor, "system.secret.update", selfupdate.SecretUpdateBasicAuth, map[string]any{"cleared": "update source moved to another host"})
		}
		s.store.SetSetting(selfupdate.SettingUpdateTokenOrigin, "")
	}
	if touchProxy {
		s.goSafe("proxy apply", s.applyProxy)
	}
	if rootChanged {
		s.files.RootChanged()
		s.goSafe("files walk", func() { s.files.Walk(s.bgContext()) })
	}
	// Everything sealed under the passphrase follows the change, or
	// recovery on a new machine needs the old one: a fresh kit, so the
	// newest kit (and the copy offsite sync uploads) opens with the new
	// passphrase, and the lockers' wrap of their keys. A KDF and a bucket
	// round trip per target — off the request. A blank passphrase leaves
	// both as they are.
	if passChanged && strings.TrimSpace(req[backup.SettingRecoveryKitPassphrase]) != "" {
		s.goSafe("passphrase change", func() {
			ctx, cancel := context.WithTimeout(s.bgContext(), 5*time.Minute)
			defer cancel()
			if s.backup != nil {
				if err := s.backup.WriteRecoveryKit(ctx); err != nil {
					s.log.Warn("recovery kit after passphrase change", "err", err)
				}
			}
			if s.offsite != nil {
				if err := s.offsite.PassphraseChanged(ctx); err != nil {
					s.log.Warn("offsite rewrap after passphrase change", "err", err)
				}
			}
		})
	}
	s.handleGetSettings(w, r)
}

// ---- audit ----

// ?app=<name> narrows the log to one app's own entries — what fog did to
// it, and what it did through fog — for the app page's activity feed.
// ?before=<id> pages the whole log back past its newest 200.
func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	var entries []store.AuditEntry
	var err error
	if app := r.URL.Query().Get("app"); app != "" {
		entries, err = s.store.AppAudit(app, 100)
	} else if b := r.URL.Query().Get("before"); b != "" {
		before, perr := strconv.ParseInt(b, 10, 64)
		if perr != nil {
			badRequest(w, "before must be an entry id")
			return
		}
		entries, err = s.store.AuditBefore(before, 200)
	} else {
		entries, err = s.store.RecentAudit(200)
	}
	if err != nil {
		writeError(w, err)
		return
	}
	if entries == nil {
		entries = []store.AuditEntry{}
	}
	writeJSON(w, http.StatusOK, entries)
}

// pathInt64 reads a numeric path segment such as {id}. A malformed one names
// nothing that could exist, so it answers 404 like a missing row would.
func pathInt64(w http.ResponseWriter, r *http.Request, key string) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue(key), 10, 64)
	if err != nil {
		notFound(w, "not found")
		return 0, false
	}
	return id, true
}

// appFromPath resolves the {name} path segment to its app: 404 when there is
// no such app, 500 when the lookup itself failed.
func (s *Server) appFromPath(w http.ResponseWriter, r *http.Request) (*store.App, bool) {
	app, err := s.store.AppByName(r.PathValue("name"))
	if errors.Is(err, store.ErrNotFound) {
		notFound(w, "no such app")
		return nil, false
	}
	if err != nil {
		writeError(w, err)
		return nil, false
	}
	return app, true
}

// audit records an action by the signed-in person. detail becomes the JSON
// object the activity views parse; nil records none.
func (s *Server) audit(r *http.Request, action, subject string, detail map[string]any) {
	s.auditAs(s.actor(r), action, subject, detail)
}

// auditAs is audit for an actor other than the request's signed-in person:
// an app, the federation plane, or someone signing in.
func (s *Server) auditAs(actor, action, subject string, detail map[string]any) {
	s.store.Audit(actor, action, subject, store.AuditDetail(detail))
	// A change to someone's sign-in is also told to them
	// (people_handlers_notify.go).
	s.tellAccountChange(actor, action, subject, detail)
}
