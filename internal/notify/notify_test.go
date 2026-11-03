package notify

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"time"

	"gitea.lan/matthew/fog/internal/appevents"
	"gitea.lan/matthew/fog/internal/store"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// send's status handling: only 2xx is success. A 4xx (wrong topic, auth
// required) must not be swallowed as success — it warns and stops, because
// retrying an identical rejection can never help.
func TestSendStatusHandling(t *testing.T) {
	t.Run("2xx succeeds quietly", func(t *testing.T) {
		var hits atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()
		var buf bytes.Buffer
		n := &Notifier{Log: slog.New(slog.NewTextHandler(&buf, nil))}
		n.send(context.Background(), srv.URL, "fog", "title", "message", true)
		if got := hits.Load(); got != 1 {
			t.Fatalf("want one request, got %d", got)
		}
		if buf.Len() != 0 {
			t.Fatalf("success must not log, got %q", buf.String())
		}
	})

	t.Run("4xx warns once and stops", func(t *testing.T) {
		var hits atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			w.WriteHeader(http.StatusForbidden)
		}))
		defer srv.Close()
		var buf bytes.Buffer
		n := &Notifier{Log: slog.New(slog.NewTextHandler(&buf, nil))}
		n.send(context.Background(), srv.URL, "secret-topic-x9", "critical thing", "message", false)
		if got := hits.Load(); got != 1 {
			t.Fatalf("a rejection must not retry, got %d requests", got)
		}
		log := buf.String()
		if !strings.Contains(log, "ntfy rejected") || !strings.Contains(log, "403") || !strings.Contains(log, "critical thing") {
			t.Fatalf("rejection must warn with status and title, got %q", log)
		}
		// A topic is a bearer secret on a public ntfy: it stays out of logs.
		if strings.Contains(log, "secret-topic-x9") {
			t.Fatalf("rejection logged the topic: %q", log)
		}
	})
}

// The "Send test" request's context bounds SendNow: a server answering 503
// would otherwise hold the request through two retry sleeps (5s, then
// 25s), long after the person who pressed the button gave up.
func TestSendNowStopsWithItsContext(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		cancel() // the person leaves while ntfy is down
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	st.SetSetting(SettingNtfyServer, srv.URL)
	st.SetSetting(SettingNtfyTopic, "fog")
	n := &Notifier{Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	start := time.Now()
	err = n.SendNow(ctx, "Fog test notification", "hello", true)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("SendNow = %v, want the context's cancellation", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("SendNow took %v after its context ended", d)
	}
	if hits.Load() != 1 {
		t.Errorf("ntfy was asked %d times, want once before the context ended", hits.Load())
	}
}

// Every notification lands on the event stream, ntfy configured or not, so
// a consumer can take over routing fog's own alerts.
func TestSendPublishesNoticeEvent(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	bus := appevents.New(st)
	n := &Notifier{Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Events: bus}
	n.Send("Deploy failed: hud", "build exited 1", false)
	p, err := bus.Read(t.Context(), 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Events) != 1 || p.Events[0].Type != NoticeEvent || p.Events[0].Source != appevents.FogSource ||
		!strings.Contains(string(p.Events[0].Data), `"level":"alert"`) {
		t.Fatalf("events = %+v", p.Events)
	}
}

// A household member picks their own topic. One that is more than a path
// segment would aim fog's POST elsewhere on the ntfy server — a query
// ntfy acts on (?email=, ?call=), another topic via an encoded slash, or a
// climb out of a path-prefixed server. None may leave fog.
func TestSendRefusesTopicsThatAreNotOneSegment(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	n := &Notifier{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for _, topic := range []string{
		"me?email=victim@example.com",
		"me?call=yes",
		"other%2Ftopic",
		"..",
		"me#frag",
		"me/../admin",
		"me\r\nX-Injected: 1",
		"",
		strings.Repeat("a", 65),
	} {
		if err := n.send(context.Background(), srv.URL+"/ntfy", topic, "t", "m", true); err == nil {
			t.Errorf("topic %q was sent", topic)
		}
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("%d requests reached the server", got)
	}
	// The control: a plain topic under a path-prefixed server lands as one
	// segment below that prefix.
	var path string
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.RawPath + r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv2.Close()
	if err := n.send(context.Background(), srv2.URL+"/ntfy/", "alerts_home-1", "t", "m", true); err != nil {
		t.Fatal(err)
	}
	if path != "/ntfy/alerts_home-1" {
		t.Fatalf("posted to %q", path)
	}
}

// The ntfy server is the operator's choice; where it redirects is not. A
// 307 to a LAN address must not turn fog into the one POSTing there.
func TestSendDoesNotFollowRedirects(t *testing.T) {
	var inside atomic.Int32
	lan := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inside.Add(1)
	}))
	defer lan.Close()
	for _, code := range []int{http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		ntfy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, lan.URL+"/admin/reboot", code)
		}))
		n := &Notifier{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
		err := n.send(context.Background(), ntfy.URL, "fog", "t", "m", true)
		ntfy.Close()
		if err == nil || !strings.Contains(err.Error(), "redirect") {
			t.Errorf("HTTP %d: err = %v, want a redirect refusal", code, err)
		}
	}
	if got := inside.Load(); got != 0 {
		t.Fatalf("fog followed a redirect %d times", got)
	}
}

// A title carries text people typed (a room's name). It must not break the
// request — Go would refuse the header outright and the notification would
// vanish — so it is flattened to one line.
func TestSendFlattensTitle(t *testing.T) {
	var title string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		title = r.Header.Get("Title")
	}))
	defer srv.Close()
	n := &Notifier{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := n.send(context.Background(), srv.URL, "fog", "room\r\nX-Evil: 1", "m", true); err != nil {
		t.Fatal(err)
	}
	if title != "room  X-Evil: 1" {
		t.Fatalf("title = %q", title)
	}
}

func TestValidServer(t *testing.T) {
	for v, ok := range map[string]bool{
		"https://ntfy.sh":             true,
		"http://ntfy.lan:8080/prefix": true,
		"https://user:pass@ntfy.home": true,
		"httpx://ntfy.sh":             false,
		"https://":                    false,
		"ntfy.sh":                     false,
		"https://ntfy.sh/?auth=abc":   false,
		"https://ntfy.sh/#x":          false,
		"javascript:alert(1)":         false,
		"http:ntfy.sh":                false,
	} {
		if got := ValidServer(v) == nil; got != ok {
			t.Errorf("ValidServer(%q) ok=%v, want %v", v, got, ok)
		}
	}
}
