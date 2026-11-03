package issues

import (
	"context"
	"strings"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/proxy"
	"gitea.lan/matthew/fog/internal/tunnel"
)

// A tunnel set up before Caddy's tunnel listener still names
// https://localhost:443, where fog signs no visitor in. fog can't change
// it, so it says exactly what to change — loudly — until it sees tunnel
// traffic arrive through the listener after any it saw on :443.
func TestTunnelStillOn443IsCritical(t *testing.T) {
	sc, st, _ := testScanner(t, nil)
	sc.DataDir = "/var/lib/fog"
	findings := func() []string {
		t.Helper()
		rep := sc.checkTunnelService(context.Background(), nil)
		var out []string
		for _, f := range rep.findings {
			if f.Severity != "critical" || !strings.Contains(f.Fix, "unix:/var/lib/fog/caddy/tunnel/caddy.sock") || !strings.Contains(f.Fix, "https://localhost:443") {
				t.Fatalf("not loud or not exact: %+v", f)
			}
			out = append(out, f.Detail)
		}
		return out
	}
	if got := findings(); len(got) != 0 {
		t.Fatalf("no tunnel, yet: %v", got)
	}
	if err := st.SetSecret(0, tunnel.SecretToken, "sealed"); err != nil {
		t.Fatal(err)
	}
	if got := findings(); len(got) != 1 {
		t.Fatalf("a tunnel never seen on the listener: %v", got)
	}
	at := func(key string, when time.Time) {
		t.Helper()
		if err := st.SetSetting(key, when.UTC().Format(time.RFC3339)); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	at(proxy.SettingTunnelSeenListener, now.Add(-time.Hour))
	if got := findings(); len(got) != 0 {
		t.Fatalf("seen on the listener: %v", got)
	}
	// Traffic on :443 since — the operator pointed it back, or a second
	// hostname still names the old service.
	at(proxy.SettingTunnelSeenEdge, now)
	got := findings()
	if len(got) != 1 || !strings.Contains(got[0], "port 443 at") {
		t.Fatalf("seen on :443 after the listener: %v", got)
	}
	at(proxy.SettingTunnelSeenListener, now.Add(time.Minute))
	if got := findings(); len(got) != 0 {
		t.Fatalf("the listener again: %v", got)
	}
}
