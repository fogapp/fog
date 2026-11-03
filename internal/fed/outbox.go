// The outbox drainer: everything this instance pushes to its peers — chat
// fan-out, flare events, feedback reports — is queued per peer in SQLite
// and delivered in signed batches with backoff. Each subsystem keeps its own
// queue, wire payload and acknowledgement; the drain around them (which
// peers, which rows, how many per batch, what a failure costs) is this one.
package fed

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"gitea.lan/matthew/fog/internal/backoff"
	"gitea.lan/matthew/fog/internal/loop"
	"gitea.lan/matthew/fog/internal/store"
)

// OutboxRow is one queued item as the drainer plans with it. Payload is the
// source's own, handed back to it to encode.
type OutboxRow[T any] struct {
	ID       int64
	Attempts int
	Size     int    // what the row weighs against a batch's byte budget
	Label    string // names the row in a log line: an app, a room
	Payload  T
}

// OutboxSource is one subsystem's queue. The drainer never looks inside a
// payload or an answer: the source encodes a batch into its wire body and
// settles the batch on the peer's verified answer, which is where the
// subsystems differ — chat hears which invites were refused, feedback which
// reports were accepted.
type OutboxSource[T any] interface {
	// DuePeers lists the peers with rows due now.
	DuePeers() ([]int64, error)
	// Due returns up to limit of one peer's due rows, oldest first.
	Due(peerID int64, limit int) ([]OutboxRow[T], error)
	// Encode builds the request body for a batch. An error fails the batch
	// like a failed delivery: its rows back off, and the error is recorded.
	Encode(peer *store.FedPeer, batch []OutboxRow[T]) ([]byte, error)
	// Settle takes a delivered batch off the queue on the peer's answer.
	Settle(peer *store.FedPeer, batch []OutboxRow[T], answer []byte) error
	// Defer reschedules failed rows to next, counting the attempt.
	Defer(peerID int64, ids []int64, next time.Time, cause error) error
	// Drop removes rows that can never be delivered.
	Drop(peerID int64, ids []int64) error
}

// Outbox drains one source toward every peer it has rows for. Peers drain
// independently: a peer that is down holds up only its own rows, never a
// flush toward anyone else.
type Outbox[T any] struct {
	Store  *store.Store
	Fed    *Service // nil: every delivery fails, and rows back off
	Source OutboxSource[T]
	Log    *slog.Logger
	// Kind names the queue in log lines.
	Kind string
	// Path and ContentType are the peer endpoint every batch is POSTed to.
	// The answer must be signed by the peer: settling deletes queued data,
	// and the address being dialed may have outlived the peer.
	Path, ContentType string
	// BatchRows and BatchBytes bound one delivery; a lone row over
	// BatchBytes still ships by itself. RowMax, when set, is the most one
	// row may weigh at all: past it a row could never be delivered and
	// would wedge the queue behind it, so it is dropped (and logged).
	BatchRows  int
	BatchBytes int
	RowMax     int
	// HoldUnaddressed leaves rows for a peer with no stored address queued
	// and untouched, to go once it tells us one. Without it they fail like
	// any undeliverable batch, so the reason is recorded against them.
	HoldUnaddressed bool
	// Now is swappable for tests.
	Now func() time.Time

	mu       sync.Mutex
	inflight map[int64]*drainState
	kicks    map[int64]*time.Timer // peer → a Kick's pending flush
}

type drainState struct {
	again bool          // a trigger arrived mid-drain: take one more pass
	done  chan struct{} // closed when the drain, extra pass included, ends
}

// outboxFanout bounds how many peers one Flush dials at once.
const outboxFanout = 4

// outboxMaxRounds bounds how many batches one drain of a peer sends back to
// back — far more than any queue holds (they are capped), there only so a
// source whose Settle leaves rows due can't spin a drain forever.
const outboxMaxRounds = 50

// OutboxBackoff is how long a failed delivery waits, from the most-retried
// row in its batch: 30s doubling to an hour, so a peer that is away for a
// weekend isn't hammered when it returns.
func OutboxBackoff(attempts int) time.Duration {
	return backoff.Exp(30*time.Second, time.Hour, attempts)
}

// Loop flushes on a fixed cadence until ctx ends.
func (o *Outbox[T]) Loop(ctx context.Context, interval time.Duration) {
	loop.Every(ctx, interval, interval, o.Flush)
}

// Flush drains every peer with rows due, a few peers at a time.
func (o *Outbox[T]) Flush(ctx context.Context) {
	peers, err := o.Source.DuePeers()
	if err != nil {
		o.Log.Warn("fed outbox: reading the queue", "kind", o.Kind, "err", err)
		return
	}
	o.FlushPeers(ctx, peers)
}

// Kick flushes one peer after a short wait, so rows queued in a burst go
// out together in one batch rather than one delivery each. A kick while one
// is already pending for the peer is folded into it: the flush it schedules
// reads every row due by then.
func (o *Outbox[T]) Kick(peerID int64, after time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, ok := o.kicks[peerID]; ok {
		return
	}
	if o.kicks == nil {
		o.kicks = map[int64]*time.Timer{}
	}
	o.kicks[peerID] = time.AfterFunc(after, func() {
		o.mu.Lock()
		delete(o.kicks, peerID)
		o.mu.Unlock()
		o.FlushPeer(context.Background(), peerID)
	})
}

// FlushPeers drains the named peers, a few at a time, and returns once
// each has been drained.
func (o *Outbox[T]) FlushPeers(ctx context.Context, peerIDs []int64) {
	var g errgroup.Group
	g.SetLimit(outboxFanout)
	for _, id := range peerIDs {
		g.Go(func() error { o.FlushPeer(ctx, id); return nil })
	}
	g.Wait()
}

// FlushPeer drains one peer and returns when its rows due now have been
// tried. A drain already underway for the peer is not doubled — two would
// read the same rows and hand the peer everything twice — but told to take
// one more pass when it finishes, which this call then waits for (as far as
// ctx allows). Only callers for that one peer ever wait on it.
func (o *Outbox[T]) FlushPeer(ctx context.Context, peerID int64) {
	o.mu.Lock()
	if o.inflight == nil {
		o.inflight = map[int64]*drainState{}
	}
	if st := o.inflight[peerID]; st != nil {
		st.again = true
		done := st.done
		o.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
		}
		return
	}
	st := &drainState{done: make(chan struct{})}
	o.inflight[peerID] = st
	o.mu.Unlock()

	for {
		o.drain(ctx, peerID)
		o.mu.Lock()
		if !st.again || ctx.Err() != nil {
			delete(o.inflight, peerID)
			close(st.done)
			o.mu.Unlock()
			return
		}
		st.again = false
		o.mu.Unlock()
	}
}

func (o *Outbox[T]) drain(ctx context.Context, peerID int64) {
	peer, err := o.Store.FedPeerByID(peerID)
	if err != nil {
		return
	}
	if peer.URL == "" && o.HoldUnaddressed {
		return
	}
	for range outboxMaxRounds {
		if ctx.Err() != nil {
			return
		}
		rows, err := o.Source.Due(peer.ID, o.BatchRows)
		if err != nil {
			o.Log.Warn("fed outbox: reading due rows", "kind", o.Kind, "peer", peer.Name, "err", err)
			return
		}
		if len(rows) == 0 {
			return
		}
		batch, oversized := o.plan(rows)
		if len(oversized) > 0 {
			ids := make([]int64, 0, len(oversized))
			for _, r := range oversized {
				ids = append(ids, r.ID)
				o.Log.Warn("fed outbox: dropping a row too large to ever deliver", "kind", o.Kind,
					"peer", peer.Name, "row", r.Label, "bytes", r.Size)
			}
			if err := o.Source.Drop(peer.ID, ids); err != nil {
				o.Log.Warn("fed outbox: dropping oversized rows failed — they re-surface next flush",
					"kind", o.Kind, "peer", peer.Name, "err", err)
				return
			}
		}
		if len(batch) > 0 && !o.deliver(ctx, peer, batch) {
			return
		}
		// Another pass only while this one may have left rows due: a full
		// read, or a batch the byte budget cut short.
		if len(rows) < o.BatchRows && len(batch)+len(oversized) == len(rows) {
			return
		}
	}
}

// plan takes due rows oldest-first until the byte budget is hit — always at
// least one, so a lone row over the budget still ships by itself. Rows over
// RowMax come back separately, to drop. Rows past the budget stay queued for
// the next pass.
func (o *Outbox[T]) plan(rows []OutboxRow[T]) (batch, oversized []OutboxRow[T]) {
	size := 0
	for _, r := range rows {
		if o.RowMax > 0 && r.Size > o.RowMax {
			oversized = append(oversized, r)
			continue
		}
		if len(batch) > 0 && size+r.Size > o.BatchBytes {
			break
		}
		batch = append(batch, r)
		size += r.Size
	}
	return batch, oversized
}

// deliver sends one batch and settles or defers it, reporting whether the
// queue moved on (so another pass is worth making).
func (o *Outbox[T]) deliver(ctx context.Context, peer *store.FedPeer, batch []OutboxRow[T]) bool {
	ids := make([]int64, 0, len(batch))
	attempts := 0
	for _, r := range batch {
		ids = append(ids, r.ID)
		attempts = max(attempts, r.Attempts)
	}
	body, err := o.Source.Encode(peer, batch)
	var answer []byte
	if err == nil && o.Fed == nil {
		err = errors.New("federation unavailable")
	}
	if err == nil {
		answer, err = o.Fed.SignedPostVerified(ctx, peer, o.Path, o.ContentType, body)
	}
	if err == nil {
		err = o.Source.Settle(peer, batch, answer)
		if err != nil && !errors.Is(err, ErrBadAnswer) {
			// Delivered but still queued: the next flush sends it again, and
			// the peer sees it twice. Nothing to retry here — say so.
			o.Log.Warn("fed outbox: delivered rows not settled — the peer will see them again",
				"kind", o.Kind, "peer", peer.Name, "rows", len(ids), "err", err)
			return false
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return false // cut short on our side: not the peer's failure
		}
		delay := OutboxBackoff(attempts)
		if derr := o.Source.Defer(peer.ID, ids, o.now().Add(delay), err); derr != nil {
			o.Log.Warn("fed outbox: backoff not recorded — rows retry on the next flush",
				"kind", o.Kind, "peer", peer.Name, "err", derr)
		}
		o.Log.Warn("fed outbox: delivery failed", "kind", o.Kind, "peer", peer.Name, "rows", len(ids),
			"retry_in", delay.String(), "err", err)
		return false
	}
	return true
}

// ErrBadAnswer is what a source's Settle wraps when it can't read the
// peer's answer: nothing was settled, and the batch backs off like a failed
// delivery rather than going straight back out.
var ErrBadAnswer = errors.New("unreadable answer from the peer")

func (o *Outbox[T]) now() time.Time {
	if o.Now != nil {
		return o.Now().UTC()
	}
	return time.Now().UTC()
}

// OutboxQueue is the fed_outbox table as a source's storage: one kind's
// rows, whose payload is the queued string itself. Chat and flare queue
// here and add only their encoding and acknowledgement; feedback keeps its
// own delivery table, since each send there is a row the report page shows.
type OutboxQueue struct {
	Store *store.Store
	Kind  string
}

func (q OutboxQueue) DuePeers() ([]int64, error) { return q.Store.PeersWithDueFedOutbox(q.Kind) }

func (q OutboxQueue) Due(peerID int64, limit int) ([]OutboxRow[string], error) {
	rows, err := q.Store.DueFedOutbox(peerID, q.Kind, limit)
	if err != nil {
		return nil, err
	}
	out := make([]OutboxRow[string], len(rows))
	for i, r := range rows {
		out[i] = OutboxRow[string]{ID: r.ID, Attempts: r.Attempts, Size: len(r.Payload), Label: r.AppName, Payload: r.Payload}
	}
	return out, nil
}

func (q OutboxQueue) Defer(_ int64, ids []int64, next time.Time, _ error) error {
	return q.Store.DeferFedOutbox(ids, next.UTC().Format(time.RFC3339))
}

func (q OutboxQueue) Drop(_ int64, ids []int64) error { return q.Store.DeleteFedOutbox(ids) }

// Delete takes delivered rows off the queue; a source's Settle calls it.
func (q OutboxQueue) Delete(batch []OutboxRow[string]) error {
	ids := make([]int64, len(batch))
	for i, r := range batch {
		ids[i] = r.ID
	}
	return q.Store.DeleteFedOutbox(ids)
}
