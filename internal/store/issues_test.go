package store

import "testing"

func finding(kind string, appID int64, subject, severity, title string) IssueFinding {
	return IssueFinding{Kind: kind, AppID: appID, Subject: subject, Severity: severity, Title: title, AppName: "feed"}
}

func TestSyncIssuesLifecycle(t *testing.T) {
	s := testStore(t)
	app, err := s.CreateApp("feed")
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	f := finding("pg-dump-skew", app.ID, "", "critical", "pg_dump is v15, server is v16")
	scope := []int64{app.ID}

	// First sweep: New.
	res, err := s.SyncIssues("pg-dump-skew", scope, []IssueFinding{f})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(res.New) != 1 || len(res.Recurred)+len(res.Reopened)+len(res.Resolved) != 0 {
		t.Fatalf("want 1 new, got %+v", res)
	}
	id := res.New[0].ID

	// Reproduce: no transitions, last_seen bumps.
	res, err = s.SyncIssues("pg-dump-skew", scope, []IssueFinding{f})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(res.New)+len(res.Recurred)+len(res.Reopened)+len(res.Resolved) != 0 {
		t.Fatalf("steady state should be silent, got %+v", res)
	}

	// Ack, reproduce unchanged: stays acked.
	if err := s.AckIssue(id); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if _, err = s.SyncIssues("pg-dump-skew", scope, []IssueFinding{f}); err != nil {
		t.Fatalf("sync: %v", err)
	}
	i, _ := s.IssueByID(id)
	if i.Status != "acked" {
		t.Fatalf("unchanged finding must not break ack, status = %s", i.Status)
	}

	// Details change: Reopened, ack cleared.
	f2 := f
	f2.Title = "pg_dump is v15, server is v17"
	res, err = s.SyncIssues("pg-dump-skew", scope, []IssueFinding{f2})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(res.Reopened) != 1 {
		t.Fatalf("changed details should reopen, got %+v", res)
	}
	i, _ = s.IssueByID(id)
	if i.Status != "open" || i.AckedAt != "" {
		t.Fatalf("reopen should clear ack, got status=%s ackedAt=%q", i.Status, i.AckedAt)
	}

	// Absent + evaluated: Resolved.
	res, err = s.SyncIssues("pg-dump-skew", scope, nil)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(res.Resolved) != 1 || res.Resolved[0].ID != id {
		t.Fatalf("absent finding should resolve, got %+v", res)
	}

	// Recur after resolve: Recurred, fresh generation.
	res, err = s.SyncIssues("pg-dump-skew", scope, []IssueFinding{f2})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(res.Recurred) != 1 {
		t.Fatalf("resolved finding reproducing should recur, got %+v", res)
	}
	i, _ = s.IssueByID(id)
	if i.Status != "open" || i.ResolvedAt != "" || i.AckedAt != "" {
		t.Fatalf("recur should reset resolution and ack, got %+v", i)
	}
}

func TestSyncIssuesSkippedScopeSurvives(t *testing.T) {
	s := testStore(t)
	a, _ := s.CreateApp("feed")
	b, _ := s.CreateApp("manor")
	fa := finding("container-drift", a.ID, "", "warning", "feed main container exited")
	fb := finding("container-drift", b.ID, "", "warning", "manor main container exited")
	if _, err := s.SyncIssues("container-drift", []int64{a.ID, b.ID}, []IssueFinding{fa, fb}); err != nil {
		t.Fatalf("sync: %v", err)
	}

	// Next sweep skips feed (mid-deploy): only manor's scope is evaluated,
	// and manor's issue cleared. Feed's issue must survive untouched.
	res, err := s.SyncIssues("container-drift", []int64{b.ID}, nil)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(res.Resolved) != 1 || res.Resolved[0].AppID != b.ID {
		t.Fatalf("only manor should resolve, got %+v", res.Resolved)
	}
	open, _ := s.ListIssues([]string{"open"}, 10)
	if len(open) != 1 || open[0].AppID != a.ID {
		t.Fatalf("feed's issue should stay open, got %+v", open)
	}
}

func TestSyncIssuesHostScope(t *testing.T) {
	s := testStore(t)
	f := IssueFinding{Kind: "host-env", Subject: "datadir", Severity: "critical", Title: "data dir not writable"}
	res, err := s.SyncIssues("host-env", []int64{0}, []IssueFinding{f})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(res.New) != 1 {
		t.Fatalf("want 1 new, got %+v", res)
	}
	// Same identity upserts rather than duplicating (NULL app_id rows are
	// distinct to UNIQUE; the COALESCE index is what dedupes).
	if _, err := s.SyncIssues("host-env", []int64{0}, []IssueFinding{f}); err != nil {
		t.Fatalf("sync: %v", err)
	}
	all, _ := s.ListIssues(nil, 10)
	if len(all) != 1 {
		t.Fatalf("host-scope identity should be unique, got %d rows", len(all))
	}
	res, err = s.SyncIssues("host-env", []int64{0}, nil)
	if err != nil || len(res.Resolved) != 1 {
		t.Fatalf("host-scope resolve failed: %v %+v", err, res)
	}
}

func TestCountAndAckGuards(t *testing.T) {
	s := testStore(t)
	f1 := IssueFinding{Kind: "host-env", Subject: "a", Severity: "critical", Title: "t"}
	f2 := IssueFinding{Kind: "host-env", Subject: "b", Severity: "warning", Title: "t"}
	res, err := s.SyncIssues("host-env", []int64{0}, []IssueFinding{f1, f2})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	counts, _ := s.CountOpenIssues()
	if counts["critical"] != 1 || counts["warning"] != 1 {
		t.Fatalf("counts wrong: %v", counts)
	}
	id := res.New[0].ID
	if err := s.AckIssue(id); err != nil {
		t.Fatalf("ack: %v", err)
	}
	counts, _ = s.CountOpenIssues()
	if counts["critical"] != 0 {
		t.Fatalf("acked issue still counted: %v", counts)
	}
	if err := s.AckIssue(id); err == nil {
		t.Fatal("double ack should fail")
	}
	if err := s.UnackIssue(id); err != nil {
		t.Fatalf("unack: %v", err)
	}
	if err := s.UnackIssue(id); err == nil {
		t.Fatal("unack of open issue should fail")
	}
}
