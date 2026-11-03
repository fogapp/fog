// Package portfwd gives every routed app a stable host port by proxying TCP
// inside fogd: listen on the app's allocated port, dial its current
// container per connection. Ports stay stable across deploys and blue-green
// swaps (the container is re-resolved every time), nothing extra appears in
// compose files, and Cloudflare Tunnel routes can simply target
// localhost:<port>.
package portfwd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/dockerx"
	"gitea.lan/matthew/fog/internal/loop"
	"gitea.lan/matthew/fog/internal/store"
)

type target struct {
	app           string
	containerPort int
}

type Forwarder struct {
	Store  *store.Store
	Docker *dockerx.Client
	Log    *slog.Logger
	// Resolve maps an app name to a dialable address; defaults to looking up
	// the running main container's IP on the fog network. Overridable in
	// tests.
	Resolve func(ctx context.Context, app string, containerPort int) (string, error)

	mu        sync.Mutex
	listeners map[int]net.Listener
	targets   map[int]target
	ipCache   sync.Map // app -> cachedIP
}

type cachedIP struct {
	ip string
	at time.Time
}

func (f *Forwarder) resolve(ctx context.Context, app string, port int) (string, error) {
	if f.Resolve != nil {
		return f.Resolve(ctx, app, port)
	}
	if c, ok := f.ipCache.Load(app); ok {
		if e := c.(cachedIP); time.Since(e.at) < 5*time.Second {
			return net.JoinHostPort(e.ip, strconv.Itoa(port)), nil
		}
	}
	containers, err := f.Docker.ListByLabel(ctx, "fog.app="+app)
	if err != nil {
		return "", err
	}
	if ip := apps.MainIP(containers, app); ip != "" {
		f.ipCache.Store(app, cachedIP{ip: ip, at: time.Now()})
		return net.JoinHostPort(ip, strconv.Itoa(port)), nil
	}
	return "", fmt.Errorf("no running container for %s", app)
}

// Reconcile aligns listeners with the store: every app with a host port and
// an un-gated first route gets one.
func (f *Forwarder) Reconcile(ctx context.Context) {
	desired := map[int]target{}
	// Every app's spec in one query: this runs on every app change and on a
	// timer, and a LatestSpec per app was a round-trip each time.
	list, err := apps.WithSpecs(f.Store)
	if err != nil {
		return
	}
	for _, a := range list {
		if a.HostPort == 0 {
			continue
		}
		spec := a.Spec
		// Gated and LAN-only routes are only safe behind Caddy: a raw port
		// would bypass the login gate, or answer the internet.
		if spec == nil || !spec.DirectPort() {
			continue
		}
		desired[a.HostPort] = target{app: a.Name, containerPort: spec.Routes[0].Port}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listeners == nil {
		f.listeners = map[int]net.Listener{}
		f.targets = map[int]target{}
	}
	for port, ln := range f.listeners {
		if _, ok := desired[port]; !ok {
			ln.Close()
			delete(f.listeners, port)
			delete(f.targets, port)
			f.Log.Info("port forward closed", "port", port)
		}
	}
	for port, t := range desired {
		f.targets[port] = t // retarget existing listeners in place
		if _, ok := f.listeners[port]; ok {
			continue
		}
		ln, err := (&net.ListenConfig{KeepAliveConfig: keepAlive}).Listen(context.Background(), "tcp", ":"+strconv.Itoa(port))
		if err != nil {
			f.Log.Warn("port forward listen failed", "port", port, "err", err)
			continue
		}
		f.listeners[port] = ln
		f.Log.Info("port forward open", "port", port, "app", t.app)
		go f.serve(ln, port)
	}
}

func (f *Forwarder) serve(ln net.Listener, port int) {
	// Only a closed listener ends this loop. A transient Accept error (EMFILE
	// under fd pressure, ECONNABORTED) must not: the listener would stay in
	// f.listeners, Reconcile would keep seeing the port as covered, and the
	// app's stable port would be silently dead until fogd restarted. So on
	// anything but net.ErrClosed we log, back off briefly, and try again —
	// the same shape as net/http's Serve loop.
	delay := 5 * time.Millisecond
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return // listener closed by reconcile
			}
			f.Log.Warn("port forward accept", "port", port, "err", err)
			time.Sleep(delay)
			if delay *= 2; delay > time.Second {
				delay = time.Second
			}
			continue
		}
		delay = 5 * time.Millisecond
		go f.handle(conn, port)
	}
}

func (f *Forwarder) handle(conn net.Conn, port int) {
	defer conn.Close()
	f.mu.Lock()
	t, ok := f.targets[port]
	f.mu.Unlock()
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	addr, err := f.resolve(ctx, t.app, t.containerPort)
	cancel()
	if err != nil {
		return
	}
	d := net.Dialer{Timeout: 5 * time.Second, KeepAliveConfig: keepAlive}
	upstream, err := d.Dial("tcp", addr)
	if err != nil {
		f.ipCache.Delete(t.app) // stale IP after a swap — re-resolve next time
		return
	}
	defer upstream.Close()

	// Wait for BOTH directions, half-closing as each finishes.
	//
	// Returning after the first would truncate responses: a client that has
	// finished sending (an HTTP request body, or simply a browser half-closing
	// its write side) ends the client→upstream copy immediately, and tearing
	// the pair down there cuts off a download still in flight. Signalling EOF
	// with CloseWrite lets the upstream see the end of the request and finish
	// replying.
	//
	// The copies run on the bare *net.TCPConns so io.Copy reaches
	// TCPConn.ReadFrom, which splices kernel-side on Linux — media streams
	// never pass through a user-space buffer. A peer that vanishes without
	// a RST (power loss, a phone leaving the network) is caught by TCP
	// keepalive on both sockets (see keepAlive), which errors the blocked
	// read and frees the pair; a stream that is merely idle, like a paused
	// video, stays up as long as the peer still answers probes.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		io.Copy(upstream, conn)
		halfClose(upstream)
	}()
	go func() {
		defer wg.Done()
		io.Copy(conn, upstream)
		halfClose(conn)
	}()
	wg.Wait()
}

// keepAlive probes both sides of every forward: after a minute of silence,
// every 15s, giving up after 4 unanswered probes — a vanished peer is
// noticed in about two minutes instead of parking two goroutines and their
// FDs forever.
var keepAlive = net.KeepAliveConfig{
	Enable:   true,
	Idle:     time.Minute,
	Interval: 15 * time.Second,
	Count:    4,
}

// halfClose shuts down the write half so the peer sees EOF, leaving the read
// half open for whatever is still coming back.
func halfClose(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	}
}

// Loop reconciles periodically as a safety net; OnChange hooks cover the
// common cases immediately.
func (f *Forwarder) Loop(ctx context.Context, interval time.Duration) {
	loop.Every(ctx, 0, interval, func(ctx context.Context) { f.Reconcile(ctx) })
}
