package store

import (
	"fmt"
	"testing"
	"time"
)

func testApp(t *testing.T, s *Store) *App {
	t.Helper()
	app, err := s.CreateApp("manor")
	if err != nil {
		t.Fatal(err)
	}
	return app
}

func ingest(fp, release, eventID string) ErrorIngest {
	return ErrorIngest{
		Fingerprint: fp, ErrorClass: "PG::UndefinedColumn",
		Title:   "PG::UndefinedColumn: column posts.slug does not exist",
		Culprit: "PostsController#show", Level: "error",
		EventID: eventID, Release: release, Environment: "production",
		Txn: "PostsController#show", Payload: `{"platform":"ruby"}`,
	}
}

func TestIngestErrorNewAndRepeat(t *testing.T) {
	s := testStore(t)
	app := testApp(t, s)

	res, err := s.IngestError(app.ID, app.Name, ingest("fp1", "abc", "e1"))
	if err != nil {
		t.Fatal(err)
	}
	if !res.New || res.Regressed || !res.Stored {
		t.Fatalf("first occurrence: %+v", res)
	}
	if res.Group.EventsCount != 1 || res.Group.Status != "open" {
		t.Fatalf("group after first: %+v", res.Group)
	}

	res, err = s.IngestError(app.ID, app.Name, ingest("fp1", "abc", "e2"))
	if err != nil {
		t.Fatal(err)
	}
	if res.New || res.Regressed {
		t.Fatalf("repeat should be neither new nor regressed: %+v", res)
	}
	if res.Group.EventsCount != 2 {
		t.Fatalf("count = %d, want 2", res.Group.EventsCount)
	}
}

func TestIngestErrorDedupesByEventID(t *testing.T) {
	s := testStore(t)
	app := testApp(t, s)

	s.IngestError(app.ID, app.Name, ingest("fp1", "abc", "e1"))
	res, err := s.IngestError(app.ID, app.Name, ingest("fp1", "abc", "e1")) // transport retry
	if err != nil {
		t.Fatal(err)
	}
	if res.Stored {
		t.Fatal("retried event_id must not store twice")
	}
	if res.Group.EventsCount != 1 {
		t.Fatalf("count = %d after retry, want 1 (no double count)", res.Group.EventsCount)
	}
}

func TestStaleRetryLeavesResolvedGroupUntouched(t *testing.T) {
	s := testStore(t)
	app := testApp(t, s)

	res, _ := s.IngestError(app.ID, app.Name, ingest("fp1", "abc", "e1"))
	if err := s.ResolveErrorGroup(res.Group.ID, "abc"); err != nil {
		t.Fatal(err)
	}
	before, err := s.ErrorGroupByID(res.Group.ID)
	if err != nil {
		t.Fatal(err)
	}

	// A transport retry of the old event arrives after the resolve. It must
	// leave the group row byte-identical: a dedup that reports Stored:false
	// while flipping status or refreshing last_seen would be a silent state
	// change — the green-lie class this project exists to avoid.
	in := ingest("fp1", "abc", "e1")
	in.Release = "" // empty release would otherwise err toward regressing
	in.Title = "PG::UndefinedColumn: stale retry title"
	res2, err := s.IngestError(app.ID, app.Name, in)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Stored || res2.New || res2.Regressed {
		t.Fatalf("retry must be a no-op: %+v", res2)
	}
	after, err := s.ErrorGroupByID(res.Group.ID)
	if err != nil {
		t.Fatal(err)
	}
	if *after != *before {
		t.Fatalf("retry changed the group row:\nbefore %+v\nafter  %+v", before, after)
	}
}

func TestResolveAndRegression(t *testing.T) {
	s := testStore(t)
	app := testApp(t, s)

	res, _ := s.IngestError(app.ID, app.Name, ingest("fp1", "abc", "e1"))
	if err := s.ResolveErrorGroup(res.Group.ID, "abc"); err != nil {
		t.Fatal(err)
	}

	// Same release: the fix isn't deployed yet — not a regression.
	res2, err := s.IngestError(app.ID, app.Name, ingest("fp1", "abc", "e2"))
	if err != nil {
		t.Fatal(err)
	}
	if res2.Regressed {
		t.Fatal("same release as resolve must not regress")
	}
	if res2.Group.Status != "resolved" {
		t.Fatalf("status = %s, want resolved", res2.Group.Status)
	}

	// New release: the fix shipped and the error came back.
	res3, err := s.IngestError(app.ID, app.Name, ingest("fp1", "def", "e3"))
	if err != nil {
		t.Fatal(err)
	}
	if !res3.Regressed || res3.Group.Status != "regressed" {
		t.Fatalf("new release on resolved group: %+v", res3)
	}
}

func TestIgnoreUntilCount(t *testing.T) {
	s := testStore(t)
	app := testApp(t, s)

	res, _ := s.IngestError(app.ID, app.Name, ingest("fp1", "abc", "e1"))
	if err := s.IgnoreErrorGroup(res.Group.ID, "", 3); err != nil {
		t.Fatal(err)
	}

	res2, _ := s.IngestError(app.ID, app.Name, ingest("fp1", "abc", "e2"))
	if res2.Group.Status != "ignored" || res2.New {
		t.Fatalf("below threshold should stay ignored: %+v", res2)
	}
	res3, _ := s.IngestError(app.ID, app.Name, ingest("fp1", "abc", "e3"))
	if res3.Group.Status != "open" || !res3.New {
		t.Fatalf("threshold reached should reopen and notify: %+v", res3)
	}
}

func TestIgnoreUntilTime(t *testing.T) {
	s := testStore(t)
	app := testApp(t, s)

	res, _ := s.IngestError(app.ID, app.Name, ingest("fp1", "abc", "e1"))
	past := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	if err := s.IgnoreErrorGroup(res.Group.ID, past, 0); err != nil {
		t.Fatal(err)
	}
	res2, _ := s.IngestError(app.ID, app.Name, ingest("fp1", "abc", "e2"))
	if res2.Group.Status != "open" || !res2.New {
		t.Fatalf("expired ignore should reopen: %+v", res2)
	}
}

func TestEventsPerGroupCap(t *testing.T) {
	s := testStore(t)
	app := testApp(t, s)

	for i := 0; i < eventsPerGroupCap+10; i++ {
		if _, err := s.IngestError(app.ID, app.Name, ingest("fp1", "abc", fmt.Sprintf("e%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	g, err := s.ErrorGroupByID(1)
	if err != nil {
		t.Fatal(err)
	}
	if g.EventsCount != int64(eventsPerGroupCap+10) {
		t.Fatalf("counter = %d, want %d (counters are never capped)", g.EventsCount, eventsPerGroupCap+10)
	}
	events, err := s.ListErrorEvents(g.ID, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != eventsPerGroupCap {
		t.Fatalf("stored = %d, want cap %d", len(events), eventsPerGroupCap)
	}
	// The survivors are the newest.
	if events[0].EventID != fmt.Sprintf("e%d", eventsPerGroupCap+9) {
		t.Fatalf("newest stored = %s", events[0].EventID)
	}
}

func TestPruneErrorEvents(t *testing.T) {
	s := testStore(t)
	app := testApp(t, s)

	s.IngestError(app.ID, app.Name, ingest("fp1", "abc", "e1"))
	future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	n, err := s.PruneErrorEvents(future)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("pruned %d, want 1", n)
	}
	// The group survives event pruning; only stale resolved groups go.
	if _, err := s.ErrorGroupByID(1); err != nil {
		t.Fatal("group should survive event pruning")
	}
	if n, _ := s.PruneStaleErrorGroups(future); n != 0 {
		t.Fatal("open group must never be pruned")
	}
	s.ResolveErrorGroup(1, "abc")
	if n, _ := s.PruneStaleErrorGroups(future); n != 1 {
		t.Fatal("stale resolved group should be pruned")
	}
}

func TestErrorGroupsCascadeOnAppDelete(t *testing.T) {
	s := testStore(t)
	app := testApp(t, s)
	s.IngestError(app.ID, app.Name, ingest("fp1", "abc", "e1"))
	if err := s.DeleteApp(app.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ErrorGroupByID(1); err != ErrNotFound {
		t.Fatalf("group should cascade with app: %v", err)
	}
	if _, err := s.ErrorEventByID(1); err != ErrNotFound {
		t.Fatalf("events should cascade with group: %v", err)
	}
}

func TestCountOpenErrorGroups(t *testing.T) {
	s := testStore(t)
	app := testApp(t, s)
	s.IngestError(app.ID, app.Name, ingest("fp1", "abc", "e1"))
	s.IngestError(app.ID, app.Name, ingest("fp2", "abc", "e2"))
	res, _ := s.IngestError(app.ID, app.Name, ingest("fp3", "abc", "e3"))
	s.ResolveErrorGroup(res.Group.ID, "abc")

	counts, err := s.CountOpenErrorGroups()
	if err != nil {
		t.Fatal(err)
	}
	if counts[app.ID] != 2 {
		t.Fatalf("open = %d, want 2", counts[app.ID])
	}
}

// The "open" filter must list exactly what the badge counts, regressed
// groups included, or the badge points at an empty page.
func TestListOpenErrorGroupsMatchesBadge(t *testing.T) {
	s := testStore(t)
	app := testApp(t, s)
	s.IngestError(app.ID, app.Name, ingest("fp1", "abc", "e1"))
	res, _ := s.IngestError(app.ID, app.Name, ingest("fp2", "abc", "e2"))
	s.ResolveErrorGroup(res.Group.ID, "abc")
	s.IngestError(app.ID, app.Name, ingest("fp2", "def", "e3")) // regresses
	res, _ = s.IngestError(app.ID, app.Name, ingest("fp3", "abc", "e4"))
	s.ResolveErrorGroup(res.Group.ID, "abc")

	counts, _ := s.CountOpenErrorGroups()
	open, err := s.ListErrorGroups(app.ID, "open", 10)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(open)) != counts[app.ID] || len(open) != 2 {
		t.Fatalf("open list = %d, badge = %d, want 2", len(open), counts[app.ID])
	}
	if regressed, _ := s.ListErrorGroups(app.ID, "regressed", 10); len(regressed) != 1 {
		t.Fatalf("regressed list = %d, want 1", len(regressed))
	}
}
