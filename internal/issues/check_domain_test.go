package issues

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/dockerx"
)

// inspectDocker serves just enough of the Engine API for the check: one
// container inspect, carrying the env the app actually runs with.
func inspectDocker(t *testing.T, env []string) *dockerx.Client {
	t.Helper()
	// os.MkdirTemp, not t.TempDir: its nested name can exceed sun_path.
	sockDir, err := os.MkdirTemp("", "fog")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/json") {
			http.NotFound(w, r)
			return
		}
		detail := map[string]any{
			"Id":     "c1",
			"State":  map[string]any{"Status": "running", "Running": true},
			"Config": map[string]any{"Image": "manor:latest", "Env": env},
		}
		json.NewEncoder(w).Encode(detail)
	}))
	t.Setenv("DOCKER_SOCK", sock)
	return dockerx.New()
}

func manorSnapshot(t *testing.T, baseDomain string) *snapshot {
	t.Helper()
	info := appWith(1, "manor", &apps.AppSpec{
		Routes: []apps.Route{{Port: 80}},
		Env: map[string]string{
			"MANOR_WEBAUTHN_RP_ID":   "{{fqdn}}",
			"MANOR_WEBAUTHN_ORIGINS": "https://{{fqdn}}",
			"JOB_CONCURRENCY":        "1",
		},
	})
	info.main = &dockerx.ContainerSummary{ID: "c1"}
	return &snapshot{dockerOK: true, baseDomain: baseDomain, apps: []appInfo{info}}
}

func domainScanner(t *testing.T, env []string) *Scanner {
	t.Helper()
	return &Scanner{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Docker: inspectDocker(t, env)}
}

// The failure this check exists for: booted before the domain was set, so
// the hostname vars were baked degenerate and stayed that way once the
// domain arrived — the proxy served manor.jen.is while the app rejected it.
func TestStaleDomainEnvFlagsPreDomainDeploy(t *testing.T) {
	sc := domainScanner(t, []string{"MANOR_WEBAUTHN_RP_ID=", "MANOR_WEBAUTHN_ORIGINS=https://", "JOB_CONCURRENCY=1"})
	rep := sc.checkStaleDomainEnv(context.Background(), manorSnapshot(t, "jen.is"))

	if len(rep.findings) != 1 {
		t.Fatalf("expected one finding, got %+v", rep.findings)
	}
	f := rep.findings[0]
	if f.Kind != "stale-domain-env" || f.AppName != "manor" {
		t.Errorf("unexpected finding %+v", f)
	}
	for _, want := range []string{"MANOR_WEBAUTHN_ORIGINS", "MANOR_WEBAUTHN_RP_ID"} {
		if !strings.Contains(f.Detail, want) {
			t.Errorf("detail should name %s, got %q", want, f.Detail)
		}
	}
	if !strings.Contains(f.Title, "manor.jen.is") {
		t.Errorf("title should name the current hostname, got %q", f.Title)
	}
}

func TestStaleDomainEnvCleanWhenContainerMatches(t *testing.T) {
	sc := domainScanner(t, []string{
		"MANOR_WEBAUTHN_RP_ID=manor.jen.is",
		"MANOR_WEBAUTHN_ORIGINS=https://manor.jen.is",
		"JOB_CONCURRENCY=1",
	})
	rep := sc.checkStaleDomainEnv(context.Background(), manorSnapshot(t, "jen.is"))
	if len(rep.findings) != 0 {
		t.Fatalf("matching env should file nothing, got %+v", rep.findings)
	}
	if len(rep.evaluated) != 1 {
		t.Fatalf("a checked app must be evaluated so a fixed issue resolves, got %+v", rep.evaluated)
	}
}

// No domain yet: the deploy drops those vars, so an app without them is
// correct. The app must still be evaluated — otherwise an issue filed
// before the domain was removed would never resolve.
func TestStaleDomainEnvQuietWithoutBaseDomain(t *testing.T) {
	sc := domainScanner(t, []string{"JOB_CONCURRENCY=1"})
	rep := sc.checkStaleDomainEnv(context.Background(), manorSnapshot(t, ""))
	if len(rep.findings) != 0 || len(rep.evaluated) != 1 {
		t.Fatalf("no domain should evaluate quietly, got %+v", rep)
	}
}

// Same missing-evidence rule as the other container checks.
func TestStaleDomainEnvSkipsWhenDockerListingFailed(t *testing.T) {
	sc := &Scanner{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Docker: &dockerx.Client{}}
	sn := manorSnapshot(t, "jen.is")
	sn.dockerOK = false
	if rep := sc.checkStaleDomainEnv(context.Background(), sn); len(rep.findings) != 0 || len(rep.evaluated) != 0 {
		t.Fatalf("failed listing must report nothing, got %+v", rep)
	}
}

// An app whose spec hardcodes the hostname (the user typed it in Config →
// Environment) has no template to go stale.
func TestStaleDomainEnvIgnoresUntemplatedApps(t *testing.T) {
	sc := domainScanner(t, []string{"MANOR_WEBAUTHN_ORIGINS=https://typed-by-hand.example"})
	sn := manorSnapshot(t, "jen.is")
	sn.apps[0].spec.Env = map[string]string{"MANOR_WEBAUTHN_ORIGINS": "https://typed-by-hand.example"}
	if rep := sc.checkStaleDomainEnv(context.Background(), sn); len(rep.findings) != 0 {
		t.Fatalf("hardcoded env is the operator's choice, got %+v", rep.findings)
	}
}
