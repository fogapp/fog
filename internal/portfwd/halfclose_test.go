package portfwd

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/store/storetest"
)

// A client that finishes sending and half-closes must still receive the whole
// response. This is the exact shape of an HTTP request against a forwarded
// port, and the previous implementation — which returned as soon as EITHER
// direction finished — tore the pair down the moment the client stopped
// writing, truncating whatever was still being sent back.
func TestForwarderDeliversResponseAfterClientHalfClose(t *testing.T) {
	const payload = 512 * 1024 // big enough to outlast the teardown race

	// Backend: read until EOF, then send a large reply. It cannot start
	// replying until the client has half-closed, which is the point.
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	go func() {
		c, err := backend.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		io.Copy(io.Discard, c) // returns when the client half-closes
		c.Write(make([]byte, payload))
	}()

	st, err := storetest.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	app, err := st.CreateApp("late")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateRevision(app.ID, `{"source":{"type":"image","image":"x"},"routes":[{"port":9}]}`, "", "t"); err != nil {
		t.Fatal(err)
	}
	port, err := st.AllocateHostPort(app.ID)
	if err != nil {
		t.Fatal(err)
	}

	f := &Forwarder{
		Store: st,
		Log:   slog.New(slog.NewTextHandler(os.Stderr, nil)),
		Resolve: func(context.Context, string, int) (string, error) {
			return backend.Addr().String(), nil
		},
	}
	f.Reconcile(context.Background())
	defer func() {
		f.mu.Lock()
		for _, ln := range f.listeners {
			ln.Close()
		}
		f.mu.Unlock()
	}()

	conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("request")); err != nil {
		t.Fatal(err)
	}
	// Signal "done sending" — the browser's behaviour at the end of a request.
	if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}

	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	got, err := io.Copy(io.Discard, conn)
	if err != nil {
		t.Fatalf("reading the response: %v", err)
	}
	if got != payload {
		t.Errorf("received %d bytes of a %d byte response — the response was truncated", got, payload)
	}
}
