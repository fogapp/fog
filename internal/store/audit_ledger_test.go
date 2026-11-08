package store

import "testing"

// The per-peer ledger finds a peer's crossings whether the row was written
// with a JSON detail or, before that, with the bare name or id as detail.
func TestFederationLedgerMatchesOldAndNewDetails(t *testing.T) {
	t.Parallel()

	s := testStore(t)
	// Old-style and JSON details for aunt (peer id 7), a crossing she made
	// herself, then two rows for other peers that must stay out.
	rows := []struct{ actor, action, subject, detail string }{
		{"admin", "files.subscribe", "Peers/aunt/Photos", "aunt"},
		{"admin", "files.subscribe", "Peers/aunt/Docs", AuditDetail(map[string]any{"peer": "aunt"})},
		{"admin", "files.share.peer", "Photos", "7"},
		{"admin", "files.share.peer", "Music", AuditDetail(map[string]any{"peerId": int64(7)})},
		{FederationActor("aunt"), "files.contribution", "Photos/x.jpg", ""},
		{"admin", "files.subscribe", "Peers/uncle/Photos", AuditDetail(map[string]any{"peer": "uncle"})},
		{"admin", "files.share.peer", "Video", AuditDetail(map[string]any{"peerId": int64(8)})},
	}
	for _, r := range rows {
		if err := s.Audit(r.actor, r.action, r.subject, r.detail); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.FederationLedger(50, "aunt", "7")
	if err != nil {
		t.Fatal(err)
	}
	subjects := map[string]bool{}
	for _, e := range got {
		subjects[e.Subject] = true
	}
	for _, want := range []string{"Peers/aunt/Photos", "Peers/aunt/Docs", "Photos", "Music", "Photos/x.jpg"} {
		if !subjects[want] {
			t.Errorf("missing %q in %v", want, subjects)
		}
	}
	if len(got) != 5 {
		t.Errorf("got %d rows, want 5: %v", len(got), subjects)
	}
}
