package issues

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"

	"gitea.lan/matthew/fog/internal/store"
	"gitea.lan/matthew/fog/internal/store/storetest"
)

func testScanner(t *testing.T, checks []check) (*Scanner, *store.Store, *[]string) {
	t.Helper()
	st, err := storetest.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	var pushed []string
	sc := &Scanner{
		Store: st,
		Log:   slog.Default(),
		Notify: func(title, msg string, ok bool) {
			pushed = append(pushed, title)
		},
		checksOverride: checks,
	}
	return sc, st, &pushed
}

func staticCheck(kind string, findings []store.IssueFinding) check {
	return check{kind: kind, run: func(context.Context, *snapshot) report {
		return report{findings: findings, evaluated: []int64{0}}
	}}
}

func TestSweepPanicIsolation(t *testing.T) {
	good := store.IssueFinding{Kind: "good", Subject: "s", Severity: "warning", Title: "t"}
	sc, st, _ := testScanner(t, []check{
		{kind: "bad", run: func(context.Context, *snapshot) report { panic("boom") }},
		staticCheck("good", []store.IssueFinding{good}),
	})
	sc.Sweep(context.Background())
	open, _ := st.ListIssues([]string{"open"}, 10)
	if len(open) != 1 || open[0].Kind != "good" {
		t.Fatalf("panicking check must not stop the others, got %+v", open)
	}
}

func TestSweepFailedCheckResolvesNothing(t *testing.T) {
	f := store.IssueFinding{Kind: "flaky", Subject: "s", Severity: "warning", Title: "t"}
	sc, st, _ := testScanner(t, []check{staticCheck("flaky", []store.IssueFinding{f})})
	sc.Sweep(context.Background())

	// Same check now returns nothing evaluated (it failed): the issue
	// must survive rather than resolve on missing evidence.
	sc.checksOverride = []check{{kind: "flaky", run: func(context.Context, *snapshot) report { return report{} }}}
	sc.Sweep(context.Background())
	open, _ := st.ListIssues([]string{"open"}, 10)
	if len(open) != 1 {
		t.Fatalf("failed check must not resolve its issues, got %+v", open)
	}
}

func TestSweepNotifyPolicy(t *testing.T) {
	crit := store.IssueFinding{Kind: "c", Subject: "s", Severity: "critical", Title: "critical thing"}
	warn := store.IssueFinding{Kind: "w", Subject: "s", Severity: "warning", Title: "warning thing"}
	sc, _, pushed := testScanner(t, []check{
		staticCheck("c", []store.IssueFinding{crit}),
		staticCheck("w", []store.IssueFinding{warn}),
	})
	sc.Sweep(context.Background())
	if len(*pushed) != 1 {
		t.Fatalf("only the critical issue should push, got %v", *pushed)
	}
	// Steady state: no re-push.
	sc.Sweep(context.Background())
	if len(*pushed) != 1 {
		t.Fatalf("steady state must not re-push, got %v", *pushed)
	}
}

// An issue is its own Inbox row. A sweep must not also file a notice
// counting those rows: that was a second copy of the Issues tab's badge,
// sitting in the Notices tab where only things waiting on a person belong.
func TestSweepFilesNoNotice(t *testing.T) {
	f := store.IssueFinding{Kind: "k", Subject: "s", Severity: "warning", Title: "t"}
	sc, st, _ := testScanner(t, []check{staticCheck("k", []store.IssueFinding{f})})
	sc.Sweep(context.Background())
	if open, _ := st.ListIssues([]string{"open"}, 10); len(open) != 1 {
		t.Fatalf("the finding should be an open issue, got %+v", open)
	}
	notices, _ := st.ActiveNotices()
	if len(notices) != 0 {
		t.Fatalf("a sweep should file no notice, got %+v", notices)
	}
}

func TestSweepAckSilence(t *testing.T) {
	f := store.IssueFinding{Kind: "k", Subject: "s", Severity: "warning", Title: "t"}
	sc, st, _ := testScanner(t, []check{staticCheck("k", []store.IssueFinding{f})})
	sc.Sweep(context.Background())
	open, _ := st.ListIssues([]string{"open"}, 10)
	if err := st.AckIssue(open[0].ID); err != nil {
		t.Fatalf("ack: %v", err)
	}
	// Acking is how the operator says "seen, not now": the issue stays on
	// the list but stops counting toward the badge.
	sc.Sweep(context.Background())
	counts, err := st.CountOpenIssues()
	if err != nil {
		t.Fatal(err)
	}
	if n := counts["critical"] + counts["warning"] + counts["info"]; n != 0 {
		t.Fatalf("acked-only issues should not count toward the badge, got %d", n)
	}
}
