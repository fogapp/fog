package logring

import (
	"io"
	"log/slog"
	"strings"
	"testing"
)

func TestRingKeepsNewestInOrder(t *testing.T) {
	r := New(3)
	log := slog.New(r.Handler(slog.NewTextHandler(io.Discard, nil)))
	for _, m := range []string{"one", "two", "three", "four"} {
		log.Info(m)
	}
	got := r.Recent(0)
	if len(got) != 3 || got[0].Msg != "two" || got[2].Msg != "four" {
		t.Fatalf("recent = %+v", got)
	}
	if last := r.Recent(1); len(last) != 1 || last[0].Msg != "four" {
		t.Fatalf("recent(1) = %+v", last)
	}
}

func TestRingRendersAttrsAndGroups(t *testing.T) {
	r := New(10)
	log := slog.New(r.Handler(slog.NewTextHandler(io.Discard, nil))).With("app", "manor").WithGroup("req")
	log.Warn("deploy failed", "err", "exit status 1", "id", 7)
	rec := r.Recent(1)[0]
	if rec.Level != "WARN" || !strings.Contains(rec.Attrs, "app=manor") ||
		!strings.Contains(rec.Attrs, `req.err="exit status 1"`) || !strings.Contains(rec.Attrs, "req.id=7") {
		t.Fatalf("record = %+v", rec)
	}
}

func TestRingSkipsWhatTheNextHandlerDrops(t *testing.T) {
	r := New(10)
	log := slog.New(r.Handler(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelInfo})))
	log.Debug("noise")
	if n := len(r.Recent(0)); n != 0 {
		t.Fatalf("debug records captured: %d", n)
	}
	var nilRing *Ring
	if nilRing.Recent(5) != nil {
		t.Fatal("nil ring should hold nothing")
	}
}
