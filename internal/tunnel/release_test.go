package tunnel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLatestAssetReadsTheDigest(t *testing.T) {
	sum := strings.Repeat("ab", 32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"tag_name":"2026.9.0","assets":[
			{"name":"cloudflared-linux-arm64","browser_download_url":"https://example.invalid/arm64","digest":"sha256:` + strings.Repeat("cd", 32) + `"},
			{"name":"cloudflared-linux-amd64","browser_download_url":"https://example.invalid/amd64","digest":"sha256:` + strings.ToUpper(sum) + `"},
			{"name":"cloudflared-linux-386","browser_download_url":"https://example.invalid/386"}]}`))
	}))
	defer srv.Close()
	ctx := context.Background()

	got, err := latestAsset(ctx, srv.Client(), srv.URL, "cloudflared-linux-amd64")
	if err != nil || got.URL != "https://example.invalid/amd64" || got.SHA256 != sum {
		t.Fatalf("amd64 = %+v, %v", got, err)
	}
	// An asset without a digest still resolves; the install then falls
	// back to the shape check alone.
	got, err = latestAsset(ctx, srv.Client(), srv.URL, "cloudflared-linux-386")
	if err != nil || got.SHA256 != "" {
		t.Fatalf("386 = %+v, %v", got, err)
	}
	if _, err := latestAsset(ctx, srv.Client(), srv.URL, "cloudflared-linux-riscv64"); err == nil {
		t.Fatal("missing asset resolved")
	}
}

func TestLatestAssetRefusesAPlainHTTPDownload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"assets":[{"name":"x","browser_download_url":"http://example.invalid/x"}]}`))
	}))
	defer srv.Close()
	if _, err := latestAsset(context.Background(), srv.Client(), srv.URL, "x"); err == nil {
		t.Fatal("http download URL accepted")
	}
}
