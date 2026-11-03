// Delegated backups: the fog side of the fog-backup/1 contract
// (docs/backup-contract.md). The app produces a single artifact; fog starts
// the job, polls it, downloads the artifact, and independently verifies the
// digest before recording anything. An unverifiable download is a failed
// backup, not a recorded one.
package backup

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/fsx"
	"gitea.lan/matthew/fog/internal/store"
)

const (
	// ArtifactFormat is the contract version fog speaks.
	ArtifactFormat = "fog-backup/1"
	// defaultDelegatedTimeout bounds produce + download when the spec
	// doesn't say otherwise. Generous: a timeout that fails a slow but
	// progressing dump is worse than a long wait.
	defaultDelegatedTimeout = 30 * time.Minute
	pollInterval            = 5 * time.Second
	// manifestLimit caps how much manifest fog will hold in memory/DB.
	manifestLimit = 1 << 20
	// DefaultMaxArtifactBytes caps a delegated download. The artifact is
	// produced and served by the app, which is untrusted, so without a cap an
	// app could stream without end and fill the backup disk. Generous — a
	// home-server app's database plus file state — and overridable per app
	// (spec.backup.maxBytes) or fleet-wide (SettingBackupMaxBytes).
	DefaultMaxArtifactBytes = 50 << 30
)

// artifactInfo is the app's claim about the finished artifact; fog verifies
// both fields independently after download.
type artifactInfo struct {
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

type jobStatus struct {
	Status   string        `json:"status"`
	Progress string        `json:"progress,omitempty"`
	Error    string        `json:"error,omitempty"`
	Artifact *artifactInfo `json:"artifact,omitempty"`
}

// manifestSummary is the slice of manifest.json fog itself understands.
// The full document is stored verbatim for the UI and preflight.
type manifestSummary struct {
	Format string `json:"format"`
	App    string `json:"app"`
}

// ManifestApp is the app identity a stored delegated manifest names, or ""
// when there is none or it doesn't parse.
func ManifestApp(manifest string) string {
	if manifest == "" {
		return ""
	}
	var ms manifestSummary
	if json.Unmarshal([]byte(manifest), &ms) != nil {
		return ""
	}
	return ms.App
}

func delegatedTimeout(spec *apps.AppSpec) time.Duration {
	if spec.Backup != nil && spec.Backup.Timeout != "" {
		if d, err := time.ParseDuration(spec.Backup.Timeout); err == nil {
			return d
		}
	}
	return defaultDelegatedTimeout
}

// Descriptor is the app's answer to GET /fog/backup: what it would back up
// and the environment it believes it runs in. The issues scanner reads it
// to catch discrepancies (wrong Postgres, dead contract endpoint) between
// backups instead of during one.
type Descriptor struct {
	Contract string `json:"contract"`
	App      string `json:"app"`
	Version  struct {
		GitSHA        string `json:"git_sha"`
		SchemaVersion string `json:"schema_version"`
	} `json:"version"`
	PG struct {
		ServerMajor int               `json:"server_major"`
		Extensions  map[string]string `json:"extensions"`
	} `json:"pg"`
	Fresh          bool  `json:"fresh"`
	EstimatedBytes int64 `json:"estimated_bytes"`
}

// descriptorTimeout bounds the probe: the contract says the descriptor
// answers instantly, so anything slow is itself a finding.
const descriptorTimeout = 10 * time.Second

// Descriptor fetches an app's fog-backup/1 descriptor.
func (m *Manager) Descriptor(ctx context.Context, app *store.App, spec *apps.AppSpec) (*Descriptor, error) {
	ctx, cancel := context.WithTimeout(ctx, descriptorTimeout)
	defer cancel()
	client, err := m.newAppClient(ctx, app, spec)
	if err != nil {
		return nil, err
	}
	return client.descriptor(ctx)
}

// delegatedBackup runs one delegated snapshot. Caller holds the app lock.
func (m *Manager) delegatedBackup(ctx context.Context, app *store.App, spec *apps.AppSpec, kind string) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, delegatedTimeout(spec))
	defer cancel()

	client, err := m.newAppClient(ctx, app, spec)
	if err != nil {
		return 0, err
	}

	// What the app says it is, for checking the artifact it hands back.
	// Fog's name for the app is a label ("hub-lxc"); the identity is the
	// software ("hub"), declared by descriptor and manifest alike.
	identity := ""
	if d, err := client.descriptor(ctx); err == nil {
		identity = d.App
	}

	jobID, err := client.startBackup(ctx)
	if err != nil {
		return 0, err
	}
	m.Log.Info("delegated backup started", "app", app.Name, "job", jobID)

	art, err := m.pollJob(ctx, client, app.Name, jobID)
	if err != nil {
		return 0, err
	}

	snap, err := m.newSnapshotDir(app.Name)
	if err != nil {
		return 0, err
	}
	defer snap.cleanup()
	dir := snap.path
	artifactPath := filepath.Join(dir, "artifact.tar")

	gotSHA, gotBytes, err := client.downloadArtifact(ctx, jobID, artifactPath, m.maxArtifactBytes(spec))
	if err != nil {
		return 0, err
	}
	// The verification handshake: the app claimed bytes+sha256 in job
	// status; fog hashed what actually arrived. Any disagreement means the
	// backup does not exist.
	if gotBytes != art.Bytes || !strings.EqualFold(gotSHA, art.SHA256) {
		return 0, fmt.Errorf("artifact verification failed: got %d bytes sha256 %s, app claimed %d bytes sha256 %s",
			gotBytes, gotSHA, art.Bytes, art.SHA256)
	}

	manifest, err := readManifest(artifactPath)
	if err != nil {
		return 0, fmt.Errorf("artifact manifest: %w", err)
	}
	var ms manifestSummary
	if err := json.Unmarshal(manifest, &ms); err != nil {
		return 0, fmt.Errorf("artifact manifest is not valid JSON: %w", err)
	}
	if ms.Format != ArtifactFormat {
		return 0, fmt.Errorf("artifact declares format %q, expected %q", ms.Format, ArtifactFormat)
	}
	if msg, _ := checkIdentity(ms.App, identity, app.Name); msg != "" {
		return 0, errors.New(msg)
	}
	// Denormalize beside the artifact so a backup directory is
	// self-describing even without fog's database.
	if err := fsx.WriteFileAtomic(filepath.Join(dir, "manifest.json"), manifest, 0o600); err != nil {
		return 0, err
	}

	// Verified; the app's staging copy is no longer needed. Best-effort —
	// the app sweeps stale staging on its own.
	client.deleteJob(ctx, jobID)

	id, err := m.Store.CreateArtifactBackup(app.ID, kind, dir, fsx.DirSize(context.Background(), dir), ArtifactFormat, gotSHA, string(manifest))
	if err != nil {
		return 0, err
	}
	snap.commit()
	m.Store.Audit("system", "backup.create", app.Name, fmt.Sprintf(`{"kind":%q,"mode":"delegated"}`, kind))
	m.prune(app.ID)
	return id, nil
}

func (m *Manager) pollJob(ctx context.Context, client *appClient, appName, jobID string) (*artifactInfo, error) {
	t := time.NewTicker(pollInterval)
	defer t.Stop()
	lastProgress := ""
	for {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.Canceled) {
				return nil, fmt.Errorf("stopped waiting for the app's producer job: %w", ctx.Err())
			}
			return nil, fmt.Errorf("delegated backup timed out waiting for the app's producer job")
		case <-t.C:
			st, err := client.jobStatus(ctx, jobID)
			if err != nil {
				return nil, err
			}
			switch st.Status {
			case "completed":
				if st.Artifact == nil || st.Artifact.SHA256 == "" {
					return nil, fmt.Errorf("producer job completed without artifact metadata")
				}
				return st.Artifact, nil
			case "failed":
				return nil, fmt.Errorf("producer job failed: %s", st.Error)
			case "running":
				if st.Progress != "" && st.Progress != lastProgress {
					lastProgress = st.Progress
					if m.Engine != nil {
						m.Engine.OpStage(appName, "taking a snapshot: "+st.Progress)
					}
				}
			default:
				return nil, fmt.Errorf("producer job reported unknown status %q", st.Status)
			}
		}
	}
}

// readManifest pulls manifest.json out of the artifact tar. The contract
// puts it first; scanning tolerates producers that don't.
func readManifest(artifactPath string) ([]byte, error) {
	f, err := os.Open(artifactPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	var manifest []byte
	guard := newEntryGuard()
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if err := guard.count(); err != nil {
			return nil, err
		}
		if len(hdr.Name) > maxEntryNameBytes {
			return nil, fmt.Errorf("artifact has an entry name %d bytes long, past the %d-byte limit", len(hdr.Name), maxEntryNameBytes)
		}
		if SparseEntry(hdr) {
			return nil, errSparse(hdr.Name)
		}
		if strings.TrimPrefix(filepath.Clean(hdr.Name), "./") != "manifest.json" {
			continue
		}
		// A second manifest.json makes the artifact ambiguous — refuse rather
		// than trust whichever copy a given reader happens to pick.
		if manifest != nil {
			return nil, fmt.Errorf("artifact has more than one manifest.json")
		}
		if manifest, err = io.ReadAll(io.LimitReader(tr, manifestLimit)); err != nil {
			return nil, err
		}
	}
	if manifest == nil {
		return nil, fmt.Errorf("no manifest.json in artifact")
	}
	return manifest, nil
}

// ---- HTTP client for the app's /fog surface ----

// appHTTP talks to apps' /fog surface. No overall timeout — an artifact
// download runs as long as the delegated timeout lets it — but a dial or
// a response that never starts fails on its own instead of waiting out
// that budget.
//
// Redirects are never followed: the app is untrusted, and a 307 would let a
// sealed app steer fog — bearer token included — at an internal URL or an
// outside host. A 3xx comes back as the response and fails the call (no ok
// list includes one). Same policy as internal/appcall's client; kept local
// because the artifact download needs these dial and header timeouts.
var appHTTP = func() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	t.ResponseHeaderTimeout = time.Minute
	return &http.Client{
		Transport:     t,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}()

type appClient struct {
	base  string
	token string
}

// newAppClient resolves an app's /fog base URL and API token once, for
// every call a backup makes.
func (m *Manager) newAppClient(ctx context.Context, app *store.App, spec *apps.AppSpec) (*appClient, error) {
	token, err := m.Engine.EnsureFogAPIToken(app.ID)
	if err != nil {
		return nil, err
	}
	base, err := m.Engine.AppBaseURL(ctx, app.Name, spec)
	if err != nil {
		return nil, err
	}
	return &appClient{base: base, token: token}, nil
}

// do sends one request and hands back a response whose status is one of
// ok (200 when none is given). what names the call in errors: "the
// backup descriptor", "job status".
func (c *appClient) do(ctx context.Context, method, path, what string, ok ...int) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	resp, err := appHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reaching the app for %s: %w", what, err)
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		resp.Body.Close()
		return nil, fmt.Errorf("app answered HTTP %d (a redirect) for %s — redirects are refused", resp.StatusCode, what)
	}
	if len(ok) == 0 {
		ok = []int{http.StatusOK}
	}
	if !slices.Contains(ok, resp.StatusCode) {
		defer resp.Body.Close()
		return nil, fmt.Errorf("app answered HTTP %d for %s: %s", resp.StatusCode, what, errorBody(resp))
	}
	return resp, nil
}

// getJSON is do plus decoding at most limit bytes of the answer into out.
func (c *appClient) getJSON(ctx context.Context, method, path, what string, limit int64, out any, ok ...int) error {
	resp, err := c.do(ctx, method, path, what, ok...)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(io.LimitReader(resp.Body, limit)).Decode(out); err != nil {
		return fmt.Errorf("%s was not valid JSON: %w", what, err)
	}
	return nil
}

// errorBody extracts the contract's {"errors": [...]} envelope (the Rails
// apps' house error shape) without trusting the app to be terse.
func errorBody(resp *http.Response) string {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var e struct {
		Errors []string `json:"errors"`
	}
	if json.Unmarshal(raw, &e) == nil && len(e.Errors) > 0 {
		return strings.Join(e.Errors, "; ")
	}
	return strings.TrimSpace(string(raw))
}

// descriptor fetches the fog-backup/1 descriptor.
func (c *appClient) descriptor(ctx context.Context) (*Descriptor, error) {
	ctx, cancel := context.WithTimeout(ctx, descriptorTimeout)
	defer cancel()
	var d Descriptor
	if err := c.getJSON(ctx, "GET", "/fog/backup", "the backup descriptor", manifestLimit, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

func (c *appClient) startBackup(ctx context.Context) (string, error) {
	// 202 = started; 409 = already producing — adopt the running job rather
	// than stacking a second dump behind the first.
	var out struct {
		Job string `json:"job"`
	}
	err := c.getJSON(ctx, "POST", "/fog/backup", "starting backup", 4096, &out, http.StatusAccepted, http.StatusConflict)
	if err != nil {
		return "", err
	}
	if out.Job == "" {
		return "", fmt.Errorf("app's backup response had no job id")
	}
	// The job id is the app's word, and it is interpolated into the URL of
	// every poll, download and delete below. A strict charset keeps it from
	// carrying a path (`../…`) or a query into those URLs; the uses also
	// path-escape it, belt and braces.
	if !jobIDRe.MatchString(out.Job) {
		return "", fmt.Errorf("app returned a job id that is not a plain token: %q", out.Job)
	}
	return out.Job, nil
}

// jobIDRe is the charset a delegated job id may use: a plain token (UUID,
// ULID, number…), no path or URL metacharacters, bounded.
var jobIDRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

func (c *appClient) jobStatus(ctx context.Context, jobID string) (*jobStatus, error) {
	st := &jobStatus{}
	if err := c.getJSON(ctx, "GET", "/fog/backup/jobs/"+url.PathEscape(jobID), "job status", 1<<16, st); err != nil {
		return nil, err
	}
	return st, nil
}

// downloadArtifact streams the artifact to disk, hashing as it goes, and
// never writes more than limit bytes: the app serves the stream, so an
// unbounded copy is a way to fill the backup disk. A claimed length over the
// cap is refused before a byte lands; a stream that runs past it mid-download
// aborts (the snapshot directory is cleaned up by the caller).
func (c *appClient) downloadArtifact(ctx context.Context, jobID, dst string, limit int64) (sha string, n int64, err error) {
	resp, err := c.do(ctx, "GET", "/fog/backup/jobs/"+url.PathEscape(jobID)+"/artifact", "artifact download")
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if limit > 0 && resp.ContentLength > limit {
		return "", 0, fmt.Errorf("the app's artifact claims %d bytes, over the %d-byte cap — refusing it (raise spec.backup.maxBytes if this is legitimate)", resp.ContentLength, limit)
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	var body io.Reader = resp.Body
	if limit > 0 {
		// +1 so an artifact exactly at the cap still lands, while one byte
		// past it is caught below.
		body = io.LimitReader(resp.Body, limit+1)
	}
	n, err = io.Copy(io.MultiWriter(f, h), body)
	// fsync before recording the hash: it was computed from bytes still in
	// the page cache, and a power cut after CreateArtifactBackup would
	// otherwise leave a truncated artifact on disk stamped as verified.
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", 0, fmt.Errorf("downloading artifact: %w", err)
	}
	if limit > 0 && n > limit {
		return "", 0, fmt.Errorf("the app's artifact ran past the %d-byte cap — download aborted (raise spec.backup.maxBytes if this is legitimate)", limit)
	}
	if resp.ContentLength >= 0 && n != resp.ContentLength {
		return "", 0, fmt.Errorf("artifact download truncated: got %d of %d bytes", n, resp.ContentLength)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// maxArtifactBytes resolves the delegated-download cap for an app: its own
// spec.backup.maxBytes, then the fleet setting, then the default.
func (m *Manager) maxArtifactBytes(spec *apps.AppSpec) int64 {
	if spec != nil && spec.Backup != nil && spec.Backup.MaxBytes > 0 {
		return spec.Backup.MaxBytes
	}
	if v, _ := m.Store.GetSetting(SettingBackupMaxBytes); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return DefaultMaxArtifactBytes
}

func (c *appClient) deleteJob(ctx context.Context, jobID string) {
	if resp, err := c.do(ctx, "DELETE", "/fog/backup/jobs/"+url.PathEscape(jobID), "deleting the job", http.StatusOK, http.StatusNoContent, http.StatusNotFound); err == nil {
		resp.Body.Close()
	}
}
