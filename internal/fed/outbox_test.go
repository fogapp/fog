package fed

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/store"
)

// memQueue is an in-memory outbox source: payloads are strings, the answer
// is ignored, and every call is recorded.
type memQueue struct {
	mu      sync.Mutex
	rows    map[int64][]*memRow
	nextID  int64
	sent    map[int64][][]string // peer → batches delivered
	dropped []int64
	defers  []time.Time
}

type memRow struct {
	OutboxRow[string]
	next time.Time
}

func newMemQueue() *memQueue {
	return &memQueue{rows: map[int64][]*memRow{}, sent: map[int64][][]string{}}
}

func (q *memQueue) add(peerID int64, payload string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.nextID++
	q.rows[peerID] = append(q.rows[peerID], &memRow{OutboxRow: OutboxRow[string]{ID: q.nextID, Size: len(payload), Label: "r", Payload: payload}})
}

func (q *memQueue) left(peerID int64) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.rows[peerID])
}

func (q *memQueue) DuePeers() ([]int64, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []int64
	for id, rs := range q.rows {
		for _, r := range rs {
			if !r.next.After(time.Now()) {
				out = append(out, id)
				break
			}
		}
	}
	return out, nil
}

func (q *memQueue) Due(peerID int64, limit int) ([]OutboxRow[string], error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []OutboxRow[string]
	for _, r := range q.rows[peerID] {
		if len(out) < limit && !r.next.After(time.Now()) {
			out = append(out, r.OutboxRow)
		}
	}
	return out, nil
}

func (q *memQueue) Encode(_ *store.FedPeer, batch []OutboxRow[string]) ([]byte, error) {
	var parts []string
	for _, r := range batch {
		parts = append(parts, r.Payload)
	}
	return []byte(strings.Join(parts, ",")), nil
}

func (q *memQueue) remove(peerID int64, ids []int64) {
	q.rows[peerID] = slices.DeleteFunc(q.rows[peerID], func(r *memRow) bool { return slices.Contains(ids, r.ID) })
}

func (q *memQueue) Settle(peer *store.FedPeer, batch []OutboxRow[string], _ []byte) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	var ids []int64
	var payloads []string
	for _, r := range batch {
		ids = append(ids, r.ID)
		payloads = append(payloads, r.Payload)
	}
	q.sent[peer.ID] = append(q.sent[peer.ID], payloads)
	q.remove(peer.ID, ids)
	return nil
}

func (q *memQueue) Defer(peerID int64, ids []int64, next time.Time, _ error) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.defers = append(q.defers, next)
	for _, r := range q.rows[peerID] {
		if slices.Contains(ids, r.ID) {
			r.Attempts++
			r.next = next
		}
	}
	return nil
}

func (q *memQueue) Drop(peerID int64, ids []int64) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.dropped = append(q.dropped, ids...)
	q.remove(peerID, ids)
	return nil
}

// outboxPeer is a paired peer backed by a live server that answers signed,
// with its handler swappable per test.
func outboxPeer(t *testing.T, st *store.Store, name string, h http.HandlerFunc) *store.FedPeer {
	t.Helper()
	pst, pvlt := testDeps(t)
	them, err := EnsureIdentity(pst, pvlt)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wire, _ := io.ReadAll(r.Body)
		sr, _, err := OpenSealedRequest(them, r, wire, them.InstanceID)
		if err == nil && sr.Hello {
			WriteRecorded(w, them, sr, http.StatusOK, nil, []byte("{}"))
			return
		}
		if h != nil {
			h(w, r)
			return
		}
		WriteRecorded(w, them, sr, http.StatusOK, nil, []byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	p, err := st.CreatePairedPeer(them.InstanceID, name, srv.URL, them.PublicKeyB64())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func testOutbox(t *testing.T, q *memQueue) (*Outbox[string], *store.Store) {
	st, vlt := testDeps(t)
	log := slog.New(slog.DiscardHandler)
	return &Outbox[string]{
		Store: st, Fed: &Service{Store: st, Vault: vlt, Log: log}, Source: q, Log: log,
		Kind: "test", Path: "/fed/v1/test", ContentType: "text/plain",
		BatchRows: 3, BatchBytes: 100, RowMax: 150,
	}, st
}

// A peer that hangs holds up only its own queue: another peer's flush —
// the coalesced kind, or its share of a full Flush — goes straight through.
func TestOutboxDeadPeerDoesNotBlockAnother(t *testing.T) {
	q := newMemQueue()
	o, st := testOutbox(t, q)
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	dead := outboxPeer(t, st, "dead", func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
		}
		http.Error(w, "gone", http.StatusBadGateway)
	})
	live := outboxPeer(t, st, "live", nil)
	defer close(release)
	q.add(dead.ID, "a")
	q.add(live.ID, "b")

	go o.FlushPeer(context.Background(), dead.ID)
	<-entered

	done := make(chan struct{})
	go func() { o.FlushPeer(context.Background(), live.ID); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a hanging peer blocked delivery to another")
	}
	if q.left(live.ID) != 0 {
		t.Fatal("the live peer's row was not settled")
	}

	q.add(live.ID, "c")
	flushed := make(chan struct{})
	go func() { o.Flush(context.Background()); close(flushed) }()
	deadline := time.Now().Add(5 * time.Second)
	for q.left(live.ID) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("a full Flush held the live peer behind the hanging one")
		}
		time.Sleep(10 * time.Millisecond)
	}
	release <- struct{}{}
	<-flushed
}

// A failed delivery backs the whole batch off by the most-retried row's
// count: 30s doubling, capped at an hour.
func TestOutboxBackoff(t *testing.T) {
	for attempts, want := range map[int]time.Duration{0: 30 * time.Second, 1: time.Minute, 3: 4 * time.Minute, 6: 32 * time.Minute, 7: time.Hour, 40: time.Hour} {
		if got := OutboxBackoff(attempts); got != want {
			t.Errorf("OutboxBackoff(%d) = %s, want %s", attempts, got, want)
		}
	}

	q := newMemQueue()
	o, st := testOutbox(t, q)
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	o.Now = func() time.Time { return now }
	p := outboxPeer(t, st, "down", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	})
	q.add(p.ID, "a")
	q.add(p.ID, "b")
	q.rows[p.ID][1].Attempts = 3

	o.FlushPeer(context.Background(), p.ID)
	if len(q.defers) != 1 || !q.defers[0].Equal(now.Add(4*time.Minute)) {
		t.Fatalf("defers = %v, want one at +4m", q.defers)
	}
	if q.left(p.ID) != 2 || q.rows[p.ID][0].Attempts != 1 {
		t.Fatal("a failed batch must stay queued with its attempt counted")
	}
}

// A row too large to ever deliver is dropped, not left to wedge the queue;
// the rows around it still ship.
func TestOutboxDropsOversizedRows(t *testing.T) {
	q := newMemQueue()
	o, st := testOutbox(t, q)
	p := outboxPeer(t, st, "p", nil)
	q.add(p.ID, "small")
	q.add(p.ID, strings.Repeat("x", 151))
	q.add(p.ID, "tiny")

	o.FlushPeer(context.Background(), p.ID)
	if len(q.dropped) != 1 || q.dropped[0] != 2 {
		t.Fatalf("dropped = %v, want row 2", q.dropped)
	}
	if q.left(p.ID) != 0 || len(q.sent[p.ID]) != 1 || !slices.Equal(q.sent[p.ID][0], []string{"small", "tiny"}) {
		t.Fatalf("sent = %v, left %d", q.sent[p.ID], q.left(p.ID))
	}
}

// Batches respect the row and byte budgets — a lone row over the byte
// budget still ships by itself — and one flush keeps sending until nothing
// is due, settling each acknowledged batch off the queue.
func TestOutboxBatchesAndSettles(t *testing.T) {
	q := newMemQueue()
	o, st := testOutbox(t, q)
	p := outboxPeer(t, st, "p", nil)
	big := strings.Repeat("b", 120) // over BatchBytes, under RowMax
	for _, s := range []string{"1", "2", "3", "4", big, strings.Repeat("c", 60), strings.Repeat("d", 60)} {
		q.add(p.ID, s)
	}
	o.FlushPeer(context.Background(), p.ID)
	if q.left(p.ID) != 0 {
		t.Fatalf("%d rows left after a flush", q.left(p.ID))
	}
	var shape []int
	for _, b := range q.sent[p.ID] {
		shape = append(shape, len(b))
	}
	// Three by the row limit; "4" alone before the big row; the big row by
	// itself; then c and d can't share 100 bytes.
	if want := []int{3, 1, 1, 1, 1}; !slices.Equal(shape, want) {
		t.Fatalf("batch sizes %v, want %v", shape, want)
	}
}

// A trigger for a peer mid-drain doesn't start a second drain (which would
// send the same rows twice); the running one takes another pass, and the
// trigger returns once that pass has carried its row.
func TestOutboxConcurrentTriggerTakesAnotherPass(t *testing.T) {
	q := newMemQueue()
	o, st := testOutbox(t, q)
	var mu sync.Mutex
	posts := 0
	gate := make(chan struct{})
	var p *store.FedPeer
	pst, pvlt := testDeps(t)
	them, _ := EnsureIdentity(pst, pvlt)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wire, _ := io.ReadAll(r.Body)
		sr, _, err := OpenSealedRequest(them, r, wire, them.InstanceID)
		if err != nil {
			return
		}
		if sr.Hello {
			WriteRecorded(w, them, sr, http.StatusOK, nil, []byte("{}"))
			return
		}
		mu.Lock()
		posts++
		first := posts == 1
		mu.Unlock()
		if first {
			<-gate
		}
		WriteRecorded(w, them, sr, http.StatusOK, nil, nil)
	}))
	defer srv.Close()
	p, _ = st.CreatePairedPeer(them.InstanceID, "p", srv.URL, them.PublicKeyB64())

	q.add(p.ID, "a")
	first := make(chan struct{})
	go func() { o.FlushPeer(context.Background(), p.ID); close(first) }()
	for {
		mu.Lock()
		n := posts
		mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	q.add(p.ID, "b")
	second := make(chan struct{})
	go func() { o.FlushPeer(context.Background(), p.ID); close(second) }()
	time.Sleep(50 * time.Millisecond)
	close(gate)
	<-second
	<-first
	if q.left(p.ID) != 0 {
		t.Fatal("the triggering row was not carried by the extra pass")
	}
	if got := q.sent[p.ID]; len(got) != 2 || !slices.Equal(got[0], []string{"a"}) || !slices.Equal(got[1], []string{"b"}) {
		t.Fatalf("sent = %v, want [a] then [b], nothing twice", got)
	}
}

// Kicks in a burst fold into one flush, which carries every row queued by
// the time it runs in a single batch.
func TestOutboxKickCoalesces(t *testing.T) {
	q := newMemQueue()
	o, st := testOutbox(t, q)
	p := outboxPeer(t, st, "p", nil)
	for _, payload := range []string{"a", "b", "c"} {
		q.add(p.ID, payload)
		o.Kick(p.ID, 30*time.Millisecond)
	}
	deadline := time.Now().Add(5 * time.Second)
	for q.left(p.ID) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the kicked flush never ran")
		}
		time.Sleep(5 * time.Millisecond)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if got := q.sent[p.ID]; len(got) != 1 || !slices.Equal(got[0], []string{"a", "b", "c"}) {
		t.Fatalf("sent = %v, want one batch [a b c]", got)
	}
}
