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
	sn := &snapshot{apps: []appInfo{
		{app: store.App{ID: 1, Name: "app"}, spec: &apps.AppSpec{Env: map[string]string{
			"DB": "postgres://u@db:5432/x", "S": "https://lucid/ingest", "OK": "http://lucid", "EXT": "https://example.com",
		}}},
		{app: store.App{ID: 2, Name: "db"}, spec: &apps.AppSpec{}},
		{app: store.App{ID: 3, Name: "lucid"}, spec: &apps.AppSpec{}},
		{app: store.App{ID: 4, Name: "fit"}, spec: &apps.AppSpec{Network: apps.NetworkSealed, Reach: []string{"lucid"}, Env: map[string]string{"L": "http://lucid"}}},
	}}
	rep := checkWireUnsupported(context.Background(), sn)
	var got []string
	for _, f := range rep.findings {
		got = append(got, f.AppName+" "+f.Subject)
	}
	if strings.Join(got, ",") != "app db (postgres://),app lucid (https://)" {
		t.Fatalf("findings = %q", got)
	}
	if rep := checkSealedWireRefused(context.Background(), sn); len(rep.findings) != 1 || rep.findings[0].AppName != "fit" {
		t.Fatalf("sealed: %+v", rep.findings)
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
