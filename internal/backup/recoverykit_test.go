package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/scrypt"

	"gitea.lan/matthew/fog/internal/store"
	"gitea.lan/matthew/fog/internal/vault"
)

// legacyScryptKit seals bytes the way fog did before Argon2id (FOGKIT1,
// scrypt N=2^15) — the format DecryptKit must still read.
func legacyScryptKit(t *testing.T, plain []byte, passphrase string) []byte {
	t.Helper()
	salt := make([]byte, 16)
	rand.Read(salt)
	key, err := scrypt.Key([]byte(passphrase), salt, 1<<15, 8, 1, 32)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	nonce := make([]byte, gcm.NonceSize())
	rand.Read(nonce)
	ct := gcm.Seal(nil, nonce, plain, []byte("FOGKIT1\x00"))
	out := append([]byte("FOGKIT1\x00"), salt...)
	out = append(out, nonce...)
	return append(out, ct...)
}

// The kit is the cheapest passphrase oracle fog has, so it is sealed with
// Argon2id (the locker's strength), not scrypt — while kits written by an
// older fog still open.
func TestKitUsesArgon2idAndReadsLegacyScrypt(t *testing.T) {
	payload := []byte("master.key + fog.db stand-in")
	sealed, err := EncryptKit(payload, "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if string(sealed[:8]) != "FOGKIT2\x00" {
		t.Fatalf("new kits must be the Argon2id format, got magic %q", sealed[:8])
	}
	if !IsKit(sealed[:8]) {
		t.Fatal("IsKit must accept the Argon2id format")
	}
	if got, err := DecryptKit(sealed, "correct horse battery staple"); err != nil || string(got) != string(payload) {
		t.Fatalf("Argon2id kit round trip: %q %v", got, err)
	}

	legacy := legacyScryptKit(t, payload, "correct horse battery staple")
	if !IsKit(legacy[:8]) {
		t.Fatal("IsKit must still accept the legacy scrypt format")
	}
	if got, err := DecryptKit(legacy, "correct horse battery staple"); err != nil || string(got) != string(payload) {
		t.Fatalf("legacy scrypt kit must still open: %q %v", got, err)
	}
	if _, err := DecryptKit(legacy, "wrong one entirely"); err == nil {
		t.Fatal("wrong passphrase opened a legacy kit")
	}
}

func TestCheckKitPassphrase(t *testing.T) {
	for _, bad := range []string{"short", "aaaaaaaaaaaaaaaaaaaa", "0123456789012"} {
		if err := CheckKitPassphrase(bad); err == nil {
			t.Errorf("%q should be rejected as too weak", bad)
		}
	}
	for _, ok := range []string{"", "correct horse battery staple", "a-long-enough-phrase-1"} {
		if err := CheckKitPassphrase(ok); err != nil {
			t.Errorf("%q should be accepted: %v", ok, err)
		}
	}
}

// The passphrase fog must keep to run unattended is sealed with the vault,
// never left in the settings table in the clear — a leak of fog.db's
// settings alone must not hand over the key to every kit and locker.
func TestKitPassphraseSealedAtRest(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	v, err := vault.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	const pass = "correct horse battery staple"
	if err := StoreKitPassphrase(st, v, pass); err != nil {
		t.Fatal(err)
	}
	raw, _ := st.GetSetting(SettingRecoveryKitPassphrase)
	if raw == "" || strings.Contains(raw, pass) {
		t.Fatalf("the settings row must hold ciphertext, not the passphrase: %q", raw)
	}
	if got := KitPassphrase(st, v); got != pass {
		t.Fatalf("KitPassphrase = %q, want the passphrase back", got)
	}

	// An older fog's plaintext is sealed in place on migration, and reads the
	// same before and after.
	if err := st.SetSetting(SettingRecoveryKitPassphrase, pass); err != nil {
		t.Fatal(err)
	}
	if got := KitPassphrase(st, v); got != pass {
		t.Fatalf("a pre-migration plaintext must still read: %q", got)
	}
	if err := MigrateKitPassphrase(st, v); err != nil {
		t.Fatal(err)
	}
	if raw, _ := st.GetSetting(SettingRecoveryKitPassphrase); raw == pass {
		t.Fatal("migration must seal the plaintext")
	}
	if got := KitPassphrase(st, v); got != pass {
		t.Fatalf("KitPassphrase after migration = %q", got)
	}
}

func TestKitEncryptionRoundTrip(t *testing.T) {
	dir := t.TempDir()
	payload := []byte("pretend this is a tar.gz of master.key and fog.db")
	dst := filepath.Join(dir, "kit.enc")
	if err := sealKitTo(payload, dst, "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}

	sealed, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !IsKit(sealed[:8]) {
		t.Fatal("a sealed kit does not start with the kit magic")
	}
	plain, err := DecryptKit(sealed, "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if string(plain) != string(payload) {
		t.Fatal("round trip corrupted the payload")
	}

	// Wrong passphrase must fail authentication, not yield garbage.
	if _, err := DecryptKit(sealed, "wrong passphrase entirely"); err == nil {
		t.Fatal("wrong passphrase decrypted successfully")
	}
	// Tampering must fail authentication.
	sealed[len(sealed)-1] ^= 0xFF
	if _, err := DecryptKit(sealed, "correct horse battery staple"); err == nil {
		t.Fatal("tampered kit decrypted successfully")
	}
	// Not a kit at all.
	if _, err := DecryptKit([]byte("junk"), "x"); err == nil {
		t.Fatal("junk accepted as a kit")
	}
	if IsKit([]byte("FOGKIT")) || IsKit([]byte("junkjunk")) {
		t.Fatal("IsKit accepted a head that is not a kit's")
	}
}

// Nothing but the sealed stamp binds a kit's bytes to its name, so an
// attacker with bucket-write could copy an old kit to a newer-dated (or junk)
// name and roll a fresh-machine recovery back. VerifyKit refuses a mismatch,
// a junk name, and a current-format kit that carries no sealed stamp; a
// legacy kit is accepted only as Legacy, with the date found inside it.
func TestVerifyKitCatchesReplay(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(data, "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := os.WriteFile(filepath.Join(data, "master.key"), []byte("mk"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := &Manager{Store: st, DataDir: data, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	const pass = "correct horse battery staple"

	plain, err := m.buildKitTar(context.Background(), m.kitMeta("20260101-010000"))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := EncryptKit(plain, pass)
	if err != nil {
		t.Fatal(err)
	}
	// Read under its real name: fine, with its sealed stamp and date.
	if c, err := VerifyKit(sealed, plain, "fog-recovery-kit-20260101-010000.enc"); err != nil || c.Legacy || c.Stamp != "20260101-010000" || c.Date == "" {
		t.Fatalf("a kit read under its own name must verify: %+v %v", c, err)
	}
	// The same bytes copied to a later-dated name: a replay, refused.
	if _, err := VerifyKit(sealed, plain, "fog-recovery-kit-20260401-010000.enc"); err == nil {
		t.Fatal("a kit renamed to a newer stamp must be refused as a replay")
	}
	// A junk name that would sort newest is refused outright, not trusted.
	for _, junk := range []string{"zzzz.enc", "evil-fog-recovery-kit-20260401-010000.enc", "fog-recovery-kit-20260401-010000.enc.enc"} {
		if KitObjectName(junk) {
			t.Fatalf("KitObjectName accepted a junk name %q", junk)
		}
		if _, err := VerifyKit(sealed, plain, junk); err == nil {
			t.Fatalf("a junk kit name %q must be refused", junk)
		}
	}
	// A current-format kit without its sealed stamp is refused: every fog
	// that writes FOGKIT2 writes kit-meta.json, so its absence is tampering.
	bare := legacyKitPlain(t)
	bareSealed, _ := EncryptKit(bare, pass)
	if _, err := VerifyKit(bareSealed, bare, "fog-recovery-kit-20260401-010000.enc"); err == nil {
		t.Fatal("a FOGKIT2 kit without kit-meta.json must be refused")
	}
	// A legacy (FOGKIT1) kit is accepted only as Legacy, with its inside date.
	legacySealed := legacyScryptKit(t, bare, pass)
	c, err := VerifyKit(legacySealed, bare, "fog-recovery-kit-20260401-010000.enc")
	if err != nil || !c.Legacy {
		t.Fatalf("a legacy kit must be accepted but flagged legacy: %+v %v", c, err)
	}
	if c.Date == "" {
		t.Fatal("a legacy kit's embedded date must be reported")
	}
}

// legacyKitPlain is a decrypted kit tar.gz holding only master.key and
// fog.db — what an older fog produced, before kit-meta.json.
func legacyKitPlain(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range []struct{ name, body string }{{"master.key", "mk"}, {"fog.db", "db"}} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o600, Size: int64(len(f.body)), ModTime: time.Date(2025, 3, 1, 4, 0, 0, 0, time.UTC)}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(f.body))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// WriteRecoveryKit runs on the schedule loop's ctx; a fogd shutdown must
// abort it rather than leave it grinding through VACUUM and scrypt.
func TestWriteRecoveryKitHonorsCancel(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.SetSetting(SettingRecoveryKitPassphrase, "correct horse battery staple")
	st.SetSetting(SettingBackupDir, t.TempDir())

	m := &Manager{Store: st, DataDir: t.TempDir()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.WriteRecoveryKit(ctx); err == nil {
		t.Fatal("a cancelled ctx must abort the kit, not write one")
	}
}

// The scheduled kit opens to master.key and fog.db, and the only files it
// leaves behind are the sealed kit and what was there before: the tar.gz
// is built in memory, so no plaintext archive is staged on disk, in the
// kit directory, DataDir or the system temp dir (which may be tmpfs).
func TestWriteRecoveryKitRoundTripLeavesNoPlaintext(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	data := t.TempDir()
	st, err := store.Open(filepath.Join(data, "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := os.WriteFile(filepath.Join(data, "master.key"), []byte("the master key"), 0o600); err != nil {
		t.Fatal(err)
	}
	backups := t.TempDir()
	st.SetSetting(SettingRecoveryKitPassphrase, "correct horse battery staple")
	st.SetSetting(SettingBackupDir, backups)
	before, _ := os.ReadDir(data)

	m := &Manager{Store: st, DataDir: data, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := m.WriteRecoveryKit(context.Background()); err != nil {
		t.Fatal(err)
	}

	kits, err := os.ReadDir(filepath.Join(backups, KitsDirName))
	if err != nil {
		t.Fatal(err)
	}
	if len(kits) != 1 || filepath.Ext(kits[0].Name()) != ".enc" {
		t.Fatalf("kit dir holds %v, want one sealed kit", kits)
	}
	if after, _ := os.ReadDir(data); len(after) != len(before) {
		t.Fatalf("DataDir went from %v to %v: plaintext left behind", before, after)
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Fatalf("plaintext left in the temp dir: %v", left)
	}

	sealed, err := os.ReadFile(filepath.Join(backups, KitsDirName, kits[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := DecryptKit(sealed, "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(plain))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	got := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got[hdr.Name], _ = io.ReadAll(tr)
	}
	if string(got["master.key"]) != "the master key" || len(got["fog.db"]) == 0 || len(got) != 3 {
		t.Fatalf("kit holds %d files (master.key %q, fog.db %d bytes)", len(got), got["master.key"], len(got["fog.db"]))
	}
	if len(got["kit-meta.json"]) == 0 {
		t.Fatal("kit must carry kit-meta.json binding it to its name")
	}
}

// sealKitTo lands the kit atomically: the destination name only ever holds
// a complete, decryptable kit, and no temp files are left behind.
func TestSealKitToWritesAtomically(t *testing.T) {
	out := t.TempDir()
	dst := filepath.Join(out, "kit.enc")
	if err := sealKitTo([]byte("kit payload"), dst, "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "kit.enc" {
		t.Errorf("output dir holds %v, want only kit.enc — a leftover temp file would shadow the kit", entries)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("kit mode = %v, want 0600", perm)
	}
	sealed, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if plain, err := DecryptKit(sealed, "correct horse battery staple"); err != nil || string(plain) != "kit payload" {
		t.Fatalf("written kit does not round-trip: %v", err)
	}
}
