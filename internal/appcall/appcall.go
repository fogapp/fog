// Package appcall is fog calling its own apps: a contract endpoint under
// an app's /fog/* surface, reached on the fog network with the app's
// FOG_API_TOKEN as the bearer. Federation grants, the ledger, directory and
// Files notices and integrations all make the same call; they differ only in
// how they read a refusal, which a Caller takes as a Refusal.
package appcall

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/store"
)

// maxAnswer bounds what fog reads of an app's answer.
const maxAnswer = 1 << 20

// fanOutLimit is how many apps a fan-out calls at once: enough that one
// hung app costs the others nothing, few enough that a fleet of them doesn't
// open a connection per app in one burst.
const fanOutLimit = 8

// Resolver answers how to reach each app in list, in its order.
// apps.Engine.AppEndpoints is the real one; tests answer a local server.
type Resolver func(ctx context.Context, list []store.App) []apps.AppEndpoint

// Refusal turns an answer outside 2xx — resp's status, raw its body — into
// the error the call returns.
type Refusal func(resp *http.Response, raw []byte) error

// StatusRefusal reports the status line alone: what most contract calls
// can say about an app that answered no.
func StatusRefusal(resp *http.Response, _ []byte) error {
	return fmt.Errorf("app answered %s", resp.Status)
}

// ErrUnreachable is matched (errors.Is) by a call that never reached the
// app: it isn't running, has no address, or fog can't read its token.
var ErrUnreachable = errors.New("app unreachable")

type unreachable struct{ err error }

func (u *unreachable) Error() string        { return u.err.Error() }
func (u *unreachable) Unwrap() error        { return u.err }
func (u *unreachable) Is(target error) bool { return target == ErrUnreachable }

// Caller makes contract calls to local apps.
type Caller struct {
	resolve Resolver
	refusal Refusal
	client  *http.Client
}

// New is a Caller that reads refusals with StatusRefusal. It never follows
// a redirect: an app's answer is its answer. Followed, a 307 sent a contract
// call — the app's token, and whatever fog relays back to another app — to
// an address the app chose, which is a sealed app's way out.
func New(resolve Resolver) *Caller {
	return &Caller{resolve: resolve, refusal: StatusRefusal, client: &http.Client{
		Transport:     http.DefaultClient.Transport, // fogd's tracing, set in main
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// Refusing is a copy of c that reads refusals with r.
func (c *Caller) Refusing(r Refusal) *Caller {
	cp := *c
	cp.refusal = r
	return &cp
}

// Call makes one call to app: method on path, with body as JSON (nil sends
// none; []byte is sent as it is). It answers the app's raw body.
func (c *Caller) Call(ctx context.Context, app *store.App, method, path string, body any) ([]byte, error) {
	return c.target(*app, c.resolve(ctx, []store.App{*app})[0]).Call(ctx, method, path, body)
}

// noRedirects hands an app's redirect back as its answer. Its token goes with
// a contract call, and only to the address fog resolved.
var noRedirects = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// Open is a GET of path from app that answers the response as it is, every
// status included, for a relay to read and close: Call keeps only a body, and
// a relay has headers and a 304 to pass on. header goes with the request.
func (c *Caller) Open(ctx context.Context, app *store.App, path string, header http.Header) (*http.Response, error) {
	ep := c.resolve(ctx, []store.App{*app})[0]
	if ep.Err != nil {
		return nil, &unreachable{ep.Err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ep.Base+path, nil)
	if err != nil {
		return nil, err
	}
	for name, values := range header {
		req.Header[name] = values
	}
	req.Header.Set("Authorization", "Bearer "+ep.Token)
	return noRedirects.Do(req)
}

// Target is one app already resolved, as a fan-out hands it over.
type Target struct {
	App store.App
	c   *Caller
	ep  apps.AppEndpoint
}

func (c *Caller) target(app store.App, ep apps.AppEndpoint) *Target {
	return &Target{App: app, c: c, ep: ep}
}

// Call is Caller.Call against the resolved app.
func (t *Target) Call(ctx context.Context, method, path string, body any) ([]byte, error) {
	if t.ep.Err != nil {
		return nil, &unreachable{t.ep.Err}
	}
	var rdr io.Reader
	if body != nil {
		raw, ok := body.([]byte)
		if !ok {
			var err error
			if raw, err = json.Marshal(body); err != nil {
				return nil, err
			}
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, t.ep.Base+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+t.ep.Token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := t.c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, t.c.refusal(resp, raw)
	}
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// FanOut calls fn once for every app in list that can be reached, all at
// once (fanOutLimit in flight), each under its own timeout: one hung app
// costs only its own call. The apps are resolved together first, from one
// container listing; an app that can't be reached is skipped — every
// fan-out is best-effort, and the callback endpoints are optional anyway.
// FanOut returns when every call has.
func (c *Caller) FanOut(ctx context.Context, list []store.App, timeout time.Duration, fn func(ctx context.Context, t *Target)) {
	if len(list) == 0 {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	eps := c.resolve(rctx, list)
	cancel()
	sem := make(chan struct{}, fanOutLimit)
	var wg sync.WaitGroup
	for i, a := range list {
		if eps[i].Err != nil {
			continue
		}
		t := c.target(a, eps[i])
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			if ctx.Err() != nil {
				return // shutting down: the rest of the sweep is moot
			}
			cctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			fn(cctx, t)
		}()
	}
	wg.Wait()
}

// Cached resolves through resolve, reusing each app's address for ttl: a
// caller polling an app several times a second (the compute relay) asks
// Docker once in a while instead of on every call. A failed resolution is
// never kept, so an app that comes back is reached at once.
func Cached(resolve Resolver, ttl time.Duration) Resolver {
	type entry struct {
		ep apps.AppEndpoint
		at time.Time
	}
	var mu sync.Mutex
	cache := map[int64]entry{}
	return func(ctx context.Context, list []store.App) []apps.AppEndpoint {
		out := make([]apps.AppEndpoint, len(list))
		var missing []store.App
		var where []int
		mu.Lock()
		for i, a := range list {
			if e, ok := cache[a.ID]; ok && time.Since(e.at) < ttl {
				out[i] = e.ep
			} else {
				missing = append(missing, a)
				where = append(where, i)
			}
		}
		mu.Unlock()
		if len(missing) == 0 {
			return out
		}
		got := resolve(ctx, missing)
		mu.Lock()
		defer mu.Unlock()
		for j, ep := range got {
			out[where[j]] = ep
			if ep.Err == nil {
				cache[missing[j].ID] = entry{ep: ep, at: time.Now()}
			} else {
				delete(cache, missing[j].ID)
			}
		}
		return out
	}
}
