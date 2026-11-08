package server

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"gitea.lan/matthew/fog/internal/chat"
	"gitea.lan/matthew/fog/internal/store"
)

// A hub's refusal this side has no mapping for keeps its status and code;
// it is the hub's answer, not our failure.
func TestChatErrorPassesRemoteRefusalsThrough(t *testing.T) {
	t.Parallel()

	s := &Server{log: slog.New(slog.DiscardHandler)}
	rec := httptest.NewRecorder()
	s.chatError(rec, fmt.Errorf("relay: %w", &chat.RemoteError{Status: http.StatusNotFound, Code: "not_found", Msg: "no such room"}))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d", rec.Code)
	}
	var body map[string]string
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["code"] != "not_found" {
		t.Fatalf("body %v", body)
	}

	rec = httptest.NewRecorder()
	s.chatError(rec, &chat.RemoteError{Status: 0, Code: "", Msg: "garbled"})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("statusless remote error: %d", rec.Code)
	}
}

// An epoch_stale answer re-reads the room for where it is now; a room that
// can't be read again (deleted under the submit) answers from the copy in
// hand instead of dereferencing a nil room.
func TestEpochStaleSurvivesAFailedRoomLookup(t *testing.T) {
	t.Parallel()

	srv, _ := newTestServer(t)
	rec := httptest.NewRecorder()
	srv.writeEpochStale(rec, &store.ChatRoom{ID: "gone", Epoch: 4, Seq: 9}, chat.ErrEpochStale)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status %d", rec.Code)
	}
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["code"] != "epoch_stale" || body["epoch"] != float64(4) || body["seq"] != float64(9) {
		t.Fatalf("body %v", body)
	}
}
