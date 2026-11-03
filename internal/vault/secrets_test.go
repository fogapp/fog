package vault

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"gitea.lan/matthew/fog/internal/store"
)

// memSecrets is the store's contract in memory: ErrNotFound for a missing
// key, last write wins.
type memSecrets struct {
	mu sync.Mutex
	m  map[[2]any]string
}

func (s *memSecrets) GetSecret(appID int64, key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ct, ok := s.m[[2]any{appID, key}]
	if !ok {
		return "", store.ErrNotFound
	}
	return ct, nil
}

func (s *memSecrets) SetSecret(appID int64, key, ct string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[[2]any{appID, key}] = ct
	return nil
}

func newSecrets(t *testing.T) (Secrets, *memSecrets) {
	v, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mem := &memSecrets{m: map[[2]any]string{}}
	return Secrets{Store: mem, Vault: v}, mem
}

func TestSecretsPutGetEncryptsAtRest(t *testing.T) {
	s, mem := newSecrets(t)
	if _, err := s.Get(0, "k"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing key: %v", err)
	}
	if err := s.Put(7, "k", "hunter2"); err != nil {
		t.Fatal(err)
	}
	if ct, _ := mem.GetSecret(7, "k"); ct == "hunter2" {
		t.Fatal("stored in plaintext")
	}
	if got, err := s.Get(7, "k"); err != nil || got != "hunter2" {
		t.Fatalf("Get = %q, %v", got, err)
	}
}

func TestGetOrMintMintsOnceUnderConcurrency(t *testing.T) {
	s, _ := newSecrets(t)
	var mints atomic.Int32
	var wg sync.WaitGroup
	got := make([]string, 16)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := s.GetOrMint(0, "tok", func() (string, error) {
				return string(rune('a' + mints.Add(1))), nil
			})
			if err != nil {
				t.Error(err)
			}
			got[i] = v
		}()
	}
	wg.Wait()
	if mints.Load() != 1 {
		t.Errorf("minted %d times", mints.Load())
	}
	for _, v := range got {
		if v != got[0] {
			t.Fatalf("callers saw different secrets: %q", got)
		}
	}
}

func TestGetOrMintPassesGenError(t *testing.T) {
	s, mem := newSecrets(t)
	boom := errors.New("boom")
	if _, err := s.GetOrMint(0, "tok", func() (string, error) { return "", boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if _, err := mem.GetSecret(0, "tok"); !errors.Is(err, store.ErrNotFound) {
		t.Error("stored something after a failed mint")
	}
}
