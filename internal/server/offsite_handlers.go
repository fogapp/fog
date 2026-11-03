package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"gitea.lan/matthew/fog/internal/backup"
	"gitea.lan/matthew/fog/internal/ipguard"
	"gitea.lan/matthew/fog/internal/jobs"
	"gitea.lan/matthew/fog/internal/objstore"
	"gitea.lan/matthew/fog/internal/offsite"
	"gitea.lan/matthew/fog/internal/store"
)

// ---- offsite targets ----

// offsiteTargetView is a target as the UI sees it: the row, the parsed
// probe, the byte totals behind the cost meter. Never the credentials.
type offsiteTargetView struct {
	store.OffsiteTarget
	ProbeResult *offsite.ProbeResult    `json:"probeResult,omitempty"`
	GradeLabel  string                  `json:"gradeLabel"`
	LockerRoot  string                  `json:"lockerRoot"`
	Totals      store.OffsiteCopyTotals `json:"totals"`
	// MonthlyCost is stored + hidden bytes at the target's price, USD.
	MonthlyCost float64 `json:"monthlyCost"`
	// Job is the running sync, when one is.
	Job *jobs.Job `json:"job,omitempty"`
}

// offsiteTargetDetail is what the audit log records about a target: where
// it points and how well it protects, never its credentials.
func offsiteTargetDetail(t *store.OffsiteTarget) map[string]any {
	return map[string]any{"endpoint": t.Endpoint, "bucket": t.Bucket, "prefix": t.Prefix, "grade": t.Grade}
}

func (s *Server) offsiteView(t *store.OffsiteTarget) offsiteTargetView {
	v := offsiteTargetView{OffsiteTarget: *t, ProbeResult: offsite.ParseProbe(t.Probe), GradeLabel: offsite.GradeLabel(t.Grade),
		LockerRoot: offsite.LayoutOf(t).Root()}
	v.Probe = "" // the parsed form travels; the raw JSON is noise
	if totals, err := s.store.OffsiteCopyTotals(t.ID); err == nil {
		v.Totals = totals
		v.MonthlyCost = float64(totals.StoredBytes+totals.HiddenBytes) / 1e12 * t.PricePerTB
	}
	if job, ok := s.offsite.CurrentJobFor(t.Name); ok {
		v.Job = &job
	}
	return v
}

// ---- restore doors ----

func (s *Server) handleOffsiteJob(w http.ResponseWriter, r *http.Request) {
	job, ok := s.offsite.Job(r.PathValue("id"))
	if !ok {
		notFound(w, "no such job")
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// handleAppOffsite lists an app's backups as the lockers describe them —
// including ones this box never made.
func (s *Server) handleAppOffsite(w http.ResponseWriter, r *http.Request) {
	rows, err := s.offsite.AppRows(r.Context(), r.PathValue("name"))
	if err != nil && len(rows) == 0 {
		writeJSON(w, http.StatusBadGateway, offsiteRowsResponse{Error: err.Error(), Rows: rows})
		return
	}
	writeJSON(w, http.StatusOK, offsiteRowsResponse{Rows: rows})
}

func (s *Server) handleOffsiteFetch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Target string `json:"target"`
		App    string `json:"app"`
		Stamp  string `json:"stamp"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	job, err := s.offsite.StartFetch(req.Target, req.App, req.Stamp)
	if err != nil {
		offsiteError(w, err, nil)
		return
	}
	s.audit(r, "offsite.fetch.start", req.App, map[string]any{"target": req.Target, "stamp": req.Stamp})
	writeJSON(w, http.StatusAccepted, offsiteJobStarted{Job: job})
}

func (s *Server) handleOffsiteFiles(w http.ResponseWriter, r *http.Request) {
	list, err := s.offsite.FilesSnapshots(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, offsiteFilesResponse{Snapshots: list})
}

func (s *Server) handleOffsiteFilesRestore(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Target string `json:"target"`
		Stamp  string `json:"stamp"`
		To     string `json:"to"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	job, err := s.offsite.StartFilesRestore(req.Target, req.Stamp, req.To)
	if err != nil {
		offsiteError(w, err, nil)
		return
	}
	s.audit(r, "offsite.files.restore.start", req.Stamp, map[string]any{"target": req.Target})
	writeJSON(w, http.StatusAccepted, offsiteJobStarted{Job: job})
}

func (s *Server) handleOffsiteRecover(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Target string `json:"target"`
		// FromListing is the operator's go-ahead to recover from the
		// bucket's listing when the index fog recorded is missing.
		FromListing bool `json:"fromListing"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	job, err := s.offsite.StartRecover(req.Target, req.FromListing)
	if errors.Is(err, offsite.ErrIndexMissing) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "code": "index_missing"})
		return
	}
	if err != nil {
		offsiteError(w, err, nil)
		return
	}
	s.audit(r, "offsite.recover.start", req.Target, nil)
	writeJSON(w, http.StatusAccepted, offsiteJobStarted{Job: job})
}

// handleRestoreOffsite is the setup screen's door: with nothing but bucket
// credentials and the kit passphrase, look into the locker (dryRun) or
// pull its newest kit and restore it exactly as handleRestoreKit would.
// Closed once setup has happened, like restore-kit.
func (s *Server) handleRestoreOffsite(w http.ResponseWriter, r *http.Request) {
	// A costly key derivation and an outbound fetch to an operator-named host,
	// reachable before the first operator exists — rate-limit it so it cannot
	// be hammered as a DoS or an SSRF probe.
	if !s.limitStranger(w, r, "setup-restore", "too many recovery attempts; wait a few minutes") {
		return
	}
	n, err := s.store.CountOperators()
	if err != nil {
		writeError(w, err)
		return
	}
	if n > 0 {
		forbidden(w, "setup already completed — a running fog does not accept state replacement")
		return
	}
	var req struct {
		offsite.TargetSpec
		KeyID      string `json:"keyId"`
		Secret     string `json:"secret"`
		Passphrase string `json:"passphrase"`
		DryRun     bool   `json:"dryRun"`
		// AcceptLegacyKit is the operator's go-ahead to restore one particular
		// legacy kit, whose date cannot be verified: the name and digest the
		// preview showed. A different kit — swapped in after the preview —
		// does not inherit the consent.
		AcceptLegacyKit *struct {
			Name   string `json:"name"`
			SHA256 string `json:"sha256"`
		} `json:"acceptLegacyKit"`
		// Kit is the kit the preview showed, by name and digest. The restore
		// uses exactly that kit or none: a bucket writer who swaps, deletes
		// or adds a kit between the preview and the click cannot change what
		// is restored.
		Kit *offsite.KitPin `json:"kit"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Name == "" {
		req.Name = "offsite"
	}
	// Before the first operator exists this endpoint fetches whatever URL the
	// caller gives, which is SSRF: confine it to a public object-storage
	// endpoint so it cannot be aimed at a link-local metadata service or a
	// host inside the LAN.
	if err := restoreEndpointGuard(req.Endpoint); err != nil {
		badRequest(w, err.Error())
		return
	}
	ctx, cancel := offsiteCtx(r)
	defer cancel()
	reader, lk, err := offsite.OpenForRecovery(ctx, req.TargetSpec, req.KeyID, req.Secret, req.Passphrase, restoreDialGuard)
	if err != nil {
		// Do not reflect the upstream's error body back to an unauthenticated
		// caller: it is how an SSRF probe reads an internal service's reply.
		badRequest(w, lockerReachError(s, err))
		return
	}
	if req.DryRun {
		pv, err := offsite.Preview(ctx, reader, lk)
		if err != nil {
			badRequest(w, lockerReachError(s, err))
			return
		}
		// Say, before anything is restored, which kit it would use and what
		// it is — a sealed date, or a legacy kit whose date cannot be
		// verified — and which newer kits had to be skipped, and why.
		if len(pv.Kits) > 0 {
			if kit, err := s.openNewestKit(ctx, reader, req.Passphrase); err != nil {
				pv.NewestKit = &offsite.NewestKit{Name: pv.Kits[0].Name, Error: err.Error()}
			} else {
				pv.NewestKit = newestKitView(kit)
			}
		}
		writeJSON(w, http.StatusOK, pv)
		return
	}
	if req.Kit == nil || req.Kit.Name == "" || req.Kit.SHA256 == "" {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "preview the locker first: a restore names the kit the preview showed",
			"code":  "kit_unconfirmed",
		})
		return
	}
	kit, err := offsite.OpenKit(ctx, reader, req.Passphrase, req.Kit, time.Now())
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "the kit the preview showed cannot be restored now — preview the locker again: " + lockerReachError(s, err),
			"code":  "kit_changed",
		})
		return
	}
	for _, sk := range kit.Skipped {
		s.log.Warn("offsite recovery: skipped a kit", "kit", sk.Name, "reason", sk.Reason)
	}
	digest := kitDigest(kit.Sealed)
	if kit.Check.Legacy {
		a := req.AcceptLegacyKit
		if a == nil || a.Name != kit.Name || a.SHA256 != digest {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":   "the kit to restore is a legacy kit: " + backup.LegacyKitWarning,
				"code":    "legacy_kit",
				"kit":     kit.Name,
				"sha256":  digest,
				"kitDate": kit.Check.Date,
			})
			return
		}
	}
	plain, name := kit.Plain, kit.Name
	if err := s.stageKit(plain); err != nil {
		badRequest(w, "kit contents: "+err.Error())
		return
	}
	s.log.Info("recovery kit restored from offsite", "kit", name, "locker", lk.ID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "restarting": true, "kit": name, "skipped": kit.Skipped})
	s.restartAfterKit()
}

// openNewestKit finds the kit a cloud recovery would use (offsite.OpenNewestKit):
// names dated in the future ignored; kits fog recorded uploading, in the
// sealed index, first and checked against their recorded digests; every
// current-format kit before any legacy one; only decryptions counted against
// the attempt cap, so planted junk cannot exhaust it. Kits passed over are
// reported, never allowed to block the recovery, and a choice older than the
// newest kit says so (FellBack). The restore then opens exactly the kit the
// preview showed (offsite.OpenKit with its pin).
func (s *Server) openNewestKit(ctx context.Context, reader *offsite.Reader, passphrase string) (*offsite.OpenedKit, error) {
	kit, err := offsite.OpenNewestKit(ctx, reader, passphrase)
	if err != nil {
		return nil, errors.New(lockerReachError(s, err))
	}
	return kit, nil
}

// newestKitView is what the setup preview shows about the kit it would use.
func newestKitView(kit *offsite.OpenedKit) *offsite.NewestKit {
	nk := &offsite.NewestKit{Name: kit.Name, Legacy: kit.Check.Legacy, Date: kit.Check.Date,
		SHA256: kitDigest(kit.Sealed), Skipped: kit.Skipped, Recorded: kit.Recorded, FellBack: kit.FellBack, Latest: kit.Latest}
	if kit.Check.Legacy {
		nk.Warning = backup.LegacyKitWarning
	}
	return nk
}

// kitDigest is a sealed kit's hex SHA-256.
func kitDigest(sealed []byte) string {
	sum := sha256.Sum256(sealed)
	return hex.EncodeToString(sum[:])
}

// restoreEndpointGuard vets the restore-offsite endpoint. A var so the
// recovery-door tests, whose fake bucket is on loopback, can relax it; in
// production it is the real public-endpoint check.
var restoreEndpointGuard = publicStorageEndpoint

// restoreDialGuard vets the address of every connection the recovery door
// makes — including redirect hops — so a redirect or DNS rebinding past the
// URL-level endpoint check cannot reach an internal host. A var for the same
// reason: the loopback tests relax it.
var restoreDialGuard = func(network, address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || !publicUnicast(ip) {
		return fmt.Errorf("refusing to connect to non-public address %s", address)
	}
	return nil
}

// publicStorageEndpoint confines the pre-setup restore-offsite door to a
// public object-storage URL. Before the first operator exists the door
// fetches whatever endpoint the caller names, so without this it is an SSRF
// primitive: aim it at 169.254.169.254 or a host on the LAN and read the
// reply. Every address the host resolves to must be public unicast.
func publicStorageEndpoint(raw string) error {
	bad := errors.New("the endpoint must be a public object-storage service, like https://s3.us-west-004.backblazeb2.com")
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" {
		return bad
	}
	host := u.Hostname()
	var ips []net.IP
	if ip := net.ParseIP(host); ip != nil {
		ips = []net.IP{ip}
	} else {
		resolved, err := net.LookupIP(host)
		if err != nil || len(resolved) == 0 {
			return errors.New("the endpoint host does not resolve")
		}
		ips = resolved
	}
	for _, ip := range ips {
		if !publicUnicast(ip) {
			return bad
		}
	}
	return nil
}

// publicUnicast is the shared public-unicast rule (internal/ipguard): every
// special-purpose range — carrier NAT, benchmarking, "this network",
// documentation — and every IPv6 spelling of a private IPv4 (mapped, NAT64,
// 6to4, Teredo) is refused. The link-preview and push guards use the same.
func publicUnicast(ip net.IP) bool { return ipguard.PublicUnicast(ip) }

// lockerReachError keeps an upstream HTTP body out of the reply to an
// unauthenticated caller — the body is how an SSRF probe would read an
// internal service's response. fog's own messages (wrong passphrase, no
// locker) pass through; a raw bucket error is logged and generalised.
func lockerReachError(s *Server, err error) string {
	// A bucket's HTTP answer, or any transport failure (a refused or
	// guard-blocked dial, DNS, TLS, timeout), says something about hosts the
	// caller pointed fog at — exactly what an SSRF probe wants back. None of
	// it is reflected; fog's own verdicts (wrong passphrase, no locker) are.
	var oe *objstore.Error
	var ue *url.Error
	var ne net.Error
	var ope *net.OpError
	if errors.As(err, &oe) || errors.As(err, &ue) || errors.As(err, &ne) || errors.As(err, &ope) {
		s.log.Warn("offsite recovery: bucket error", "err", err)
		return "could not read the locker from that bucket — check the endpoint, region, bucket, access key, and that a locker exists there"
	}
	return err.Error()
}

func (s *Server) handleOffsiteCopy(w http.ResponseWriter, r *http.Request) {
	name, dest := r.PathValue("name"), r.PathValue("dest")
	job, err := s.offsite.StartCopy(name, dest)
	if err != nil {
		if errors.Is(err, offsite.ErrCopyRunning) {
			conflict(w, err.Error())
			return
		}
		offsiteError(w, err, nil)
		return
	}
	s.audit(r, "offsite.copy.start", dest, map[string]any{"from": name})
	writeJSON(w, http.StatusAccepted, offsiteJobStarted{Job: job})
}

func (s *Server) handleOffsiteRetire(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.offsite.Retire(name); err != nil {
		offsiteError(w, err, nil)
		return
	}
	s.audit(r, "offsite.target.retire", name, nil)
	writeOK(w)
}

func (s *Server) handleOffsiteActivate(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.offsite.Activate(name); err != nil {
		offsiteError(w, err, nil)
		return
	}
	s.audit(r, "offsite.target.activate", name, nil)
	writeOK(w)
}

func (s *Server) handleOffsiteSync(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	job, err := s.offsite.StartSync(name)
	if err != nil {
		if errors.Is(err, offsite.ErrSyncRunning) {
			conflict(w, err.Error())
			return
		}
		offsiteError(w, err, nil)
		return
	}
	s.audit(r, "offsite.sync", name, nil)
	writeJSON(w, http.StatusAccepted, offsiteJobStarted{Job: job})
}

// offsiteCtx detaches probe-and-create work from the request: creating a
// locker derives a key (a second or more) and writes locker.json, and a
// closed tab must not leave credentials stored for a target that never
// got its row.
func offsiteCtx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Minute)
}

func (s *Server) handleOffsitePresets(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, offsitePresetsResponse{Presets: offsite.Presets})
}

func (s *Server) handleOffsiteTargets(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListOffsiteTargets()
	if err != nil {
		writeError(w, err)
		return
	}
	views := make([]offsiteTargetView, 0, len(list))
	for i := range list {
		views = append(views, s.offsiteView(&list[i]))
	}
	pass := backup.KitPassphrase(s.store, s.vault)
	writeJSON(w, http.StatusOK, offsiteTargetsResponse{PassphraseSet: pass != "", Targets: views})
}

// offsiteTargetsResponse is every locker, and whether the recovery-kit
// passphrase that seals what goes into them is set yet.
type offsiteTargetsResponse struct {
	PassphraseSet bool                `json:"passphraseSet"`
	Targets       []offsiteTargetView `json:"targets"`
}

// offsiteSaved is a target as it stands after it was added, changed or
// probed, with the probe that just graded it. Setup is the B2 report, for
// a target fog set up itself. The probe is never nil here: the manager
// only succeeds with one.
type offsiteSaved struct {
	Probe  offsite.ProbeResult    `json:"probe"`
	Setup  *offsite.B2SetupReport `json:"setup,omitempty"`
	Target offsiteTargetView      `json:"target"`
}

// offsiteRefusal is a 400 over a bucket that cannot be used, with the
// probe that showed it, when one got that far.
type offsiteRefusal struct {
	Error string               `json:"error"`
	Probe *offsite.ProbeResult `json:"probe"`
}

// offsiteRowsResponse is an app's backups as the lockers describe them.
// Error is set, on a 502, when no locker could be read at all.
type offsiteRowsResponse struct {
	Error string               `json:"error,omitempty"`
	Rows  []offsite.OffsiteRow `json:"rows"`
}

type offsiteFilesResponse struct {
	Snapshots []offsite.OffsiteFilesSnapshot `json:"snapshots"`
}

type offsitePresetsResponse struct {
	Presets []offsite.Preset `json:"presets"`
}

// offsiteJobStarted answers every offsite verb that runs as a job.
type offsiteJobStarted struct {
	Job jobs.Job `json:"job"`
}

type offsiteTargetReq struct {
	offsite.TargetSpec
	KeyID  string `json:"keyId"`
	Secret string `json:"secret"`
}

// offsiteError maps the manager's refusals onto statuses the UI can act
// on: a missing passphrase is a precondition (409), a bucket that cannot
// be used is the operator's input (400), a low grade is not an error at
// all — it comes back with the target.
func offsiteError(w http.ResponseWriter, err error, probe *offsite.ProbeResult) {
	switch {
	case errors.Is(err, offsite.ErrNoPassphrase):
		conflict(w, err.Error())
	case errors.Is(err, store.ErrNotFound):
		notFound(w, "no such target")
	case errors.Is(err, offsite.ErrProbeFailed), errors.Is(err, offsite.ErrLockerLocked):
		writeJSON(w, http.StatusBadRequest, offsiteRefusal{Error: err.Error(), Probe: probe})
	default:
		badRequest(w, err.Error())
	}
}

func (s *Server) handleOffsiteCreate(w http.ResponseWriter, r *http.Request) {
	var req offsiteTargetReq
	if !readJSON(w, r, &req) {
		return
	}
	ctx, cancel := offsiteCtx(r)
	defer cancel()
	t, probe, err := s.offsite.AddTarget(ctx, req.TargetSpec, req.KeyID, req.Secret)
	if err != nil {
		offsiteError(w, err, probe)
		return
	}
	s.audit(r, "offsite.target.create", t.Name, offsiteTargetDetail(t))
	writeJSON(w, http.StatusCreated, offsiteSaved{Probe: *probe, Target: s.offsiteView(t)})
}

// handleOffsiteB2Setup makes the bucket and a key that cannot destroy
// from the operator's master key, then adds the target with that key. The
// master key lives for this request only: not stored, not audited, not
// echoed back.
func (s *Server) handleOffsiteB2Setup(w http.ResponseWriter, r *http.Request) {
	var req offsite.B2SetupRequest
	if !readJSON(w, r, &req) {
		return
	}
	ctx, cancel := offsiteCtx(r)
	defer cancel()
	rep, probe, err := s.offsite.SetupB2(ctx, req)
	if err != nil {
		offsiteError(w, err, probe)
		return
	}
	t, err := s.store.OffsiteTargetByName(strings.ToLower(strings.TrimSpace(req.Name)))
	if err != nil {
		writeError(w, err)
		return
	}
	how := "created"
	if rep.Adopted {
		how = "adopted"
	}
	s.audit(r, "offsite.target.setup", t.Name, map[string]any{"bucket": rep.Bucket, "bucketWas": how, "key": rep.KeyName, "capabilities": strings.Join(rep.Capabilities, ","), "grade": t.Grade})
	writeJSON(w, http.StatusCreated, offsiteSaved{Probe: *probe, Setup: rep, Target: s.offsiteView(t)})
}

func (s *Server) handleOffsiteUpdate(w http.ResponseWriter, r *http.Request) {
	var req offsiteTargetReq
	if !readJSON(w, r, &req) {
		return
	}
	ctx, cancel := offsiteCtx(r)
	defer cancel()
	name := r.PathValue("name")
	t, probe, err := s.offsite.UpdateTarget(ctx, name, req.TargetSpec, req.KeyID, req.Secret)
	if err != nil {
		offsiteError(w, err, probe)
		return
	}
	s.audit(r, "offsite.target.update", name, offsiteTargetDetail(t))
	writeJSON(w, http.StatusOK, offsiteSaved{Probe: *probe, Target: s.offsiteView(t)})
}

func (s *Server) handleOffsiteProbe(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := offsiteCtx(r)
	defer cancel()
	name := r.PathValue("name")
	t, probe, err := s.offsite.Reprobe(ctx, name)
	if err != nil {
		offsiteError(w, err, probe)
		return
	}
	s.audit(r, "offsite.target.probe", name, map[string]any{"grade": t.Grade})
	writeJSON(w, http.StatusOK, offsiteSaved{Probe: *probe, Target: s.offsiteView(t)})
}

func (s *Server) handleOffsiteDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.offsite.DeleteTarget(name); err != nil {
		offsiteError(w, err, nil)
		return
	}
	s.audit(r, "offsite.target.delete", name, nil)
	writeOK(w)
}
