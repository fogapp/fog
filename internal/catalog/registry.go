package catalog

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"gitea.lan/matthew/fog/internal/build"
	"gitea.lan/matthew/fog/internal/execx"
)

// A fog's store is built from a list of catalogs, each a git repository it
// keeps a clone of. The list is a setting; out of the box it is fog's own
// two — the household apps built for fog and the general catalog — and an
// operator can add anyone else's. Entries from a catalog that isn't one of
// fog's own install held for review (Entry.Trusted).

// SettingRemotes is the setting holding the list: a JSON array of Remote,
// first to last in priority order. Unset means DefaultRemotes.
const SettingRemotes = "catalog_sources"

// Remote is one catalog a fog reads.
type Remote struct {
	// Name is what this fog calls the catalog: a slug, unique in the list,
	// which its artwork is served under.
	Name string `json:"name"`
	// URL is the repository to clone: https:// for a public one, ssh:// or
	// user@host:path for one that needs this fog's catalog key, or an
	// absolute path on this machine.
	URL    string `json:"url"`
	Branch string `json:"branch,omitempty"`
}

// DefaultRemotes are fog's own catalogs — the only ones whose apps install
// without being held for review.
var DefaultRemotes = []Remote{
	{Name: "fog-apps", URL: "https://github.com/fogapp/catalog.git", Branch: "main"},
	{Name: "fog-catalog", URL: "https://github.com/fogapp/catalog-homelab.git", Branch: "main"},
}

// movedRemotes are addresses fog's own catalogs had before, and where they
// are now. A list saved with an old one reads as the new, so it is still
// trusted, and fetched from somewhere that exists: v0.14.29 named
// repositories that were never published.
var movedRemotes = map[string]string{
	"https://github.com/fogapp/fog-apps.git":    "https://github.com/fogapp/catalog.git",
	"https://github.com/fogapp/fog-catalog.git": "https://github.com/fogapp/catalog-homelab.git",
}

// Trusted reports whether r is one of fog's own catalogs, at its own
// address.
func Trusted(r Remote) bool {
	for _, d := range DefaultRemotes {
		if d.Name == r.Name && d.URL == strings.TrimSpace(r.URL) {
			return true
		}
	}
	return false
}

var remoteName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// ParseRemotes reads the setting; empty is DefaultRemotes.
func ParseRemotes(raw string) ([]Remote, error) {
	if strings.TrimSpace(raw) == "" {
		return slices.Clone(DefaultRemotes), nil
	}
	var rs []Remote
	if err := json.Unmarshal([]byte(raw), &rs); err != nil {
		return nil, fmt.Errorf("catalogs must be a JSON list of {name, url, branch}")
	}
	seen := map[string]bool{}
	for i, r := range rs {
		r.Name, r.URL, r.Branch = strings.TrimSpace(r.Name), strings.TrimSpace(r.URL), strings.TrimSpace(r.Branch)
		if to, ok := movedRemotes[r.URL]; ok {
			r.URL = to
		}
		if !remoteName.MatchString(r.Name) {
			return nil, fmt.Errorf("catalog name %q: use lowercase letters, digits and dashes", r.Name)
		}
		if seen[r.Name] {
			return nil, fmt.Errorf("two catalogs are named %q", r.Name)
		}
		seen[r.Name] = true
		if err := validRemoteURL(r.URL); err != nil {
			return nil, fmt.Errorf("%s: %w", r.Name, err)
		}
		if r.Branch == "" {
			r.Branch = "main"
		}
		if err := build.ValidateBranch(r.Branch); err != nil {
			return nil, fmt.Errorf("%s: %w", r.Name, err)
		}
		rs[i] = r
	}
	return rs, nil
}

// ValidRemotes checks a value for the setting.
func ValidRemotes(raw string) error {
	_, err := ParseRemotes(raw)
	return err
}

var scpRemote = regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9.-]+:[^\s]+$`)

func validRemoteURL(u string) error {
	switch {
	case u == "":
		return errors.New("a repository URL is required")
	case strings.ContainsAny(u, " \t\n"):
		return errors.New("the repository URL contains whitespace")
	case strings.HasPrefix(u, "https://"), strings.HasPrefix(u, "ssh://"), strings.HasPrefix(u, "/"), scpRemote.MatchString(u):
	default:
		return errors.New("use an https:// or ssh:// clone URL, user@host:path, or an absolute path")
	}
	if strings.HasPrefix(u, "https://") && strings.Contains(strings.TrimPrefix(u, "https://"), "@") {
		return errors.New("no credentials in the URL — a private catalog clones over SSH with this fog's catalog key")
	}
	return nil
}

func sshRemote(u string) bool { return strings.HasPrefix(u, "ssh://") || scpRemote.MatchString(u) }

// Status is how one catalog stands on this fog.
type Status struct {
	Remote
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Trusted     bool   `json:"trusted"`
	// From is what the store is reading: "clone" (fetched from URL),
	// "seed" (the copy this fog shipped with, until a fetch succeeds), or
	// "" when there is nothing to read yet.
	From     string    `json:"from,omitempty"`
	Commit   string    `json:"commit,omitempty"`
	SyncedAt time.Time `json:"syncedAt,omitzero"`
	// Error is why the last fetch or read failed; the catalog may still be
	// served from an earlier one.
	Error string `json:"error,omitempty"`
	// Detail is what git said, when Error puts it in other words.
	Detail   string    `json:"detail,omitempty"`
	Apps     int       `json:"apps"`
	Problems []Problem `json:"problems,omitempty"`
}

// seed holds the catalogs a release ships with, under seed/<name>/ — what a
// fog that has never reached the network (or a dev build) reads until its
// first fetch. scripts/catalog-seed.sh fills it; the checkout has only the
// README.
//
//go:embed all:seed
var seed embed.FS

// Registry keeps this fog's catalogs: clones under Dir, read into one
// store.
type Registry struct {
	Dir string // <data>/catalogs
	// Key returns the private key ssh catalogs are cloned with.
	Key func() (string, error)
	Log *slog.Logger
	// Seed overrides the embedded seed (tests).
	Seed fs.FS

	syncMu sync.Mutex // one fetch at a time

	mu       sync.RWMutex
	remotes  []Remote
	cats     map[string]*Catalog
	status   map[string]*Status
	entries  []Entry
	featured []string
}

func (r *Registry) builder() *build.Builder { return &build.Builder{DataDir: r.Dir} }

func (r *Registry) repoDir(name string) string {
	return filepath.Join(r.Dir, "builds", name, "repo")
}

func (r *Registry) seedFS() fs.FS {
	if r.Seed != nil {
		return r.Seed
	}
	sub, _ := fs.Sub(seed, "seed")
	return sub
}

// SetRemotes changes the list and rereads what is on disk for it. Nothing
// is fetched; Sync does that.
func (r *Registry) SetRemotes(rs []Remote) {
	r.mu.Lock()
	r.remotes = slices.Clone(rs)
	if r.status == nil {
		r.status = map[string]*Status{}
	}
	r.mu.Unlock()
	r.reload()
}

// Remotes is the current list.
func (r *Registry) Remotes() []Remote {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.Clone(r.remotes)
}

// Sync fetches every catalog and rereads the store. A catalog that can't be
// fetched keeps serving what this fog had of it, with the error in its
// status.
func (r *Registry) Sync(ctx context.Context) {
	r.syncMu.Lock()
	defer r.syncMu.Unlock()
	remotes := r.Remotes()
	errs := map[string][2]string{}
	synced := map[string]time.Time{}
	for _, rm := range remotes {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		err := r.fetch(ctx, rm)
		cancel()
		if err != nil {
			raw := err.Error()
			errs[rm.Name] = [2]string{explainFetch(rm, raw), raw}
			if r.Log != nil {
				r.Log.Warn("catalog fetch failed", "catalog", rm.Name, "err", err)
			}
			continue
		}
		synced[rm.Name] = time.Now()
	}
	r.prune(remotes)
	r.mu.Lock()
	for _, rm := range remotes {
		st := r.statusFor(rm.Name)
		st.Error = errs[rm.Name][0]
		if st.Detail = errs[rm.Name][1]; st.Detail == st.Error {
			st.Detail = ""
		}
		if t, ok := synced[rm.Name]; ok {
			st.SyncedAt = t
		}
	}
	r.mu.Unlock()
	r.reload()
}

func (r *Registry) statusFor(name string) *Status {
	if r.status == nil {
		r.status = map[string]*Status{}
	}
	st := r.status[name]
	if st == nil {
		st = &Status{}
		r.status[name] = st
	}
	return st
}

func (r *Registry) fetch(ctx context.Context, rm Remote) error {
	key := ""
	if sshRemote(rm.URL) {
		if r.Key == nil {
			return errors.New("this fog has no catalog key for ssh")
		}
		k, err := r.Key()
		if err != nil {
			return fmt.Errorf("catalog key: %w", err)
		}
		key = k
	}
	// A clone of another URL is another catalog: start over rather than
	// fetch one repository's branch into another's history.
	if dir := r.repoDir(rm.Name); dirExists(filepath.Join(dir, ".git")) {
		if cur, err := execx.Output(ctx, "git", "-C", dir, "remote", "get-url", "origin"); err == nil && strings.TrimSpace(cur) != rm.URL {
			os.RemoveAll(filepath.Dir(dir))
		}
	}
	var log strings.Builder
	if _, err := r.builder().Fetch(ctx, rm.Name, rm.URL, rm.Branch, key, &limitWriter{w: &log, n: 4096}); err != nil {
		if tail := gitSaid(log.String()); tail != "" {
			return fmt.Errorf("%w: %s", err, tail)
		}
		return err
	}
	return nil
}

// prune removes clones of catalogs no longer in the list.
func (r *Registry) prune(remotes []Remote) {
	keep := map[string]bool{}
	for _, rm := range remotes {
		keep[rm.Name] = true
	}
	ents, _ := os.ReadDir(filepath.Join(r.Dir, "builds"))
	for _, e := range ents {
		if e.IsDir() && !keep[e.Name()] {
			os.RemoveAll(filepath.Join(r.Dir, "builds", e.Name()))
		}
	}
	r.mu.Lock()
	for name := range r.status {
		if !keep[name] {
			delete(r.status, name)
		}
	}
	r.mu.Unlock()
}

// reload reads every catalog from disk — its clone, or the seed — and
// rebuilds the store.
func (r *Registry) reload() {
	remotes := r.Remotes()
	cats := map[string]*Catalog{}
	var ordered []*Catalog
	type read struct {
		from, commit, err string
	}
	reads := map[string]read{}
	for _, rm := range remotes {
		c, rd := r.read(rm)
		reads[rm.Name] = rd
		if c == nil {
			continue
		}
		trusted := Trusted(rm)
		for i := range c.Entries {
			c.Entries[i].Trusted = trusted
		}
		cats[rm.Name] = c
		ordered = append(ordered, c)
	}
	entries := Merge(ordered)
	var featured []string
	for _, c := range ordered {
		featured = append(featured, c.Featured...)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.cats, r.entries, r.featured = cats, entries, featured
	for _, rm := range remotes {
		st := r.statusFor(rm.Name)
		rd := reads[rm.Name]
		st.Remote, st.Trusted, st.From, st.Commit = rm, Trusted(rm), rd.from, rd.commit
		if rd.err != "" && st.Error == "" {
			st.Error = rd.err
		}
		st.Title, st.Description, st.Apps, st.Problems = "", "", 0, nil
		if c := cats[rm.Name]; c != nil {
			st.Title, st.Description, st.Problems = c.Title, c.Description, c.Problems
			for _, e := range entries {
				if e.Catalog == rm.Name {
					st.Apps++
				}
			}
		}
	}
}

func (r *Registry) read(rm Remote) (*Catalog, struct{ from, commit, err string }) {
	var rd struct{ from, commit, err string }
	dir := r.repoDir(rm.Name)
	if dirExists(filepath.Join(dir, ".git")) {
		// Read through an os.Root: a symlink in someone's catalog must not
		// reach a file outside it.
		root, err := os.OpenRoot(dir)
		if err == nil {
			c, lerr := Load(root.FS(), rm.Name)
			if lerr == nil {
				rd.from = "clone"
				rd.commit, _ = execx.Output(context.Background(), "git", "-C", dir, "rev-parse", "--short=12", "HEAD")
				return c, rd
			}
			err = lerr
		}
		rd.err = err.Error()
	}
	if sub, err := fs.Sub(r.seedFS(), rm.Name); err == nil && Trusted(rm) {
		if c, err := Load(sub, rm.Name); err == nil {
			rd.from = "seed"
			return c, rd
		}
	}
	if rd.err == "" {
		rd.err = "not fetched yet"
	}
	return nil, rd
}

// Entries is the store: every catalog's entries, merged.
func (r *Registry) Entries() []Entry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.Clone(r.entries)
}

// Featured is what the store shows before the operator has chosen.
func (r *Registry) Featured() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.Clone(r.featured)
}

// Statuses is every catalog in the list, in order.
func (r *Registry) Statuses() []Status {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Status, 0, len(r.remotes))
	for _, rm := range r.remotes {
		if st := r.status[rm.Name]; st != nil {
			out = append(out, *st)
		} else {
			out = append(out, Status{Remote: rm, Trusted: Trusted(rm)})
		}
	}
	return out
}

// Asset opens a listing's artwork: a file an entry of the named catalog
// has, of a type a listing may name.
func (r *Registry) Asset(catalog, id, file string) (fs.File, string, error) {
	typ := ArtworkType(file)
	if typ == "" || strings.ContainsAny(file, `/\`) || strings.HasPrefix(file, ".") || strings.ContainsAny(id, `/\.`) {
		return nil, "", fs.ErrNotExist
	}
	r.mu.RLock()
	c := r.cats[catalog]
	r.mu.RUnlock()
	if c == nil {
		return nil, "", fs.ErrNotExist
	}
	f, err := c.FS.Open("apps/" + id + "/" + file)
	if err != nil {
		return nil, "", err
	}
	return f, typ, nil
}

// Loop fetches every catalog now and then every interval; wake makes it
// fetch early (the list changed, or someone asked).
func (r *Registry) Loop(ctx context.Context, every time.Duration, wake <-chan struct{}) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		r.Sync(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-wake:
		}
	}
}

func dirExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// gitSaid is what git said about a failure, as one line: the lines that
// carry the cause, not the advice ssh's failures end with ("Please make
// sure you have the correct access rights / and the repository exists"),
// which reads as the cause when it is all that is kept.
func gitSaid(s string) string {
	var keep []string
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(l)
		low := strings.ToLower(l)
		if l == "" || strings.HasPrefix(low, "please make sure you have the correct access rights") ||
			strings.HasPrefix(low, "and the repository exists") || strings.HasPrefix(low, "cloning into") {
			continue
		}
		keep = append(keep, l)
	}
	if len(keep) > 4 {
		keep = keep[len(keep)-4:]
	}
	return strings.Join(keep, " ")
}

type limitWriter struct {
	w io.Writer
	n int
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if l.n <= 0 {
		return len(p), nil
	}
	q := p
	if len(q) > l.n {
		q = q[:l.n]
	}
	l.n -= len(q)
	l.w.Write(q)
	return len(p), nil
}
