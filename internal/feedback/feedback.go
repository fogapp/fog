// Package feedback carries the reports people write from the header's bug
// button: what fog attaches to them (the dossier), the shape they cross
// the federation in, and the forwarder that sends them to the peers this
// household reports to.
//
// It rides the same rails as Flare's error forwarding — a use row toward a
// peer sends, a grant row on the receiver admits — but not the same outbox.
// A report is written by a person and read by one, so where it went matters
// to the person who sent it: each send is a row that ends delivered or
// refused, and the report page shows which.
package feedback

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"gitea.lan/matthew/fog/internal/fed"
	"gitea.lan/matthew/fog/internal/store"
)

// Modes a use:feedback.ingest row may carry.
const (
	ModeNote = "note" // the words, the page, the fog version
	ModeFull = "full" // all of that and the dossier
)

// Bounds, enforced on both sides of the wire.
const (
	MaxBody    = 10_000
	MaxPage    = 2048
	MaxDossier = 1 << 20
	maxUID     = 64
	maxAuthor  = 128
)

// Item is one report as it crosses to a peer.
type Item struct {
	UID        string          `json:"uid"`
	Author     string          `json:"author"`
	Body       string          `json:"body"`
	Page       string          `json:"page"`
	FogVersion string          `json:"fogVersion"`
	CreatedAt  string          `json:"createdAt"`
	Dossier    json.RawMessage `json:"dossier,omitempty"`
}

// Batch is the body of POST /fed/v1/feedback.
type Batch struct {
	Items []Item `json:"items"`
}

// Answer is the receiver's signed reply: every uid lands in exactly one of
// the two, so the sender can settle each delivery on the receiver's word.
type Answer struct {
	Accepted []string          `json:"accepted"`
	Refused  map[string]string `json:"refused,omitempty"`
}

// Validate checks an item a peer sent before it is stored.
func (it *Item) Validate() error {
	switch {
	case it.UID == "" || len(it.UID) > maxUID:
		return errors.New("uid is missing or too long")
	case strings.TrimSpace(it.Body) == "":
		return errors.New("the report is empty")
	case utf8.RuneCountInString(it.Body) > MaxBody:
		return fmt.Errorf("the report is longer than %d characters", MaxBody)
	case len(it.Page) > MaxPage || len(it.Author) > maxAuthor:
		return errors.New("page or author is too long")
	case len(it.Dossier) > MaxDossier:
		return errors.New("the dossier is too large")
	}
	if len(it.Dossier) > 0 && !json.Valid(it.Dossier) {
		return errors.New("the dossier is not JSON")
	}
	if _, err := time.Parse(time.RFC3339, it.CreatedAt); err != nil {
		return errors.New("createdAt is not an RFC 3339 time")
	}
	return nil
}

// Destinations returns the peers this instance sends reports to, with the
// mode each was configured with. A row with an unreadable config sends
// nothing — silence is the safe reading of a consent we cannot parse.
func Destinations(st *store.Store) (map[int64]string, error) {
	peers, err := st.FedPeersWithCapability(store.CapFeedbackIngest, store.DirUse)
	if err != nil {
		return nil, err
	}
	// Every peer's config in one read rather than one per peer.
	configs, err := st.FedCapabilityConfigs(store.CapFeedbackIngest, store.DirUse)
	if err != nil {
		return nil, err
	}
	out := map[int64]string{}
	for _, p := range peers {
		raw, ok := configs[p.ID]
		if !ok || raw == "" {
			continue
		}
		var c struct {
			Mode string `json:"mode"`
		}
		if json.Unmarshal([]byte(raw), &c) != nil || !ValidMode(c.Mode) {
			continue
		}
		out[p.ID] = c.Mode
	}
	return out, nil
}

func ValidMode(m string) bool { return m == ModeNote || m == ModeFull }

// Forwarder drains queued deliveries toward peers.
type Forwarder struct {
	Store *store.Store
	Fed   *fed.Service
	Log   *slog.Logger

	outOnce sync.Once
	out     *fed.Outbox[Item]
}

const (
	batchMax   = 20
	batchBytes = 4 << 20
)

func (f *Forwarder) Loop(ctx context.Context, interval time.Duration) {
	f.outbox().Loop(ctx, interval)
}

// SendNow tries one report's deliveries immediately, so the person who just
// pressed Send can be told whether it arrived. Bounded by ctx: a peer that
// is away leaves the report queued, which is then the true answer.
func (f *Forwarder) SendNow(ctx context.Context, feedbackID int64) {
	peers, err := f.Store.PeersWithDueFeedback(feedbackID)
	if err != nil {
		f.Log.Warn("feedback forward: reading the queue", "err", err)
		return
	}
	f.outbox().FlushPeers(ctx, peers)
}

func (f *Forwarder) outbox() *fed.Outbox[Item] {
	f.outOnce.Do(func() {
		f.out = &fed.Outbox[Item]{
			Store: f.Store, Fed: f.Fed, Source: reportSource{f.Store}, Log: f.Log,
			// Verified, like Flare: the answer settles deliveries for good,
			// so it must provably come from the peer.
			Kind: "feedback", Path: "/fed/v1/feedback", ContentType: "application/json",
			BatchRows: batchMax, BatchBytes: batchBytes,
		}
	})
	return f.out
}

// reportSource is the feedback delivery table as the drainer sees it: a row
// is one report's delivery to one peer, keyed by the report's id.
type reportSource struct{ st *store.Store }

func (s reportSource) DuePeers() ([]int64, error) { return s.st.PeersWithDueFeedback(0) }

func (s reportSource) Due(peerID int64, limit int) ([]fed.OutboxRow[Item], error) {
	due, err := s.st.DueFeedbackDeliveries(peerID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]fed.OutboxRow[Item], len(due))
	for i, d := range due {
		it := Item{
			UID: d.Feedback.UID, Author: d.Feedback.Author, Body: d.Feedback.Body, Page: d.Feedback.Page,
			FogVersion: d.Feedback.FogVersion, CreatedAt: d.Feedback.CreatedAt,
		}
		if d.Mode == ModeFull && d.Feedback.Dossier != "" {
			it.Dossier = json.RawMessage(d.Feedback.Dossier)
		}
		out[i] = fed.OutboxRow[Item]{ID: d.Feedback.ID, Attempts: d.Attempts, Size: len(it.Body) + len(it.Dossier),
			Label: it.UID, Payload: it}
	}
	return out, nil
}

func (s reportSource) Encode(peer *store.FedPeer, batch []fed.OutboxRow[Item]) ([]byte, error) {
	if peer.URL == "" {
		return nil, errors.New("no address stored for this peer — it dials us, and we cannot reach it")
	}
	b := Batch{Items: make([]Item, len(batch))}
	for i, r := range batch {
		b.Items[i] = r.Payload
	}
	return json.Marshal(b)
}

// Settle records each delivery on the receiver's word: accepted, or refused
// with its reason. Anything the peer did not accept counts as refused — a
// report the receiver will never take must not sit in the queue retrying
// forever.
func (s reportSource) Settle(peer *store.FedPeer, batch []fed.OutboxRow[Item], answer []byte) error {
	var ans Answer
	if err := json.Unmarshal(answer, &ans); err != nil {
		return fmt.Errorf("%w: %v", fed.ErrBadAnswer, err)
	}
	ids := map[string]int64{}
	for _, r := range batch {
		ids[r.Payload.UID] = r.ID
	}
	var delivered []int64
	for _, uid := range ans.Accepted {
		if id, ok := ids[uid]; ok {
			delivered = append(delivered, id)
			delete(ids, uid)
		}
	}
	reasons := map[string][]int64{}
	for uid, id := range ids {
		reason := ans.Refused[uid]
		if reason == "" {
			reason = "the peer did not accept it"
		}
		reasons[reason] = append(reasons[reason], id)
	}
	if err := s.st.SettleFeedbackDeliveries(peer.ID, delivered, nil, ""); err != nil {
		return fmt.Errorf("recording delivered reports: %w", err)
	}
	for reason, rids := range reasons {
		if err := s.st.SettleFeedbackDeliveries(peer.ID, nil, rids, reason); err != nil {
			return fmt.Errorf("recording a refusal: %w", err)
		}
	}
	return nil
}

func (s reportSource) Defer(peerID int64, ids []int64, next time.Time, cause error) error {
	return s.st.DeferFeedbackDeliveries(peerID, ids, next.UTC().Format(time.RFC3339), cause.Error())
}

// Drop is never called: reports are bounded well under a batch at intake,
// so the drainer has no RowMax here.
func (s reportSource) Drop(int64, []int64) error { return nil }
