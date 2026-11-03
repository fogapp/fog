package offsite

import (
	"context"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"gitea.lan/matthew/fog/internal/vault"
	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
)

// The locker's one plaintext object, locker.json, and the key ring it
// unlocks.
//
// A locker holds one or more root keys, each wrapped twice: under a key
// derived from the operator's passphrase (Argon2id), and under fog's
// vault master key. Nightly runs open the master wrap and never pay the
// KDF; a fresh machine has only the passphrase and pays it once. Changing
// the passphrase rewraps a few hundred bytes; the data is never touched.
// Every sealed object names the root key it was written under, so a
// rotated locker keeps reading its history.

// KDFParams are the Argon2id costs. The defaults are the ones plakar
// adopted after its 2025 audit — about a second of work and 256 MiB of
// memory, paid at setup, restore, and passphrase change only.
type KDFParams struct {
	Time    uint32 `json:"time"`
	Memory  uint32 `json:"memory_kib"`
	Threads uint8  `json:"threads"`
}

// DefaultKDF is what new lockers use. A var so tests can shrink it.
var DefaultKDF = KDFParams{Time: 4, Memory: 256 * 1024, Threads: 1}

// KDF bounds. locker.json is unsealed and the operator does not sign it, so
// anyone with bucket-write can rewrite its KDF cost. Derivation runs before
// any authentication can happen (it produces the key the authentication
// needs), so unbounded memory or time is an OOM or an unbounded hang on
// every sync, on the setup-screen recovery, and in fog-locker — triggered by
// a stolen bucket key alone. The derivation refuses anything past these
// maxima rather than attempting it. The ceilings sit well above DefaultKDF
// so a future raise stays openable; the KDF inputs (salt, time, memory,
// threads) are themselves authenticated by the wrap, since any change to
// them yields a different KEK and the unwrap then fails.
const (
	maxKDFTime      = 16
	maxKDFMemoryKiB = 512 * 1024 // 512 MiB — twice DefaultKDF, what a home server tolerates; a bucket writer cannot force a 2 GiB allocation
	maxKDFThreads   = 16
)

// kdfMu serialises Argon2id derivations across the process, so a flurry of
// opens (several targets, the setup screen, fog-locker) cannot stack several
// 512 MiB allocations at once into an OOM. kekCache remembers a KEK already
// derived for a (salt, params, passphrase), so repeated opens of the same
// locker.json — including repeated wrong-passphrase attempts a bucket writer
// might provoke — pay the cost once.
var (
	kdfMu    sync.Mutex // guards kekCache
	kekCache = map[string][]byte{}
)

// heavy is the one slot for memory-heavy key work in this process: a
// locker's KDF (up to 512 MiB) or opening a recovery kit (its bytes, its
// plaintext and a 256 MiB derivation). Ten recovery requests at once used
// to run ten of these side by side into gigabytes; now they queue.
var heavy = make(chan struct{}, 1)

// heavyHook, when set (tests), is told +1 when the slot is taken and -1
// when it is given back.
var heavyHook func(int)

// takeHeavy waits for the slot, or for ctx.
func takeHeavy(ctx context.Context) (release func(), err error) {
	select {
	case heavy <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if heavyHook != nil {
		heavyHook(+1)
	}
	return func() {
		if heavyHook != nil {
			heavyHook(-1)
		}
		<-heavy
	}, nil
}

// Locker is locker.json: what a reader needs to open everything else.
type Locker struct {
	Format    string    `json:"format"`
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt string    `json:"created_at"`
	KDF       KDFParams `json:"kdf"`
	Salt      string    `json:"salt"` // base64, for the passphrase KEK
	Keys      []KeyWrap `json:"keys"`
	// WrapFormat records that every wrap is bound to this locker's id
	// (WrapFormatBound). Once set, readers refuse the legacy, unbound wrap
	// forms for this locker, so a bucket writer cannot splice an old-format
	// wrap back in by deleting the bound entries. fog also keeps the fact in
	// its own store, so deleting this field is caught too.
	WrapFormat int `json:"wrap_format,omitempty"`
}

// WrapFormatBound is the WrapFormat of a locker whose wraps all name it.
const WrapFormatBound = 2

// Bound reports whether the locker's wraps are all locker-bound, so legacy
// unbound wraps must be refused.
func (l *Locker) Bound() bool { return l.WrapFormat >= WrapFormatBound }

// KeyWrap is one root key sealed under one wrapper.
type KeyWrap struct {
	ID        uint32 `json:"id"`
	WrappedBy string `json:"wrapped_by"` // passphrase | master
	// Ciphertext: for passphrase wraps, base64(nonce ‖ XChaCha20-Poly1305
	// ciphertext); for master wraps, the vault's own ciphertext string.
	Ciphertext string `json:"ciphertext"`
}

// Keyring is an opened locker: the root keys, and the data and id keys
// derived from them.
type Keyring struct {
	LockerID string
	roots    map[uint32][]byte
	data     map[uint32][]byte
	idKey    []byte
	newest   uint32
}

var ErrWrongPassphrase = errors.New("wrong passphrase (or the locker is corrupt)")

const (
	rootKeySize = 32
	saltSize    = 16
	lockerIDLen = 16
)

func derive(root []byte, purpose string) []byte {
	out, err := hkdf.Key(sha256.New, root, nil, Format+" "+purpose, 32)
	if err != nil {
		panic(err) // fixed-size inputs; cannot fail
	}
	return out
}

func newKeyring(id string, roots map[uint32][]byte) *Keyring {
	k := &Keyring{LockerID: id, roots: roots, data: map[uint32][]byte{}}
	for kid, root := range roots {
		k.data[kid] = derive(root, "data key")
		if kid > k.newest {
			k.newest = kid
		}
	}
	// Blob ids come from key 1 for the life of the locker so rotation
	// never changes which blob a file maps to; a rotated locker keeps
	// key 1 for that purpose.
	if root, ok := roots[1]; ok {
		k.idKey = derive(root, "blob id")
	}
	return k
}

func (k *Keyring) current() ([]byte, uint32) { return k.data[k.newest], k.newest }
func (k *Keyring) key(id uint32) []byte      { return k.data[id] }

// BlobIDForDigest names a file's content from its SHA-256: hex
// HMAC-SHA256 of "sha256:<hex>" under the locker's id key.
// Content-addressed (the same bytes give the same id, so a file in seven
// snapshots is stored once), keyed (the provider cannot confirm a known
// file's presence from its name), and computable from the Files snapshot
// manifest — which already carries every file's digest — so a nightly
// sync never re-reads what has not changed.
func (k *Keyring) BlobIDForDigest(sha256hex string) (string, error) {
	if k.idKey == nil {
		return "", errors.New("locker has no id key (key 1 missing)")
	}
	if len(sha256hex) != 64 {
		return "", fmt.Errorf("not a sha256 hex digest: %q", sha256hex)
	}
	h := hmac.New(sha256.New, k.idKey)
	h.Write([]byte("sha256:" + strings.ToLower(sha256hex)))
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ---- create / open / rewrap ----

// CreateLocker mints a new locker under a fresh root key, wrapped for the
// passphrase and, when master is non-nil, for the vault.
func CreateLocker(name, passphrase string, master vault.Cipher) (*Locker, *Keyring, error) {
	if passphrase == "" {
		return nil, nil, errors.New("a passphrase is required")
	}
	id := make([]byte, lockerIDLen)
	if _, err := rand.Read(id); err != nil {
		return nil, nil, err
	}
	salt := make([]byte, saltSize)
	if _, err := rand.Read(salt); err != nil {
		return nil, nil, err
	}
	root := make([]byte, rootKeySize)
	if _, err := rand.Read(root); err != nil {
		return nil, nil, err
	}
	l := &Locker{
		Format:    Format,
		ID:        hex.EncodeToString(id),
		Name:      name,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		KDF:       DefaultKDF,
		Salt:      base64.StdEncoding.EncodeToString(salt),
	}
	kr := newKeyring(l.ID, map[uint32][]byte{1: root})
	if err := l.wrapAll(kr, passphrase, master); err != nil {
		return nil, nil, err
	}
	return l, kr, nil
}

// ParseLocker decodes locker.json and checks it is one.
func ParseLocker(data []byte) (*Locker, error) {
	var l Locker
	if err := json.Unmarshal(data, &l); err != nil {
		return nil, fmt.Errorf("locker.json is not valid JSON: %w", err)
	}
	if l.Format != Format {
		return nil, fmt.Errorf("locker.json declares format %q, this fog speaks %q", l.Format, Format)
	}
	if len(l.ID) != 2*lockerIDLen || len(l.Keys) == 0 {
		return nil, errors.New("locker.json is incomplete")
	}
	return &l, nil
}

// Marshal renders locker.json.
func (l *Locker) Marshal() ([]byte, error) {
	sort.SliceStable(l.Keys, func(i, j int) bool {
		if l.Keys[i].ID != l.Keys[j].ID {
			return l.Keys[i].ID < l.Keys[j].ID
		}
		return l.Keys[i].WrappedBy < l.Keys[j].WrappedBy
	})
	return json.MarshalIndent(l, "", "  ")
}

func (l *Locker) kek(passphrase string) ([]byte, error) {
	return l.kekCtx(context.Background(), passphrase)
}

// kekCtx is kek giving up when ctx ends while it waits for the heavy slot:
// a request that has gone away does not keep its place in the queue.
func (l *Locker) kekCtx(ctx context.Context, passphrase string) ([]byte, error) {
	salt, err := base64.StdEncoding.DecodeString(l.Salt)
	if err != nil || len(salt) != saltSize {
		return nil, errors.New("locker.json has a bad salt")
	}
	p := l.KDF
	if p.Time == 0 || p.Memory == 0 || p.Threads == 0 {
		return nil, errors.New("locker.json has bad KDF parameters")
	}
	// Bound before deriving: the derivation itself is the DoS if these are
	// unbounded, so an out-of-range cost is refused, never attempted.
	if p.Time > maxKDFTime || p.Memory > maxKDFMemoryKiB || p.Threads > maxKDFThreads {
		return nil, fmt.Errorf("locker.json asks for a key derivation beyond safe limits (time %d, memory %d KiB, threads %d) — refusing; the locker.json may have been tampered with", p.Time, p.Memory, p.Threads)
	}
	// Serialise and cache: one 512 MiB derivation at a time (no stacked OOM),
	// and the same (salt, params, passphrase) derived only once per process.
	cacheKey := fmt.Sprintf("%x|%d|%d|%d|%s", salt, p.Time, p.Memory, p.Threads, passphrase)
	kdfMu.Lock()
	kek, ok := kekCache[cacheKey]
	kdfMu.Unlock()
	if ok {
		return kek, nil
	}
	release, err := takeHeavy(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	kdfMu.Lock()
	if kek, ok := kekCache[cacheKey]; ok {
		kdfMu.Unlock()
		return kek, nil
	}
	kdfMu.Unlock()
	kek = argon2.IDKey([]byte(passphrase), salt, p.Time, p.Memory, p.Threads, 32)
	kdfMu.Lock()
	kekCache[cacheKey] = kek
	kdfMu.Unlock()
	return kek, nil
}

// wrapAAD binds a passphrase wrap to its key id and its locker id, so a wrap
// lifted from another locker (same vault or passphrase) cannot be spliced in
// under a matching id: the associated data would not match and the open
// fails. legacyWrapAAD is the pre-binding form, accepted on read so lockers
// written before this still open (and upgrade on the next rewrap).
func wrapAAD(id uint32, lockerID string) []byte {
	return []byte(fmt.Sprintf("%s key %d locker %s", Format, id, lockerID))
}
func legacyWrapAAD(id uint32) []byte { return []byte(fmt.Sprintf("%s key %d", Format, id)) }

func wrapWithKEK(kek []byte, id uint32, lockerID string, root []byte) (string, error) {
	aead, err := chacha20poly1305.NewX(kek)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ct := aead.Seal(nonce, nonce, root, wrapAAD(id, lockerID))
	return base64.StdEncoding.EncodeToString(ct), nil
}

func unwrapWithKEK(kek []byte, id uint32, lockerID, ciphertext string, allowLegacy bool) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return nil, err
	}
	aead, err := chacha20poly1305.NewX(kek)
	if err != nil {
		return nil, err
	}
	if len(raw) < aead.NonceSize() {
		return nil, errors.New("wrapped key too short")
	}
	nonce, body := raw[:aead.NonceSize()], raw[aead.NonceSize():]
	if root, err := aead.Open(nil, nonce, body, wrapAAD(id, lockerID)); err == nil {
		return root, nil
	}
	// A locker written before the binding: accept the legacy AAD so it still
	// opens; the next rewrap upgrades it.
	if !allowLegacy {
		return nil, ErrWrongPassphrase
	}
	if root, err := aead.Open(nil, nonce, body, legacyWrapAAD(id)); err == nil {
		return root, nil
	}
	return nil, ErrWrongPassphrase
}

// wrapAll (re)writes every wrap for every root key in kr.
func (l *Locker) wrapAll(kr *Keyring, passphrase string, master vault.Cipher) error {
	var keys []KeyWrap
	var kek []byte
	if passphrase != "" {
		var err error
		if kek, err = l.kek(passphrase); err != nil {
			return err
		}
	}
	for id, root := range kr.roots {
		if kek != nil {
			ct, err := wrapWithKEK(kek, id, l.ID, root)
			if err != nil {
				return err
			}
			keys = append(keys, KeyWrap{ID: id, WrappedBy: "passphrase", Ciphertext: ct})
		}
		if master != nil {
			// Bind the master wrap to this locker: a wrap lifted from another
			// locker (same vault) names a different locker id and is refused.
			ct, err := master.Encrypt(fmt.Sprintf("%s:%d:%s", l.ID, id, hex.EncodeToString(root)))
			if err != nil {
				return err
			}
			keys = append(keys, KeyWrap{ID: id, WrappedBy: "master", Ciphertext: ct})
		}
	}
	if len(keys) == 0 {
		return errors.New("nothing to wrap the keys with")
	}
	l.Keys = keys
	l.WrapFormat = WrapFormatBound // everything just written is locker-bound
	return nil
}

// OpenWithPassphrase unlocks the ring with the operator's passphrase —
// the fresh-machine door. Pays the KDF once.
func (l *Locker) OpenWithPassphrase(passphrase string) (*Keyring, error) {
	return l.OpenWithPassphraseCtx(context.Background(), passphrase)
}

// OpenWithPassphraseCtx is OpenWithPassphrase bounded by ctx.
func (l *Locker) OpenWithPassphraseCtx(ctx context.Context, passphrase string) (*Keyring, error) {
	kek, err := l.kekCtx(ctx, passphrase)
	if err != nil {
		return nil, err
	}
	roots := map[uint32][]byte{}
	for _, w := range l.Keys {
		if w.WrappedBy != "passphrase" {
			continue
		}
		root, err := unwrapWithKEK(kek, w.ID, l.ID, w.Ciphertext, !l.Bound())
		if err != nil {
			return nil, err
		}
		roots[w.ID] = root
	}
	if len(roots) == 0 {
		return nil, errors.New("locker has no passphrase-wrapped keys")
	}
	return newKeyring(l.ID, roots), nil
}

// OpenWithMaster unlocks the ring with the vault — the nightly door.
// ErrNoMasterWrap means this locker was never wrapped for this vault
// (a locker inherited from another machine); rewrap it with the
// passphrase first.
func (l *Locker) OpenWithMaster(master vault.Cipher) (*Keyring, error) {
	roots := map[uint32][]byte{}
	for _, w := range l.Keys {
		if root, ok := unwrapMaster(master, l.ID, w, !l.Bound()); ok {
			roots[w.ID] = root
		}
	}
	if len(roots) == 0 {
		return nil, ErrNoMasterWrap
	}
	return newKeyring(l.ID, roots), nil
}

// unwrapMaster opens one vault wrap; false means it is not a master wrap,
// was made by a different vault, or belongs to another locker. A wrap bound
// to a locker id ("<locker>:<id>:<hex>") must name this locker, so a wrap
// spliced in from another locker is refused; a legacy wrap ("<id>:<hex>",
// written before binding) is accepted and upgrades on the next rewrap.
func unwrapMaster(master vault.Cipher, lockerID string, w KeyWrap, allowLegacy bool) ([]byte, bool) {
	if w.WrappedBy != "master" {
		return nil, false
	}
	plain, err := master.Decrypt(w.Ciphertext)
	if err != nil {
		return nil, false
	}
	var id uint32
	var hexRoot string
	if wl, rest, ok := strings.Cut(plain, ":"); ok && !isUint(wl) {
		// Bound form "<lockerID>:<id>:<hex>": the first field is the locker id.
		if wl != lockerID {
			return nil, false
		}
		plain = rest
	} else if !allowLegacy {
		// Legacy "<id>:<hex>" names no locker; once a locker is bound it is
		// exactly what a splice would look like, so it is refused.
		return nil, false
	}
	if _, err := fmt.Sscanf(plain, "%d:%s", &id, &hexRoot); err != nil || id != w.ID {
		return nil, false
	}
	root, err := hex.DecodeString(hexRoot)
	if err != nil || len(root) != rootKeySize {
		return nil, false
	}
	return root, true
}

// isUint reports whether s is all digits (a legacy "<id>:…" first field).
func isUint(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

var ErrNoMasterWrap = errors.New("locker is not wrapped for this machine's vault")

// Rewrap replaces every wrap: a new passphrase, a (possibly new) vault,
// or both. The root keys and every sealed object stay as they are.
func (l *Locker) Rewrap(kr *Keyring, passphrase string, master vault.Cipher) error {
	if kr.LockerID != l.ID {
		return errors.New("keyring belongs to a different locker")
	}
	if passphrase != "" {
		// A new passphrase gets a new salt; the old KEK must not remain
		// derivable from the file.
		salt := make([]byte, saltSize)
		if _, err := rand.Read(salt); err != nil {
			return err
		}
		l.Salt = base64.StdEncoding.EncodeToString(salt)
		l.KDF = DefaultKDF
	}
	return l.wrapAll(kr, passphrase, master)
}
