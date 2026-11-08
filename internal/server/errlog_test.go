package server

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A 500 sent through writeError has to reach the log with its message:
// the client may never render it, and the log ring is what feedback reports
// read — a bare "status=500" there tells nobody what broke.
func TestInternalErrorReachesTheLog(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	s := &Server{log: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	h := s.protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, errors.New("disk on fire"))
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/thing", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}
	line := buf.String()
	if !strings.Contains(line, "level=ERROR") || !strings.Contains(line, `err="disk on fire"`) {
		t.Errorf("log line lacks the error: %q", line)
	}
}
