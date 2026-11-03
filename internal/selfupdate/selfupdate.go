// Package selfupdate keeps fogd itself boring to upgrade: check a signed
// release manifest, download the platform binary, verify its checksum,
// atomically swap the executable (previous binary kept as .prev), and exit
// so systemd restarts the new version — which has a few minutes to prove
// itself healthy before the previous binary is put back (rollback.go). Apps
// and the proxy are containers — they don't blink during a fogd restart.
package selfupdate

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"gitea.lan/matthew/fog/internal/config"
	"gitea.lan/matthew/fog/internal/fsx"
	"gitea.lan/matthew/fog/internal/loop"
	"gitea.lan/matthew/fog/internal/notify"
	"gitea.lan/matthew/fog/internal/store"
	"gitea.lan/matthew/fog/internal/supervise"
	"gitea.lan/matthew/fog/internal/vault"
)

const (
	SettingUpdateBaseURL = "update_base_url" // hosts latest.json + binaries
	// SecretUpdateBasicAuth is a system secret holding an access token, sent
	// with the "token" scheme on manifest/binary fetches — lets releases live
	// in a private Gitea repo. The name is historical: it was once a
	// "user:token" pair sent as basic auth, and a value stored in that shape
	// still works (see authorize).
	SecretUpdateBasicAuth = "update_basic_auth"
	// SettingUpdateTokenOrigin is the scheme://host the access token was
	// saved for. The token is only ever sent there, so pointing the source
	// somewhere else can't carry it along (and the settings handler deletes
	// it when the source's host changes; see TokenFollowsSource).
	SettingUpdateTokenOrigin = "update_token_origin"
)

type Manifest struct {
	Version   string                      `json:"version"`
	Artifacts map[string]ManifestArtifact `json:"artifacts"` // key: "linux-amd64"
	// IssuedAt is when release.sh signed the manifest (Unix seconds). It is
	// inside the signed bytes, so a mirror can't refresh it; a manifest
	// older than MaxManifestAge is refused (sign.go).
	IssuedAt int64 `json:"issued_at"`
	// Notes is release-body text discovered via the git host's API (not part
	// of latest.json itself).
	Notes string `json:"-"`
	// FetchedFrom is the URL the manifest was actually read from. When a
	// mirror serves a manifest whose absolute artifact URLs point at a host
	// this network can't reach (the original gitea.lan, say), the download
	// falls back to the artifact's filename resolved against this URL.
	FetchedFrom string `json:"-"`
	// SourceOrigin is scheme://host of the update URL the operator configured.
	// A git host names its links by its own configured root (Gitea's ROOT_URL),
	// not by the address it was reached at — a server on another subnet that
	// reaches Gitea by IP still gets http://gitea.lan/... links back. The
	// download retries such a URL's path on this origin.
	SourceOrigin string `json:"-"`
	// GitHubRepo is https://github.com/OWNER/REPO when the source is a
	// GitHub repo: an artifact is also looked for in that repo's release of
	// the manifest's version, so a committed manifest naming another
	// host's downloads still installs from GitHub.
	GitHubRepo string `json:"-"`
}

type ManifestArtifact struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

type Updater struct {
	Store *store.Store
	Vault *vault.Vault

	mu          sync.Mutex
	cached      CheckResult
	lastChecked time.Time
	checking    bool

	// applyMu serializes Apply. It is separate from mu, which guards the
	// check cache with short critical sections — an Apply holds its lock for
	// the whole download.
	applyMu sync.Mutex

	// Busy names the apps with a deploy or update in flight. Apply refuses
	// while any are listed: fogd's restart would kill the run halfway, leaving
	// the app half-deployed. Nil means nothing is ever busy.
	Busy func() []string

	// exePath overrides os.Executable in tests, so Apply swaps a scratch
	// file instead of the test binary. Empty in production.
	exePath string
	// verify overrides checkBinary in tests, whose stand-in "binaries" are
	// a few bytes of text. Nil in production.
	verify func(path string) error

	// Test seams, all zero in production: the release keys manifests must
	// be signed with, the HTTP transport, the running version, the
	// rollback guard's arming (instead of systemd-run) and deadline, and
	// the health check confirmUpdate polls.
	keys         []ed25519.PublicKey
	transport    http.RoundTripper
	version      string
	arm          func(guard string, after time.Duration) error
	delay        time.Duration
	healthURL    string
	healthTick   time.Duration
	healthWindow time.Duration
	clock        func() time.Time
	units        *unitSeams
}

func (u *Updater) now() time.Time {
	if u.clock != nil {
		return u.clock()
	}
	return time.Now()
}

// current is the running fogd's version.
func (u *Updater) current() string {
	if u.version != "" {
		return u.version
	}
	return config.Version
}

// maxBinaryBytes caps the download: fogd is tens of megabytes, so anything
// past this is not a fogd, and must not be allowed to fill the disk the
// executable lives on.
const maxBinaryBytes = 256 << 20

// checkBinary is the shape check a downloaded fogd must pass before it
// replaces the running one: on Linux, an ELF executable for this machine's
// architecture. The checksum says the bytes are the ones published; this
// says the published bytes can run here — a release whose manifest pointed
// this platform at the wrong build would otherwise install a fogd that
// systemd can never start. Other platforms are dev machines, and pass.
func checkBinary(path, goos, goarch string) error {
	if goos != "linux" {
		return nil
	}
	if err := supervise.VerifyELF(path, goarch); err != nil {
		return fmt.Errorf("the downloaded fogd failed verification: %w", err)
	}
	return nil
}

// MaybeRefresh kicks a background check when the cache is stale — called by
// the banner endpoint so an open UI converges on new releases quickly
// without every poll hitting the network.
func (u *Updater) MaybeRefresh(maxAge time.Duration) {
	u.mu.Lock()
	stale := time.Since(u.lastChecked) > maxAge && !u.checking
	if stale {
		u.checking = true
	}
	u.mu.Unlock()
	if !stale {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		// Deferred so a panicking Check can't leave checking=true forever,
		// which would silence every future refresh.
		defer func() {
			u.mu.Lock()
			u.checking = false
			u.mu.Unlock()
		}()
		u.Check(ctx)
	}()
}

// authorize attaches the stored access token, when configured — but only to
// the update source's own host (and, for a GitHub source, GitHub's raw and
// API hosts). A manifest may point its downloads anywhere, and the token
// must not follow it to a mirror or CDN. A bare token is expected; a legacy
// "user:token" value still works (the token half is used). The "token"
// scheme is accepted by both Gitea and GitHub on API and download routes.
func (u *Updater) authorize(req *http.Request) {
	// Never in the clear, and only to the origin it was saved for.
	if u.Vault == nil || req.URL.Scheme != "https" || !u.tokenHost(req.URL.Host) {
		return
	}
	if bound, _ := u.Store.GetSetting(SettingUpdateTokenOrigin); bound != "" && bound != SourceOrigin(u.baseURL()) {
		return
	}
	v, err := vault.Secrets{Store: u.Store, Vault: u.Vault}.Get(0, SecretUpdateBasicAuth)
	if err != nil {
		return
	}
	v = strings.TrimSpace(v)
	if _, tok, ok := strings.Cut(v, ":"); ok {
		v = tok
	}
	if v != "" {
		req.Header.Set("Authorization", "token "+v)
	}
}

type CheckResult struct {
	CurrentVersion  string `json:"currentVersion"`
	LatestVersion   string `json:"latestVersion,omitempty"`
	UpdateAvailable bool   `json:"updateAvailable"`
	Notes           string `json:"notes,omitempty"`
	CheckedAt       string `json:"checkedAt,omitempty"`
	Error           string `json:"error,omitempty"`
}

// tokenHost reports whether host is one the access token may be sent to.
func (u *Updater) tokenHost(host string) bool { return u.tokenHostFor(u.baseURL(), host) }

func (u *Updater) tokenHostFor(source, host string) bool {
	s, err := parseSource(source)
	if err != nil {
		return false
	}
	host = strings.ToLower(host)
	if host == s.host {
		return true
	}
	return s.isGitHub() && (host == "raw.githubusercontent.com" || host == "api.github.com")
}

func (u *Updater) platformKey() string { return runtime.GOOS + "-" + runtime.GOARCH }

// baseURL is where updates come from: the stored setting first, else the
// source install.sh recorded, so updates follow the install source without
// the operator pasting the URL a second time.
func (u *Updater) baseURL() string { return EffectiveSource(u.Store) }

// EffectiveSource is the update source in force: the setting, else the
// FOG_UPDATE_BASE_URL install.sh put on the unit.
func EffectiveSource(st *store.Store) string {
	base, _ := st.GetSetting(SettingUpdateBaseURL)
	if strings.TrimSpace(base) == "" {
		base = os.Getenv("FOG_UPDATE_BASE_URL")
	}
	return strings.TrimSpace(base)
}

// SourceOrigin is a source's scheme://host, "" when it isn't a valid source.
func SourceOrigin(raw string) string {
	s, err := parseSource(raw)
	if err != nil {
		return ""
	}
	return s.scheme + "://" + s.host
}

// CheckTokenSource refuses an access token for a source it would travel to
// in the clear.
func CheckTokenSource(source string) error {
	s, err := parseSource(source)
	if err != nil {
		return fmt.Errorf("set an update source before its access token: %w", err)
	}
	if s.scheme != "https" {
		return fmt.Errorf("the update source %s is plain http, so an access token would cross the network in the clear — fog only sends tokens over https", s.url)
	}
	return nil
}

// TokenFollowsSource reports whether a saved token may stay when the
// source changes from oldSource to newSource: only when both name the same
// origin. Otherwise the caller deletes it — a token is for one host.
func TokenFollowsSource(oldSource, newSource string) bool {
	o := SourceOrigin(oldSource)
	return o != "" && o == SourceOrigin(newSource)
}

func (u *Updater) fetchManifest(ctx context.Context) (*Manifest, error) {
	return u.resolveManifest(ctx, u.baseURL())
}

func (u *Updater) Check(ctx context.Context) CheckResult {
	res, _ := u.check(ctx)
	return res
}

// check is Check, also answering the resolver's error for Loop to classify.
func (u *Updater) check(ctx context.Context) (CheckResult, error) {
	res := CheckResult{CurrentVersion: u.current(), CheckedAt: time.Now().UTC().Format(time.RFC3339)}
	m, err := u.fetchManifest(ctx)
	if err != nil {
		res.Error = err.Error()
	} else {
		res.LatestVersion = m.Version
		res.Notes = m.Notes
		// Forward only: an older manifest (a stale mirror, or a replayed
		// one) is reported, never offered.
		res.UpdateAvailable = isNewer(m.Version, u.current())
	}
	u.mu.Lock()
	u.cached = res
	u.lastChecked = time.Now()
	u.mu.Unlock()
	return res, err
}

// Cached returns the most recent check result without touching the network —
// what the header banner polls.
func (u *Updater) Cached() CheckResult {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.cached.CurrentVersion == "" {
		return CheckResult{CurrentVersion: u.current()}
	}
	return u.cached
}

// Loop checks periodically (and shortly after boot), notifies once per
// newly discovered version, and — just as important — says so when the
// update source has become unreachable. A server moved to a network where
// its source doesn't resolve used to fail these checks silently forever;
// three consecutive misses now raise a persistent notice that clears
// itself on the next success.
func (u *Updater) Loop(ctx context.Context, interval time.Duration, send func(title, msg string, ok bool)) {
	const failThreshold = 3
	// One instance-wide streak (key ""); FailStreak owns the raise/re-raise/
	// dismiss cadence, the wording here stays selfupdate's own.
	streak := &notify.FailStreak{Store: u.Store, Kind: "selfupdate-unreachable", Threshold: failThreshold}
	// First the last update's outcome: a rollback the guard performed is
	// posted, and an update still on trial confirms itself once healthy.
	u.settle(ctx, send)
	loop.Every(ctx, time.Minute, interval, func(ctx context.Context) { u.checkOnce(ctx, streak, send) })
}

const lastNotifiedKey = "update_last_notified"

// staleNotifiedKey remembers which stale manifest was last pushed to the
// phone, so a freeze is announced once, not on every check.
const staleNotifiedKey = "update_stale_notified"

// checkOnce is one pass of Loop.
func (u *Updater) checkOnce(ctx context.Context, streak *notify.FailStreak, send func(title, msg string, ok bool)) {
	res, err := u.check(ctx)
	// A source that answers with nothing newer than MaxManifestAge is
	// reachable but withholding: a freeze, whether by neglect or on
	// purpose. Said at once, and apart from "unreachable".
	var stale *StaleError
	if errors.As(err, &stale) {
		streak.Success("")
		const title = "Fog's update source offers only an old release"
		if u.Store.CreateNotice(staleNoticeKind, "", title, stale.notice(u.baseURL())) == nil {
			key := stale.URL + "@" + strconv.FormatInt(stale.Issued.Unix(), 10)
			if last, _ := u.Store.GetSetting(staleNotifiedKey); last != key {
				send(title, stale.Error(), false)
				u.Store.SetSetting(staleNotifiedKey, key)
			}
		}
		return
	}
	u.Store.DismissNoticeByKind(staleNoticeKind, "")
	if res.Error != "" {
		if n, raise := streak.Fail(""); raise {
			u.Store.CreateNotice("selfupdate-unreachable", "",
				"Fog can't reach its update source",
				fmt.Sprintf("Update checks have failed %d times in a row against %s: %s\n\n"+
					"If this server moved to a new network, its update source may not be reachable from here — "+
					"point Settings → Server → Updates at a source this network can reach (docs/hosting-the-source.md covers the options). "+
					"Everything else keeps running; fog just can't update itself until this is fixed.",
					n, u.baseURL(), res.Error))
			send("Fog can't check for updates", res.Error, false)
		}
		return
	}
	streak.Success("")
	if !res.UpdateAvailable {
		return
	}
	last, _ := u.Store.GetSetting(lastNotifiedKey)
	if res.LatestVersion != last {
		send("Fog "+res.LatestVersion+" is available",
			"Running "+res.CurrentVersion+". Update from the header or Settings → Server → Updates.", true)
		u.Store.SetSetting(lastNotifiedKey, res.LatestVersion)
	}
}

// artifactURLs is where an artifact may be fetched from, in order — the
// same list install.sh's artifact_urls gives: the manifest's URL; the same
// path on the configured origin (a git host names its links by its ROOT_URL,
// which this network may reach only by IP); for a GitHub source, the file
// in that repo's release of this version; the file beside the manifest (a
// mirror serving a manifest whose URLs still name the original host).
func (u *Updater) artifactURLs(m *Manifest, art ManifestArtifact) []string {
	urls := []string{art.URL}
	if rebased := rebaseOrigin(art.URL, m.SourceOrigin); rebased != "" {
		urls = append(urls, rebased)
	}
	if m.GitHubRepo != "" {
		urls = append(urls, m.GitHubRepo+"/releases/download/"+url.PathEscape(m.Version)+"/"+path.Base(art.URL))
	}
	if m.FetchedFrom != "" {
		if base, err := url.Parse(m.FetchedFrom); err == nil {
			if rel, err := url.Parse(path.Base(art.URL)); err == nil {
				urls = append(urls, base.ResolveReference(rel).String())
			}
		}
	}
	seen := map[string]bool{}
	out := urls[:0]
	for _, u := range urls {
		if !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	return out
}

// downloadArtifact stages the artifact at dst — bounded, fsynced — from the
// first location whose bytes match the manifest's sha256, and answers that
// digest. A location that answers with other bytes (another server where the
// manifest's host used to be, a captive portal) is passed over like one that
// doesn't answer, never accepted; dst holds nothing unverified on return.
func (u *Updater) downloadArtifact(ctx context.Context, m *Manifest, art ManifestArtifact, dst string) (string, error) {
	var fails []string
	for _, rawURL := range u.artifactURLs(m, art) {
		got, err := u.stageFrom(ctx, rawURL, dst)
		if err != nil {
			fails = append(fails, err.Error())
			continue
		}
		if strings.EqualFold(got, art.SHA256) {
			return got, nil
		}
		os.Remove(dst)
		fails = append(fails, fmt.Sprintf("%s: checksum mismatch: got %s", rawURL, got))
	}
	return "", fmt.Errorf("binary download: no location gave the release's bytes (%s)", strings.Join(fails, "; "))
}

// stageFrom writes one download to dst and answers its sha256.
func (u *Updater) stageFrom(ctx context.Context, rawURL, dst string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return "", err
	}
	u.authorize(req)
	resp, err := u.client(10 * time.Minute).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("%s: HTTP %d", rawURL, resp.StatusCode)
	}
	// Stage bounds the copy and fsyncs before the checksum is taken: the
	// checksum must describe bytes the disk has, or a power cut after the
	// rename leaves a truncated fogd.
	if err := supervise.Stage(resp.Body, dst, maxBinaryBytes); err != nil {
		os.Remove(dst)
		return "", fmt.Errorf("%s: %w", rawURL, err)
	}
	return fsx.FileSHA256(dst)
}

// rebaseOrigin moves rawURL onto origin (scheme://host), keeping its path and
// query. Empty when either doesn't parse or rawURL is already on origin.
func rebaseOrigin(rawURL, origin string) string {
	if origin == "" {
		return ""
	}
	o, err := url.Parse(origin)
	if err != nil || o.Host == "" {
		return ""
	}
	r, err := url.Parse(rawURL)
	if err != nil || r.Host == "" {
		return ""
	}
	if strings.EqualFold(r.Host, o.Host) && r.Scheme == o.Scheme {
		return ""
	}
	r.Scheme, r.Host = o.Scheme, o.Host
	return r.String()
}

// executable is os.Executable unless a test has pointed exePath elsewhere.
func (u *Updater) executable() (string, error) {
	if u.exePath != "" {
		return u.exePath, nil
	}
	return os.Executable()
}

// keepPrevious leaves a copy of the running binary at exe.prev without
// ever moving exe itself: a hard link where the filesystem allows one,
// a copy otherwise. Renaming exe away first left a moment with no binary
// at the path systemd starts.
func keepPrevious(exe string) error {
	prev := exe + ".prev"
	if err := os.Remove(prev); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Link(exe, prev); err == nil {
		return nil
	}
	src, err := os.Open(exe)
	if err != nil {
		return err
	}
	defer src.Close()
	return fsx.WriteAtomic(prev, 0o755, func(w io.Writer) error {
		_, err := io.Copy(w, src)
		return err
	})
}

func (u *Updater) checkIdle() error {
	if u.Busy == nil {
		return nil
	}
	names := u.Busy()
	if len(names) == 0 {
		return nil
	}
	what, it, finishes := "an app is", "it", "it finishes"
	if len(names) > 1 {
		what, it, finishes = "apps are", "them", "they finish"
	}
	return fmt.Errorf("Fog can't update while %s deploying or updating (%s) — restarting fog now would interrupt %s partway through. Try again once %s",
		what, strings.Join(names, ", "), it, finishes)
}

// Apply downloads, verifies, and swaps the binary, then exits(0) so systemd
// restarts into the new version. Never call without operator intent. One
// Apply at a time: a second call while one is in flight is refused, not
// queued — the handler exposes this to the UI directly, and two concurrent
// swaps of the same executable can't both be what the operator meant.
func (u *Updater) Apply(ctx context.Context, exitFn func(int)) error {
	if !u.applyMu.TryLock() {
		return fmt.Errorf("an update is already in progress")
	}
	defer u.applyMu.Unlock()

	if err := u.checkIdle(); err != nil {
		return err
	}
	m, err := u.fetchManifest(ctx)
	if err != nil {
		return err
	}
	if !ValidVersion(u.current()) && os.Getenv(EnvFromUnversioned) != "1" {
		return fmt.Errorf("this fogd's own version, %q, isn't a release version, so fog can't tell %s from a downgrade and won't install it. "+
			"To take it anyway, set %s=1 on fogd's unit (a drop-in) and restart fog", u.current(), m.Version, EnvFromUnversioned)
	}
	if !isNewer(m.Version, u.current()) {
		return fmt.Errorf("the release at the update source is %s, which isn't newer than the running %s — fog never installs an older or equal version", m.Version, u.current())
	}
	art, ok := m.Artifacts[u.platformKey()]
	if !ok {
		return fmt.Errorf("no artifact for %s", u.platformKey())
	}
	exe, err := u.executable()
	if err != nil {
		return err
	}
	// One update on trial at a time: the guard compares the binary against
	// the marker, and a second swap would leave it nothing true to compare.
	if p, err := readPending(exe + pendingSuffix); err == nil {
		return fmt.Errorf("the update to %s is still proving itself (until %s) — try again once it's confirmed", p.ToVersion, p.Deadline.Format(time.Kitchen))
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("an earlier update's marker is unreadable (%v); restart fog to clear it", err)
	}

	// A unique temp name in the executable's own directory: unique so no
	// other writer can ever share the file, same directory so the final
	// rename stays atomic.
	f, err := os.CreateTemp(filepath.Dir(exe), ".fogd-new-*")
	if err != nil {
		if strings.Contains(err.Error(), "read-only file system") {
			return fmt.Errorf("%w — the systemd sandbox makes /usr read-only; add 'ReadWritePaths=%s' to fogd.service (see packaging/fogd.service) and restart", err, filepath.Dir(exe))
		}
		return err
	}
	tmp := f.Name()
	f.Close()
	fail := func(err error) error {
		os.Remove(tmp)
		return err
	}
	// The file as written is what is checked, not the download stream —
	// the checksum guards exactly the bytes the rename below installs.
	got, err := u.downloadArtifact(ctx, m, art, tmp)
	if err != nil {
		return fail(err)
	}
	verify := u.verify
	if verify == nil {
		verify = func(path string) error { return checkBinary(path, runtime.GOOS, runtime.GOARCH) }
	}
	if err := verify(tmp); err != nil {
		return fail(err)
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		return fail(err)
	}

	// Again after the download: a deploy may have started while it ran.
	if err := u.checkIdle(); err != nil {
		return fail(err)
	}

	// The way back, before anything changes: the previous binary kept as
	// .prev, the trial marker, and the guard that acts on it (rollback.go).
	// Then one atomic rename over exe: it names a complete binary at every
	// instant.
	fromSHA, err := fsx.FileSHA256(exe)
	if err != nil {
		return fail(err)
	}
	if err := keepPrevious(exe); err != nil {
		return fail(err)
	}
	delay := u.guardDelay()
	marker := exe + pendingSuffix
	p := pending{
		ToVersion: m.Version, FromVersion: u.current(),
		ToSHA: strings.ToLower(got), FromSHA: fromSHA,
		Deadline: time.Now().Add(delay), Unit: unitName,
	}
	if err := fsx.WriteAtomic(marker, 0o644, func(w io.Writer) error {
		_, err := w.Write(p.encode())
		return err
	}); err != nil {
		return fail(err)
	}
	if err := u.armGuard(ctx, exe, delay); err != nil {
		os.Remove(marker)
		return fail(err)
	}
	if err := os.Rename(tmp, exe); err != nil {
		os.Remove(marker)
		return fail(err)
	}
	// fsync the directory so the renames themselves survive a power cut.
	if d, err := os.Open(filepath.Dir(exe)); err == nil {
		d.Sync()
		d.Close()
	}
	u.Store.Audit("system", "fogd.self-update", m.Version, "")
	go func() {
		time.Sleep(500 * time.Millisecond) // let the HTTP response flush
		exitFn(0)
	}()
	return nil
}

// DownloadArtifact saves another artifact of the current release — a
// fog-node build, say — to dst, checked against the manifest's digest. It
// answers the release's version and the digest.
func (u *Updater) DownloadArtifact(ctx context.Context, key, dst string) (version, sha string, err error) {
	m, err := u.fetchManifest(ctx)
	if err != nil {
		return "", "", err
	}
	art, ok := m.Artifacts[key]
	if !ok {
		return "", "", fmt.Errorf("release %s has no %s", m.Version, key)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", "", err
	}
	tmp := dst + ".part"
	got, err := u.downloadArtifact(ctx, m, art, tmp)
	if err != nil {
		return "", "", err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return "", "", err
	}
	return m.Version, got, nil
}
