package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/backup"
	"gitea.lan/matthew/fog/internal/bootmark"
	"gitea.lan/matthew/fog/internal/fsx"
	"gitea.lan/matthew/fog/internal/store"
	"gitea.lan/matthew/fog/internal/updates"
)

// ---- backups ----

// snapshotCtx detaches long-running snapshot work from the request while
// keeping the handler waiting for a verdict.
//
// The disconnect matters: a restore killed between DROP DATABASE and
// pg_restore leaves the app with no database at all, and a browser tab closing
// is not a reason to do that. WithoutCancel keeps the work alive; the handler
// still blocks, so the operator gets a real answer rather than an optimistic
// 202 for something that may have failed.
func snapshotCtx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Hour)
}

func (s *Server) handleCreateBackup(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	ctx, cancel := snapshotCtx(r)
	defer cancel()
	id, err := s.backup.BackupApp(ctx, name, "manual")
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "id": id})
}

func (s *Server) handleListBackups(w http.ResponseWriter, r *http.Request) {
	app, ok := s.appFromPath(w, r)
	if !ok {
		return
	}
	list, err := s.store.ListBackups(app.ID, 50)
	if err != nil {
		writeError(w, err)
		return
	}
	if list == nil {
		list = []store.Backup{}
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleRestoreBackup(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	// The body is optional: a bare POST restores as it always has.
	var req struct {
		ForgetSecrets bool `json:"forgetSecrets"`
	}
	if r.ContentLength != 0 && !readJSON(w, r, &req) {
		return
	}
	ctx, cancel := snapshotCtx(r)
	defer cancel()
	if err := s.backup.RestoreAppWith(ctx, id, req.ForgetSecrets); err != nil {
		badRequest(w, err.Error())
		return
	}
	if req.ForgetSecrets {
		s.audit(r, "backup.restore.forget-secrets", strconv.FormatInt(id, 10), nil)
	}
	writeOK(w)
}

// handleRestoreArtifact restores a foreign fog-backup/1 artifact (an upload,
// or a file already on this host) into an existing delegated app.
func (s *Server) handleRestoreArtifact(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path            string `json:"path"`
		ConfirmIdentity bool   `json:"confirmIdentity"`
		ForgetSecrets   bool   `json:"forgetSecrets"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Path == "" {
		badRequest(w, "path required — upload the artifact first (POST /api/system/backup-uploads)")
		return
	}
	ctx, cancel := snapshotCtx(r)
	defer cancel()
	name := r.PathValue("name")
	err := s.backup.RestoreAppFromArtifact(ctx, name, req.Path, req.ConfirmIdentity, req.ForgetSecrets)
	// The upload has been copied into the app's staging area either way, and
	// a failed restore is retried by uploading again — keeping the original
	// only leaves GB-scale files in the uploads directory for good.
	s.cleanupUpload(req.Path)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	s.audit(r, "backup.restore.artifact", name, nil)
	writeOK(w)
}

// handleInspectArtifact reads an uploaded artifact's manifest and reports what
// it is — which app produced it, when, from what code, and which env values its
// data was encrypted with.
//
// The fingerprints travel deliberately: they are 12 hex characters of a digest,
// never a value, and handing them to the install form is what lets it check a
// pasted secret against the artifact before the app is created rather than
// after its first boot fails.
func (s *Server) handleInspectArtifact(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	path, err := s.resolveRestoreArtifact(&restoreFromReq{Path: req.Path})
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	m, err := backup.InspectArtifact(path)
	if err != nil {
		badRequest(w, "this file is not a fog-backup/1 artifact: "+err.Error())
		return
	}
	var totalBytes int64
	for _, c := range m.Components {
		totalBytes += c.Bytes
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"app":           m.App,
		"format":        m.Format,
		"createdAt":     m.CreatedAt,
		"gitSha":        m.ProducedBy.GitSHA,
		"schemaVersion": m.ProducedBy.SchemaVersion,
		"components":    m.Components,
		"totalBytes":    totalBytes,
		"secrets":       m.SecretsFingerprint,
		"identityHost":  m.Identity.Host,
		"excluded":      m.Excluded,
	})
}

// handleCancelRestore calls off a restore that is failing or stuck: it
// interrupts fog's wait, disarms the staged marker so the app stops retrying
// it on every boot, and brings the app back up.
//
// Detached from the request like the restore itself, and for a sharper
// reason: a cancel abandoned halfway — marker deleted, app never started —
// is a worse state than either the restore or the cancel completing.
func (s *Server) handleCancelRestore(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 15*time.Minute)
	defer cancel()
	if err := s.backup.CancelRestore(ctx, name, s.actor(r)); err != nil {
		badRequest(w, err.Error())
		return
	}
	writeOK(w)
}

// handleBackupUpload streams a multipart artifact upload to disk and answers
// with the server-side path — the handle create/restore calls then use.
// Streamed part-by-part: artifacts are GB-scale and must never be buffered
// in memory.
func (s *Server) handleBackupUpload(w http.ResponseWriter, r *http.Request) {
	mr, err := r.MultipartReader()
	if err != nil {
		badRequest(w, "expected a multipart upload with an `artifact` part")
		return
	}
	dir := filepath.Join(s.cfg.DataDir, "uploads")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		writeError(w, err)
		return
	}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			badRequest(w, "reading upload: "+err.Error())
			return
		}
		if part.FormName() != "artifact" {
			continue
		}
		f, err := os.CreateTemp(dir, "artifact-*.tar")
		if err != nil {
			writeError(w, err)
			return
		}
		n, err := io.Copy(f, part)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(f.Name())
			writeError(w, fmt.Errorf("saving upload: %w", err))
			return
		}
		s.audit(r, "backup.upload", filepath.Base(f.Name()), map[string]any{"bytes": n})
		writeJSON(w, http.StatusCreated, map[string]any{"path": f.Name(), "bytes": n})
		return
	}
	badRequest(w, "no `artifact` part in the upload")
}

// cleanupUpload removes a consumed upload, but only within the uploads dir —
// a caller-supplied path anywhere else on the host is not ours to delete.
func (s *Server) cleanupUpload(path string) {
	dir := filepath.Join(s.cfg.DataDir, "uploads")
	if filepath.Dir(path) == dir {
		os.Remove(path)
	}
}

func (s *Server) handleDownloadBackup(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	b, err := s.store.BackupByID(id)
	if err != nil {
		notFound(w, "no such backup")
		return
	}
	if b.Format == backup.ArtifactFormat {
		f, err := os.Open(filepath.Join(b.Path, "artifact.tar"))
		if err != nil {
			notFound(w, "artifact missing on disk")
			return
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			writeError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/x-tar")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="backup-%d-%s.fogpack.tar"`, b.ID, filepath.Base(b.Path)))
		w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
		io.Copy(w, f)
		s.audit(r, "backup.download", strconv.FormatInt(b.ID, 10), nil)
		return
	}

	// Legacy volume snapshots are a directory (volume tarballs, database
	// dumps, meta.json) rather than a single file; bundle it into one tar
	// stream so it can be stored off-box. The bundle is for safekeeping and
	// manual extraction — the upload-restore door only accepts fog-backup/1
	// artifacts, so restoring one of these stays on-box.
	entries, err := os.ReadDir(b.Path)
	if err != nil {
		notFound(w, "snapshot missing on disk")
		return
	}
	w.Header().Set("Content-Type", "application/x-tar")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="backup-%d-%s.snapshot.tar"`, b.ID, filepath.Base(b.Path)))
	tw := tar.NewWriter(w)
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return // headers are sent; a truncated stream is the only remaining failure mode
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return
		}
		f, err := os.Open(filepath.Join(b.Path, e.Name()))
		if err != nil {
			return
		}
		if err := tw.WriteHeader(hdr); err != nil {
			f.Close()
			return
		}
		_, err = io.Copy(tw, f)
		f.Close()
		if err != nil {
			return
		}
	}
	if err := tw.Close(); err != nil {
		return
	}
	s.audit(r, "backup.download", strconv.FormatInt(b.ID, 10), nil)
}

func (s *Server) handleDeleteBackup(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	force := r.URL.Query().Get("force") == "1"
	if err := s.backup.DeleteBackup(r.Context(), id, force); err != nil {
		badRequest(w, err.Error())
		return
	}
	s.audit(r, "backup.delete", strconv.FormatInt(id, 10), nil)
	writeOK(w)
}

// ---- updates ----

// handleUpdatesCheck runs a check on demand and answers with what it found.
// The verdict travels in the response so the UI never has to guess from "the
// POST succeeded" — a check that turns up an update, or that failed to reach
// a registry, can't be reported as "up to date". ?app=<name> checks one app.
func (s *Server) handleUpdatesCheck(w http.ResponseWriter, r *http.Request) {
	// Manual check: refresh badges without firing auto policies.
	var results []updates.CheckResult
	if name := r.URL.Query().Get("app"); name != "" {
		res, err := s.updates.CheckOne(r.Context(), name)
		if err != nil {
			notFound(w, "no such app")
			return
		}
		results = []updates.CheckResult{res}
	} else {
		results = s.updates.CheckAll(r.Context(), false)
	}
	available, failed := 0, 0
	for _, res := range results {
		switch {
		case res.Error != "":
			failed++
		case res.UpdateAvailable:
			available++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"checked":          len(results),
		"updatesAvailable": available,
		"failed":           failed,
		"results":          results,
	})
}

func (s *Server) handleApplyUpdate(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	actor := s.actor(r)
	// The body is optional: a bare POST is a plain update.
	var opts updates.ApplyOptions
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&opts); err != nil && err != io.EOF {
		badRequest(w, "invalid JSON body")
		return
	}
	// Already updating/deploying: say so instead of quietly stacking a second
	// run behind the first. Apply re-checks authoritatively in the goroutine.
	if s.engine.Deploying(name) {
		conflict(w, updates.ErrUpdateInProgress.Error())
		return
	}
	if opts.SkipSnapshot {
		s.auditAs(actor, "app.update.skip-snapshot", name, nil)
	}
	s.goSafe("apply update", func() {
		ctx, cancel := s.deployCtx()
		defer cancel()
		var cancelled *apps.CancelledError
		if err := s.updates.Apply(ctx, name, actor, opts); errors.As(err, &cancelled) {
			s.log.Info("apply update", "app", name, "result", err)
		} else if err != nil {
			s.log.Error("apply update", "app", name, "err", err)
			s.updates.Notify("Update failed: "+name, err.Error(), false)
		}
	})
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}

// ---- recovery kit ----

// handleRecoveryKit sends Fog's unrecoverable state — the vault master key
// and a consistent SQLite snapshot — sealed with the recovery-kit passphrase
// (backup.EncryptKit, the format handleRestoreKit accepts). Store it off-box.
// Plaintext never leaves the box: the kit carries the vault master key, and
// an unsealed download would also be useless at the DR door.
//
// The kit is built and sealed fully in memory (a few MB) before any header is
// written. Streaming it straight to the client meant any mid-archive failure
// arrived after a 200 and a Content-Disposition, so the browser saved a
// truncated file that looked like a completed download — of the one file that
// cannot be regenerated if it is ever needed and found wanting.
func (s *Server) handleRecoveryKit(w http.ResponseWriter, r *http.Request) {
	sealed, err := s.backup.SealKit(r.Context())
	if errors.Is(err, backup.ErrNoKitPassphrase) {
		badRequest(w, "set a recovery-kit passphrase (Settings → Storage → Recovery kit) first — the kit is only ever stored encrypted")
		return
	}
	if err != nil {
		s.log.Error("recovery kit", "err", err)
		writeError(w, fmt.Errorf("could not build the recovery kit: %w", err))
		return
	}

	// Named like the scheduled kits so the restore screen's "pick your
	// fog-recovery-kit-*.enc" instruction covers both.
	name := "fog-recovery-kit-" + fsx.Stamp(time.Now()) + ".enc"
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, name))
	w.Header().Set("Content-Length", strconv.Itoa(len(sealed)))
	if _, err := w.Write(sealed); err != nil {
		s.log.Error("recovery kit: sending", "err", err)
		return
	}
	s.audit(r, "system.recovery-kit.download", "", nil)
}

// handleRestoreKit is the full-server DR door: on a fresh fog install
// (before any user exists), upload an encrypted recovery kit + passphrase
// and fog restores its own state — master key and database — then restarts
// itself; systemd brings it back with the restored identity, vault, and app
// specs, ready to recreate apps with restoreFrom. Closed permanently once
// setup has happened: after that, state restoration is not a thing an
// unauthenticated caller may do.
func (s *Server) handleRestoreKit(w http.ResponseWriter, r *http.Request) {
	// Decrypting a kit is a deliberately expensive Argon2id derivation,
	// reachable before the first operator exists — rate-limit it per client
	// so it cannot be used to grind the box.
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
	// Kits are small (master.key + a vacuumed SQLite file, a few MB); the cap
	// only needs headroom, not GBs of buffered upload.
	r.Body = http.MaxBytesReader(w, r.Body, 64<<20)
	if err := r.ParseMultipartForm(16 << 20); err != nil {
		badRequest(w, "expected a multipart upload with `kit` and `passphrase`")
		return
	}
	pass := r.FormValue("passphrase")
	file, _, err := r.FormFile("kit")
	if err != nil {
		badRequest(w, "missing `kit` file")
		return
	}
	defer file.Close()
	sealed, err := io.ReadAll(file)
	if err != nil {
		writeError(w, err)
		return
	}
	plain, err := backup.DecryptKit(sealed, pass)
	if err != nil {
		badRequest(w, err.Error())
		return
	}

	if err := s.stageKit(plain); err != nil {
		badRequest(w, "kit contents: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "restarting": true})
	s.restartAfterKit()
}

// stageKit unpacks a decrypted kit into restore-pending, where the next
// boot picks it up (cmd/fogd/main.go).
func (s *Server) stageKit(plain []byte) error {
	pending := filepath.Join(s.cfg.DataDir, "restore-pending")
	if err := os.MkdirAll(pending, 0o700); err != nil {
		return err
	}
	if err := unpackKit(plain, pending); err != nil {
		os.RemoveAll(pending)
		return err
	}
	return nil
}

// restartAfterKit exits after the response flushes; systemd restarts fogd
// and the staged files move into place before the store opens. A
// deliberate restart, not a crash — the boot marker says so.
func (s *Server) restartAfterKit() {
	go func() {
		time.Sleep(1 * time.Second)
		bootmark.EndClean(s.cfg.DataDir)
		os.Exit(0)
	}()
}

// unpackKit extracts exactly the two files a kit may contain — nothing else
// in an uploaded archive gets to touch the data dir.
func unpackKit(kitTarGz []byte, dst string) error {
	gz, err := gzip.NewReader(bytes.NewReader(kitTarGz))
	if err != nil {
		return fmt.Errorf("not a gzipped kit: %w", err)
	}
	tr := tar.NewReader(gz)
	seen := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name := filepath.Base(filepath.Clean(hdr.Name))
		if name != "master.key" && name != "fog.db" {
			continue
		}
		out, err := os.OpenFile(filepath.Join(dst, name), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, tr)
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		seen[name] = true
	}
	if !seen["fog.db"] || !seen["master.key"] {
		return fmt.Errorf("kit must contain fog.db and master.key")
	}
	return nil
}

// ---- self-update ----

func (s *Server) handleSelfUpdateCheck(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.updater.Check(r.Context()))
}

func (s *Server) handleIgnoreUpdate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Version string `json:"version"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Version == "" {
		badRequest(w, "version required")
		return
	}
	// An unsaved ignore would resurface the update banner on the next check
	// while this response claims it's dismissed.
	if err := s.store.SetSetting(store.SettingIgnoredUpdateVersion, req.Version); err != nil {
		writeError(w, err)
		return
	}
	s.audit(r, "fogd.update.ignore", req.Version, nil)
	writeOK(w)
}

func (s *Server) handleSelfUpdateApply(w http.ResponseWriter, r *http.Request) {
	// The updater exits so systemd restarts the new binary; mark the stop
	// deliberate first so the next boot doesn't read as a crash.
	exit := func(code int) {
		bootmark.EndClean(s.cfg.DataDir)
		os.Exit(code)
	}
	if err := s.updater.Apply(r.Context(), exit); err != nil {
		badRequest(w, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "restarting": true})
}
