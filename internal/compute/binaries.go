package compute

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"gitea.lan/matthew/fog/internal/fsx"
)

// Platforms fog-node is built for.
var platformPattern = regexp.MustCompile(`^(darwin|linux)-(amd64|arm64)$`)

// ErrNoBinary is a platform fog has no fog-node for.
var ErrNoBinary = errors.New("no fog-node build for that platform")

// Binary is one fog-node build fog can hand out.
type Binary struct {
	Path    string
	SHA256  string
	Version string
}

// Binaries finds fog-node builds: from Dir when set (a development fogd,
// pointed at `make fog-node`'s output), else downloaded once per release
// from fog's own update source into Cache.
type Binaries struct {
	// Dir holds fog-node-<os>-<arch> files to serve as they are.
	Dir string
	// Cache is where downloads are kept, per version.
	Cache string
	// Version is the fogd release a downloaded fog-node must match.
	Version string
	// Download saves the release artifact key to dst (selfupdate's
	// DownloadArtifact); nil without an update source.
	Download func(ctx context.Context, key, dst string) (version, sha string, err error)
	// Manifest answers the release manifest the update source serves and
	// its signature, as signed (selfupdate's SignedManifest): a machine
	// installs a build only against it. nil offers no updates.
	Manifest func(ctx context.Context) (body, sig []byte, err error)

	mu sync.Mutex
	// sums caches digests of Dir's files by path and modification time.
	sums map[string]sumAt
}

type sumAt struct {
	mod string
	sum string
}

// Get answers the build for platform ("darwin-arm64"), fetching it when it
// isn't here yet.
func (b *Binaries) Get(ctx context.Context, platform string) (*Binary, error) {
	if !platformPattern.MatchString(platform) {
		return nil, ErrNoBinary
	}
	name := "fog-node-" + platform
	if b.Dir != "" {
		p := filepath.Join(b.Dir, name)
		info, err := os.Stat(p)
		if err != nil {
			return nil, ErrNoBinary
		}
		sum, err := b.sum(p, info.ModTime().String())
		if err != nil {
			return nil, err
		}
		return &Binary{Path: p, SHA256: sum, Version: b.Version}, nil
	}
	if b.Cache == "" || b.Download == nil || b.Version == "" || b.Version == "dev" {
		return nil, ErrNoBinary
	}
	dir := filepath.Join(b.Cache, b.Version)
	p := filepath.Join(dir, name)
	if raw, err := os.ReadFile(p + ".sha256"); err == nil {
		if _, err := os.Stat(p); err == nil {
			return &Binary{Path: p, SHA256: string(raw), Version: b.Version}, nil
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	version, sum, err := b.Download(ctx, name, p)
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", name, err)
	}
	if version != b.Version {
		os.Remove(p)
		return nil, fmt.Errorf("the update source has %s, not this fog's %s", version, b.Version)
	}
	if err := os.WriteFile(p+".sha256", []byte(sum), 0o644); err != nil {
		return nil, err
	}
	return &Binary{Path: p, SHA256: sum, Version: b.Version}, nil
}

// signed answers the signed release manifest that names bin, checking here
// what the machine will check again: that it is this release and carries
// bin's digest. (Its signature the machine checks against the key it pins.)
func (b *Binaries) signed(ctx context.Context, platform string, bin *Binary) ([]byte, string, error) {
	if b.Manifest == nil {
		return nil, "", errors.New("fog has no signed release manifest to offer machines (a development build?)")
	}
	body, sig, err := b.Manifest(ctx)
	if err != nil {
		return nil, "", err
	}
	var m struct {
		Version   string `json:"version"`
		Artifacts map[string]struct {
			SHA256 string `json:"sha256"`
		} `json:"artifacts"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, "", fmt.Errorf("the release manifest can't be read: %w", err)
	}
	if m.Version != bin.Version {
		return nil, "", fmt.Errorf("the release manifest is for %s, not %s", m.Version, bin.Version)
	}
	if a, ok := m.Artifacts["fog-node-"+platform]; !ok || !strings.EqualFold(a.SHA256, bin.SHA256) {
		return nil, "", fmt.Errorf("the release manifest doesn't name this fog-node-%s", platform)
	}
	return body, string(sig), nil
}

func (b *Binaries) sum(p, mod string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if s, ok := b.sums[p]; ok && s.mod == mod {
		return s.sum, nil
	}
	sum, err := fsx.FileSHA256(p)
	if err != nil {
		return "", err
	}
	if b.sums == nil {
		b.sums = map[string]sumAt{}
	}
	b.sums[p] = sumAt{mod: mod, sum: sum}
	return sum, nil
}
