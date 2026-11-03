package flare

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"gitea.lan/matthew/fog/internal/fed"
	"gitea.lan/matthew/fog/internal/store"
)

// Flush removes an undeliverable row from the outbox outright — with the
// batch empty no delivery (and so no defer) runs, so the row's absence from
// the due set proves deletion, not rescheduling.
func TestFlushDropsOversizedOutboxRow(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p, err := s.CreatePairedPeer("inst1", "peer1", "http://peer.lan", "pubkey")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetFedCapability(p.ID, store.CapFlareIngest, store.DirUse, `{"mode":"full"}`); err != nil {
		t.Fatal(err)
	}
	if err := s.EnqueueFedOutbox(p.ID, "flare", "manor", strings.Repeat("x", forwardRowMax+1)); err != nil {
		t.Fatal(err)
	}

	f := &Forwarder{Store: s, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	f.Flush(context.Background())

	due, err := s.DueFedOutbox(p.ID, "flare", forwardBatch)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Fatalf("oversized row still queued (%d rows) — it can never deliver and wedges the queue", len(due))
	}
}

// Turning forwarding off stops it at once: the very next event is not
// queued, and a batch read before the switch is not sent after it.
func TestForwardingStopsTheMomentItIsTurnedOff(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p, _ := s.CreatePairedPeer("inst1", "peer1", "http://peer.lan", "pubkey")
	set := func(on bool) {
		t.Helper()
		if err := s.EditFedPeer(p.ID, store.FedPeerEdit{Capabilities: []store.FedCapabilityEdit{
			{Capability: store.CapFlareIngest, Direction: store.DirUse, Config: `{"mode":"redacted"}`, Enabled: on}}}); err != nil {
			t.Fatal(err)
		}
	}
	queued := func() int {
		rows, _ := s.DueFedOutbox(p.ID, "flare", 100)
		return len(rows)
	}
	f := &Forwarder{Store: s, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	set(true)
	f.Enqueue("manor", []byte(`{"event_id":"a","message":"x"}`))
	if queued() != 1 {
		t.Fatalf("forwarding on: %d queued", queued())
	}
	batch, _ := s.DueFedOutbox(p.ID, "flare", 100)
	set(false)
	f.Enqueue("manor", []byte(`{"event_id":"b","message":"y"}`))
	if queued() != 0 {
		t.Fatalf("an event was queued after forwarding was turned off: %d", queued())
	}
	src := envelopeSource{f, fed.OutboxQueue{Store: s, Kind: "flare"}}
	rows := []fed.OutboxRow[string]{{ID: batch[0].ID, Payload: batch[0].Payload}}
	if _, err := src.Encode(p, rows); !errors.Is(err, errNotForwarding) {
		t.Fatalf("a batch read before the switch was encoded after it: %v", err)
	}
}
