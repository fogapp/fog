// Package vault encrypts secret values at rest with AES-256-GCM. The master
// key lives at <datadir>/master.key (0600) and is part of the recovery kit —
// losing it means losing every stored secret, so Phase 3's recovery-kit export
// treats it as first-class backup material.
package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gitea.lan/matthew/fog/internal/fsx"
)

type Vault struct {
	aead cipher.AEAD
}

// Open loads the master key, generating one on first run.
func Open(dataDir string) (*Vault, error) {
	path := filepath.Join(dataDir, "master.key")
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		switch err := writeKeyExclusive(path, []byte(hex.EncodeToString(key)+"\n")); {
		case err == nil:
			raw = []byte(hex.EncodeToString(key))
		case errors.Is(err, os.ErrExist):
			// Another process minted the key first. Its key is the vault's key
			// now — using ours would orphan every secret it already stored.
			raw, err = os.ReadFile(path)
			if err != nil {
				return nil, err
			}
		default:
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("master.key is corrupt: expected 64 hex chars")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Vault{aead: aead}, nil
}

// writeKeyExclusive lands the key at path atomically and only if no key
// exists: temp file in the same directory, fsync, then link into place. A
// crash mid-write can never leave a truncated master.key (every secret ever
// stored would be unreadable), and of two racing first runs exactly one wins —
// the loser sees os.ErrExist. An existing key is never overwritten.
func writeKeyExclusive(path string, data []byte) error {
	// Link, not rename: rename would silently replace an existing key.
	return fsx.WriteFileExclusive(path, data, 0o600)
}

func (v *Vault) Encrypt(plaintext string) (string, error) {
	nonce := make([]byte, v.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ct := v.aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(ct), nil
}

func (v *Vault) Decrypt(encoded string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", err
	}
	ns := v.aead.NonceSize()
	if len(raw) < ns {
		return "", errors.New("ciphertext too short")
	}
	pt, err := v.aead.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}
