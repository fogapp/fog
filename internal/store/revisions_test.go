package store

import "testing"

// LatestRevisionSpecs must return exactly each app's newest spec, and no
// entry for an app with no revisions — the batch shape of LatestRevision's
// ErrNotFound.
func TestLatestRevisionSpecs(t *testing.T) {
	s := testStore(t)
	one, err := s.CreateApp("one")
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range []string{"spec-1", "spec-2", "spec-3"} {
		if _, err := s.CreateRevision(one.ID, spec, "", "test"); err != nil {
			t.Fatal(err)
		}
	}
	two, err := s.CreateApp("two")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRevision(two.ID, "solo", "", "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateApp("bare"); err != nil { // never revised
		t.Fatal(err)
	}

	got, err := s.LatestRevisionSpecs()
	if err != nil {
		t.Fatal(err)
	}
	want := map[int64]string{one.ID: "spec-3", two.ID: "solo"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for id, spec := range want {
		if got[id] != spec {
			t.Errorf("app %d: got %q, want %q", id, got[id], spec)
		}
	}
}

// The latest-spec query must ride the (app_id, rev) index rather than scan
// every revision's spec: it runs on every dashboard poll.
func TestLatestRevisionSpecsAvoidsFullScan(t *testing.T) {
	s := testStore(t)
	rows, err := s.db.Query(`EXPLAIN QUERY PLAN SELECT r.app_id, r.spec FROM
		(SELECT app_id, MAX(rev) AS rev FROM app_revisions GROUP BY app_id) AS latest
		JOIN app_revisions AS r ON r.app_id = latest.app_id AND r.rev = latest.rev`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatal(err)
		}
		if detail == "SCAN r" || detail == "SCAN app_revisions" {
			t.Errorf("plan reads every revision row: %q", detail)
		}
	}
}

// PruneRevisions keeps the newest keep revisions and anything still
// referenced, and drops the rest.
func TestPruneRevisions(t *testing.T) {
	s := testStore(t)
	app, err := s.CreateApp("app")
	if err != nil {
		t.Fatal(err)
	}
	ids := map[int64]int64{} // rev → row id
	for i := 0; i < 10; i++ {
		r, err := s.CreateRevision(app.ID, "spec", "", "test")
		if err != nil {
			t.Fatal(err)
		}
		ids[r.Rev] = r.ID
	}
	// Rev 2 has a deploy row, rev 3 a version-history row, rev 4 is current.
	if _, err := s.CreateDeploy(app.ID, ids[2], "test"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordAppVersion(app.ID, "image", "sha256:x", "img", 3, "test"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAppCurrentRev(app.ID, 4); err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateApp("other")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRevision(other.ID, "solo", "", "test"); err != nil {
		t.Fatal(err)
	}

	n, err := s.PruneRevisions(3) // keep revs 8–10 (rev <= 10-3 is eligible)
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 { // revs 1, 5, 6, 7
		t.Errorf("pruned %d, want 4", n)
	}
	for rev := int64(1); rev <= 10; rev++ {
		_, err := s.RevisionByRev(app.ID, rev)
		kept := err == nil
		want := rev >= 8 || rev == 2 || rev == 3 || rev == 4
		if kept != want {
			t.Errorf("rev %d kept=%v, want %v", rev, kept, want)
		}
	}
	if _, err := s.RevisionByRev(other.ID, 1); err != nil {
		t.Errorf("another app's only revision went: %v", err)
	}
	// Numbering carries on from the newest, which always survives.
	r, err := s.CreateRevision(app.ID, "next", "", "test")
	if err != nil {
		t.Fatal(err)
	}
	if r.Rev != 11 {
		t.Errorf("next rev = %d, want 11", r.Rev)
	}
}
