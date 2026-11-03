package backup

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The app is untrusted, so a 3xx from its /fog surface must fail the call,
// never be followed: a 307 would otherwise carry fog — and its bearer token
// — to an internal URL or an outside host.
func TestAppClientRefusesRedirects(t *testing.T) {
	var hit bool
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.Write([]byte(`{"contract":"fog-backup/1","app":"x"}`))
	}))
	defer elsewhere.Close()
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer app.Close()
	c := &appClient{base: app.URL, token: "tok"}
	ctx := context.Background()

	if _, err := c.descriptor(ctx); err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("a redirected descriptor call must fail: %v", err)
	}
	if _, _, err := c.downloadArtifact(ctx, "j1", filepath.Join(t.TempDir(), "a.tar"), 1<<20); err == nil {
		t.Fatal("a redirected artifact download must fail")
	}
	if _, err := c.startBackup(ctx); err == nil {
		t.Fatal("a redirected backup start must fail")
	}
	if hit {
		t.Fatal("the redirect target was contacted — redirects must not be followed")
	}
}

// The job id is the app's word and goes into the URL of every poll, download
// and delete. A crafted id must not carry a path or a query: it is refused at
// the strict charset, and (for anything that slips a legal-looking token) the
// uses path-escape it so it stays one path segment.
func TestStartBackupRejectsHostileJobID(t *testing.T) {
	jobID := "x"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/fog/backup" {
			w.WriteHeader(http.StatusAccepted)
			w.Write([]byte(`{"job":` + jsonString(jobID) + `}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := &appClient{base: srv.URL, token: "tok"}
	ctx := context.Background()

	for _, bad := range []string{"../../admin", "x/../y", "x?rm=1", "a b", "x/y", "", strings.Repeat("a", 200)} {
		jobID = bad
		if _, err := c.startBackup(ctx); err == nil {
			t.Errorf("job id %q must be refused", bad)
		}
	}
	// A legal token is accepted and lands as exactly one path segment.
	jobID = "01HRA9F3-ok_id.v2"
	got, err := c.startBackup(ctx)
	if err != nil || got != jobID {
		t.Fatalf("a plain token must be accepted: %q %v", got, err)
	}
	var gotEscaped string
	esc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEscaped = r.URL.EscapedPath()
		w.Write([]byte(`{"status":"running"}`))
	}))
	defer esc.Close()
	c2 := &appClient{base: esc.URL, token: "tok"}
	// Fed a slash straight to jobStatus, the use site must escape it so it
	// stays one path segment rather than reaching "jobs/a/b".
	if _, err := c2.jobStatus(ctx, "a/b"); err != nil {
		t.Fatalf("jobStatus: %v", err)
	}
	if !strings.Contains(gotEscaped, "a%2Fb") {
		t.Fatalf("job id must be path-escaped into one segment, server saw %q", gotEscaped)
	}
}

// jsonString quotes s as a JSON string literal for the fake app's response.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// The artifact is produced and served by the app, which is untrusted, so the
// download must be capped: a stream that never ends would otherwise fill the
// backup disk. A claimed length over the cap is refused before a byte lands;
// a stream that lies about its length and runs past the cap is aborted. The
// job id selects the behaviour the fake app serves.
func TestDownloadArtifactCapsSize(t *testing.T) {
	const capBytes = 1 << 20
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "honest-oversize"):
			w.Header().Set("Content-Length", "2097152") // 2 MiB, claimed up front
			w.Write(make([]byte, 2<<20))
		case strings.Contains(r.URL.Path, "lying-stream"):
			// No Content-Length: just keep sending, well past the cap.
			for i := 0; i < 64; i++ {
				if _, err := w.Write(make([]byte, 64<<10)); err != nil {
					return
				}
			}
		default:
			w.Header().Set("Content-Length", "524288")
			w.Write(make([]byte, capBytes/2))
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	c := &appClient{base: srv.URL, token: "tok"}

	dst := filepath.Join(t.TempDir(), "artifact.tar")
	if _, n, err := c.downloadArtifact(ctx, "ok", dst, capBytes); err != nil || n != capBytes/2 {
		t.Fatalf("an in-cap artifact must download: n=%d err=%v", n, err)
	}

	if _, _, err := c.downloadArtifact(ctx, "honest-oversize", dst, capBytes); err == nil || !strings.Contains(err.Error(), "cap") {
		t.Fatalf("a claimed-oversize artifact must be refused before downloading: %v", err)
	}

	if _, _, err := c.downloadArtifact(ctx, "lying-stream", dst, capBytes); err == nil || !strings.Contains(err.Error(), "cap") {
		t.Fatalf("a stream past the cap must be aborted: %v", err)
	}
	if info, err := os.Stat(dst); err == nil && info.Size() > capBytes+1 {
		t.Fatalf("more than the cap was written to disk: %d bytes", info.Size())
	}
}

func TestAppClientCallsShareOneShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "POST /fog/backup":
			w.WriteHeader(http.StatusConflict) // already producing: adopt it
			w.Write([]byte(`{"job":"j1"}`))
		case "GET /fog/backup/jobs/j1":
			w.Write([]byte(`{"status":"running","progress":"half"}`))
		default:
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"errors":["disk full","try later"]}`))
		}
	}))
	defer srv.Close()
	c := &appClient{base: srv.URL, token: "tok"}
	ctx := context.Background()

	job, err := c.startBackup(ctx)
	if err != nil || job != "j1" {
		t.Fatalf("start: %q %v", job, err)
	}
	st, err := c.jobStatus(ctx, "j1")
	if err != nil || st.Status != "running" || st.Progress != "half" {
		t.Fatalf("status: %+v %v", st, err)
	}
	_, err = c.jobStatus(ctx, "nope")
	if err == nil || !strings.Contains(err.Error(), "HTTP 500 for job status: disk full; try later") {
		t.Fatalf("error envelope: %v", err)
	}
}
