package flare

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"gitea.lan/matthew/fog/internal/fed"
	"gitea.lan/matthew/fog/internal/store"
)

// SettingPeerPush toggles ntfy pushes for peer-relayed errors ("" = on).
const SettingPeerPush = "flare_peer_push"

// Forwarder relays locally-ingested error events to the peers this instance
// holds a use:flare.ingest capability toward, over the signed federation
// channel. Unlike the app→fog hop (drop on failure), this WAN hop is
// store-and-forward: scrub at enqueue, queue per peer in SQLite, drain in
// batches with backoff. Bounded — the local copy is the durable record and
// these streams are a courtesy.
type Forwarder struct {
	Store *store.Store
	Fed   *fed.Service
	Log   *slog.Logger

	destMu  sync.Mutex
	dests   []destination
	destAt  time.Time
	destCfg map[int64]string // the use rows the cached set was built from

	outOnce sync.Once
	out     *fed.Outbox[string]
}

// destination is one peer this instance forwards to, with its parsed mode.
type destination struct {
	peer store.FedPeer
	mode string   // "redacted" | "full"
	apps []string // optional filter; empty = all apps
}

// destCacheTTL bounds how long a cached destination set keeps the peers'
// rows (their addresses) it was built from. Consent is never cached: every
// call reads the use rows themselves — one indexed query — and rebuilds the
// set the moment they differ, so turning forwarding off (or narrowing it)
// takes effect for the very next event. Rebuilding costs 1+peers queries,
// which is what an error storm would otherwise multiply.
const destCacheTTL = 30 * time.Second

func (f *Forwarder) destinations() []destination {
	cfgs, err := f.Store.FedCapabilityConfigs(store.CapFlareIngest, store.DirUse)
	if err != nil {
		// A store that can't say what was consented to forwards nothing.
		return nil
	}
	f.destMu.Lock()
	defer f.destMu.Unlock()
	if !f.destAt.IsZero() && time.Since(f.destAt) < destCacheTTL && maps.Equal(cfgs, f.destCfg) {
		return f.dests
	}
	var out []destination
	for id, raw := range cfgs {
		var c struct {
			Mode string   `json:"mode"`
			Apps []string `json:"apps"`
		}
		if json.Unmarshal([]byte(raw), &c) != nil || (c.Mode != "redacted" && c.Mode != "full") {
			continue
		}
		p, err := f.Store.FedPeerByID(id)
		if err != nil || p.URL == "" {
			continue
		}
		out = append(out, destination{peer: *p, mode: c.Mode, apps: c.Apps})
	}
	f.dests, f.destAt, f.destCfg = out, time.Now(), cfgs
	return out
}

// Enqueue is the Processor's forward hook, called once per locally stored
// event. Scrubbing happens here, per destination — in redacted mode an
// unscrubbed payload never sits in any outbox row.
func (f *Forwarder) Enqueue(appName string, payload []byte) {
	for _, d := range f.destinations() {
		if len(d.apps) > 0 && !slices.Contains(d.apps, appName) {
			continue
		}
		out := payload
		if d.mode == "redacted" {
			var err error
			out, err = RedactEvent(payload)
			if err != nil {
				f.Log.Warn("flare forward: redact failed, dropping", "peer", d.peer.Name, "err", err)
				continue
			}
		}
		out, err := SetOriginTag(out, appName)
		if err != nil {
			f.Log.Warn("flare forward: tag failed, dropping", "peer", d.peer.Name, "err", err)
			continue
		}
		if err := f.Store.EnqueueFedOutbox(d.peer.ID, "flare", appName, string(out)); err != nil {
			f.Log.Warn("flare forward: enqueue failed", "peer", d.peer.Name, "err", err)
		}
	}
}

// Loop drains the outboxes on a short cadence. Wired in main like every
// other background loop.
func (f *Forwarder) Loop(ctx context.Context, interval time.Duration) {
	f.outbox().Loop(ctx, interval)
}

const (
	forwardBatch = 50
	// forwardBatchBytes caps one envelope's cumulative payload bytes. The
	// receiving peer caps request bodies at 8MB and the fed client times out
	// at 15s; without a byte budget a burst of near-256KB rows builds an
	// envelope that fails every retry identically — DueFedOutbox returns the
	// same oldest rows each flush, so delivery wedges until cap trimming
	// silently drops the queue head.
	forwardBatchBytes = 2 << 20
	// forwardRowMax is the most a single row may weigh. A row past this can
	// never be delivered, so keeping it queued would wedge everything behind
	// it forever — the drainer drops it instead. The local copy is the
	// durable record; this stream is a courtesy.
	forwardRowMax = 4 << 20
)

// Flush attempts delivery to every destination of everything due. One
// envelope carries a whole batch — Sentry envelopes hold multiple event
// items, so a backlog costs one POST per peer, not one per event.
func (f *Forwarder) Flush(ctx context.Context) { f.outbox().Flush(ctx) }

func (f *Forwarder) outbox() *fed.Outbox[string] {
	f.outOnce.Do(func() {
		f.out = &fed.Outbox[string]{
			Store: f.Store, Fed: f.Fed, Source: envelopeSource{f, fed.OutboxQueue{Store: f.Store, Kind: "flare"}}, Log: f.Log,
			// Verified: a 2xx deletes the queued rows, so the answer must
			// provably come from the peer — not from whatever device inherited
			// a stale address. A peer too old to sign responses keeps its rows
			// queued (and the error names the fix: update that peer's fog).
			Kind: "flare", Path: "/fed/v1/flare/envelope", ContentType: "application/x-sentry-envelope",
			BatchRows: forwardBatch, BatchBytes: forwardBatchBytes, RowMax: forwardRowMax,
			HoldUnaddressed: true,
		}
	})
	return f.out
}

// envelopeSource is the flare outbox as the drainer sees it: each row is one
// scrubbed event, and a batch is one Sentry envelope.
type envelopeSource struct {
	f *Forwarder
	fed.OutboxQueue
}

// DuePeers is the destinations with rows due: a peer no longer forwarded to
// keeps its queue, untouched, in case forwarding is turned back on.
func (s envelopeSource) DuePeers() ([]int64, error) {
	due, err := s.OutboxQueue.DuePeers()
	if err != nil {
		return nil, err
	}
	var out []int64
	for _, d := range s.f.destinations() {
		if slices.Contains(due, d.peer.ID) {
			out = append(out, d.peer.ID)
		}
	}
	return out, nil
}

// errNotForwarding: forwarding to this peer was turned off after the batch
// was read; it is not sent.
var errNotForwarding = errors.New("forwarding to this peer is off")

func (s envelopeSource) Encode(peer *store.FedPeer, batch []fed.OutboxRow[string]) ([]byte, error) {
	// Consent is read again at the last moment: a batch read just before
	// the operator turned forwarding off is not sent after it.
	if !slices.ContainsFunc(s.f.destinations(), func(d destination) bool { return d.peer.ID == peer.ID }) {
		return nil, errNotForwarding
	}
	var body bytes.Buffer
	header, _ := json.Marshal(map[string]string{"sent_at": time.Now().UTC().Format(time.RFC3339)})
	body.Write(header)
	body.WriteByte('\n')
	for _, r := range batch {
		item, _ := json.Marshal(map[string]any{"type": "event", "length": len(r.Payload)})
		body.Write(item)
		body.WriteByte('\n')
		body.WriteString(r.Payload)
		body.WriteByte('\n')
	}
	return body.Bytes(), nil
}

// Settle dequeues a delivered envelope; the receiver's answer says nothing
// per event (redelivery is deduped there by event id).
func (s envelopeSource) Settle(_ *store.FedPeer, batch []fed.OutboxRow[string], _ []byte) error {
	return s.Delete(batch)
}
