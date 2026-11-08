package store

import (
	"fmt"
	"testing"
	"time"
)

func TestPeerCRUDAndCascade(t *testing.T) {
	t.Parallel()

	s := testStore(t)
	p, err := s.CreatePairedPeer("01ARZ3NDEKTSV4RRFFQ69G5FAV", "dave", "https://fog.dave.house", "pubkey")
	if err != nil {
		t.Fatal(err)
	}
	if p.URL != "https://fog.dave.house" || p.PublicKey != "pubkey" {
		t.Fatalf("paired peer: %+v", p)
	}
	if _, err := s.CreatePairedPeer("01OTHER", "dave", "", "k"); err == nil {
		t.Fatal("duplicate peer name must fail")
	}
	if _, err := s.CreatePairedPeer("01ARZ3NDEKTSV4RRFFQ69G5FAV", "dave2", "", "k"); err == nil {
		t.Fatal("duplicate instance_id must fail")
	}
	byInst, err := s.FedPeerByInstanceID("01ARZ3NDEKTSV4RRFFQ69G5FAV")
	if err != nil || byInst.ID != p.ID {
		t.Fatalf("by instance: %v", err)
	}
	if _, err := s.FedPeerByInstanceID("nope"); err != ErrNotFound {
		t.Fatalf("unknown instance: %v", err)
	}

	// Re-pairing refreshes identity in place, keeping the row and history.
	res, err := s.IngestPeerError(p.ID, "manor", ingest("fp0", "abc", "e0"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RefreshPairedPeer(p.ID, "pubkey", "https://new.dave.house"); err != nil {
		t.Fatal(err)
	}
	up, _ := s.FedPeerByID(p.ID)
	if up.URL != "https://new.dave.house" || up.PublicKey != "pubkey" {
		t.Fatalf("identity refresh: %+v", up)
	}
	// An empty address leaves the known one; another key changes nothing.
	if err := s.RefreshPairedPeer(p.ID, "pubkey", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.RefreshPairedPeer(p.ID, "newkey", "https://evil.example"); err != ErrPeerKeyMismatch {
		t.Fatalf("refresh under another key: %v", err)
	}
	if up, _ = s.FedPeerByID(p.ID); up.URL != "https://new.dave.house" || up.PublicKey != "pubkey" {
		t.Fatalf("refused refresh changed the row: %+v", up)
	}
	if g, err := s.ErrorGroupByID(res.Group.ID); err != nil || g.PeerID != p.ID {
		t.Fatalf("refresh lost groups: %v", err)
	}

	// A peer's groups (and their events, and capabilities) go with it.
	if err := s.SetFedCapability(p.ID, CapFlareIngest, DirGrant, "{}"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteFedPeer(p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ErrorGroupByID(res.Group.ID); err != ErrNotFound {
		t.Fatalf("peer group should cascade: %v", err)
	}
	if caps, _ := s.ListFedCapabilities(p.ID); len(caps) != 0 {
		t.Fatalf("capabilities should cascade: %+v", caps)
	}
}

func TestFedCapabilities(t *testing.T) {
	t.Parallel()

	s := testStore(t)
	p, _ := s.CreatePairedPeer("01INST", "dave", "", "k")
	if err := s.SetFedCapability(p.ID, CapFlareIngest, DirUse, `{"mode":"redacted"}`); err != nil {
		t.Fatal(err)
	}
	cfg, ok, err := s.FedCapabilityConfig(p.ID, CapFlareIngest, DirUse)
	if err != nil || !ok || cfg != `{"mode":"redacted"}` {
		t.Fatalf("config: %v %v %s", err, ok, cfg)
	}
	// Upsert replaces config.
	s.SetFedCapability(p.ID, CapFlareIngest, DirUse, `{"mode":"full"}`)
	cfg, _, _ = s.FedCapabilityConfig(p.ID, CapFlareIngest, DirUse)
	if cfg != `{"mode":"full"}` {
		t.Fatalf("upsert: %s", cfg)
	}
	// Direction is part of the key.
	if _, ok, _ := s.FedCapabilityConfig(p.ID, CapFlareIngest, DirGrant); ok {
		t.Fatal("grant direction should be absent")
	}
	// Peers-with-capability drives the forwarding loop.
	p2, _ := s.CreatePairedPeer("01INST2", "erin", "", "k2")
	holders, _ := s.FedPeersWithCapability(CapFlareIngest, DirUse)
	if len(holders) != 1 || holders[0].ID != p.ID {
		t.Fatalf("holders: %+v", holders)
	}
	_ = p2
	if err := s.RemoveFedCapability(p.ID, CapFlareIngest, DirUse); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.FedCapabilityConfig(p.ID, CapFlareIngest, DirUse); ok {
		t.Fatal("capability should be removed")
	}
}

func TestFedInvites(t *testing.T) {
	t.Parallel()

	s := testStore(t)
	future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	inv, err := s.CreateFedInvite("digest-a", `["flare.ingest"]`, future)
	if err != nil {
		t.Fatal(err)
	}
	open, _ := s.ListOpenFedInvites()
	if len(open) != 1 || open[0].ID != inv.ID {
		t.Fatalf("open invites: %+v", open)
	}
	claimed, err := s.ClaimFedInvite("digest-a", "01INST")
	if err != nil || claimed.Grants != `["flare.ingest"]` {
		t.Fatalf("claim: %v %+v", err, claimed)
	}
	// Single use: a second claim fails.
	if _, err := s.ClaimFedInvite("digest-a", "01INST2"); err != ErrNotFound {
		t.Fatalf("second claim: %v", err)
	}
	// Expired invites can't be claimed.
	past := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	s.CreateFedInvite("digest-b", "[]", past)
	if _, err := s.ClaimFedInvite("digest-b", "01INST"); err != ErrNotFound {
		t.Fatalf("expired claim: %v", err)
	}
	// The open list sweeps claimed and expired rows.
	if open, _ := s.ListOpenFedInvites(); len(open) != 0 {
		t.Fatalf("sweep: %+v", open)
	}
}

func TestFedIdentityRow(t *testing.T) {
	t.Parallel()

	s := testStore(t)
	if _, err := s.FedIdentity(); err != ErrNotFound {
		t.Fatalf("empty identity: %v", err)
	}
	if err := s.SetFedIdentity("01INST", "pubkey"); err != nil {
		t.Fatal(err)
	}
	id, err := s.FedIdentity()
	if err != nil || id.InstanceID != "01INST" || id.PublicKey != "pubkey" {
		t.Fatalf("identity: %v %+v", err, id)
	}
}

func TestOutboxLifecycle(t *testing.T) {
	t.Parallel()

	s := testStore(t)
	p, _ := s.CreatePairedPeer("01OUT", "dave", "https://x", "k")
	other, _ := s.CreatePairedPeer("01OUT2", "erin", "https://y", "k2")
	for i := 0; i < 3; i++ {
		if err := s.EnqueueFedOutbox(p.ID, "flare", "manor", fmt.Sprintf(`{"n":%d}`, i)); err != nil {
			t.Fatal(err)
		}
	}
	// Queues are per peer: erin's queue stays empty.
	s.EnqueueFedOutbox(other.ID, "flare", "manor", `{"other":1}`)
	due, err := s.DueFedOutbox(p.ID, "flare", 10)
	if err != nil || len(due) != 3 {
		t.Fatalf("due: %v %d", err, len(due))
	}
	// Defer two into the future; only the third stays due.
	future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	if err := s.DeferFedOutbox([]int64{due[0].ID, due[1].ID}, future); err != nil {
		t.Fatal(err)
	}
	due2, _ := s.DueFedOutbox(p.ID, "flare", 10)
	if len(due2) != 1 || due2[0].ID != due[2].ID {
		t.Fatalf("after defer: %+v", due2)
	}
	if err := s.DeleteFedOutbox([]int64{due[2].ID}); err != nil {
		t.Fatal(err)
	}
	if left, _ := s.DueFedOutbox(p.ID, "flare", 10); len(left) != 0 {
		t.Fatalf("after delete: %d", len(left))
	}
	if otherDue, _ := s.DueFedOutbox(other.ID, "flare", 10); len(otherDue) != 1 {
		t.Fatalf("other peer's queue disturbed: %d", len(otherDue))
	}
	// Deleting a peer takes its queue with it.
	s.DeleteFedPeer(other.ID)
	if otherDue, _ := s.DueFedOutbox(other.ID, "flare", 10); len(otherDue) != 0 {
		t.Fatal("outbox should cascade with the peer")
	}
}

func TestOutboxCapDropsOldest(t *testing.T) {
	t.Parallel()

	s := testStore(t)
	p, _ := s.CreatePairedPeer("01CAP", "dave", "https://x", "k")
	for i := 0; i < outboxCap+20; i++ {
		if err := s.EnqueueFedOutbox(p.ID, "flare", "manor", fmt.Sprintf(`{"n":%d}`, i)); err != nil {
			t.Fatal(err)
		}
	}
	due, err := s.DueFedOutbox(p.ID, "flare", outboxCap*2)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != outboxCap {
		t.Fatalf("outbox rows = %d, want cap %d", len(due), outboxCap)
	}
	// The survivors are the newest.
	if due[0].Payload != fmt.Sprintf(`{"n":%d}`, 20) {
		t.Fatalf("oldest surviving payload: %s", due[0].Payload)
	}
}

func TestPeerAndLocalGroupsNeverMerge(t *testing.T) {
	t.Parallel()

	s := testStore(t)
	app := testApp(t, s)
	p, _ := s.CreatePairedPeer("01MERGE", "dave", "", "k")

	// Identical fingerprint, three owners: local, dave/manor, dave/other.
	local, err := s.IngestError(app.ID, app.Name, ingest("fp1", "abc", "e1"))
	if err != nil {
		t.Fatal(err)
	}
	peer1, err := s.IngestPeerError(p.ID, "manor", ingest("fp1", "def", "e2"))
	if err != nil {
		t.Fatal(err)
	}
	peer2, err := s.IngestPeerError(p.ID, "other", ingest("fp1", "def", "e3"))
	if err != nil {
		t.Fatal(err)
	}
	ids := map[int64]bool{local.Group.ID: true, peer1.Group.ID: true, peer2.Group.ID: true}
	if len(ids) != 3 {
		t.Fatalf("expected 3 distinct groups, got %v", ids)
	}
	// Repeat on the peer group folds into it, not into the local one.
	again, _ := s.IngestPeerError(p.ID, "manor", ingest("fp1", "def", "e4"))
	if again.Group.ID != peer1.Group.ID || again.Group.EventsCount != 2 {
		t.Fatalf("peer repeat: %+v", again.Group)
	}

	// The per-app listing stays local-only; the fleet listing sees all.
	appGroups, _ := s.ListErrorGroups(app.ID, "", 10)
	if len(appGroups) != 1 {
		t.Fatalf("app listing leaked peer groups: %d", len(appGroups))
	}
	all, _ := s.ListOpenErrorGroupsAll(10)
	if len(all) != 3 {
		t.Fatalf("fleet listing: %d, want 3", len(all))
	}
	if n, err := s.CountOpenErrorGroupsAll(); err != nil || n != 3 {
		t.Fatalf("fleet count: %d %v, want 3", n, err)
	}
	counts, _ := s.CountOpenErrorGroups()
	if counts[app.ID] != 1 {
		t.Fatalf("local badge count: %d, want 1", counts[app.ID])
	}
}

func TestPeerLabelShadowsNameForDisplay(t *testing.T) {
	t.Parallel()

	s := testStore(t)
	p, err := s.CreatePairedPeer("01ARZ3NDEKTSV4RRFFQ69G5FAV", "dave", "", "pubkey")
	if err != nil {
		t.Fatal(err)
	}
	// No label: the display name is the name paired under.
	if p.Label != "" || p.DisplayName() != "dave" {
		t.Fatalf("fresh peer: label %q display %q", p.Label, p.DisplayName())
	}

	if err := s.SetFedPeerLabel(p.ID, "Dave's basement"); err != nil {
		t.Fatal(err)
	}
	got, err := s.FedPeerByID(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DisplayName() != "Dave's basement" {
		t.Fatalf("labelled display name: %q", got.DisplayName())
	}
	// The label shadows the name; it does not replace it. Everything that
	// addresses the peer still finds it under the name.
	if got.Name != "dave" {
		t.Fatalf("label must not touch name: %q", got.Name)
	}
	if byName, err := s.FedPeerByInstanceID("01ARZ3NDEKTSV4RRFFQ69G5FAV"); err != nil || byName.Name != "dave" {
		t.Fatalf("lookup after label: %v", err)
	}
	// Labels address nothing, so two peers may share one.
	other, err := s.CreatePairedPeer("01OTHER", "dave-2", "", "k")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetFedPeerLabel(other.ID, "Dave's basement"); err != nil {
		t.Fatalf("duplicate label must be allowed: %v", err)
	}

	// Clearing goes back to the advertised name.
	if err := s.SetFedPeerLabel(p.ID, ""); err != nil {
		t.Fatal(err)
	}
	cleared, _ := s.FedPeerByID(p.ID)
	if cleared.Label != "" || cleared.DisplayName() != "dave" {
		t.Fatalf("cleared label: %+v", cleared)
	}

	// ListFedPeers reads the column too — a scan that forgot it would be
	// invisible everywhere else.
	if err := s.SetFedPeerLabel(p.ID, "Basement"); err != nil {
		t.Fatal(err)
	}
	peers, err := s.ListFedPeers()
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, q := range peers {
		if q.ID == p.ID {
			found = q.Label == "Basement"
		}
	}
	if !found {
		t.Fatalf("list did not carry the label: %+v", peers)
	}
}

// Withdrawing or narrowing a use row reaches what is already queued under
// the old consent: flare rows scrubbed for "full" don't ship after the
// operator switched to "redacted" (or off and on again), and a report
// queued with its dossier goes as a note once sending is narrowed to notes.
func TestUseConsentReachesQueuedRows(t *testing.T) {
	t.Parallel()

	s := testStore(t)
	p, _ := s.CreatePairedPeer("01INST", "dave", "https://fog.dave.house", "k")
	set := func(capability, config string, on bool) {
		t.Helper()
		if err := s.EditFedPeer(p.ID, FedPeerEdit{Capabilities: []FedCapabilityEdit{
			{Capability: capability, Direction: DirUse, Config: config, Enabled: on}}}); err != nil {
			t.Fatal(err)
		}
	}
	queued := func() int {
		t.Helper()
		rows, err := s.DueFedOutbox(p.ID, "flare", 100)
		if err != nil {
			t.Fatal(err)
		}
		return len(rows)
	}

	set(CapFlareIngest, `{"mode":"full"}`, true)
	s.EnqueueFedOutbox(p.ID, "flare", "feed", `{"vars":"secret"}`)
	set(CapFlareIngest, `{"mode":"full"}`, true) // unchanged: the queue stays
	if queued() != 1 {
		t.Fatal("re-saving the same consent dropped the queue")
	}
	set(CapFlareIngest, `{"mode":"redacted"}`, true)
	if queued() != 0 {
		t.Fatal("rows scrubbed for full survived a switch to redacted")
	}
	s.EnqueueFedOutbox(p.ID, "flare", "feed", `{}`)
	set(CapFlareIngest, "", false)
	if queued() != 0 {
		t.Fatal("rows survived forwarding being turned off")
	}

	set(CapFeedbackIngest, `{"mode":"full"}`, true)
	fb, err := s.CreateFeedback(Feedback{UID: "u1", Body: "hi", Dossier: `{"logs":[]}`}, map[int64]string{p.ID: "full"})
	if err != nil {
		t.Fatal(err)
	}
	set(CapFeedbackIngest, `{"mode":"note"}`, true)
	ds, _ := s.FeedbackDeliveries(fb.ID)
	if len(ds) != 1 || ds[0].Mode != "note" {
		t.Fatalf("a queued report kept its dossier after sending narrowed to notes: %+v", ds)
	}
	set(CapFeedbackIngest, `{"mode":"full"}`, true)
	if ds, _ = s.FeedbackDeliveries(fb.ID); ds[0].Mode != "note" {
		t.Fatal("widening upgraded a report written under narrower consent")
	}
}
