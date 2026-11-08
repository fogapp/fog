package pg

import (
	"path/filepath"
	"testing"

	"gitea.lan/matthew/fog/internal/store/storetest"
	"gitea.lan/matthew/fog/internal/vault"
)

// A password is minted once and read back after. A store that fails for
// any reason other than "not found" is an error, never a cue to mint a new
// password Postgres doesn't know.
func TestVaultSecretMintsOnlyWhenMissing(t *testing.T) {
	dir := t.TempDir()
	st, err := storetest.Open(filepath.Join(dir, "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	vlt, err := vault.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	app, err := st.CreateApp("demo")
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{Store: st, Vault: vlt}
	first, err := m.vaultSecret(app.ID, "_pg_password")
	if err != nil || first == "" {
		t.Fatalf("mint: %q %v", first, err)
	}
	again, err := m.vaultSecret(app.ID, "_pg_password")
	if err != nil || again != first {
		t.Fatalf("second read = %q %v, want the minted %q", again, err, first)
	}
	st.Close()
	if pw, err := m.vaultSecret(app.ID, "_pg_password"); err == nil {
		t.Fatalf("store failure read as missing; minted %q", pw)
	}
}
