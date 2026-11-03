package tunnel

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/store"
	"gitea.lan/matthew/fog/internal/vault"
)

// Stop and Start while the loop sleeps in its restart backoff must leave
// one supervisor, not two: the sleeping loop carries on, and no second
// loop starts a second cloudflared beside it.
func TestStopStartDuringBackoffKeepsOneLoop(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	vlt, err := vault.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ct, _ := vlt.Encrypt("tok")
	if err := st.SetSecret(0, SecretToken, ct); err != nil {
		t.Fatal(err)
	}
	// The first run crashes at once, putting the loop into backoff; every
	// later run stays up. Each run leaves a line in the log.
	runs := filepath.Join(dir, "runs")
	bin := filepath.Join(dir, "cloudflared")
	script := "#!/bin/sh\necho run >> " + runs + "\n[ $(wc -l < " + runs + ") -eq 1 ] && exit 1\nexec sleep 30\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	m := &Manager{Store: st, Vault: vlt, BinPath: bin, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	t.Cleanup(m.Stop)

	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); m.Status().Restarts == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("first run never crashed")
		}
	}
	m.Stop()
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	// Past the whole backoff: a second loop would have started its own run.
	time.Sleep(3 * time.Second)
	b, _ := os.ReadFile(runs)
	if n := strings.Count(string(b), "run"); n != 2 {
		t.Fatalf("cloudflared started %d times, want 2 (one crash, one supervised run)", n)
	}
}
