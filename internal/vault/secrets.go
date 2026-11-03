package vault

import (
	"errors"
	"sync"

	"gitea.lan/matthew/fog/internal/store"
)

// SecretStore is where encrypted secrets rest: *store.Store. App id 0
// holds system secrets.
type SecretStore interface {
	GetSecret(appID int64, key string) (string, error)
	SetSecret(appID int64, key, ciphertext string) error
}

// Cipher is the vault's shape: fog's master-key AEAD over strings. *Vault
// is the one fogd runs; offsite also wraps locker keys with it, and its
// tests stand in other instances' masters.
type Cipher interface {
	Encrypt(plaintext string) (string, error)
	Decrypt(ciphertext string) (string, error)
}

// Secrets reads and writes vault-encrypted secrets, so callers deal in
// plaintext and never pair GetSecret with Decrypt by hand.
type Secrets struct {
	Store SecretStore
	Vault Cipher
}

// mintMu serialises every GetOrMint in the process. Mints are rare (once
// per secret, ever), and one lock means two components that build their
// own Secrets still can't both mint the same key and keep different values.
var mintMu sync.Mutex

// Get returns a secret's plaintext, or store.ErrNotFound.
func (s Secrets) Get(appID int64, key string) (string, error) {
	ct, err := s.Store.GetSecret(appID, key)
	if err != nil {
		return "", err
	}
	return s.Vault.Decrypt(ct)
}

// Put encrypts value and stores it, replacing any previous value.
func (s Secrets) Put(appID int64, key, value string) error {
	ct, err := s.Vault.Encrypt(value)
	if err != nil {
		return err
	}
	return s.Store.SetSecret(appID, key, ct)
}

// GetOrMint returns the secret, minting it with gen and storing it first
// if there is none yet. Concurrent first calls all get the one value that
// was stored.
func (s Secrets) GetOrMint(appID int64, key string, gen func() (string, error)) (string, error) {
	mintMu.Lock()
	defer mintMu.Unlock()
	v, err := s.Get(appID, key)
	if !errors.Is(err, store.ErrNotFound) {
		return v, err
	}
	if v, err = gen(); err != nil {
		return "", err
	}
	if err := s.Put(appID, key, v); err != nil {
		return "", err
	}
	return v, nil
}
