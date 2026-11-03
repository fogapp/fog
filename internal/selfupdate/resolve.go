package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Where fog comes from is one URL, the source, and three things read it:
// install.sh, the Proxmox scripts (to find install.sh), and this updater.
// They must agree on what a source means, so the rules live in one table —
// docs/hosting-the-source.md — implemented here and in install.sh's
// candidates(), and shellsource_test.go runs both over every shape and
// requires the same answer.
//
// A source is resolved by probing: a URL alone can't tell a repo from a
// directory, so each shape it could be is tried in a fixed order and the
// first manifest that fetches (and verifies) decides.
//
//   - ends in latest.json      → itself
//   - github.com/OWNER/REPO    → …/releases/latest/download/latest.json
//   - a host naming gitlab     → …/-/raw/BRANCH/release/latest.json
//   - any other OWNER/REPO     → …/raw/branch/BRANCH/release/latest.json (Gitea, Forgejo)
//   - then, always             → …/latest.json (a plain directory)
//
// BRANCH is FOG_UPDATE_BRANCH (install.sh's FOG_BRANCH, carried onto the
// unit), default main.

// EnvBranch names the branch a repo source's committed manifest is read from.
const EnvBranch = "FOG_UPDATE_BRANCH"

// Candidate is one guess at a source's layout: where the manifest would be,
// and where repo-relative files would be under the same guess (install.sh
// reads the unit from there; the updater only needs the manifest).
type Candidate struct {
	Manifest string
	Raw      string
}

// source is a validated, normalized source URL.
type source struct {
	url    string // scheme://host[/seg/...], no trailing slash, "//" collapsed
	scheme string
	host   string // lower-cased, port kept
	segs   []string
}

var errNoSource = errors.New("no update URL configured")

// allowedURLRune is the character set a source may use: enough for any
// real URL path (percent-escapes included), and nothing a systemd unit, a
// shell, or a query string would read as syntax — no space, quote, $, ;,
// backslash, ?, #, or @.
func allowedURLRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	}
	return strings.ContainsRune("._~:/%+=,-[]", r)
}

func parseSource(raw string) (source, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return source{}, errNoSource
	}
	scheme, rest, ok := strings.Cut(s, "://")
	if !ok || (scheme != "http" && scheme != "https") {
		return source{}, fmt.Errorf("update URL is not a valid URL: it must start with http:// or https://")
	}
	for _, r := range rest {
		if !allowedURLRune(r) {
			return source{}, fmt.Errorf("update URL is not a valid URL: %q isn't allowed in it", r)
		}
	}
	host, p, _ := strings.Cut(rest, "/")
	if host == "" {
		return source{}, fmt.Errorf("update URL is not a valid URL: it names no host")
	}
	host = strings.ToLower(host)
	// "host:3000//owner/repo" is a common paste; collapse the empty segment.
	segs := strings.FieldsFunc(p, func(r rune) bool { return r == '/' })
	u := scheme + "://" + host
	if len(segs) > 0 {
		u += "/" + strings.Join(segs, "/")
	}
	return source{url: u, scheme: scheme, host: host, segs: segs}, nil
}

// ValidateSource reports why raw can't be an update source, with the same
// rules resolveManifest (and install.sh) apply — the settings handler calls
// it so a bad source is refused when it's saved, not at the next check.
func ValidateSource(raw string) error {
	_, err := parseSource(raw)
	return err
}

// validBranch keeps a branch name to characters a URL path takes literally.
func validBranch(b string) bool {
	if b == "" || strings.HasPrefix(b, "/") || strings.HasPrefix(b, "-") || strings.Contains(b, "..") {
		return false
	}
	for _, r := range b {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._/-", r)) {
			return false
		}
	}
	return true
}

func (s source) isGitHub() bool { return s.host == "github.com" || s.host == "www.github.com" }

// Candidates is the probe order for a source, the same list install.sh's
// candidates() prints under FOG_PRINT_CANDIDATES=1.
func Candidates(rawSource, branch string) ([]Candidate, error) {
	s, err := parseSource(rawSource)
	if err != nil {
		return nil, err
	}
	if branch == "" {
		branch = "main"
	}
	if !validBranch(branch) {
		return nil, fmt.Errorf("branch %q isn't a branch name fog will use", branch)
	}
	return s.candidates(branch), nil
}

func (s source) candidates(branch string) []Candidate {
	if strings.HasSuffix(s.url, "latest.json") {
		return []Candidate{{Manifest: s.url, Raw: s.url[:strings.LastIndex(s.url, "/")]}}
	}
	var c []Candidate
	if len(s.segs) >= 2 {
		owner, repo := s.segs[0], s.segs[1]
		switch {
		case s.isGitHub():
			// Only the release: its manifest is the relative one release.sh
			// signs for the release directory. The committed
			// release/latest.json names the host the release was cut on,
			// which a GitHub install may not reach — or may reach as
			// something else.
			raw := "https://raw.githubusercontent.com/" + owner + "/" + repo + "/" + branch
			c = append(c, Candidate{s.url + "/releases/latest/download/latest.json", raw})
		case strings.Contains(s.host, "gitlab"):
			raw := s.url + "/-/raw/" + branch
			c = append(c, Candidate{raw + "/release/latest.json", raw})
		default:
			// Gitea/Forgejo, and the assumption for any other repo-shaped URL.
			raw := s.url + "/raw/branch/" + branch
			c = append(c, Candidate{raw + "/release/latest.json", raw})
		}
	}
	return append(c, Candidate{s.url + "/latest.json", s.url})
}

func (u *Updater) branch() string {
	if b := strings.TrimSpace(os.Getenv(EnvBranch)); b != "" {
		return b
	}
	return "main"
}

// resolveManifest turns whatever the operator pasted into a verified
// manifest.
func (u *Updater) resolveManifest(ctx context.Context, base string) (*Manifest, error) {
	s, err := parseSource(base)
	if err != nil {
		return nil, err
	}
	branch := u.branch()
	if !validBranch(branch) {
		return nil, fmt.Errorf("%s=%q isn't a branch name fog will use", EnvBranch, branch)
	}
	var errs []string
	var stale *StaleError
	for _, c := range s.candidates(branch) {
		m, err := u.fetchManifestURL(ctx, c.Manifest)
		if err != nil {
			errs = append(errs, err.Error())
			var se *StaleError
			if errors.As(err, &se) && (stale == nil || se.Issued.After(stale.Issued)) {
				stale = se
			}
			continue
		}
		m.SourceOrigin = s.scheme + "://" + s.host
		if s.isGitHub() && len(s.segs) >= 2 {
			m.GitHubRepo = "https://github.com/" + s.segs[0] + "/" + s.segs[1]
		}
		m.Notes = u.releaseNotes(ctx, s, m.Version)
		return m, nil
	}
	if stale != nil {
		return nil, fmt.Errorf("no current release manifest found (%s): %w", strings.Join(errs, "; "), stale)
	}
	return nil, fmt.Errorf("no release manifest found (%s)", strings.Join(errs, "; "))
}

// releaseNotes asks a Gitea/Forgejo or GitHub repo for the release body of
// version, shown in the header's update dropdown. Best effort: a source
// without an API, or a release with no notes, just shows none.
func (u *Updater) releaseNotes(ctx context.Context, s source, version string) string {
	if len(s.segs) < 2 || strings.Contains(s.host, "gitlab") || strings.HasSuffix(s.url, "latest.json") {
		return ""
	}
	owner, repo := s.segs[0], s.segs[1]
	api := fmt.Sprintf("%s://%s/api/v1/repos/%s/%s/releases/tags/%s", s.scheme, s.host, owner, repo, url.PathEscape(version))
	if s.isGitHub() {
		api = fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/tags/%s", owner, repo, url.PathEscape(version))
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	body, err := u.getBytes(ctx, api, maxManifestBytes)
	if err != nil {
		return ""
	}
	var release struct {
		TagName string `json:"tag_name"`
		Body    string `json:"body"`
	}
	if json.Unmarshal(body, &release) != nil || release.TagName != version {
		return ""
	}
	return release.Body
}

func (u *Updater) client(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: u.transport, CheckRedirect: u.redirect}
}

// redirect decides the token afresh on every hop. Go's own rule keeps the
// Authorization header across a redirect to the same host name — including
// to plain http, or to another port on it — so the header is dropped on
// each hop and authorize re-adds it only where it may go: https, a token
// host, the origin it was saved for.
func (u *Updater) redirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	req.Header.Del("Authorization")
	u.authorize(req)
	return nil
}

// getBytes fetches rawURL whole, refusing anything over max bytes.
func (u *Updater) getBytes(ctx context.Context, rawURL string, max int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return nil, err
	}
	u.authorize(req)
	resp, err := u.client(30 * time.Second).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%s: HTTP %d", rawURL, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", rawURL, err)
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%s: larger than %d bytes", rawURL, max)
	}
	return b, nil
}

// fetchManifestURL reads one manifest and its signature (rawURL + ".sig"),
// and accepts it only when the signature verifies against the pinned
// release keys — the bytes checked are the bytes then parsed.
func (u *Updater) fetchManifestURL(ctx context.Context, rawURL string) (*Manifest, error) {
	body, err := u.getBytes(ctx, rawURL, maxManifestBytes)
	if err != nil {
		return nil, err
	}
	m := &Manifest{}
	if err := json.Unmarshal(body, m); err != nil || m.Version == "" {
		return nil, fmt.Errorf("%s: not a release manifest", rawURL)
	}
	if !allowUnsigned() {
		keys, err := u.releaseKeys()
		if err != nil {
			return nil, err
		}
		sig, err := u.getBytes(ctx, rawURL+".sig", 4096)
		if err != nil {
			return nil, fmt.Errorf("%s: unsigned (no %s.sig: %v) — refusing it", rawURL, path0(rawURL), err)
		}
		if err := verifySignature(body, sig, keys); err != nil {
			return nil, fmt.Errorf("%s: %w — refusing it", rawURL, err)
		}
	}
	if _, ok := parseVersion(m.Version); !ok {
		return nil, fmt.Errorf("%s: version %q isn't a release version", rawURL, m.Version)
	}
	// The issue date is inside the signed bytes: an old manifest can't be
	// made to look new, only withheld.
	if err := checkIssued(body, rawURL, u.now()); err != nil {
		return nil, err
	}
	m.FetchedFrom = rawURL
	// A manifest may name its artifacts relative to itself. That makes a
	// release directory portable: copy it to another host, or serve it from
	// a mirror, and nothing inside it needs rewriting.
	if base, err := url.Parse(rawURL); err == nil {
		for k, a := range m.Artifacts {
			if a.URL == "" || strings.Contains(a.URL, "://") {
				continue
			}
			if ref, err := url.Parse(a.URL); err == nil {
				a.URL = base.ResolveReference(ref).String()
				m.Artifacts[k] = a
			}
		}
	}
	return m, nil
}

// path0 is the last element of a URL's path, for messages.
func path0(rawURL string) string {
	if i := strings.LastIndex(rawURL, "/"); i >= 0 {
		return rawURL[i+1:]
	}
	return rawURL
}
