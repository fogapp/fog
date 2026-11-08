package proxy

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/store/storetest"
)

// The attack: an app publishing a host port one of fog's own containers
// publishes — 18443 was Caddy's tunnel listener, and on no app's refusal
// list — takes it at boot, before fogd brings Caddy up. Every port fog's
// compose files publish, read back from the files themselves, is one an app
// is refused: a port added to them by hand, not through fogports, fails
// here.
func TestEveryPortFogPublishesIsRefusedToApps(t *testing.T) {
	st, err := storetest.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	p := &Proxy{Store: st, DataDir: t.TempDir()}
	caddy, err := p.composeFile()
	if err != nil {
		t.Fatal(err)
	}
	wire, err := p.wireCompose()
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for name, file := range map[string][]byte{"caddy": caddy, "wire": wire} {
		var doc struct {
			Services map[string]struct {
				Ports []string `yaml:"ports"`
			} `yaml:"services"`
		}
		if err := yaml.Unmarshal(file, &doc); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for svc, s := range doc.Services {
			for _, spec := range s.Ports {
				hp, proto := hostPort(t, spec)
				seen++
				app := &apps.AppSpec{Source: apps.Source{Type: "image", Image: "x"}, DeployStrategy: "restart",
					Ports: []apps.Port{{Port: 8080, HostPort: hp, Protocol: proto}}}
				if err := app.Validate(); err == nil {
					t.Errorf("%s/%s publishes %s, which an app may publish too", name, svc, spec)
				}
			}
		}
	}
	if seen == 0 {
		t.Fatal("read no published port from fog's compose files")
	}
}

// hostPort reads the host port and protocol of a short-syntax port.
func hostPort(t *testing.T, spec string) (int, string) {
	t.Helper()
	proto := "tcp"
	if s, p, ok := strings.Cut(spec, "/"); ok {
		spec, proto = s, p
	}
	parts := strings.Split(spec, ":")
	if len(parts) < 2 {
		t.Fatalf("port %q publishes no host port", spec)
	}
	hp, err := strconv.Atoi(parts[len(parts)-2])
	if err != nil {
		t.Fatalf("port %q: %v", spec, err)
	}
	return hp, proto
}
