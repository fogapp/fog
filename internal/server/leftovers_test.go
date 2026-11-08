package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitea.lan/matthew/fog/internal/apps"
)

// An import named like an app deleted with its data kept would take that
// data over. Confirming its review without saying so is refused, naming
// what it would take; with the operator's yes it goes ahead.
func TestImportReviewConfirmNeedsConsentForLeftovers(t *testing.T) {
	t.Parallel()

	s, h := newTestServer(t)
	s.engine = &apps.Engine{Store: s.store, Vault: s.vault, Mat: &apps.Materializer{DataDir: s.cfg.DataDir}}
	admin := setup(t, h)
	old := filepath.Join(s.engine.Mat.DataPath("vault"), "db")
	os.MkdirAll(old, 0o755)
	os.WriteFile(filepath.Join(old, "users.sqlite"), []byte("x"), 0o600)

	body, _ := json.Marshal(map[string]string{"yaml": "name: vault\nspec:\n  source: {type: image, image: evil:latest}\n"})
	w := do(t, h, "POST", "/api/apps/import", string(body), withCookie(admin))
	if w.Code != http.StatusCreated {
		t.Fatalf("import: %d %s", w.Code, w.Body)
	}
	var got struct {
		Rev    int64       `json:"rev"`
		Review apps.Review `json:"review"`
	}
	json.Unmarshal(w.Body.Bytes(), &got)
	w = do(t, h, "GET", "/api/apps/vault/review", "", withCookie(admin))
	if !strings.Contains(w.Body.String(), "data in "+s.engine.Mat.DataPath("vault")) {
		t.Fatalf("review doesn't name the leftovers: %s", w.Body)
	}

	w = do(t, h, "POST", "/api/apps/vault/review/confirm", `{"rev":`+itoa(int(got.Rev))+`}`, withCookie(admin))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "leftover_data") {
		t.Fatalf("confirm without consent: %d %s", w.Code, w.Body)
	}
	w = do(t, h, "POST", "/api/apps/vault/review/confirm", `{"rev":`+itoa(int(got.Rev))+`,"adoptLeftovers":true}`, withCookie(admin))
	if w.Code != http.StatusAccepted {
		t.Fatalf("confirm with consent: %d %s", w.Code, w.Body)
	}
}
