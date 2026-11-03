package portfwd

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"testing"
	"time"
)

type acceptResult struct {
	conn net.Conn
	err  error
}

// scriptedListener plays back a fixed sequence of Accept results, then
// reports net.ErrClosed — the shape of a listener that hiccuped and was
// later closed by Reconcile.
type scriptedListener struct {
	results chan acceptResult
}

func (l *scriptedListener) Accept() (net.Conn, error) {
	r, ok := <-l.results
	if !ok {
		return nil, net.ErrClosed
	}
	return r.conn, r.err
}

func (l *scriptedListener) Close() error   { return nil }
func (l *scriptedListener) Addr() net.Addr { return &net.TCPAddr{} }

// A transient Accept error (EMFILE, ECONNABORTED) must not end the serve
// loop: the listener stays registered in f.listeners, so Reconcile would
// treat the port as covered and never restart it — the app's stable port
// would be dead until fogd restarted. serve has to ride the error out and
// keep accepting.
func TestServeSurvivesTransientAcceptError(t *testing.T) {
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
		io.Copy(c, c)
	}()

	const port = 12345
	f := &Forwarder{
		Log: slog.New(slog.NewTextHandler(os.Stderr, nil)),
		Resolve: func(context.Context, string, int) (string, error) {
			return backend.Addr().String(), nil
		},
	}
	f.mu.Lock()
	f.targets = map[int]target{port: {app: "echo", containerPort: 9}}
	f.mu.Unlock()

	ln := &scriptedListener{results: make(chan acceptResult, 2)}
	client, server := net.Pipe()
	defer client.Close()
	ln.results <- acceptResult{err: errors.New("accept tcp: too many open files")} // transient
	ln.results <- acceptResult{conn: server}
	close(ln.results)

	done := make(chan struct{})
	go func() {
		f.serve(ln, port)
		close(done)
	}()

	// The connection scripted AFTER the error must still be proxied.
	client.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := client.Write([]byte("ping")); err != nil {
		t.Fatalf("write through forwarder after transient accept error: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(client, buf); err != nil {
		t.Fatalf("read echo after transient accept error: %v", err)
	}
	if string(buf) != "ping" {
		t.Fatalf("got %q", buf)
	}

	// And net.ErrClosed (the scripted channel closing) must end the loop.
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return on net.ErrClosed")
	}
}
