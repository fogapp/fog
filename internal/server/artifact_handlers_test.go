package server

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTar builds a tar of name->body at path, manifest entries included.
func writeTar(t *testing.T, path string, entries [][2]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tw := tar.NewWriter(f)
	for _, e := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: e[0], Mode: 0o600, Size: int64(len(e[1]))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e[1])); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
}

// The inspect endpoint is what lets the install form check a pasted secret
// against the artifact before an app is created, so it has to report the
// fingerprints — and has to refuse anything that isn't an artifact.
func TestInspectArtifactEndpoint(t *testing.T) {
	_, h := newTestServer(t)
	c := setup(t, h)
	dir := t.TempDir()

	if w := do(t, h, "POST", "/api/system/backup-artifacts/inspect", `{"path":"/tmp/x.tar"}`); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated inspect: %d", w.Code)
	}

	body := "rows"
	sum := sha256.Sum256([]byte(body))
	manifest := map[string]any{
		"format":     "fog-backup/1",
		"app":        "hub",
		"created_at": "2026-08-17T00:00:00Z",
		"produced_by": map[string]string{
			"git_sha": "deadbeefcafe0123", "schema_version": "20260810000000",
		},
		"components": []map[string]any{
			{"name": "primary", "path": "db/primary.dump", "kind": "db",
				"bytes": len(body), "sha256": hex.EncodeToString(sum[:])},
		},
		"excluded":            []string{"cache", "queue"},
		"identity":            map[string]any{"host": "hub.old.lan"},
		"secrets_fingerprint": map[string]string{"SECRET_KEY_BASE": "sha256:9f2c11ab04d2"},
	}
	raw, _ := json.Marshal(manifest)
	artifact := filepath.Join(dir, "artifact.tar")
	writeTar(t, artifact, [][2]string{{"manifest.json", string(raw)}, {"db/primary.dump", body}})

	w := do(t, h, "POST", "/api/system/backup-artifacts/inspect",
		`{"path":`+quote(artifact)+`}`, withCookie(c))
	if w.Code != http.StatusOK {
		t.Fatalf("inspect: %d %s", w.Code, w.Body)
	}
	var got struct {
		App          string            `json:"app"`
		GitSha       string            `json:"gitSha"`
		TotalBytes   int64             `json:"totalBytes"`
		Secrets      map[string]string `json:"secrets"`
		IdentityHost string            `json:"identityHost"`
		Excluded     []string          `json:"excluded"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.App != "hub" || got.GitSha != "deadbeefcafe0123" {
		t.Fatalf("identity fields: %+v", got)
	}
	if got.TotalBytes != int64(len(body)) {
		t.Fatalf("totalBytes = %d, want %d", got.TotalBytes, len(body))
	}
	if got.Secrets["SECRET_KEY_BASE"] != "sha256:9f2c11ab04d2" {
		t.Fatalf("fingerprints must reach the form: %v", got.Secrets)
	}
	if got.IdentityHost != "hub.old.lan" || len(got.Excluded) != 2 {
		t.Fatalf("identity/excluded: %+v", got)
	}

	// A downloaded volume snapshot bundle is a different thing with the same
	// extension — the mistake this whole path exists to catch.
	snapshot := filepath.Join(dir, "hub.snapshot.tar")
	writeTar(t, snapshot, [][2]string{{"db-hub_production.dump", body}, {"meta.json", "{}"}})
	w = do(t, h, "POST", "/api/system/backup-artifacts/inspect",
		`{"path":`+quote(snapshot)+`}`, withCookie(c))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("snapshot bundle should be refused, got %d %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "not a fog-backup/1 artifact") {
		t.Fatalf("refusal should say what's wrong: %s", w.Body)
	}

	w = do(t, h, "POST", "/api/system/backup-artifacts/inspect",
		`{"path":`+quote(filepath.Join(dir, "absent.tar"))+`}`, withCookie(c))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing file: %d %s", w.Code, w.Body)
	}
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
