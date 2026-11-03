// The scheduled recovery kit: fog's own unrecoverable state — the vault
// master key and a consistent SQLite snapshot — written to the backup
// directory on the nightly schedule, encrypted. App artifacts are only half
// of disaster recovery; without this file their encrypted credentials are
// bricked even with a perfect restore. Encryption is scrypt + AES-256-GCM
// (authenticated from day one — Home Assistant had to migrate off
// unauthenticated CBC the hard way) under an operator-set passphrase.
// Full-server DR: install fog → restore the kit (setup screen) → recreate
// apps with restoreFrom.
package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/scrypt"

	"gitea.lan/matthew/fog/internal/fsx"
	"gitea.lan/matthew/fog/internal/store"
	"gitea.lan/matthew/fog/internal/vault"
)

const (
	// SettingRecoveryKitPassphrase enables the scheduled kit. Stored in
	// fog's DB — which never leaves the box unencrypted; the copies on the
	// NAS are what the passphrase protects.
	SettingRecoveryKitPassphrase = "recovery_kit_passphrase"
	// NoticeRecoveryKitUnscheduled is the operator to-do raised while no
	// passphrase is set. Saving one retracts it (settings handler) so the
	// nudge doesn't outlive the condition it describes.
	NoticeRecoveryKitUnscheduled = "recovery-kit-unscheduled"
	// NoticeRecoveryKitLarge: the newest kit is past half of MaxKitBytes,
	// the most a recovery will read.
	NoticeRecoveryKitLarge = "recovery-kit-large"
	// kitMagicV1 sealed with scrypt (N=2^15) — read for recovery, never
	// written any more. kitMagicV2 seals with Argon2id, the same strength the
	// offsite locker uses, because a kit is the cheapest passphrase oracle
	// there is: cracking one yields master.key, fog.db and the vault.
	kitMagicV1 = "FOGKIT1\x00"
	kitMagicV2 = "FOGKIT2\x00"
	kitMagic   = kitMagicV2 // what EncryptKit writes
	kitKeep    = 7
)

// Kit Argon2id costs — the locker's, so the two agree on how hard a
// passphrase is to attack (docs/locker-contract.md).
const (
	kitArgonTime   = 4
	kitArgonMemKiB = 256 * 1024
	kitArgonThread = 1
)

// The recovery-kit passphrase at rest. It is the one secret fog must keep in
// the clear-at-runtime to run unattended — it seals every nightly kit and
// opens the offsite locker on a fresh machine, so fog cannot simply not
// store it. What it must not do is sit in plaintext in the settings table,
// the most-read, most-copied part of fog.db: a leak of that table alone
// (without master.key) would otherwise hand over the key to every kit and
// locker. So it is sealed with the vault master key and the ciphertext,
// never the passphrase, is what the settings row holds.

// sealedPassPrefix marks a settings value as vault ciphertext. Explicit, so a
// sealed value is never mistaken for a legacy plaintext passphrase (and used
// as one) by a reader that happens to have no vault.
const sealedPassPrefix = "vault:"

// KitPassphrase decrypts the stored recovery-kit passphrase, "" when unset.
// A sealed value needs the vault: without one, or if it does not decrypt, the
// answer is "" — no passphrase — never the ciphertext. An unprefixed value is
// a pre-migration plaintext (an older fog's), read as it is until
// MigrateKitPassphrase seals it.
func KitPassphrase(st *store.Store, v vault.Cipher) string {
	raw, _ := st.GetSetting(SettingRecoveryKitPassphrase)
	if raw == "" {
		return ""
	}
	ct, sealed := strings.CutPrefix(raw, sealedPassPrefix)
	if !sealed {
		return raw
	}
	if v == nil {
		return ""
	}
	p, err := v.Decrypt(ct)
	if err != nil {
		return ""
	}
	return p
}

// StoreKitPassphrase seals pass with the vault and writes it to the settings
// table; "" clears it. A vault is required to store one: the passphrase is
// never written in the clear.
func StoreKitPassphrase(st *store.Store, v vault.Cipher, pass string) error {
	if pass == "" {
		return st.SetSetting(SettingRecoveryKitPassphrase, "")
	}
	if v == nil {
		return errors.New("no vault to seal the recovery-kit passphrase with")
	}
	ct, err := v.Encrypt(pass)
	if err != nil {
		return err
	}
	return st.SetSetting(SettingRecoveryKitPassphrase, sealedPassPrefix+ct)
}

// MigrateKitPassphrase seals a passphrase an older fog left in the settings
// table in plaintext. It runs at boot; an already-sealed value is left alone.
func MigrateKitPassphrase(st *store.Store, v vault.Cipher) error {
	if v == nil {
		return nil
	}
	raw, _ := st.GetSetting(SettingRecoveryKitPassphrase)
	if raw == "" || strings.HasPrefix(raw, sealedPassPrefix) {
		return nil
	}
	return StoreKitPassphrase(st, v, raw)
}

// kitPassphrase is the configured passphrase in plaintext, "" when unset.
func (m *Manager) kitPassphrase() string { return KitPassphrase(m.Store, m.Vault) }

// MinKitPassphraseLen is the floor for a recovery-kit passphrase. The kit is
// the cheapest offline oracle fog has — a cracked kit yields master.key,
// fog.db and the vault — so even with Argon2id the passphrase has to carry
// real entropy. A passphrase, not a password: length does the work.
const MinKitPassphraseLen = 16

// CheckKitPassphrase refuses a passphrase too weak to stand behind the kit.
// Empty is allowed (it clears the schedule); the caller decides what that
// means.
func CheckKitPassphrase(pass string) error {
	if pass == "" {
		return nil
	}
	if len(pass) < MinKitPassphraseLen {
		return fmt.Errorf("the recovery-kit passphrase must be at least %d characters — it is the one key to everything fog encrypts", MinKitPassphraseLen)
	}
	distinct := map[rune]bool{}
	for _, r := range pass {
		distinct[r] = true
	}
	if len(distinct) < 5 {
		return errors.New("the recovery-kit passphrase is too repetitive — use a longer phrase of several words")
	}
	return nil
}

// WriteRecoveryKit builds and encrypts one kit into <backupDir>/fog-recovery/.
// No-op (with a gentle nudge via notice) when no passphrase is configured.
func (m *Manager) WriteRecoveryKit(ctx context.Context) error {
	pass := m.kitPassphrase()
	if pass == "" {
		m.Store.CreateNotice(NoticeRecoveryKitUnscheduled, "",
			"Recovery kit is not scheduled",
			"Set a recovery-kit passphrase in Settings → Storage → Recovery kit and fog will write an encrypted copy of its "+
				"master key + database beside the nightly backups. Without it, losing this machine bricks every "+
				"app's encrypted credentials even with perfect app backups.")
		return nil
	}

	release, err := m.HoldDir()
	if err != nil {
		return err
	}
	defer release()
	// The kit exists to survive this machine; writing it to the machine's
	// own disk because the NAS is away defeats the point.
	if err := m.checkTarget(); err != nil {
		return err
	}
	dir := filepath.Join(m.baseDir(), KitsDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	stamp := fsx.Stamp(time.Now())
	plain, err := m.buildKitTar(ctx, m.kitMeta(stamp))
	if err != nil {
		return err
	}
	defer clear(plain)

	// Last exit before sealing: sealKitTo's derivation is CPU-bound and
	// uncancellable once started, so a shutdown-time cancel lands here.
	if err := ctx.Err(); err != nil {
		return err
	}

	out := filepath.Join(dir, "fog-recovery-kit-"+stamp+".enc")
	if err := sealKitTo(plain, out, pass); err != nil {
		return err
	}
	m.pruneKits(dir)
	m.Log.Info("recovery kit written", "path", out)
	m.checkKitSize(out)
	return nil
}

// MaxKitBytes is the largest sealed kit a recovery reads from a bucket: it
// is buffered whole, decrypted beside itself, and the derivation alone takes
// 256 MiB, so the cap is what keeps a planted giant from exhausting memory.
// Measured: a kit is a few hundred KB plus ~100 bytes per Drive file per
// offsite target (each blob's copy row in fog.db, after gzip), so this is
// about 2.5 million Drive files offsite. A kit past half of it raises an
// Inbox issue (checkKitSize) long before it reaches it.
const MaxKitBytes = 256 << 20

// checkKitSize raises NoticeRecoveryKitLarge while the kit at path is past
// half of MaxKitBytes, and clears it otherwise.
func (m *Manager) checkKitSize(path string) {
	if m.Store == nil {
		return
	}
	fi, err := os.Stat(path)
	if err != nil {
		return
	}
	if fi.Size() <= MaxKitBytes/2 {
		m.Store.DismissNoticeByKind(NoticeRecoveryKitLarge, "")
		return
	}
	m.Store.CreateNotice(NoticeRecoveryKitLarge, "", "The recovery kit is getting large",
		fmt.Sprintf("Tonight's recovery kit is %s; a recovery from a bucket reads kits up to %s. It grows with the number of Drive files "+
			"copied offsite. Past the limit a cloud recovery will refuse it, so download a kit now (Settings → Storage → "+
			"Recovery kit) and keep it somewhere safe off this machine.",
			fsx.HumanBytes(fi.Size()), fsx.HumanBytes(MaxKitBytes)))
}

// ErrNoKitPassphrase: a kit is only ever handed out sealed, and nothing
// seals it until the operator sets a passphrase.
var ErrNoKitPassphrase = errors.New("no recovery-kit passphrase is set")

// SealKit builds a kit and seals it under the current passphrase, all in
// memory (a few MB) — the manual download's form, so a failure anywhere
// surfaces before a single byte goes to the browser.
func (m *Manager) SealKit(ctx context.Context) ([]byte, error) {
	pass := m.kitPassphrase()
	if pass == "" {
		return nil, ErrNoKitPassphrase
	}
	plain, err := m.buildKitTar(ctx, m.kitMeta(fsx.Stamp(time.Now())))
	if err != nil {
		return nil, err
	}
	defer clear(plain)
	return EncryptKit(plain, pass)
}

// buildKitTar assembles master.key + a VACUUM'd SQLite snapshot into a
// tar.gz in memory (the same contents scheduled or downloaded; a few MB).
// The snapshot is the one plaintext file on disk, since SQLite writes it
// to a path; it lives under DataDir, never os.TempDir(): /tmp may be
// tmpfs, and it is every secret in fog.db, unencrypted.
func (m *Manager) buildKitTar(ctx context.Context, meta KitMeta) ([]byte, error) {
	snap := filepath.Join(m.DataDir, fmt.Sprintf(".fog-kit-db-%d.db", time.Now().UnixNano()))
	defer os.Remove(snap)
	if err := m.Store.SnapshotTo(ctx, snap); err != nil {
		return nil, fmt.Errorf("snapshotting fog.db: %w", err)
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range []struct{ src, name string }{
		{filepath.Join(m.DataDir, "master.key"), "master.key"},
		{snap, "fog.db"},
	} {
		if err := addToTar(tw, f.src, f.name); err != nil {
			clear(buf.Bytes())
			return nil, fmt.Errorf("adding %s: %w", f.name, err)
		}
	}
	// kit-meta.json binds the kit's bytes to its stamp and the fog that wrote
	// it (checked by VerifyKit). Without it, copying an old kit to a
	// newer-dated name in the bucket would roll a recovery back to stale
	// state; with it, the embedded stamp no longer matches the name and the
	// read refuses.
	mb, err := json.Marshal(meta)
	if err != nil {
		clear(buf.Bytes())
		return nil, err
	}
	if err := addBytesToTar(tw, kitMetaName, mb); err != nil {
		clear(buf.Bytes())
		return nil, fmt.Errorf("adding %s: %w", kitMetaName, err)
	}
	if err := tw.Close(); err != nil {
		clear(buf.Bytes())
		return nil, err
	}
	if err := gz.Close(); err != nil {
		clear(buf.Bytes())
		return nil, err
	}
	return buf.Bytes(), nil
}

// KitMeta binds a sealed kit to its name and origin (kit-meta.json inside the
// tar). Additive: an older kit without it still restores.
type KitMeta struct {
	Format    string `json:"format"`
	Stamp     string `json:"stamp"`
	Fog       string `json:"fog"`
	CreatedAt string `json:"created_at"`
}

const (
	kitMetaName   = "kit-meta.json"
	KitMetaFormat = "fog-recovery-kit/1"
)

func (m *Manager) kitMeta(stamp string) KitMeta {
	fog := ""
	if m.Store != nil {
		fog, _ = m.Store.GetSetting(store.SettingServerName)
	}
	return KitMeta{Format: KitMetaFormat, Stamp: stamp, Fog: fog, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
}

// kitNameStampRe pulls the stamp out of a kit object name,
// fog-recovery-kit-<stamp>.enc. Anchored at both ends: a name with anything
// before "fog-recovery-kit-" or after ".enc" is not a kit fog wrote.
var kitNameStampRe = regexp.MustCompile(`^fog-recovery-kit-(\d{8}-\d{6}(?:-\d+)?)\.enc$`)

// KitObjectName reports whether name is a well-formed recovery-kit object
// name. A name that is not — a planted "zzzz.enc" that would sort newest — is
// never a kit fog will restore; it must be ignored, not trusted.
func KitObjectName(name string) bool { return kitNameStampRe.MatchString(name) }

// KitCheck is what VerifyKit learned about a kit chosen from a bucket.
type KitCheck struct {
	// Legacy: a kit written before kits sealed their own stamp (FOGKIT1). Its
	// date cannot be verified — an old one copied to a newer name would look
	// the same — so the recovery screen and fog-locker say so.
	Legacy bool `json:"legacy"`
	// Stamp is the sealed stamp (current kits).
	Stamp string `json:"stamp,omitempty"`
	// Date is the sealed creation time for a current kit, or for a legacy one
	// the timestamp its fog.db entry carries inside the kit — shown, but not
	// proof of anything.
	Date string `json:"date,omitempty"`
}

// LegacyKitWarning is what the recovery surfaces say about a legacy kit.
const LegacyKitWarning = "this kit was written by an older fog and does not seal its own date, so fog cannot verify it is really the newest — it could be an older kit renamed. Restore it only if that date is what you expect."

// VerifyKit checks a kit read from a bucket against the object name it was
// chosen under. Nothing else binds a kit's bytes to its name, so this is what
// stops a replayed or renamed kit — an old kit copied to a newer-dated name,
// or to a junk name that sorts newest — from rolling a fresh-machine
// recovery back to stale state.
//
//   - The name must be a well-formed fog-recovery-kit-<stamp>.enc.
//   - A current-format kit (FOGKIT2) must carry kit-meta.json, sealed with
//     it, and its stamp must equal the name's. A FOGKIT2 kit without one is
//     refused: every fog that writes FOGKIT2 writes the meta.
//   - A legacy kit (FOGKIT1) can carry no seal of its date. It is accepted —
//     refusing it would strand recovery from an older fog's locker — but
//     reported Legacy, with the date inside it, so the operator decides.
func VerifyKit(sealed, plain []byte, objectName string) (KitCheck, error) {
	m := kitNameStampRe.FindStringSubmatch(objectName)
	if m == nil {
		return KitCheck{}, fmt.Errorf("recovery kit %q is not a fog-recovery-kit-<stamp>.enc name — refusing to trust it", objectName)
	}
	if len(sealed) >= 8 && string(sealed[:8]) == kitMagicV1 {
		return KitCheck{Legacy: true, Date: kitEmbeddedDate(plain)}, nil
	}
	meta, ok := kitMetaOf(plain)
	if !ok || meta.Stamp == "" {
		return KitCheck{}, fmt.Errorf("recovery kit %q carries no sealed stamp — a current-format kit always does; refusing it", objectName)
	}
	if meta.Stamp != m[1] {
		return KitCheck{}, fmt.Errorf("recovery kit %q carries stamp %q inside — its contents do not match its name, which is what a replayed or renamed kit looks like; refusing to restore stale state", objectName, meta.Stamp)
	}
	return KitCheck{Stamp: meta.Stamp, Date: meta.CreatedAt}, nil
}

// kitEmbeddedDate is the modification time on a legacy kit's fog.db entry —
// roughly when it was written, but set by whoever built the tar.
func kitEmbeddedDate(plain []byte) string {
	gz, err := gzip.NewReader(bytes.NewReader(plain))
	if err != nil {
		return ""
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err != nil {
			return ""
		}
		if filepath.Base(filepath.Clean(hdr.Name)) == "fog.db" && !hdr.ModTime.IsZero() {
			return hdr.ModTime.UTC().Format(time.RFC3339)
		}
	}
}

// kitMetaOf reads kit-meta.json out of a decrypted kit tar.gz. ok is false
// for a legacy kit that has none.
func kitMetaOf(plain []byte) (KitMeta, bool) {
	gz, err := gzip.NewReader(bytes.NewReader(plain))
	if err != nil {
		return KitMeta{}, false
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err != nil {
			return KitMeta{}, false
		}
		if filepath.Base(filepath.Clean(hdr.Name)) == kitMetaName {
			raw, err := io.ReadAll(io.LimitReader(tr, 1<<16))
			if err != nil {
				return KitMeta{}, false
			}
			var meta KitMeta
			if json.Unmarshal(raw, &meta) != nil {
				return KitMeta{}, false
			}
			return meta, true
		}
	}
}

func addBytesToTar(tw *tar.Writer, name string, data []byte) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), ModTime: time.Now()}); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

func (m *Manager) pruneKits(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var kits []string
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".enc" {
			kits = append(kits, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(kits))) // timestamped names: newest first
	for i, name := range kits {
		if i >= kitKeep {
			os.Remove(filepath.Join(dir, name))
		}
	}
}

// ---- encryption ----

// sealKitTo seals plain into dst: magic || salt(16) || nonce(12) || AES-256-GCM.
// Kits are small (a few MB of SQLite), so whole-file sealing is fine — no
// streaming construction needed at this size.
//
// The sealed bytes are opened again before they are written; a kit that
// does not open now will not open during a disaster, and one that fails
// never reaches dst. The write is temp+fsync+rename: dst must never exist
// half-written, because a partial .enc with a fresh mtime would make
// maybeWriteKit skip rewriting it for a day — a day with no usable kit.
func sealKitTo(plain []byte, dst, passphrase string) error {
	sealed, err := EncryptKit(plain, passphrase)
	if err != nil {
		return err
	}
	check, err := DecryptKit(sealed, passphrase)
	if err != nil {
		return fmt.Errorf("kit self-check: sealed kit does not decrypt: %w", err)
	}
	same := bytes.Equal(check, plain)
	clear(check)
	if !same {
		return errors.New("kit self-check: sealed kit does not decrypt to what was sealed")
	}
	return fsx.WriteFileAtomic(dst, sealed, 0o600)
}

// IsKit reports whether head (a file's first bytes) starts like a sealed
// recovery kit, for code that handles kits without opening them. Both the
// current Argon2id format and the legacy scrypt one count.
func IsKit(head []byte) bool {
	return bytes.HasPrefix(head, []byte(kitMagicV2)) || bytes.HasPrefix(head, []byte(kitMagicV1))
}

// IsLegacyKit reports whether sealed is a legacy (FOGKIT1) kit: one that
// cannot seal its own date, which a recovery offers only when no current
// kit opens.
func IsLegacyKit(sealed []byte) bool { return bytes.HasPrefix(sealed, []byte(kitMagicV1)) }

// EncryptKit seals plaintext kit bytes under Argon2id + AES-256-GCM (the
// current format). The inverse of DecryptKit; the installer's tests (and any
// future kit tooling) build kits with it.
func EncryptKit(plain []byte, passphrase string) ([]byte, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	gcm, err := kitAEAD(kitMagicV2, passphrase, salt)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	sealed := gcm.Seal(nil, nonce, plain, []byte(kitMagicV2))
	out := make([]byte, 0, len(kitMagicV2)+len(salt)+len(nonce)+len(sealed))
	out = append(out, kitMagicV2...)
	out = append(out, salt...)
	out = append(out, nonce...)
	out = append(out, sealed...)
	return out, nil
}

// DecryptKit opens a sealed kit and returns the plaintext tar.gz bytes. It
// reads both the current Argon2id format and the legacy scrypt one, so a kit
// written by an older fog still restores. A wrong passphrase (or any
// tampering) fails GCM authentication cleanly.
func DecryptKit(sealed []byte, passphrase string) ([]byte, error) {
	if len(sealed) < 8+16+12+16 {
		return nil, fmt.Errorf("not a fog recovery kit")
	}
	magic := string(sealed[:8])
	if magic != kitMagicV2 && magic != kitMagicV1 {
		return nil, fmt.Errorf("not a fog recovery kit")
	}
	rest := sealed[8:]
	salt, rest := rest[:16], rest[16:]
	gcm, err := kitAEAD(magic, passphrase, salt)
	if err != nil {
		return nil, err
	}
	if len(rest) < gcm.NonceSize() {
		return nil, fmt.Errorf("truncated recovery kit")
	}
	nonce, ct := rest[:gcm.NonceSize()], rest[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ct, []byte(magic))
	if err != nil {
		return nil, fmt.Errorf("wrong passphrase (or the kit is corrupt)")
	}
	return plain, nil
}

// kitAEAD derives the kit's AES-256-GCM key from the passphrase and salt.
// The magic selects the derivation: Argon2id for v2 (current), scrypt for v1
// (legacy, read only), so one magic byte decides how a kit is opened.
func kitAEAD(magic, passphrase string, salt []byte) (cipher.AEAD, error) {
	var key []byte
	switch magic {
	case kitMagicV2:
		key = argon2.IDKey([]byte(passphrase), salt, kitArgonTime, kitArgonMemKiB, kitArgonThread, 32)
	case kitMagicV1:
		var err error
		if key, err = scrypt.Key([]byte(passphrase), salt, 1<<15, 8, 1, 32); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unknown recovery kit format")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func addToTar(tw *tar.Writer, src, name string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: info.Size(), ModTime: info.ModTime()}); err != nil {
		return err
	}
	_, err = io.Copy(tw, f)
	return err
}
