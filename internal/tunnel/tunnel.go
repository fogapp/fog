// Package tunnel manages a Cloudflare Tunnel: it installs the cloudflared
// binary and supervises it as a child process of fogd — start, restart on
// crash with backoff, clean stop. Routes are configured in the Cloudflare
// Zero Trust dashboard; Fog's job is keeping the connector alive.
package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"gitea.lan/matthew/fog/internal/store"
	"gitea.lan/matthew/fog/internal/supervise"
	"gitea.lan/matthew/fog/internal/vault"
)

// SecretToken is the vault key for the tunnel token (from the Zero Trust
// dashboard's connector setup). Never returned by any API.
const SecretToken = "cloudflare_tunnel_token"

const defaultBinPath = "/usr/local/bin/cloudflared"

type Manager struct {
	Store   *store.Store
	Vault   *vault.Vault
	Log     *slog.Logger
	BinPath string // tests/dev override; default /usr/local/bin/cloudflared
	// ReleasesAPI overrides the GitHub latest-release endpoint (tests).
	ReleasesAPI string

	once sync.Once
	sup  *supervise.Process
}

type Status struct {
	supervise.Status
	TokenConfigured bool `json:"tokenConfigured"`
	// Service is what the tunnel's public hostnames must name as their
	// service: Caddy's tunnel socket (internal/proxy, TunnelService).
	Service string `json:"service,omitempty"`
}

func (m *Manager) bin() string {
	if m.BinPath != "" {
		return m.BinPath
	}
	return defaultBinPath
}

func (m *Manager) installed() bool { return supervise.Executable(m.bin()) }

func (m *Manager) secrets() vault.Secrets { return vault.Secrets{Store: m.Store, Vault: m.Vault} }

func (m *Manager) token() (string, error) {
	tok, err := m.secrets().Get(0, SecretToken)
	if errors.Is(err, store.ErrNotFound) {
		return "", fmt.Errorf("no tunnel token saved — paste one from the Cloudflare Zero Trust dashboard first")
	}
	return tok, err
}

// proc is the supervisor, built on first use so a Manager stays a plain
// struct literal.
func (m *Manager) proc() *supervise.Process {
	m.once.Do(func() {
		m.sup = &supervise.Process{
			Name:      "cloudflared",
			Log:       m.Log,
			Installed: m.installed,
			Install:   m.install,
			Command:   m.command,
		}
	})
	return m.sup
}

func (m *Manager) Status() Status {
	_, tokErr := m.Store.GetSecret(0, SecretToken)
	return Status{
		Status:          m.proc().Status(m.installed()),
		TokenConfigured: tokErr == nil,
	}
}

// Start begins supervision (installing cloudflared first when needed).
// Returns immediately; progress is visible via Status.
func (m *Manager) Start() error {
	if _, err := m.token(); err != nil {
		return err
	}
	m.proc().Start()
	return nil
}

// Stop terminates cloudflared and disables restarts.
func (m *Manager) Stop() {
	m.proc().Stop()
}

// command builds one cloudflared run, reading the token afresh each time so
// a token replaced while the tunnel runs takes effect at the next restart.
func (m *Manager) command() (*exec.Cmd, error) {
	tok, err := m.token()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(m.bin(), "tunnel", "--no-autoupdate", "run")
	// Token via env, not argv — keeps it out of process listings.
	cmd.Env = append(os.Environ(), "TUNNEL_TOKEN="+tok)
	return cmd, nil
}

// install downloads the official binary for this platform (Linux only —
// dev machines run a stub via BinPath).
func (m *Manager) install(ctx context.Context) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("automatic cloudflared install is only supported on Linux")
	}
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		return fmt.Errorf("unsupported architecture %s", runtime.GOARCH)
	}
	asset := "cloudflared-linux-" + runtime.GOARCH
	url := "https://github.com/cloudflare/cloudflared/releases/latest/download/" + asset
	// Generous but bounded: the binary is ~40MB, which a slow home uplink
	// may take minutes to fetch — but a hung transfer must not wedge the
	// install forever.
	client := &http.Client{Timeout: 5 * time.Minute}
	// The release API names the latest version's asset and, where GitHub
	// has recorded one, its sha256: with it the download is checked
	// against the published digest. Without it (the API rate-limited or
	// unreachable, an older asset with no digest) the install falls back
	// to the shape check below rather than failing outright.
	var wantSHA string
	if rel, err := latestAsset(ctx, client, m.releasesAPI(), asset); err == nil {
		url, wantSHA = rel.URL, rel.SHA256
	} else {
		m.Log.Warn("cloudflared release lookup; installing without a published checksum", "err", err)
	}
	tmp := m.bin() + ".download"
	// Whatever arrived must also be an ELF executable built for this
	// machine before it replaces the binary under the real path. A
	// truncated download, an HTML error page, or a wrong-arch asset all
	// fail there.
	if err := supervise.Fetch(ctx, client, url, tmp, maxDownload, wantSHA); err != nil {
		return err
	}
	if err := supervise.InstallVerified(tmp, m.bin(), runtime.GOARCH); err != nil {
		return fmt.Errorf("downloaded cloudflared %w", err)
	}
	m.Log.Info("cloudflared installed", "path", m.bin(), "verified", wantSHA != "")
	return nil
}

const releasesAPI = "https://api.github.com/repos/cloudflare/cloudflared/releases/latest"

func (m *Manager) releasesAPI() string {
	if m.ReleasesAPI != "" {
		return m.ReleasesAPI
	}
	return releasesAPI
}

type releaseAsset struct {
	URL    string // browser download URL
	SHA256 string // hex; empty when GitHub published no digest
}

// latestAsset finds name among the latest release's assets.
func latestAsset(ctx context.Context, client *http.Client, api, name string) (releaseAsset, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, api, nil)
	if err != nil {
		return releaseAsset{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return releaseAsset{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return releaseAsset{}, fmt.Errorf("release lookup: HTTP %d", resp.StatusCode)
	}
	var rel struct {
		Assets []struct {
			Name   string `json:"name"`
			URL    string `json:"browser_download_url"`
			Digest string `json:"digest"` // "sha256:<hex>"
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&rel); err != nil {
		return releaseAsset{}, fmt.Errorf("release lookup: %w", err)
	}
	for _, a := range rel.Assets {
		if a.Name != name {
			continue
		}
		if !strings.HasPrefix(a.URL, "https://") {
			return releaseAsset{}, fmt.Errorf("release lookup: %s has no https download URL", name)
		}
		out := releaseAsset{URL: a.URL}
		if hex, ok := strings.CutPrefix(a.Digest, "sha256:"); ok && len(hex) == 64 {
			out.SHA256 = strings.ToLower(hex)
		}
		return out, nil
	}
	return releaseAsset{}, fmt.Errorf("release lookup: no asset named %s", name)
}

// maxDownload bounds the cloudflared download; the binary is ~40MB, so
// hitting this cap means the response is not the file we expect.
const maxDownload = 256 << 20
