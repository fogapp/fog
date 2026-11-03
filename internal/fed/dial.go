package fed

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/hpke"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"gitea.lan/matthew/fog/internal/store"
)

// Proven connections.
//
// A peer's stored address can outlive the peer: stale DNS, a recycled DHCP
// lease, a lapsed domain someone else registered. Signed answers stop
// whatever answers there from changing anything here, but a request's body
// and target would still be handed to it before any answer came back — a
// heartbeat carries the people and apps published to that peer, a share's
// blob path names a file. So no request is written to a connection until
// whatever is at the other end has proven, on that same connection, that it
// holds the peer's key: the first thing written to every new connection is
// a signed GET /fed/v1/hello, and the connection is handed to the request
// only if the answer is the peer's ANSWER, bound to that hello. A
// connection that can't prove it is closed with nothing else sent.
//
// Connections are pooled per peer — never shared between two peers, even
// at the same address — and the fed plane follows no redirects: a 3xx is an
// answer like any other, unsigned, and so proves nothing.

// HelloPath is the endpoint every new connection proves itself on.
const HelloPath = "/fed/v1/hello"

// maxHelloBody caps what a proof's answer may carry.
const maxHelloBody = 64 << 10

func noRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// peerConns is one peer's pool: a transport whose every connection is
// proven, and the two clients over it.
type peerConns struct {
	url, key         string
	tr               *http.Transport
	buffered, stream *http.Client
}

// clientFor is the client for one request to peer. stream: the body may be
// gigabytes either way, so no total timeout — the idle watchdog in do
// bounds it instead.
func (s *Service) clientFor(peer *store.FedPeer, stream bool) *http.Client {
	if s.HTTP != nil {
		// A test seam: an injected client keeps its own transport, but the
		// fed plane's rule on redirects holds there too.
		c := *s.HTTP
		c.CheckRedirect = noRedirects
		return &c
	}
	if peer.ID == 0 || peer.PublicKey == "" {
		// An address being paired with: there is no key to prove yet. The
		// invite secret it is sent is single-use and made for that address.
		return plainClients(stream)
	}
	s.connsMu.Lock()
	defer s.connsMu.Unlock()
	if s.conns == nil {
		s.conns = map[int64]*peerConns{}
	}
	pc := s.conns[peer.ID]
	if pc == nil || pc.url != peer.URL || pc.key != peer.PublicKey {
		if pc != nil {
			pc.tr.CloseIdleConnections()
		}
		pc = s.newPeerConns(*peer)
		s.conns[peer.ID] = pc
	}
	if stream {
		return pc.stream
	}
	return pc.buffered
}

func (s *Service) newPeerConns(peer store.FedPeer) *peerConns {
	dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	tr := &http.Transport{
		// No proxy: the proof must reach the peer itself, not a proxy's
		// CONNECT, and fed traffic is between two households directly.
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return s.prove(ctx, conn, &peer, "http", addr)
		},
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			raw, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			host, _, _ := net.SplitHostPort(addr)
			// HTTP/1.1 only: the proof is written as one, and the connection
			// carries the requests that follow it as one.
			tc := tls.Client(raw, &tls.Config{ServerName: host, NextProtos: []string{"http/1.1"}, MinVersion: tls.VersionTLS12})
			hctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			if err := tc.HandshakeContext(hctx); err != nil {
				raw.Close()
				return nil, err
			}
			return s.prove(ctx, tc, &peer, "https", addr)
		},
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: streamIdle,
	}
	return &peerConns{
		url: peer.URL, key: peer.PublicKey, tr: tr,
		buffered: &http.Client{Transport: tr, Timeout: 15 * time.Second, CheckRedirect: noRedirects},
		stream:   &http.Client{Transport: tr, CheckRedirect: noRedirects},
	}
}

// ErrUnproven: whatever answered at the peer's address could not prove it
// holds the peer's key, so nothing was sent to it.
var ErrUnproven = errors.New("the address did not prove it is the peer; nothing was sent")

// prove writes the connection proof on conn — an envelope with an empty
// body, signed — and reads the answer, returning conn only if it opens with
// this instance's key and its head is signed by the peer's key for that
// proof. A refusal the peer signed — "unknown peer" above all — comes back
// as a verified RemoteError, so the caller reads it like any other answer.
// The answer's head carries the peer's encryption key, bound to its
// identity: the signed refresh a pairing from before sealed bodies learns it
// from, and the check that it hasn't changed for one that has it.
func (s *Service) prove(ctx context.Context, conn net.Conn, peer *store.FedPeer, scheme, addr string) (net.Conn, error) {
	fail := func(err error) (net.Conn, error) {
		conn.Close()
		return nil, err
	}
	id, err := s.Identity()
	if err != nil {
		return fail(err)
	}
	pub, err := ParsePublicKey(peer.PublicKey)
	if err != nil {
		return fail(err)
	}
	base, err := url.Parse(peer.URL)
	if err != nil {
		return fail(err)
	}
	u := &url.URL{Scheme: scheme, Host: base.Host, Path: strings.TrimRight(base.Path, "/") + SealedPath, RawQuery: "to=" + url.QueryEscape(peer.InstanceID)}
	req, err := http.NewRequestWithContext(ctx, "POST", u.String(), nil)
	if err != nil {
		return fail(err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	id.SignRequest(req, nil)
	deadline := time.Now().Add(15 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	conn.SetDeadline(deadline)
	if err := req.Write(conn); err != nil {
		return fail(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		return fail(fmt.Errorf("%w: %v", ErrUnproven, err))
	}
	wire, err := io.ReadAll(io.LimitReader(resp.Body, maxHelloBody+1))
	resp.Body.Close()
	if err != nil {
		return fail(err)
	}
	conn.SetDeadline(time.Time{})
	if err := s.readProof(id, pub, peer, req, resp.StatusCode, wire); err != nil {
		return fail(err)
	}
	// The connection is handed on clean: nothing of a later answer may sit
	// in this reader, and an answer that closes the connection can't carry
	// the request it vouched for.
	if br.Buffered() > 0 || resp.Close {
		return fail(fmt.Errorf("%w: the proof's connection can't carry a request", ErrUnproven))
	}
	return conn, nil
}

// readProof judges a proof's answer: the envelope's 200, sealed to us,
// its head signed by the peer for this proof. A signed refusal is returned
// as a verified RemoteError; a good answer pins the peer's bound key.
func (s *Service) readProof(id *Identity, pub ed25519.PublicKey, peer *store.FedPeer, req *http.Request, status int, wire []byte) error {
	if status != http.StatusOK || len(wire) > maxHelloBody {
		return fmt.Errorf("%w: the answer is not a sealed envelope (%d)", ErrUnproven, status)
	}
	target, bound := req.URL.RequestURI(), BoundTo(req.Header)
	or, err := id.openStream(bytes.NewReader(wire), AnswerInfo("POST", target, bound.Instance, bound.Sig, peer.InstanceID))
	if err != nil {
		// Not sealed to us: a fog that doesn't seal, or not a fog at all.
		return fmt.Errorf("%w: %w: %v", ErrUnproven, ErrNoEncryption, err)
	}
	head, err := ReadAnswerHead(or, pub, target, bound)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnproven, err)
	}
	rest, err := io.ReadAll(newDigestReader(or, head.Digest, nil))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnproven, err)
	}
	if head.Status != http.StatusOK {
		re := newRemoteError(head.Status, rest)
		re.Verified = true
		return re
	}
	current := peer
	if fresh, err := s.Store.FedPeerByID(peer.ID); err == nil {
		current = fresh
	}
	return s.learnEncKey(current, head.EncKey, head.EncBinding)
}

// proveNow proves a fresh connection to peer and closes it: what learning a
// pairing's key from its signed proof takes, before anything is sealed to
// it.
func (s *Service) proveNow(ctx context.Context, peer *store.FedPeer) error {
	c := s.clientFor(peer, false)
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		// A test's injected transport, which proves nothing at dial: send
		// the proof as an ordinary request over it.
		id, err := s.Identity()
		if err != nil {
			return err
		}
		pub, err := ParsePublicKey(peer.PublicKey)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(peer.URL, "/")+SealedTarget(peer.InstanceID), nil)
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/octet-stream")
		id.SignRequest(req, nil)
		resp, err := c.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		wire, err := io.ReadAll(io.LimitReader(resp.Body, maxHelloBody+1))
		if err != nil {
			return err
		}
		return s.readProof(id, pub, peer, req, resp.StatusCode, wire)
	}
	u, err := url.Parse(peer.URL)
	if err != nil {
		return err
	}
	host := u.Host
	dial := tr.DialContext
	if u.Scheme == "https" {
		dial = tr.DialTLSContext
		if u.Port() == "" {
			host += ":443"
		}
	} else if u.Port() == "" {
		host += ":80"
	}
	conn, err := dial(ctx, "tcp", host)
	if err != nil {
		return err
	}
	return conn.Close()
}

var (
	plainOnce                  sync.Once
	plainBuffered, plainStream *http.Client
)

// plainClients are for an address with no key yet: bounded, and following
// no redirects.
func plainClients(stream bool) *http.Client {
	plainOnce.Do(func() {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.DialContext = (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext
		t.TLSHandshakeTimeout = 15 * time.Second
		t.ResponseHeaderTimeout = streamIdle
		plainBuffered = &http.Client{Transport: t, Timeout: 15 * time.Second, CheckRedirect: noRedirects}
		plainStream = &http.Client{Transport: t, CheckRedirect: noRedirects}
	})
	if stream {
		return plainStream
	}
	return plainBuffered
}

// SealRequest turns req — the inner request as fogd's handlers see it:
// method, inner target, headers — into the envelope it crosses in: a POST
// to SealedPath on the same host, its body the inner request sealed to the
// recipient (instance ID and encryption key), signed as id. For tools and
// tests that build a fed request by hand.
func SealRequest(id *Identity, req *http.Request, recipient string, to hpke.PublicKey, body []byte) error {
	inner := http.Header{}
	for k, vs := range req.Header {
		inner[k] = append([]string{}, vs...)
	}
	method, target := req.Method, req.URL.RequestURI()
	req.Method = "POST"
	req.URL.Path, req.URL.RawPath, req.URL.RawQuery = SealedPath, "", "to="+url.QueryEscape(recipient)
	req.Header = http.Header{"Content-Type": {"application/octet-stream"}}
	ts := uniqueStamp()
	wire, err := Seal(to, RequestInfo("POST", req.URL.RequestURI(), id.InstanceID, recipient, ts), EncodeInnerRequest(method, target, inner, body))
	if err != nil {
		return err
	}
	req.Body = io.NopCloser(bytes.NewReader(wire))
	req.ContentLength = int64(len(wire))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(wire)), nil }
	id.signRequestAt(req, wire, ts)
	return nil
}

// OpenAnswer opens an envelope's answer to the request resp answers, sent
// to id by responder, and — when pub is given — verifies its head. It
// returns the head and the inner body, checked against the head's digest.
func OpenAnswer(id *Identity, pub ed25519.PublicKey, responder string, resp *http.Response) (*AnswerHead, []byte, error) {
	if resp.StatusCode != http.StatusOK {
		return nil, nil, errNotEnvelope
	}
	target, b := resp.Request.URL.RequestURI(), BoundTo(resp.Request.Header)
	or, err := id.openStream(resp.Body, AnswerInfo("POST", target, b.Instance, b.Sig, responder))
	if err != nil {
		return nil, nil, err
	}
	head, err := ReadAnswerHead(or, pub, target, b)
	if err != nil {
		return nil, nil, err
	}
	body, err := io.ReadAll(newDigestReader(or, head.Digest, nil))
	return head, body, err
}
