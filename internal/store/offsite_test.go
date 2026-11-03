package store

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

// OffsiteStates reports nothing before any target, pending for a subject
// with no copy, and the best copy across active targets, a retired
// target's ignored.
func TestOffsiteStatesOf(t *testing.T) {
	s := testStore(t)
	if st, _ := s.OffsiteStatesOf(OffsiteKindApp); st.Of("1") != "" {
		t.Errorf("no targets: %q, want empty", st.Of("1"))
	}

	a, err := s.CreateOffsiteTarget(OffsiteTarget{Name: "a", Endpoint: "https://a", Bucket: "a", LockerID: "la"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateOffsiteTarget(OffsiteTarget{Name: "b", Endpoint: "https://b", Bucket: "b", LockerID: "lb"})
	if err != nil {
		t.Fatal(err)
	}
	old, err := s.CreateOffsiteTarget(OffsiteTarget{Name: "old", Endpoint: "https://o", Bucket: "o", LockerID: "lo", Role: "retired"})
	if err != nil {
		t.Fatal(err)
	}
	put := func(target int64, subject, state, verified string) {
		t.Helper()
		if err := s.UpsertOffsiteCopy(OffsiteCopy{TargetID: target, Kind: "app", Subject: subject, Key: subject, State: state, VerifiedAt: verified}); err != nil {
			t.Fatal(err)
		}
	}
	put(a, "1", "uploading", "")
	put(b, "1", "stored", "")
	put(a, "2", "stored", "2026-09-01T00:00:00Z")
	put(a, "3", "hidden", "")
	put(old, "4", "stored", "2026-09-01T00:00:00Z")
	// A copy of another kind says nothing about apps.
	if err := s.UpsertOffsiteCopy(OffsiteCopy{TargetID: a, Kind: "files-snapshot", Subject: "5", Key: "f5", State: "stored"}); err != nil {
		t.Fatal(err)
	}
	batch, err := s.OffsiteStatesOf(OffsiteKindApp)
	if err != nil {
		t.Fatal(err)
	}
	for subj, want := range map[string]string{"1": "stored", "2": "verified", "3": "pending", "4": "pending", "5": "pending"} {
		if got := batch.Of(subj); got != want {
			t.Errorf("subject %s: %q, want %q", subj, got, want)
		}
	}
}

// The all-targets totals answer what the per-target read does for each
// target, and a target with no copies is absent (zero totals).
func TestAllOffsiteCopyTotalsMatchesPerTarget(t *testing.T) {
	s := testStore(t)
	var ids []int64
	for _, n := range []string{"a", "b", "empty"} {
		id, err := s.CreateOffsiteTarget(OffsiteTarget{Name: n, Endpoint: "https://" + n, Bucket: n, LockerID: "l" + n})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	for i, c := range []OffsiteCopy{
		{TargetID: ids[0], State: "stored", Bytes: 10},
		{TargetID: ids[0], State: "stored", Bytes: 5},
		{TargetID: ids[0], State: "hidden", Bytes: 7},
		{TargetID: ids[0], State: "uploading", Bytes: 1},
		{TargetID: ids[1], State: "stored", Bytes: 100},
	} {
		c.Kind, c.Subject, c.Key = "app", strconv.Itoa(i), strconv.Itoa(i)
		if err := s.UpsertOffsiteCopy(c); err != nil {
			t.Fatal(err)
		}
	}
	all, err := s.AllOffsiteCopyTotals()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		one, err := s.OffsiteCopyTotals(id)
		if err != nil {
			t.Fatal(err)
		}
		if all[id] != one {
			t.Errorf("target %d: all %+v, one %+v", id, all[id], one)
		}
	}
	if _, ok := all[ids[2]]; ok {
		t.Error("a target with no copies has an entry")
	}
	if got := all[ids[0]]; got.Stored != 2 || got.StoredBytes != 15 || got.HiddenBytes != 7 || got.Uploading != 1 {
		t.Errorf("target a = %+v", got)
	}
}

// The slim listing leaves the document out, keeps everything else, and
// filters on state in SQL.
func TestListOffsiteCopiesSlim(t *testing.T) {
	s := testStore(t)
	tid, err := s.CreateOffsiteTarget(OffsiteTarget{Name: "a", Endpoint: "https://a", Bucket: "a", LockerID: "la"})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []OffsiteCopy{
		{Kind: "files-snapshot", Subject: "1", Key: "k1", State: "stored", Doc: `{"files":[]}`, Summary: `{"stamp":"s1"}`, Bytes: 10},
		{Kind: "files-snapshot", Subject: "2", Key: "k2", State: "hidden", Doc: `{"files":[]}`, HiddenAt: "2026-01-01T00:00:00Z"},
		{Kind: "files-blob", Subject: "b", Key: "k3", State: "uploading", UploadID: "u"},
		{Kind: "kit", Subject: "kit", Key: "k4", State: "stored"},
	} {
		c.TargetID = tid
		if err := s.UpsertOffsiteCopy(c); err != nil {
			t.Fatal(err)
		}
	}
	keys := func(cs []OffsiteCopy) string {
		var out []string
		for _, c := range cs {
			out = append(out, c.Key)
		}
		return strings.Join(out, ",")
	}

	all, err := s.ListOffsiteCopiesSlim(tid, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := keys(all); got != "k4,k3,k2,k1" {
		t.Errorf("all = %s", got)
	}
	for _, c := range all {
		if c.Doc != "" {
			t.Errorf("%s carries its doc", c.Key)
		}
	}
	if all[3].Summary != `{"stamp":"s1"}` || all[3].Bytes != 10 || all[2].HiddenAt != "2026-01-01T00:00:00Z" || all[1].UploadID != "u" {
		t.Errorf("slim rows lost fields: %+v", all)
	}

	stored, _ := s.ListOffsiteCopiesSlim(tid, "", "stored")
	if got := keys(stored); got != "k4,k1" {
		t.Errorf("stored = %s", got)
	}
	both, _ := s.ListOffsiteCopiesSlim(tid, "files-snapshot", "stored", "hidden")
	if got := keys(both); got != "k2,k1" {
		t.Errorf("stored or hidden snapshots = %s", got)
	}
	none, _ := s.ListOffsiteCopiesSlim(tid, "app", "stored")
	if none == nil || len(none) != 0 {
		t.Errorf("no rows = %#v, want empty", none)
	}
	// Several kinds at once, never the blobs unless asked.
	s.UpsertOffsiteCopy(OffsiteCopy{TargetID: tid, Kind: "files-blob", Subject: "b2", Key: "k0", State: "stored"})
	kinds, _ := s.ListOffsiteCopiesOfKinds(tid, []string{"files-snapshot", "kit"}, "stored")
	if got := keys(kinds); got != "k4,k1" {
		t.Errorf("stored snapshots and kits = %s", got)
	}
	if got, _ := s.ListOffsiteCopiesOfKinds(tid, nil, "stored"); got == nil || len(got) != 0 {
		t.Errorf("no kinds = %#v, want empty", got)
	}

	// The full listing still has the document.
	full, _ := s.ListOffsiteCopies(tid, "files-snapshot")
	if len(full) != 2 || full[1].Doc == "" {
		t.Errorf("full listing = %+v", full)
	}
}

// Migration 0047 fills a Files snapshot's summary from its manifest, and
// leaves a row it cannot read alone.
func TestOffsiteSummaryBackfill(t *testing.T) {
	s := testStore(t)
	tid, err := s.CreateOffsiteTarget(OffsiteTarget{Name: "a", Endpoint: "https://a", Bucket: "a", LockerID: "la"})
	if err != nil {
		t.Fatal(err)
	}
	s.UpsertOffsiteCopy(OffsiteCopy{TargetID: tid, Kind: "files-snapshot", Subject: "1", Key: "k1", State: "stored",
		Doc: `{"format":"f","stamp":"20260101-010000","created_at":"2026-01-01T01:00:00Z","files":[{"path":"a"}],"bytes":42,"count":1}`})
	s.UpsertOffsiteCopy(OffsiteCopy{TargetID: tid, Kind: "files-snapshot", Subject: "2", Key: "k2", State: "stored", Doc: `not json`})
	s.UpsertOffsiteCopy(OffsiteCopy{TargetID: tid, Kind: "app", Subject: "3", Key: "k3", State: "stored", Doc: `{"app":"x"}`})

	raw, err := os.ReadFile("migrations/0047_offsite_copy_summary.sql")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	if _, err := s.db.Exec(src[strings.Index(src, "UPDATE"):]); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, c := range mustSlim(t, s, tid) {
		got[c.Subject] = c.Summary
	}
	if want := `{"stamp":"20260101-010000","created_at":"2026-01-01T01:00:00Z","files":1,"bytes":42}`; got["1"] != want {
		t.Errorf("backfilled summary = %s, want %s", got["1"], want)
	}
	if got["2"] != "" || got["3"] != "" {
		t.Errorf("unreadable or non-snapshot rows got summaries: %q %q", got["2"], got["3"])
	}
}

func mustSlim(t *testing.T, s *Store, tid int64) []OffsiteCopy {
	t.Helper()
	cs, err := s.ListOffsiteCopiesSlim(tid, "")
	if err != nil {
		t.Fatal(err)
	}
	return cs
}
