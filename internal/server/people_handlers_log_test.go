package server

import (
	"bytes"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"gitea.lan/matthew/fog/internal/config"
	"gitea.lan/matthew/fog/internal/store"
	"gitea.lan/matthew/fog/internal/vault"
)

// lockedBuffer is a log destination safe for the server's goroutines.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestSecretsInPathsNeverReachTheLog: every /api mutation was logged with
// its raw path, so an invite's secret (/api/join/{secret}) sat in the log —
// whose lines reach feedback reports sent to paired households. The log
// names the route pattern instead, and a path no route matches is cut short.
func TestSecretsInPathsNeverReachTheLog(t *testing.T) {
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
	logs := &lockedBuffer{}
	srv := newServerForTest(config.Config{DataDir: dir}, st, Deps{Vault: vlt},
		slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	h := srv.Handler()
	admin := setup(t, h)

	w := do(t, h, "POST", "/api/people/invites", `{"role":"member"}`, withCookie(admin))
	invite := decode(t, w)["path"].(string) // /join/<secret>
	secret := invite[strings.LastIndex(invite, "/")+1:]
	const guessed = "notarealsecretbutsecretshaped0123456789"
	for _, path := range []string{"/api/join/" + secret, "/api/join/" + guessed} {
		do(t, h, "GET", path, "")
		do(t, h, "POST", path, `{"handle":"newbie","password":"a long password here"}`)
	}
	do(t, h, "DELETE", "/api/nowhere/"+guessed+"/deeper", "", withCookie(admin)) // no route at all
	do(t, h, "GET", "/s/"+guessed, "")
	do(t, h, "POST", "/hooks/chat/"+guessed, `{}`)

	got := logs.String()
	for _, s := range []string{secret, guessed} {
		if strings.Contains(got, s) {
			t.Fatalf("a secret from a path reached the log:\n%s", got)
		}
	}
	if !strings.Contains(got, "/api/join/{secret}") {
		t.Fatalf("the join was not logged by its route:\n%s", got)
	}
}
