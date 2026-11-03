package signing

import (
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/store"
	"gitea.lan/matthew/fog/internal/vault"
)

func testKeyring(t *testing.T) (*Keyring, *time.Time) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	vlt, err := vault.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := t0
	return &Keyring{Store: st, Vault: vlt, Now: func() time.Time { return now }}, &now
}

func TestKeyringSignsAndVerifies(t *testing.T) {
	k, _ := testKeyring(t)
	tok, err := k.Sign(map[string]any{"sub": "x"}, "JWT")
	if err != nil {
		t.Fatal(err)
	}
	payload, err := k.Verify(tok)
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil || claims["sub"] != "x" {
		t.Fatalf("claims = %v (%v)", claims, err)
	}
	set, err := k.JWKS()
	if err != nil || len(set.Keys) != 1 || set.Keys[0].IsPublic() == false {
		t.Fatalf("jwks = %+v (%v)", set, err)
	}
}

func TestKeyringSurvivesAColdCache(t *testing.T) {
	k, _ := testKeyring(t)
	if _, err := k.Sign(map[string]any{}, "JWT"); err != nil {
		t.Fatal(err)
	}
	// A restarted fogd: same store and vault, nothing in memory.
	cold := &Keyring{Store: k.Store, Vault: k.Vault, Now: k.Now}
	tok, err := cold.Sign(map[string]any{"a": 1}, "JWT")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.Verify(tok); err != nil {
		t.Fatal(err)
	}
}

func TestKeyringRotatesWithoutBreakingVerification(t *testing.T) {
	k, now := testKeyring(t)
	old, err := k.Sign(map[string]any{}, "JWT")
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(Life - Lead)
	if err := k.Reconcile(); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(Lead + time.Minute) // the successor signs now
	fresh, err := k.Sign(map[string]any{}, "JWT")
	if err != nil {
		t.Fatal(err)
	}
	if kid(t, old) == kid(t, fresh) {
		t.Fatal("did not rotate")
	}
	if _, err := k.Verify(old); err != nil {
		t.Fatalf("a token from the previous key stopped verifying inside Tail: %v", err)
	}
}

func TestKeyringSignsAndVerifiesFromMemory(t *testing.T) {
	k, now := testKeyring(t)
	tok, err := k.Sign(map[string]any{}, "JWT")
	if err != nil {
		t.Fatal(err)
	}
	// Pull the row out from under the warm Keyring. Were Sign or Verify
	// still reading the table, both would now fail.
	if err := k.Store.DeleteSigningKey(kid(t, tok)); err != nil {
		t.Fatal(err)
	}
	again, err := k.Sign(map[string]any{}, "JWT")
	if err != nil {
		t.Fatalf("sign read the store: %v", err)
	}
	if kid(t, again) != kid(t, tok) {
		t.Fatal("sign minted a key instead of using the cached one")
	}
	if _, err := k.Verify(tok); err != nil {
		t.Fatalf("verify read the store: %v", err)
	}
	// The clock still decides: past its unpublish time, the cached key
	// stops verifying without anything rereading the table.
	*now = now.Add(Life + Tail + time.Hour)
	if _, err := k.Verify(tok); err == nil {
		t.Fatal("an unpublished cached key still verifies")
	}
}

func TestRetireAllInvalidatesEverything(t *testing.T) {
	k, _ := testKeyring(t)
	tok, err := k.Sign(map[string]any{}, "JWT")
	if err != nil {
		t.Fatal(err)
	}
	if err := k.RetireAll(); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Verify(tok); err == nil {
		t.Fatal("a token signed before the emergency rotation still verifies")
	}
	if _, err := k.Sign(map[string]any{}, "JWT"); err != nil {
		t.Fatalf("cannot sign after rotation: %v", err)
	}
}

func TestVerifyRefusesOtherAlgorithms(t *testing.T) {
	k, _ := testKeyring(t)
	tok, err := k.Sign(map[string]any{}, "JWT")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	for _, alg := range []string{"none", "HS256", "RS256"} {
		hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"` + alg + `","kid":"` + kid(t, tok) + `"}`))
		if _, err := k.Verify(hdr + "." + parts[1] + "." + parts[2]); err == nil {
			t.Errorf("verified a token claiming alg %s", alg)
		}
	}
}

func kid(t *testing.T, tok string) string {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(strings.Split(tok, ".")[0])
	if err != nil {
		t.Fatal(err)
	}
	var h struct {
		KID string `json:"kid"`
	}
	if err := json.Unmarshal(raw, &h); err != nil {
		t.Fatal(err)
	}
	return h.KID
}
