package chat

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPublicURLGuard(t *testing.T) {
	resolve := func(host string) ([]string, error) {
		switch host {
		case "example.com":
			return []string{"93.184.216.34"}, nil
		case "evil.example":
			return []string{"93.184.216.34", "10.0.0.5"}, nil
		case "internal.example":
			return []string{"192.168.1.200"}, nil
		}
		return nil, errors.New("no such host")
	}
	denied := []string{
		"http://localhost/", "http://127.0.0.1:8080/", "http://10.1.2.3/", "http://192.168.1.1/", "http://172.16.0.9/",
		"http://169.254.169.254/latest/meta-data", "http://[::1]/", "http://[fe80::1]/", "http://fog.lan/", "http://printer.local/",
		"ftp://example.com/", "http://user:pw@example.com/", "http://internal.example/", "http://evil.example/", "http://100.64.0.1/", "http://0.0.0.0/",
	}
	for _, u := range denied {
		if _, err := parsePublicURL(u, resolve); err == nil {
			t.Errorf("%s should be refused", u)
		}
	}
	for _, u := range []string{"https://example.com/page?x=1", "http://93.184.216.34/"} {
		if _, err := parsePublicURL(u, resolve); err != nil {
			t.Errorf("%s should be allowed: %v", u, err)
		}
	}
}

// A name that resolved to a public address at check time can resolve to a
// private one when the client dials (DNS rebinding); the dial itself is
// checked, so the connection is refused either way.
func TestPreviewClientRefusesPrivateDial(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the private server was reached")
	}))
	defer srv.Close()
	_, err := previewClient().Get(srv.URL)
	if !errors.Is(err, previewDenied) {
		t.Fatalf("dial to %s: %v, want previewDenied", srv.URL, err)
	}
}

// FetchPreview must not rewire the caller's client.
func TestFetchPreviewLeavesCallerClientAlone(t *testing.T) {
	c := &http.Client{}
	resolve := func(string) ([]string, error) { return []string{"93.184.216.34"}, nil }
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // nothing need be fetched
	FetchPreview(ctx, "https://example.com/", c, resolve)
	if c.CheckRedirect != nil {
		t.Error("caller's CheckRedirect was replaced")
	}
}

func TestParsePreview(t *testing.T) {
	html := `<html><head><title>Plain &amp; Title</title>
<meta property="og:title" content="OG Title" />
<meta name="description" content="A description that is quite plain.">
<meta property="og:description" content='The OG description &quot;quoted&quot;'>
<meta property="og:image" content="https://cdn.example.com/i.png">
<meta property="og:site_name" content="Example">
</head><body>ignored</body></html>`
	pv := parsePreview(html)
	if pv.Title != "OG Title" || pv.Description != `The OG description "quoted"` || pv.Image != "https://cdn.example.com/i.png" || pv.Site != "Example" {
		t.Fatalf("preview: %+v", pv)
	}
	pv = parsePreview(`<title>Only</title>`)
	if pv.Title != "Only" || pv.Description != "" {
		t.Fatalf("title-only preview: %+v", pv)
	}
}

func TestPrefsQuietWindow(t *testing.T) {
	var p Prefs
	p.DND.Enabled = true
	p.DND.Start, p.DND.End, p.DND.TZ = "22:00", "07:00", "UTC"
	at := func(h int) bool { return p.quiet(mustTime(t, h)) }
	if !at(23) || !at(3) || at(12) || at(7) {
		t.Errorf("crossing-midnight window wrong: 23=%v 3=%v 12=%v 7=%v", at(23), at(3), at(12), at(7))
	}
	p.DND.Start, p.DND.End = "13:00", "14:00"
	if !at(13) || at(15) {
		t.Errorf("same-day window wrong")
	}
	p.DND.Enabled = false
	if at(13) {
		t.Errorf("disabled window should never be quiet")
	}
}

func mustTime(t *testing.T, hour int) time.Time {
	t.Helper()
	return time.Date(2026, 9, 4, hour, 30, 0, 0, time.UTC)
}
