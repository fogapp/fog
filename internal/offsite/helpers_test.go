package offsite

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"

	"gitea.lan/matthew/fog/internal/store"
	"gitea.lan/matthew/fog/internal/vault"
)

// offsiteStateOf is one subject's offsite state, as the pages show it.
func offsiteStateOf(st *store.Store, kind, subject string) string {
	states, _ := st.OffsiteStatesOf(kind)
	return states.Of(subject)
}

// Helpers the tests use to reach states production doesn't create yet:
// a ring with more than one root key, and sizes worked back from sealed
// lengths.

// CurrentKeyID is the id new objects are sealed under.
func (k *Keyring) CurrentKeyID() uint32 { return k.newest }

// BlobID reads content, digests it, and names its blob.
func (k *Keyring) BlobID(r io.Reader) (string, int64, error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return "", 0, err
	}
	id, err := k.BlobIDForDigest(hex.EncodeToString(h.Sum(nil)))
	return id, n, err
}

// Rotate adds a fresh root key that new objects are sealed under. Older
// keys stay in the ring so history keeps opening; key 1 always stays
// because blob ids depend on it.
func (l *Locker) Rotate(kr *Keyring, passphrase string, master vault.Cipher) error {
	root := make([]byte, rootKeySize)
	if _, err := rand.Read(root); err != nil {
		return err
	}
	kr.roots[kr.newest+1] = root
	nk := newKeyring(kr.LockerID, kr.roots)
	*kr = *nk
	return l.wrapAll(kr, passphrase, master)
}

// PlainSize inverts SealedSize for a well-formed object; ok is false for a
// length no sealed object can have.
func PlainSize(sealed int64) (n int64, ok bool) {
	body := sealed - int64(headerSize)
	if body < tagSize {
		return 0, false
	}
	full := int64(FrameSize + tagSize)
	frames := body / full
	rest := body % full
	if rest == 0 {
		return frames * FrameSize, true
	}
	if rest < tagSize {
		return 0, false
	}
	return frames*FrameSize + rest - tagSize, true
}
