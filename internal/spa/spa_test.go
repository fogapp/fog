package spa

import (
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
)

func TestHandlerCachingAndFallback(t *testing.T) {
	h := Handler(fstest.MapFS{
		"index.html":       {Data: []byte("<html>app</html>")},
		"assets/app-1a.js": {Data: []byte("js")},
		"favicon.svg":      {Data: []byte("<svg/>")},
	})
	cases := []struct {
		path, body, cache string
	}{
		{"/", "app", "no-cache"},
		{"/assets/app-1a.js", "js", "public, max-age=31536000, immutable"},
		{"/favicon.svg", "svg", "no-cache"},
		// A deep link is index.html too, and must be as uncacheable as the
		// real thing.
		{"/apps/nextcloud/settings", "app", "no-cache"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", c.path, nil))
		if rec.Code != 200 {
			t.Errorf("%s: status %d", c.path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), c.body) {
			t.Errorf("%s: body %q, want %q", c.path, rec.Body.String(), c.body)
		}
		if got := rec.Header().Get("Cache-Control"); got != c.cache {
			t.Errorf("%s: Cache-Control %q, want %q", c.path, got, c.cache)
		}
	}
}

func TestFromDistFallsBackToThePlaceholder(t *testing.T) {
	placeholder := fstest.MapFS{"dist/index.html": {Data: []byte("no frontend built")}}
	rec := httptest.NewRecorder()
	FromDist(placeholder).ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if !strings.Contains(rec.Body.String(), "no frontend built") {
		t.Errorf("placeholder not served: %q", rec.Body.String())
	}
	built := fstest.MapFS{
		"dist/index.html":     {Data: []byte("no frontend built")},
		"dist/app/index.html": {Data: []byte("the app")},
	}
	rec = httptest.NewRecorder()
	FromDist(built).ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if !strings.Contains(rec.Body.String(), "the app") {
		t.Errorf("build not served: %q", rec.Body.String())
	}
}

// The CSP fog serves forbids inline script, so neither index.html may carry
// one — the theme bootstrap that used to sit inline is /theme-boot.js. Both
// the sources and, when a frontend has been built, the built pages are read:
// Vite has been known to inline a modulepreload polyfill.
func TestIndexHTMLHasNoInlineScript(t *testing.T) {
	pages := []string{
		"../../frontend/index.html",
		"../../frontend/setup/index.html",
		"../ui/dist/index.html",
		"../ui/dist/app/index.html",
		"../setupui/dist/index.html",
		"../setupui/dist/app/index.html",
	}
	scriptTag := regexp.MustCompile(`(?is)<script\b([^>]*)>(.*?)</script>`)
	handler := regexp.MustCompile(`(?i)\son[a-z]+\s*=`)
	checked := 0
	for _, p := range pages {
		b, err := os.ReadFile(p)
		if err != nil {
			continue // dist/app exists only after a frontend build
		}
		checked++
		for _, m := range scriptTag.FindAllStringSubmatch(string(b), -1) {
			if !strings.Contains(strings.ToLower(m[1]), "src=") || strings.TrimSpace(m[2]) != "" {
				t.Errorf("%s: inline script the CSP would block: %.80q", p, m[0])
			}
		}
		if handler.MatchString(string(b)) {
			t.Errorf("%s: inline event handler the CSP would block", p)
		}
		if strings.Contains(string(b), "javascript:") {
			t.Errorf("%s: javascript: URL the CSP would block", p)
		}
	}
	if checked < 2 {
		t.Fatalf("read %d index.html files; the frontend sources moved?", checked)
	}
}
