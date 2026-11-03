// Package dockerx is a minimal Docker Engine API client over the unix socket:
// daemon info, container listing, inspection, logs and stats, container and
// image lifecycle, networks, and disk usage. It speaks the REST API directly
// rather than pulling the full Docker SDK dependency tree into the binary.
// Converge itself shells out to `docker compose`, which is the recovery
// path fog promises works without it.
package dockerx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"
)

const apiVersion = "v1.44" // ships with Docker 25+; conservative floor

type Client struct {
	http *http.Client
	// stream is the timeout-free variant for requests that hold the
	// connection open (log following). Shared, not built per call: every
	// http.Transport owns an idle-connection pool with a readLoop goroutine
	// per parked connection, so a transport abandoned after one request
	// leaks that connection — and its goroutine — for the process lifetime.
	stream *http.Client
	sock   string
}

func New() *Client {
	sock := "/var/run/docker.sock"
	if v := os.Getenv("DOCKER_SOCK"); v != "" {
		sock = v
	}
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}
	return &Client{
		sock: sock,
		http: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				DialContext:     dial,
				IdleConnTimeout: 90 * time.Second,
			},
		},
		stream: &http.Client{
			Transport: &http.Transport{
				DialContext:     dial,
				IdleConnTimeout: 90 * time.Second,
			},
		},
	}
}

// get is a GET on the general client, decoding the answer into out (nil
// to ignore it).
func (c *Client) get(ctx context.Context, path string, out any) error {
	return c.do(ctx, c.http, http.MethodGet, path, nil, out)
}

// do sends one request on client — c.http for quick questions, c.stream for
// anything the caller's context alone should bound — with body (if any) as
// JSON, and decodes a successful answer into out (nil to drain and discard
// it). A refusal comes back as an *APIError carrying the daemon's words.
func (c *Client) do(ctx context.Context, client *http.Client, method, path string, body, out any) error {
	resp, err := c.send(ctx, client, method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10)) // let the connection be reused
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// send is do for a caller that reads the answer itself, such as a log
// stream: on success the response is the caller's to close.
func (c *Client) send(ctx context.Context, client *http.Client, method, path string, body any) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://docker/"+apiVersion+path, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 300 {
		return resp, nil
	}
	defer resp.Body.Close()
	var msg struct {
		Message string `json:"message"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if json.Unmarshal(raw, &msg) != nil || msg.Message == "" {
		msg.Message = strings.TrimSpace(string(raw))
	}
	op := method + " " + path
	if i := strings.IndexByte(op, '?'); i >= 0 {
		op = op[:i]
	}
	return nil, &APIError{Op: op, Status: resp.StatusCode, Message: msg.Message}
}

// APIError is the daemon answering a request with something other than
// success: its status, and what it said about it.
type APIError struct {
	Op      string // "METHOD /path", query left off
	Status  int
	Message string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("docker API %s: HTTP %d", e.Op, e.Status)
	}
	return fmt.Sprintf("docker API %s: HTTP %d: %s", e.Op, e.Status, e.Message)
}

// hasStatus reports whether err is the daemon answering with one of codes.
func hasStatus(err error, codes ...int) bool {
	var ae *APIError
	return errors.As(err, &ae) && slices.Contains(codes, ae.Status)
}

// Available reports whether the Docker daemon is reachable.
func (c *Client) Available(ctx context.Context) bool {
	return c.get(ctx, "/_ping", nil) == nil
}

type Version struct {
	Version    string `json:"Version"`
	APIVersion string `json:"ApiVersion"`
	Os         string `json:"Os"`
	Arch       string `json:"Arch"`
}

func (c *Client) Version(ctx context.Context) (*Version, error) {
	v := &Version{}
	if err := c.get(ctx, "/version", v); err != nil {
		return nil, err
	}
	return v, nil
}

type Info struct {
	Containers        int    `json:"Containers"`
	ContainersRunning int    `json:"ContainersRunning"`
	Images            int    `json:"Images"`
	ServerVersion     string `json:"ServerVersion"`
	OperatingSystem   string `json:"OperatingSystem"`
	NCPU              int    `json:"NCPU"`
	MemTotal          int64  `json:"MemTotal"`
	// DockerRootDir is the daemon's data-root: where its writable world
	// lands on disk. The disk that holds it is the one Docker fills.
	DockerRootDir string `json:"DockerRootDir"`
}

func (c *Client) Info(ctx context.Context) (*Info, error) {
	i := &Info{}
	if err := c.get(ctx, "/info", i); err != nil {
		return nil, err
	}
	return i, nil
}
