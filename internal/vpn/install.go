package vpn

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"gitea.lan/matthew/fog/internal/supervise"
)

// Tailscale publishes a machine-readable index of the current stable
// release beside the tarballs, so fogd can install the current version
// without pinning one that goes stale, and — unlike cloudflared's
// latest-release URL — there is a published checksum to verify it against.
const (
	pkgIndexURL = "https://pkgs.tailscale.com/stable/?mode=json"
	pkgBaseURL  = "https://pkgs.tailscale.com/stable/"
)

// maxDownload bounds the tarball; it is ~30MB, so hitting this cap means
// the response is not the file we expect.
const maxDownload = 256 << 20

type pkgIndex struct {
	Tarballs        map[string]string `json:"Tarballs"`
	TarballsVersion string            `json:"TarballsVersion"`
}

// install downloads the current stable tailscale release for this platform
// and puts both binaries in place. Linux only — dev machines point BinDir at
// a directory holding stubs.
func (m *Manager) install(ctx context.Context) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("automatic tailscale install is only supported on Linux")
	}
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		return fmt.Errorf("unsupported architecture %s", runtime.GOARCH)
	}
	// The uplink at home may be slow and the tarball is tens of megabytes,
	// but a hung transfer must not wedge the install forever.
	client := &http.Client{Timeout: 5 * time.Minute}

	name, version, err := latestTarball(ctx, client, runtime.GOARCH)
	if err != nil {
		return err
	}
	want, err := fetchSHA256(ctx, client, pkgBaseURL+name+".sha256")
	if err != nil {
		return fmt.Errorf("could not fetch the published checksum: %w", err)
	}

	tgz := filepath.Join(m.binDir(), ".tailscale-download.tgz")
	defer os.Remove(tgz)
	if err := supervise.Fetch(ctx, client, pkgBaseURL+name, tgz, maxDownload, want); err != nil {
		return fmt.Errorf("tailscale %w", err)
	}
	if err := m.extract(tgz); err != nil {
		return err
	}
	m.mu.Lock()
	m.version = version
	m.mu.Unlock()
	m.Log.Info("tailscale installed", "version", version, "dir", m.binDir())
	return nil
}

// latestTarball reads the release index and picks the tarball for goarch.
func latestTarball(ctx context.Context, client *http.Client, goarch string) (name, version string, err error) {
	resp, err := get(ctx, client, pkgIndexURL)
	if err != nil {
		return "", "", fmt.Errorf("could not reach pkgs.tailscale.com: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", "", fmt.Errorf("release index returned HTTP %d", resp.StatusCode)
	}
	var idx pkgIndex
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&idx); err != nil {
		return "", "", fmt.Errorf("could not read the release index: %w", err)
	}
	return pickTarball(idx.Tarballs, idx.TarballsVersion, goarch)
}

// pickTarball chooses this machine's tarball out of the release index and
// refuses a name that could steer the download somewhere else. The index
// arrives over TLS, but its contents still choose the file, and a name is
// not a path.
func pickTarball(tarballs map[string]string, version, goarch string) (name, ver string, err error) {
	name = tarballs[goarch]
	if name == "" {
		return "", "", fmt.Errorf("no tailscale build published for %s", goarch)
	}
	if name != path.Base(name) || strings.HasPrefix(name, ".") {
		return "", "", fmt.Errorf("release index named an unexpected file %q", name)
	}
	return name, version, nil
}

// fetchSHA256 reads a published `.sha256` file, which holds the bare hex
// digest (some mirrors append a filename, hence the field split).
func fetchSHA256(ctx context.Context, client *http.Client, url string) (string, error) {
	resp, err := get(ctx, client, url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
	if err != nil {
		return "", err
	}
	return parseSHA256(string(b))
}

func parseSHA256(s string) (string, error) {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return "", fmt.Errorf("empty checksum file")
	}
	sum := strings.ToLower(fields[0])
	if len(sum) != 64 {
		return "", fmt.Errorf("checksum is not a sha256 digest")
	}
	if _, err := hex.DecodeString(sum); err != nil {
		return "", fmt.Errorf("checksum is not hex")
	}
	return sum, nil
}

func get(ctx context.Context, client *http.Client, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return client.Do(req)
}

// wantedBinaries are the two files pulled out of the tarball; everything
// else in it (systemd units, completions) is not fogd's business, because
// fogd supervises tailscaled itself.
var wantedBinaries = map[string]bool{"tailscale": true, "tailscaled": true}

// extract pulls the two binaries out of the tarball and moves them into
// place, each one verified as an executable for this machine before it
// takes the real path.
func (m *Manager) extract(tgz string) error {
	f, err := os.Open(tgz)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("not a gzip archive: %w", err)
	}
	defer gz.Close()

	staged := map[string]string{}
	defer func() {
		for _, p := range staged {
			os.Remove(p)
		}
	}()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("could not read the tarball: %w", err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		// Entries are `tailscale_<version>_<arch>/tailscale{,d}`; match on
		// the base name and never on a path from the archive, so nothing
		// the tarball says can decide where a file lands.
		base := path.Base(h.Name)
		if !wantedBinaries[base] || staged[base] != "" {
			continue
		}
		tmp := filepath.Join(m.binDir(), "."+base+".download")
		if err := supervise.Stage(tr, tmp, maxDownload); err != nil {
			return fmt.Errorf("%s: %w", base, err)
		}
		staged[base] = tmp
		if err := supervise.VerifyELF(tmp, runtime.GOARCH); err != nil {
			return fmt.Errorf("%s failed verification: %w", base, err)
		}
		if err := os.Chmod(tmp, 0o755); err != nil {
			return err
		}
	}
	for name := range wantedBinaries {
		if staged[name] == "" {
			return fmt.Errorf("the tailscale tarball did not contain %s", name)
		}
	}
	// Both verified before either is moved: a half-installed pair (a new
	// CLI against an old daemon) is worse than not installing at all.
	for name, tmp := range staged {
		if err := os.Rename(tmp, filepath.Join(m.binDir(), name)); err != nil {
			return err
		}
		delete(staged, name)
	}
	return nil
}
