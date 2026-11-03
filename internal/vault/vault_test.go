// The vault is the one place in Fog where a bug is unrecoverable: if the
// master key or the ciphertext format changes shape, every stored secret is
// gone and no backup of the database alone can bring it back.
package vault

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	v, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, plain := range []string{
		"",
		"hunter2",
		"$2b$12$KIXQ8ZgqYQm3vBnO",
		"multi\nline\nsecret",
		strings.Repeat("long", 10000),
		"unicode: pásswörd ✓ 🔐",
		"\x00\x01\x02 control bytes",
	} {
		ct, err := v.Encrypt(plain)
		if err != nil {
			t.Fatalf("encrypt %q: %v", plain, err)
		}
		got, err := v.Decrypt(ct)
		if err != nil {
			t.Fatalf("decrypt %q: %v", plain, err)
		}
		if got != plain {
			t.Errorf("round trip changed the value: got %q, want %q", got, plain)
		}
	}
}

// GCM is nonce-based; encrypting the same value twice must not produce the
// same ciphertext, or the store would leak which apps share a secret.
func TestEncryptIsNonDeterministic(t *testing.T) {
	v, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, _ := v.Encrypt("same value")
	b, _ := v.Encrypt("same value")
	if a == b {
		t.Error("identical plaintexts produced identical ciphertexts")
	}
}

// The key is generated once and reused: a second Open that minted a fresh key
// would silently orphan every secret already stored.
func TestKeyPersistsAcrossOpen(t *testing.T) {
	dir := t.TempDir()
	first, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ct, err := first.Encrypt("persisted")
	if err != nil {
		t.Fatal(err)
	}

	second, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := second.Decrypt(ct)
	if err != nil {
		t.Fatalf("a reopened vault could not read its own ciphertext: %v", err)
	}
	if got != "persisted" {
		t.Errorf("got %q, want %q", got, "persisted")
	}
}

// Two processes racing through first run must converge on ONE key: if each
// kept its own, whichever wrote last would silently orphan the other's
// secrets. The exclusive link makes the loser adopt the winner's key.
func TestConcurrentFirstOpenSharesOneKey(t *testing.T) {
	dir := t.TempDir()
	const n = 8
	vaults := make([]*Vault, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			vaults[i], errs[i] = Open(dir)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
	}
	ct, err := vaults[0].Encrypt("shared")
	if err != nil {
		t.Fatal(err)
	}
	for i, v := range vaults {
		if got, err := v.Decrypt(ct); err != nil || got != "shared" {
			t.Errorf("vault %d holds a different key: got %q, %v", i, got, err)
		}
	}
}

// A first run must not litter the data dir: the temp file either becomes
// master.key or is removed.
func TestOpenLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(dir); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "master.key" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("data dir holds %v, want only master.key", names)
	}
}

func TestKeyFileIsPrivate(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(dir); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("master.key mode = %v, want 0600", perm)
	}
}

// Tampering must fail loudly. GCM authenticates, so a flipped bit has to
// surface as an error rather than as quietly wrong plaintext.
func TestDecryptRejectsTamperedCiphertext(t *testing.T) {
	v, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ct, err := v.Encrypt("authentic")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(ct)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 0x01
	if _, err := v.Decrypt(base64.StdEncoding.EncodeToString(raw)); err == nil {
		t.Error("a tampered ciphertext decrypted without complaint")
	}
}

func TestDecryptRejectsMalformedInput(t *testing.T) {
	v, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"not base64 at all!", "", base64.StdEncoding.EncodeToString([]byte("short"))} {
		if _, err := v.Decrypt(bad); err == nil {
			t.Errorf("Decrypt(%q) succeeded, want an error", bad)
		}
	}
}

// A corrupt key must stop fogd at startup with a clear message. Carrying on
// would mean writing new secrets under a key that cannot read the old ones.
func TestOpenRejectsCorruptKey(t *testing.T) {
	for _, tc := range []struct{ name, content string }{
		{"not hex", "zzzz\n"},
		{"too short", "abcd\n"},
		{"empty", ""},
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "master.key"), []byte(tc.content), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := Open(dir)
		if err == nil {
			t.Errorf("%s: Open succeeded on a corrupt key", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), "master.key") {
			t.Errorf("%s: error does not name the file: %v", tc.name, err)
		}
	}
}

// A trailing newline is how Open writes the key itself, so reading it back
// must tolerate surrounding whitespace.
func TestOpenToleratesWhitespaceInKeyFile(t *testing.T) {
	dir := t.TempDir()
	first, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ct, _ := first.Encrypt("value")

	path := filepath.Join(dir, "master.key")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("  "+strings.TrimSpace(string(raw))+"\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := Open(dir)
	if err != nil {
		t.Fatalf("whitespace around the key broke Open: %v", err)
	}
	if got, err := second.Decrypt(ct); err != nil || got != "value" {
		t.Errorf("got %q, %v", got, err)
	}
}
