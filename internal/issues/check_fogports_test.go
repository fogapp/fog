package issues

import (
	"context"
	"strings"
	"testing"

	"gitea.lan/matthew/fog/internal/apps"
)

// An app saved publishing a port fog holds — Caddy's, fogd's, a direct
// port — before such ports were refused is filed, critical, by name.
func TestFogPortTaken(t *testing.T) {
	sn := &snapshot{apps: []appInfo{
		appWith(1, "squat", &apps.AppSpec{Ports: []apps.Port{{Port: 443, HostPort: 443}}}),
		appWith(2, "sly", &apps.AppSpec{Ports: []apps.Port{{Port: 80, HostPort: 8080}, {Port: 9, HostPort: 20001, Protocol: "udp"}}}),
		appWith(3, "dns", &apps.AppSpec{Ports: []apps.Port{{Port: 53, HostPort: 53, Protocol: "udp"}}}),
	}}
	sc := &Scanner{FogdPort: func() int { return 8080 }}
	rep := sc.checkFogPortTaken(context.Background(), sn)
	got := map[string]string{}
	for _, f := range rep.findings {
		if f.Severity != "critical" {
			t.Errorf("%s: severity %s", f.AppName, f.Severity)
		}
		got[f.AppName] = f.Detail
	}
	if len(got) != 2 || !strings.Contains(got["squat"], "443/tcp") || !strings.Contains(got["squat"], "fog's caddy") ||
		!strings.Contains(got["sly"], "8080/tcp, 20001/udp") || !strings.Contains(got["sly"], "fogd and fog's direct ports hold") {
		t.Fatalf("findings = %+v", got)
	}
	if len(rep.evaluated) != 3 {
		t.Errorf("evaluated %v", rep.evaluated)
	}
}
