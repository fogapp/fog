package backup

import (
	"errors"
	"strings"
	"testing"
)

// pg_restore's format error is only explained when the manifest proves
// the dump came from a newer major than the server; every other failure
// passes through untouched so a real corruption is never dressed up as a
// version gap.
func TestExplainRestore(t *testing.T) {
	raw := errors.New("pg_restore fog_verify_hub: pg_restore: error: unsupported version (1.16) in file header")
	manifest := `{"pg": {"dump_tool": "pg_dump (PostgreSQL) 18.6 (Debian 18.6-1.pgdg13+2)"}}`

	got := explainRestore(raw, manifest, 16)
	if !strings.Contains(got.Error(), "dumped by pg_dump 18") || !strings.Contains(got.Error(), "Postgres 16 cannot read") || !errors.Is(got, raw) {
		t.Fatalf("newer dump than server must be explained and wrap the original, got %v", got)
	}
	for name, c := range map[string]struct {
		err      error
		manifest string
		server   int
	}{
		"server already newer": {raw, manifest, 18},
		"unknown server":       {raw, manifest, 0},
		"no dump_tool":         {raw, `{"pg": {}}`, 16},
		"a different error":    {errors.New("pg_restore: error: could not execute query"), manifest, 16},
		"unparseable manifest": {raw, "not json", 16},
	} {
		if got := explainRestore(c.err, c.manifest, c.server); got != c.err {
			t.Errorf("%s: must pass through, got %v", name, got)
		}
	}
	if explainRestore(nil, manifest, 16) != nil {
		t.Error("nil stays nil")
	}
}
