package chat

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"gitea.lan/matthew/fog/internal/store"
	"gitea.lan/matthew/fog/internal/store/storetest"
	"gitea.lan/matthew/fog/internal/vault"
)

// The first burst of pushes all ask for the VAPID keypair at once; they must
// agree on one pair, and it must be the one stored.
func TestVAPIDKeysMintedOnce(t *testing.T) {
	dir := t.TempDir()
	st, err := storetest.Open(filepath.Join(dir, "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	v, err := vault.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	n := &Notifier{Svc: &Service{Store: st}, Vault: v}

	const callers = 8
	keys := make([]string, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pub, err := n.VAPIDPublicKey()
			if err != nil {
				t.Error(err)
			}
			keys[i] = pub
		}()
	}
	wg.Wait()
	for _, k := range keys {
		if k == "" || k != keys[0] {
			t.Fatalf("callers disagree on the key: %q", keys)
		}
	}
	// A fresh notifier (a restart) reads back the same pair.
	again, err := (&Notifier{Svc: &Service{Store: st}, Vault: v}).VAPIDPublicKey()
	if err != nil || again != keys[0] {
		t.Fatalf("stored key %q, minted %q (%v)", again, keys[0], err)
	}
}

// A push subscription's endpoint is the subscriber's to choose, so fog
// never dials one on its own network: refused when subscribing, and again
// when sending, on the address actually dialed.
func TestPushNeverDialsTheLAN(t *testing.T) {
	for _, u := range []string{"https://127.0.0.1/push", "https://10.1.2.3/x", "https://[::1]/x", "https://router.lan/x", "https://nas.local/x", "http://push.example/x"} {
		if CheckPublicURL(u, true) == nil {
			t.Errorf("%s accepted", u)
		}
	}
	if err := CheckPublicURL("https://push.example/abc", true); err != nil {
		t.Errorf("a public endpoint refused: %v", err)
	}
	n := &Notifier{}
	if err := n.defaultPush(store.PushSub{Endpoint: "https://192.168.1.1/push"}, nil); !errors.Is(err, ErrSubscriptionGone) {
		t.Errorf("sending to a LAN endpoint: %v", err)
	}
	if _, err := previewClient().Get("https://127.0.0.1:1/"); err == nil || !strings.Contains(err.Error(), "not one fog will fetch") {
		t.Errorf("the push client dialed loopback: %v", err)
	}
}
