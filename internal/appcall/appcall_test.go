package appcall

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/store"
)

// resolveTo answers every app at its own server in urls, by name; a name
// with no server is unreachable. calls counts resolutions.
func resolveTo(urls map[string]string, calls *atomic.Int32) Resolver {
	return func(ctx context.Context, list []store.App) []apps.AppEndpoint {
		if calls != nil {
			calls.Add(1)
		}
		out := make([]apps.AppEndpoint, len(list))
		for i, a := range list {
			if u, ok := urls[a.Name]; ok {
				out[i] = apps.AppEndpoint{Base: u, Token: "tok-" + a.Name}
			} else {
				out[i].Err = errors.New(a.Name + " has no running container")
			}
		}
		return out
	}
}

func TestCallSendsTokenAndBody(t *testing.T) {
	var gotAuth, gotType, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotType = r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	c := New(resolveTo(map[string]string{"lucid": srv.URL}, nil))

	raw, err := c.Call(context.Background(), &store.App{Name: "lucid"}, "POST", "/fog/x", map[string]string{"a": "b"})
	if err != nil || string(raw) != `{"ok":true}` {
		t.Fatalf("call: %q %v", raw, err)
	}
	if gotAuth != "Bearer tok-lucid" || gotType != "application/json" || gotBody != `{"a":"b"}` {
		t.Fatalf("request: auth %q type %q body %q", gotAuth, gotType, gotBody)
	}
	if _, err := c.Call(context.Background(), &store.App{Name: "lucid"}, "POST", "/fog/x", []byte("{}")); err != nil || gotBody != "{}" {
		t.Fatalf("raw body: %q %v", gotBody, err)
	}
}

// A refusal reads through the Caller's Refusal; an app fog never reached
// says so apart from one that answered no.
func TestCallRefusalAndUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		json.NewEncoder(w).Encode(map[string]any{"errors": []string{"no household yet"}})
	}))
	defer srv.Close()
	c := New(resolveTo(map[string]string{"lucid": srv.URL}, nil))

	_, err := c.Call(context.Background(), &store.App{Name: "lucid"}, "GET", "/fog/x", nil)
	if err == nil || err.Error() != "app answered 422 Unprocessable Entity" || errors.Is(err, ErrUnreachable) {
		t.Fatalf("default refusal: %v", err)
	}
	words := c.Refusing(func(resp *http.Response, raw []byte) error {
		var r struct{ Errors []string }
		json.Unmarshal(raw, &r)
		return errors.New(strings.Join(r.Errors, "; "))
	})
	if _, err := words.Call(context.Background(), &store.App{Name: "lucid"}, "GET", "/fog/x", nil); err == nil || err.Error() != "no household yet" {
		t.Fatalf("own refusal: %v", err)
	}

	_, err = c.Call(context.Background(), &store.App{Name: "stream"}, "GET", "/fog/x", nil)
	if !errors.Is(err, ErrUnreachable) || err.Error() != "stream has no running container" {
		t.Fatalf("unreachable: %v", err)
	}
}

// One hung app costs a fan-out its own timeout, not every other app's
// call: they all run at once, from one resolution, and an unreachable app
// is skipped.
func TestFanOutHungAppDoesNotDelayOthers(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	hung := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer hung.Close()
	var mu sync.Mutex
	answered := map[string]time.Duration{}
	start := time.Now()
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ok.Close()

	urls := map[string]string{"hung": hung.URL}
	list := []store.App{{Name: "hung"}, {Name: "gone"}}
	for _, n := range []string{"a", "b", "c", "d"} {
		urls[n] = ok.URL
		list = append(list, store.App{Name: n})
	}
	var resolved atomic.Int32
	c := New(resolveTo(urls, &resolved))

	const timeout = 300 * time.Millisecond
	var called []string
	c.FanOut(context.Background(), list, timeout, func(ctx context.Context, tg *Target) {
		_, err := tg.Call(ctx, "POST", "/fog/changed", nil)
		mu.Lock()
		defer mu.Unlock()
		called = append(called, tg.App.Name)
		if err == nil {
			answered[tg.App.Name] = time.Since(start)
		}
	})
	elapsed := time.Since(start)

	if len(called) != 5 {
		t.Fatalf("called %v: every reachable app, and the unreachable one skipped", called)
	}
	if resolved.Load() != 1 {
		t.Fatalf("resolved %d times, want once for the whole fan-out", resolved.Load())
	}
	for _, n := range []string{"a", "b", "c", "d"} {
		if d, ok := answered[n]; !ok || d >= timeout {
			t.Fatalf("%s answered after %v (%v): held up by the hung app", n, d, ok)
		}
	}
	if _, ok := answered["hung"]; ok {
		t.Fatal("the hung app answered")
	}
	if elapsed > 3*timeout {
		t.Fatalf("fan-out took %v, want about one timeout (%v)", elapsed, timeout)
	}
}

// The attack: a sealed app answers fog's contract call with a 307 to an
// address of its choosing, with what it wants carried out in the URL. fog
// must not follow it: the app's answer is the redirect, and nothing goes
// to the address it named.
func TestCallNeverFollowsAnAppsRedirect(t *testing.T) {
	var outside atomic.Int32
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		outside.Add(1)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer elsewhere.Close()
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/exfil?secret=household-data", http.StatusTemporaryRedirect)
	}))
	defer app.Close()
	c := New(resolveTo(map[string]string{"sealed": app.URL}, nil))
	c.Call(context.Background(), &store.App{Name: "sealed"}, "POST", "/fog/x", map[string]string{"a": "b"})
	if resp, err := c.Open(context.Background(), &store.App{Name: "sealed"}, "/fog/widgets", nil); err == nil {
		resp.Body.Close()
	}
	if outside.Load() != 0 {
		t.Fatalf("fog followed the app's redirect out (%d requests)", outside.Load())
	}
}
