package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/auth"
	"gitea.lan/matthew/fog/internal/store"
)

func TestWriteErrorStatus(t *testing.T) {
	cases := []struct {
		err  error
		want int
		msg  string
	}{
		{fmt.Errorf("app x: %w", store.ErrNotFound), http.StatusNotFound, "app x: not found"},
		{fmt.Errorf("a peer named %q %w", "a", store.ErrAlreadyExists), http.StatusConflict, `a peer named "a" already exists`},
		{errors.New("docker went away"), http.StatusInternalServerError, "docker went away"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		writeError(rec, c.err)
		if rec.Code != c.want {
			t.Errorf("%v: status %d, want %d", c.err, rec.Code, c.want)
		}
		var body struct{ Error string }
		json.NewDecoder(rec.Body).Decode(&body)
		if body.Error != c.msg {
			t.Errorf("%v: message %q, want %q", c.err, body.Error, c.msg)
		}
	}
}

// Every 429 carries the same JSON body and a Retry-After of the limiter's
// own window, so a 15-minute sign-in limit doesn't tell clients to retry in
// a minute.
func TestLimitAnswersOneShape(t *testing.T) {
	lim := auth.NewRateLimiter(1, 15*time.Minute)
	if !limit(httptest.NewRecorder(), lim, "k", "slow down") {
		t.Fatal("first attempt refused")
	}
	rec := httptest.NewRecorder()
	if limit(rec, lim, "k", "slow down") {
		t.Fatal("second attempt allowed")
	}
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "900" {
		t.Fatalf("status %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil || body["error"] != "slow down" || body["code"] != "rate_limited" {
		t.Fatalf("body %v (%v)", body, err)
	}
}

func TestReadOptionalJSON(t *testing.T) {
	var v struct{ Reauth bool }
	rec := httptest.NewRecorder()
	if !readOptionalJSON(rec, httptest.NewRequest("POST", "/", nil), &v) || v.Reauth {
		t.Fatal("an empty body is allowed and leaves v alone")
	}
	if !readOptionalJSON(rec, httptest.NewRequest("POST", "/", strings.NewReader(`{"reauth":true}`)), &v) || !v.Reauth {
		t.Fatal("a body decodes")
	}
	rec = httptest.NewRecorder()
	if readOptionalJSON(rec, httptest.NewRequest("POST", "/", strings.NewReader(`{nope`)), &v) || rec.Code != http.StatusBadRequest {
		t.Fatalf("a malformed body is a 400, got %d", rec.Code)
	}
}

// An unknown share is a JSON 404 the frontend's ApiError can read, not
// net/http's plain-text page.
func TestNasUnknownShareIsJSON404(t *testing.T) {
	_, h := newTestServer(t)
	c := setup(t, h)
	w := do(t, h, "POST", "/api/nas/nope/mount", "", withCookie(c))
	if w.Code != http.StatusNotFound || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("status %d, content type %q", w.Code, w.Header().Get("Content-Type"))
	}
}
