package issues

import (
	"context"
	"strings"
	"testing"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/store"
)

// A reference fog-wire can't carry is the operator's to see, not a
// connection that silently never works; a sealed app's refused wire too.
func TestWireIssues(t *testing.T) {
	sn := &snapshot{connectedOK: true, apps: []appInfo{
		{app: store.App{ID: 1, Name: "app"}, spec: &apps.AppSpec{Env: map[string]string{
			"DB": "postgres://u@db:5432/x", "S": "https://glass/ingest", "OK": "http://glass", "EXT": "https://example.com",
		}}},
		{app: store.App{ID: 2, Name: "db"}, spec: &apps.AppSpec{}},
		{app: store.App{ID: 3, Name: "glass"}, spec: &apps.AppSpec{}},
		{app: store.App{ID: 4, Name: "fit"}, spec: &apps.AppSpec{Network: apps.NetworkSealed, Reach: []string{"glass"}, Env: map[string]string{"L": "http://glass"}}},
	}}
	rep := checkWireUnsupported(context.Background(), sn)
	var got []string
	for _, f := range rep.findings {
		got = append(got, f.AppName+" "+f.Subject)
	}
	if strings.Join(got, ",") != "app db (postgres://),app glass (https://)" {
		t.Fatalf("findings = %q", got)
	}
	if rep := checkSealedWireRefused(context.Background(), sn); len(rep.findings) != 1 || rep.findings[0].AppName != "fit" {
		t.Fatalf("sealed: %+v", rep.findings)
	}
}

// A sealed app connected to a provider that isn't a sealed peer holds a key
// and reaches nothing: that is its own issue, not also the generic one. A
// peer is wired and files neither; unreadable connections file nothing.
func TestSealedIntegrationRefused(t *testing.T) {
	apps3 := func(peer bool) []appInfo {
		return []appInfo{
			{app: store.App{ID: 1, Name: "fit"}, spec: &apps.AppSpec{Network: apps.NetworkSealed, Reach: []string{"glass", "ntfy"},
				Env: map[string]string{"GLASS_URL": "http://glass", "NTFY_URL": "http://ntfy"}}},
			{app: store.App{ID: 2, Name: "glass"}, spec: &apps.AppSpec{SealedPeer: peer}},
			{app: store.App{ID: 3, Name: "ntfy"}, spec: &apps.AppSpec{}},
		}
	}
	connected := map[int64]map[string]bool{1: {"glass": true}}
	ctx := context.Background()

	sn := &snapshot{apps: apps3(false), connected: connected, connectedOK: true}
	rep := checkSealedIntegrationRefused(ctx, sn)
	if len(rep.findings) != 1 || rep.findings[0].AppName != "fit" || rep.findings[0].Subject != "glass" || rep.findings[0].FixURL != "/apps/glass/config" {
		t.Fatalf("integration: %+v", rep.findings)
	}
	if rep := checkSealedWireRefused(ctx, sn); len(rep.findings) != 1 || rep.findings[0].Subject != "ntfy" {
		t.Fatalf("generic should name only the unconnected ntfy: %+v", rep.findings)
	}

	sn = &snapshot{apps: apps3(true), connected: connected, connectedOK: true}
	if rep := checkSealedIntegrationRefused(ctx, sn); len(rep.findings) != 0 || len(rep.evaluated) != 1 {
		t.Fatalf("a sealed peer resolves it: %+v", rep)
	}

	sn = &snapshot{apps: apps3(false), connected: connected}
	for _, rep := range []report{checkSealedIntegrationRefused(ctx, sn), checkSealedWireRefused(ctx, sn)} {
		if len(rep.findings) != 0 || len(rep.evaluated) != 0 {
			t.Fatalf("unreadable connections must neither file nor resolve: %+v", rep)
		}
	}
}

// An app whose settings name an app it isn't approved to reach is asked
// about, not wired.
func TestWireUnapprovedIssue(t *testing.T) {
	sn := &snapshot{apps: []appInfo{
		{app: store.App{ID: 1, Name: "importer"}, spec: &apps.AppSpec{Env: map[string]string{"V": "http://vault:8200"}}},
		{app: store.App{ID: 2, Name: "vault"}, spec: &apps.AppSpec{}},
	}}
	rep := checkWireUnapproved(context.Background(), sn)
	if len(rep.findings) != 1 || rep.findings[0].AppName != "importer" || rep.findings[0].Subject != "vault" {
		t.Fatalf("findings = %+v", rep.findings)
	}
}
