package apps

import (
	"context"
	"strings"
	"testing"
)

// An update wraps snapshot/pull/deploy in one live operation; the deploy
// inside must join it (still one continuous "running"), and a second
// operation must be refused until the first fully unwinds. This is what
// keeps the update check and the Update button out of an in-flight update.
func TestLiveOpNesting(t *testing.T) {
	e := &Engine{}

	if e.Deploying("app") {
		t.Fatal("idle app reported as deploying")
	}
	if !e.BeginOp("app", "starting update") {
		t.Fatal("BeginOp refused on an idle app")
	}
	if !e.Deploying("app") {
		t.Fatal("app not deploying after BeginOp")
	}
	if e.BeginOp("app", "second update") {
		t.Fatal("BeginOp allowed a second concurrent operation")
	}

	// The deploy inside the update joins the operation…
	lv := e.liveFor("app")
	lv.start()
	lv.finish()
	// …so finishing it must not end the outer operation.
	if !e.Deploying("app") {
		t.Fatal("inner deploy finish ended the outer operation")
	}

	e.EndOp("app")
	if e.Deploying("app") {
		t.Fatal("app still deploying after EndOp")
	}
	if !e.BeginOp("app", "next update") {
		t.Fatal("BeginOp refused after the previous operation ended")
	}
	e.EndOp("app")
}

// CurrentDeploy cuts the live buffer before converting it; what it shows
// must be exactly the tail LogTail takes of the whole log, wherever the cut
// lands relative to a line break. DeployStage names the stage alone, and
// nothing once the operation ends.
func TestCurrentDeployTailAndStage(t *testing.T) {
	e := &Engine{}
	if got := e.DeployStage("app"); got != "" {
		t.Fatalf("idle stage = %q", got)
	}
	e.BeginOp("app", "building")
	lv := e.liveFor("app")
	for i := 0; i < 400; i++ {
		lv.Write([]byte(strings.Repeat("x", i%37) + "\n"))
		cur := e.CurrentDeploy("app")
		if want := LogTail(string(lv.log), 4000); cur.Log != want {
			t.Fatalf("after %d lines: tail differs from LogTail of the full log (%d vs %d bytes)", i+1, len(cur.Log), len(want))
		}
	}
	if got := e.DeployStage("app"); got != "building" {
		t.Fatalf("stage = %q, want building", got)
	}
	e.EndOp("app")
	if got := e.DeployStage("app"); got != "" {
		t.Fatalf("stage after EndOp = %q", got)
	}
	if e.CurrentDeploy("app").Running || e.Deploying("app") {
		t.Fatal("still running after EndOp")
	}
}

// A standalone deploy must reset the previous run's log; a deploy nested in
// an operation must keep the operation's log intact.
func TestLiveOpLogReset(t *testing.T) {
	e := &Engine{}
	lv := e.liveFor("app")

	lv.start()
	lv.Write([]byte("first deploy\n"))
	lv.finish()
	lv.start()
	if got := lv.String(); got != "" {
		t.Fatalf("standalone start kept the old log: %q", got)
	}
	lv.finish()

	e.BeginOp("app", "starting update")
	lv.start()
	if got := lv.String(); got == "" {
		t.Fatal("nested deploy start wiped the operation log")
	}
	lv.finish()
	e.EndOp("app")
}

// Cancelling an update must reach the deploy nested inside it, name who
// called it off, and leave the next operation starting clean. Operations
// begun without a context (a restore, which has its own cancel) refuse.
func TestCancelOp(t *testing.T) {
	e := &Engine{}
	if err := e.CancelOp("app", "admin"); err != ErrNothingToCancel {
		t.Fatalf("cancel on an idle app: %v", err)
	}

	ctx, ok := e.BeginCancellableOp(context.Background(), "app", "starting update")
	if !ok {
		t.Fatal("BeginCancellableOp refused on an idle app")
	}
	inner := e.liveFor("app").startCtx(ctx) // the deploy inside the update
	if err := e.CancelOp("app", "admin"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-inner.Done():
	default:
		t.Fatal("cancel did not reach the nested deploy")
	}
	if by := e.CancelledBy("app"); by != "admin" {
		t.Fatalf("CancelledBy = %q", by)
	}
	if err := e.CancelOp("app", "someone-else"); err != nil || e.CancelledBy("app") != "admin" {
		t.Fatalf("second cancel: err %v, by %q", err, e.CancelledBy("app"))
	}
	e.liveFor("app").finish()
	if !e.Deploying("app") {
		t.Fatal("cancel ended the operation before it unwound")
	}
	e.EndOp("app")

	if _, ok := e.BeginCancellableOp(context.Background(), "app", "next update"); !ok {
		t.Fatal("refused after the cancelled operation ended")
	}
	if by := e.CancelledBy("app"); by != "" {
		t.Fatalf("next operation inherited CancelledBy %q", by)
	}
	e.EndOp("app")

	e.BeginOp("app", "restoring from a snapshot")
	if err := e.CancelOp("app", "admin"); err == nil || err == ErrNothingToCancel {
		t.Fatalf("uncancellable operation: %v", err)
	}
	e.EndOp("app")
}

// Ending an operation releases its context even when nobody cancelled it.
func TestEndOpReleasesContext(t *testing.T) {
	e := &Engine{}
	ctx, _ := e.BeginCancellableOp(context.Background(), "app", "starting update")
	e.EndOp("app")
	if ctx.Err() == nil {
		t.Fatal("operation context still live after EndOp")
	}
}
