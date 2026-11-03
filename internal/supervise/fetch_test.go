package supervise

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func serve(t *testing.T, body []byte) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			http.NotFound(w, r)
			return
		}
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestFetchChecksAndBounds(t *testing.T) {
	body := []byte("the binary")
	sum := sha256.Sum256(body)
	url := serve(t, body)
	dst := filepath.Join(t.TempDir(), "bin.download")
	ctx := context.Background()

	if err := Fetch(ctx, http.DefaultClient, url, dst, 1<<20, hex.EncodeToString(sum[:])); err != nil {
		t.Fatalf("matching checksum refused: %v", err)
	}
	if got, _ := os.ReadFile(dst); string(got) != string(body) {
		t.Fatalf("staged %q", got)
	}
	for name, err := range map[string]error{
		"wrong checksum": Fetch(ctx, http.DefaultClient, url, dst, 1<<20, hex.EncodeToString(make([]byte, 32))),
		"oversize":       Fetch(ctx, http.DefaultClient, url, dst, 4, ""),
		"HTTP error":     Fetch(ctx, http.DefaultClient, url+"/missing", dst, 1<<20, ""),
	} {
		if err == nil {
			t.Errorf("%s accepted", name)
		}
		if name != "HTTP error" {
			if _, statErr := os.Stat(dst); statErr == nil {
				t.Errorf("%s left the staged file behind", name)
			}
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := Fetch(cancelled, http.DefaultClient, url, dst, 1<<20, ""); err == nil {
		t.Error("a cancelled download succeeded")
	}
}

func TestInstallVerifiedLeavesTheOldBinaryOnFailure(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "bin")
	os.WriteFile(dst, []byte("old"), 0o755)
	tmp := filepath.Join(dir, "bin.download")
	os.WriteFile(tmp, []byte("<html>404</html>"), 0o600)
	if err := InstallVerified(tmp, dst, "amd64"); err == nil {
		t.Fatal("non-ELF installed")
	}
	if got, _ := os.ReadFile(dst); string(got) != "old" {
		t.Errorf("old binary replaced with %q", got)
	}
	if _, err := os.Stat(tmp); err == nil {
		t.Error("rejected download left behind")
	}

	os.WriteFile(tmp, minimalELF(62), 0o600)
	if err := InstallVerified(tmp, dst, "amd64"); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(dst); err != nil || fi.Mode()&0o111 == 0 {
		t.Errorf("installed binary not executable: %v %v", fi, err)
	}
}
