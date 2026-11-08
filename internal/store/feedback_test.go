package store

import (
	"testing"
	"time"
)

func TestFeedbackLifecycle(t *testing.T) {
	t.Parallel()

	st := testStore(t)
	peer, err := st.CreatePairedPeer("inst-dave", "dave", "http://dave.example", "pk")
	if err != nil {
		t.Fatal(err)
	}

	f, err := st.CreateFeedback(Feedback{UID: "u1", Author: "matthew", Body: "the save button did nothing",
		Page: "/apps/manor", FogVersion: "0.12.40", Dossier: `{"logs":[]}`}, map[int64]string{peer.ID: "full"})
	if err != nil {
		t.Fatal(err)
	}
	if f.Status != "open" || !f.HasDossier || f.PeerID != 0 {
		t.Fatalf("created = %+v", f)
	}

	// Lists leave the dossier behind; the single read carries it.
	list, _ := st.ListFeedback("open", 10)
	if len(list) != 1 || list[0].Dossier != "" || !list[0].HasDossier {
		t.Fatalf("list = %+v", list)
	}

	// The delivery is queued, due, and settles on the peer's word.
	if ids, _ := st.PeersWithDueFeedback(f.ID); len(ids) != 1 || ids[0] != peer.ID {
		t.Fatalf("due peers = %v", ids)
	}
	due, _ := st.DueFeedbackDeliveries(peer.ID, 10)
	if len(due) != 1 || due[0].Mode != "full" || due[0].Feedback.Dossier == "" {
		t.Fatalf("due = %+v", due)
	}
	if err := st.DeferFeedbackDeliveries(peer.ID, []int64{f.ID}, "2999-01-01T00:00:00Z", "connection refused"); err != nil {
		t.Fatal(err)
	}
	if due, _ := st.DueFeedbackDeliveries(peer.ID, 10); len(due) != 0 {
		t.Fatal("a deferred delivery should not be due")
	}
	ds, _ := st.FeedbackDeliveries(f.ID)
	if len(ds) != 1 || ds[0].State != "queued" || ds[0].Attempts != 1 || ds[0].LastError != "connection refused" {
		t.Fatalf("deliveries = %+v", ds)
	}
	if err := st.SettleFeedbackDeliveries(peer.ID, []int64{f.ID}, nil, ""); err != nil {
		t.Fatal(err)
	}
	ds, _ = st.FeedbackDeliveries(f.ID)
	if ds[0].State != "delivered" || ds[0].DeliveredAt == "" || ds[0].LastError != "" {
		t.Fatalf("settled = %+v", ds)
	}

	// Peer reports dedupe by uid per origin — and never collide with a
	// local report that happens to share the uid.
	in := Feedback{UID: "u1", Author: "dave", Body: "photos are slow", CreatedAt: "2026-09-10T10:00:00Z"}
	if ok, err := st.IngestPeerFeedback(peer.ID, in); err != nil || !ok {
		t.Fatalf("ingest: %v %v", ok, err)
	}
	if ok, _ := st.IngestPeerFeedback(peer.ID, in); ok {
		t.Fatal("a retried uid should not insert twice")
	}
	all, fromPeers, _ := st.CountOpenFeedback()
	if all != 2 || fromPeers != 1 {
		t.Fatalf("counts = %d/%d", all, fromPeers)
	}

	if err := st.SetFeedbackStatus(f.ID, "closed"); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.FeedbackByID(f.ID); got.Status != "closed" || got.ClosedAt == "" {
		t.Fatalf("closed = %+v", got)
	}

	// Removing the peer takes its reports and deliveries with it.
	if err := st.DeleteFedPeer(peer.ID); err != nil {
		t.Fatal(err)
	}
	all, fromPeers, _ = st.CountOpenFeedback()
	if all != 0 || fromPeers != 0 {
		t.Fatalf("after unpair = %d/%d", all, fromPeers)
	}
	if ds, _ := st.FeedbackDeliveries(f.ID); len(ds) != 0 {
		t.Fatalf("deliveries survived the peer: %+v", ds)
	}
}

// Withdrawing feedback.ingest/use drops what is still queued for the peer
// and keeps what already went.
func TestRevokingFeedbackCancelsQueuedKeepsReceipts(t *testing.T) {
	t.Parallel()

	st := testStore(t)
	peer, _ := st.CreatePairedPeer("inst-dave", "dave", "http://dave.example", "pk")
	a, _ := st.CreateFeedback(Feedback{UID: "a", Body: "one"}, map[int64]string{peer.ID: "note"})
	b, _ := st.CreateFeedback(Feedback{UID: "b", Body: "two"}, map[int64]string{peer.ID: "note"})
	st.SettleFeedbackDeliveries(peer.ID, []int64{a.ID}, nil, "")
	if err := st.EditFedPeer(peer.ID, FedPeerEdit{Capabilities: []FedCapabilityEdit{
		{Capability: CapFeedbackIngest, Direction: DirUse, Enabled: false},
	}}); err != nil {
		t.Fatal(err)
	}
	if ds, _ := st.FeedbackDeliveries(a.ID); len(ds) != 1 || ds[0].State != "delivered" {
		t.Fatalf("receipt lost: %+v", ds)
	}
	if ds, _ := st.FeedbackDeliveries(b.ID); len(ds) != 0 {
		t.Fatalf("queued delivery survived cancel: %+v", ds)
	}
}

// A receiver's word about a report comes home through the heartbeat and is
// mirrored once per stamp, on the row it is about and no other.
func TestFeedbackStatusFollowsThePeer(t *testing.T) {
	t.Parallel()

	st := testStore(t)
	dave, _ := st.CreatePairedPeer("inst-dave", "dave", "http://dave.example", "pk")
	erin, _ := st.CreatePairedPeer("inst-erin", "erin", "", "pk2")
	st.SetFedCapability(dave.ID, CapFeedbackIngest, DirUse, `{"mode":"note"}`)

	// Two local reports: one delivered to dave, one still queued.
	sent, _ := st.CreateFeedback(Feedback{UID: "u1", Body: "one"}, map[int64]string{dave.ID: "note"})
	queued, _ := st.CreateFeedback(Feedback{UID: "u2", Body: "two"}, map[int64]string{dave.ID: "note"})
	st.SettleFeedbackDeliveries(dave.ID, []int64{sent.ID}, nil, "")

	// Nothing to tell dave yet: no relayed report here has changed.
	if got, _ := st.PeerFeedbackStatuses(dave.ID, 10); len(got) != 0 {
		t.Fatalf("statuses before any change = %+v", got)
	}

	closed := FeedbackStatus{UID: "u1", Status: "closed", ChangedAt: "2026-09-15T10:00:00Z"}
	f, prev, err := st.ApplyPeerFeedbackStatus(dave.ID, closed)
	if err != nil || f == nil || prev != "open" {
		t.Fatalf("apply = %+v %q %v", f, prev, err)
	}
	// Stored in the one fixed-width form every status stamp takes.
	if want := "2026-09-15T10:00:00.000Z"; f.Status != "closed" || f.ClosedAt != want || f.StatusAt != want || f.StatusByPeerID != dave.ID {
		t.Fatalf("mirrored = %+v", f)
	}
	// The same stamp again is nothing new.
	if f, _, _ := st.ApplyPeerFeedbackStatus(dave.ID, closed); f != nil {
		t.Fatal("a notice already applied should not apply twice")
	}
	// A report the peer never received, or a peer that never received it,
	// cannot be closed on its word.
	if f, _, _ := st.ApplyPeerFeedbackStatus(dave.ID, FeedbackStatus{UID: "u2", Status: "closed", ChangedAt: "2026-09-15T10:00:00Z"}); f != nil {
		t.Fatal("an undelivered report closed on the peer's word")
	}
	if f, _, _ := st.ApplyPeerFeedbackStatus(erin.ID, FeedbackStatus{UID: "u1", Status: "open", ChangedAt: "2026-09-15T11:00:00Z"}); f != nil {
		t.Fatal("a peer that never held the report changed it")
	}
	if got, _ := st.FeedbackByID(queued.ID); got.Status != "open" {
		t.Fatalf("queued report = %+v", got)
	}

	// Reopened here, the person's word stands against the same closure.
	if err := st.SetFeedbackStatus(sent.ID, "open"); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.FeedbackByID(sent.ID); got.Status != "open" || got.StatusByPeerID != 0 || got.StatusAt == "" || got.ClosedAt != "" {
		t.Fatalf("reopened here = %+v", got)
	}
	if f, _, _ := st.ApplyPeerFeedbackStatus(dave.ID, closed); f != nil {
		t.Fatal("the old closure overruled a local reopen")
	}
	// A fresh closure from dave does apply.
	again := FeedbackStatus{UID: "u1", Status: "closed", ChangedAt: "2026-09-15T12:00:00Z"}
	if f, prev, _ := st.ApplyPeerFeedbackStatus(dave.ID, again); f == nil || prev != "open" || f.Status != "closed" {
		t.Fatalf("second closure = %+v %q", f, prev)
	}

	// The other direction: what this instance tells dave about dave's reports.
	st.IngestPeerFeedback(dave.ID, Feedback{UID: "d1", Body: "photos are slow", CreatedAt: "2026-09-10T10:00:00Z"})
	st.IngestPeerFeedback(dave.ID, Feedback{UID: "d2", Body: "and albums", CreatedAt: "2026-09-10T10:00:00Z"})
	relayed, _ := st.ListFeedback("open", 10)
	var d1 Feedback
	for _, r := range relayed {
		if r.UID == "d1" && r.PeerID == dave.ID {
			d1 = r
		}
	}
	if err := st.SetFeedbackStatus(d1.ID, "closed"); err != nil {
		t.Fatal(err)
	}
	got, _ := st.PeerFeedbackStatuses(dave.ID, 10)
	full, _ := st.FeedbackByID(d1.ID)
	if len(got) != 1 || got[0].UID != "d1" || got[0].Status != "closed" || got[0].ChangedAt != full.StatusAt || full.StatusAt == "" {
		t.Fatalf("statuses for dave = %+v (row %+v)", got, full)
	}
	if erins, _ := st.PeerFeedbackStatuses(erin.ID, 10); len(erins) != 0 {
		t.Fatalf("erin hears about dave's report: %+v", erins)
	}
}

// A peer's status stamp is held to this clock: far ahead is refused (it
// would outrank every later status), a little ahead is taken as now, and
// nothing applies once this side stops sending that peer its reports.
func TestPeerFeedbackStampsAreHeldToThisClock(t *testing.T) {
	t.Parallel()

	st := testStore(t)
	dave, _ := st.CreatePairedPeer("inst-dave", "dave", "http://dave.example", "pk")
	st.SetFedCapability(dave.ID, CapFeedbackIngest, DirUse, `{"mode":"note"}`)
	r, _ := st.CreateFeedback(Feedback{UID: "u1", Body: "one"}, map[int64]string{dave.ID: "note"})
	st.SettleFeedbackDeliveries(dave.ID, []int64{r.ID}, nil, "")

	for _, stamp := range []string{"9999-12-31T23:59:59Z", "~", "not a time", time.Now().Add(time.Hour).UTC().Format(time.RFC3339)} {
		if f, _, _ := st.ApplyPeerFeedbackStatus(dave.ID, FeedbackStatus{UID: "u1", Status: "closed", ChangedAt: stamp}); f != nil {
			t.Fatalf("stamp %q applied", stamp)
		}
	}
	ahead := time.Now().Add(2 * time.Minute).UTC().Format(time.RFC3339)
	f, _, _ := st.ApplyPeerFeedbackStatus(dave.ID, FeedbackStatus{UID: "u1", Status: "closed", ChangedAt: ahead})
	if f == nil || f.StatusAt > time.Now().UTC().Format(statusStampLayout) {
		t.Fatalf("a slightly-ahead stamp was not clamped to now: %+v", f)
	}
	// Later word still lands: nothing from the future is holding it back.
	time.Sleep(1100 * time.Millisecond)
	if f, _, _ := st.ApplyPeerFeedbackStatus(dave.ID, FeedbackStatus{UID: "u1", Status: "open", ChangedAt: time.Now().UTC().Format(time.RFC3339)}); f == nil || f.Status != "open" {
		t.Fatalf("a later status: %+v", f)
	}

	if err := st.EditFedPeer(dave.ID, FedPeerEdit{Capabilities: []FedCapabilityEdit{
		{Capability: CapFeedbackIngest, Direction: DirUse, Enabled: false}}}); err != nil {
		t.Fatal(err)
	}
	if f, _, _ := st.ApplyPeerFeedbackStatus(dave.ID, FeedbackStatus{UID: "u1", Status: "closed", ChangedAt: time.Now().UTC().Format(time.RFC3339)}); f != nil {
		t.Fatal("a status applied after sending to that peer was turned off")
	}
}
