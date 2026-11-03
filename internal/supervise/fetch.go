package supervise

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
)

// Stage copies r into a new file at path, refusing more than maxBytes, and
// flushes it to disk before returning: a crash right after the rename that
// follows must not leave a torn file behind the final name. On any failure
// the file is removed. The file is private (0600) until InstallVerified or
// the caller makes it executable.
func Stage(r io.Reader, path string, maxBytes int64) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(r, maxBytes+1))
	if err == nil && n > maxBytes {
		err = fmt.Errorf("download exceeded %dMB — not the file expected", maxBytes>>20)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
		return err
	}
	return nil
}

// Fetch downloads url to dst through Stage. When wantSHA is set (lowercase
// hex), anything whose SHA-256 differs is removed and refused; downloads
// with no published checksum pass "" and lean on VerifyELF instead.
// Cancelling ctx abandons the transfer.
func Fetch(ctx context.Context, client *http.Client, url, dst string, maxBytes int64, wantSHA string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}
	h := sha256.New()
	if err := Stage(io.TeeReader(resp.Body, h), dst, maxBytes); err != nil {
		return err
	}
	if wantSHA == "" {
		return nil
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != wantSHA {
		os.Remove(dst)
		return fmt.Errorf("download failed checksum verification (got %.16s, published %.16s)", got, wantSHA)
	}
	return nil
}

// InstallVerified moves a staged binary onto its real path once VerifyELF
// accepts it for goarch, executable. On failure the staged file is removed
// and whatever was at dst is left alone.
func InstallVerified(tmp, dst, goarch string) error {
	err := VerifyELF(tmp, goarch)
	if err != nil {
		err = fmt.Errorf("failed verification: %w", err)
	}
	if err == nil {
		err = os.Chmod(tmp, 0o755)
	}
	if err == nil {
		err = os.Rename(tmp, dst)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}
