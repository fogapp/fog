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

	"gitea.lan/matthew/fog/internal/store/storetest"
)

// The forwarder must proxy bytes to whatever the resolver currently points
// at — that per-connection resolution is what makes ports survive swaps.
func TestForwarderProxies(t *testing.T) {
	// Backend echo server.
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	go func() {
		for {
			c, err := backend.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				io.Copy(c, c)
			}(c)
		}
	}()

	st, err := storetest.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	app, err := st.CreateApp("echo")
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
		Resolve: func(_ context.Context, appName string, containerPort int) (string, error) {
			if appName != "echo" || containerPort != 9 {
				t.Errorf("resolve got %s:%d", appName, containerPort)
			}
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
		t.Fatalf("dial forwarded port: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "ping" {
		t.Fatalf("got %q", buf)
	}

	// Auth-gated first route must NOT get a listener.
	app2, _ := st.CreateApp("gated")
	st.CreateRevision(app2.ID, `{"source":{"type":"image","image":"x"},"routes":[{"port":9,"auth":true}]}`, "", "t")
	p2, _ := st.AllocateHostPort(app2.ID)
	f.Reconcile(context.Background())
	f.mu.Lock()
	_, has := f.listeners[p2]
	f.mu.Unlock()
	if has {
		t.Fatal("auth-gated route got a raw port listener")
	}

	// So must a LAN-only one: the forwarder goes around the check. An app
	// that held a port before the route went LAN-only loses its listener.
	app3, _ := st.CreateApp("lan")
	st.CreateRevision(app3.ID, `{"source":{"type":"image","image":"x"},"routes":[{"port":9}]}`, "", "t")
	p3, _ := st.AllocateHostPort(app3.ID)
	f.Reconcile(context.Background())
	f.mu.Lock()
	_, has = f.listeners[p3]
	f.mu.Unlock()
	if !has {
		t.Fatal("an ordinary route got no listener")
	}
	st.CreateRevision(app3.ID, `{"source":{"type":"image","image":"x"},"routes":[{"port":9,"lanOnly":true}]}`, "", "t")
	f.Reconcile(context.Background())
	f.mu.Lock()
	_, has = f.listeners[p3]
	f.mu.Unlock()
	if has {
		t.Fatal("a LAN-only route kept a raw port listener")
	}
}
