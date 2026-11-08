package proxy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitea.lan/matthew/fog/internal/store"
	"gitea.lan/matthew/fog/internal/store/storetest"
	"gitea.lan/matthew/fog/internal/vault"
)

// Apply converges only when the rendered files differ from what the running
// Caddy last loaded, or when Caddy isn't running.
func TestApplySkipsUnchangedConfig(t *testing.T) {
	dir := t.TempDir()
	st, err := storetest.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	vlt, err := vault.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(store.SettingBaseDomain, "home.example.com"); err != nil {
		t.Fatal(err)
	}

	converges := 0
	var fail error
	running := true
	p := &Proxy{DataDir: dir, Store: st, Vault: vlt, FogUpstream: "host.docker.internal:8080",
		converge: func(context.Context, string) error {
			converges++
			return fail
		},
		caddyRunning: func(context.Context) bool { return running },
		wireConverge: func(context.Context, string) error { return nil },
		subnets:      noSubnets,
	}
	apply := func(want int) {
		t.Helper()
		if err := p.Apply(context.Background()); err != nil && fail == nil {
			t.Fatal(err)
		}
		if converges != want {
			t.Fatalf("converges = %d, want %d", converges, want)
		}
	}

	apply(1) // first apply always converges
	if _, err := os.Stat(filepath.Join(dir, "caddy", "conf", "Caddyfile")); err != nil {
		t.Fatal("Caddyfile not written:", err)
	}
	apply(1) // nothing changed

	// A routing-relevant setting changes the Caddyfile.
	if err := st.SetSetting(SettingAcmeEmail, "ops@example.com"); err != nil {
		t.Fatal(err)
	}
	apply(2)
	apply(2)

	// Caddy gone: same files, but it must be brought back.
	running = false
	apply(3)
	running = true

	// A failed converge is not remembered as applied.
	if err := st.SetSetting(SettingAcmeEmail, "other@example.com"); err != nil {
		t.Fatal(err)
	}
	fail = errors.New("caddy reload: boom")
	if err := p.Apply(context.Background()); err == nil {
		t.Fatal("converge error swallowed")
	}
	fail = nil
	apply(5)
	apply(5)
}

// fog's own route and the gate's forward_auth go to the upstream main
// derived from the real listen port, never a guessed :8080; a proxy without
// one refuses to write a Caddyfile that would route fog to nothing.
func TestApplyUsesGivenFogUpstream(t *testing.T) {
	dir := t.TempDir()
	st, err := storetest.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	vlt, err := vault.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(store.SettingBaseDomain, "home.example.com"); err != nil {
		t.Fatal(err)
	}
	app, err := st.CreateApp("sab")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateRevision(app.ID, `{"name":"sab","image":"sab:1","routes":[{"port":8080,"auth":true}]}`, "", "t"); err != nil {
		t.Fatal(err)
	}
	p := &Proxy{DataDir: dir, Store: st, Vault: vlt,
		converge:     func(context.Context, string) error { return nil },
		caddyRunning: func(context.Context) bool { return true },
		wireConverge: func(context.Context, string) error { return nil },
		subnets:      noSubnets,
	}
	if err := p.Apply(context.Background()); err == nil {
		t.Fatal("Apply without a fog upstream must fail")
	}
	if _, err := os.Stat(filepath.Join(dir, "caddy", "conf", "Caddyfile")); err == nil {
		t.Fatal("Caddyfile written without a fog upstream")
	}

	p.FogUpstream = "host.docker.internal:9123"
	if err := p.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "caddy", "conf", "Caddyfile"))
	if err != nil {
		t.Fatal(err)
	}
	out := string(b)
	for _, want := range []string{"reverse_proxy host.docker.internal:9123", "forward_auth host.docker.internal:9123"} {
		if !strings.Contains(out, want) {
			t.Errorf("Caddyfile missing %q\n%s", want, out)
		}
	}
	if strings.Contains(out, "host.docker.internal:8080") {
		t.Errorf("Caddyfile still routes fog to the default port\n%s", out)
	}
}

func noSubnets(context.Context) (map[string]string, error) { return map[string]string{}, nil }
