package execx

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestStepFailureCarriesWhatTheToolSaid(t *testing.T) {
	ctx := context.Background()
	err := Step(ctx, time.Minute, "caddy reload", Command(ctx, "sh", "-c", "echo 'bad directive' >&2; exit 1"))
	if err == nil || err.Error() != "caddy reload failed: bad directive" {
		t.Fatalf("err = %v", err)
	}
	if TimedOut(err) {
		t.Error("a refusal reported as a timeout")
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Errorf("does not unwrap to the exit error: %v", err)
	}
}

func TestStepTimeoutReadsAsOneWhateverTheChildSaid(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := Step(ctx, 7*time.Minute, "starting containers", Command(ctx, "sh", "-c", "echo partial; sleep 10"))
	if err == nil || err.Error() != "starting containers timed out after 7m0s" {
		t.Fatalf("err = %v", err)
	}
	if !TimedOut(err) {
		t.Error("TimedOut = false for a step that ran out of time")
	}
}

func TestStepStreamedTimeoutReadsAsOne(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	var log strings.Builder
	cmd := Command(ctx, "sh", "-c", "sleep 10")
	cmd.Stdout, cmd.Stderr = &log, &log
	err := Step(ctx, time.Minute, "release command", cmd)
	if !TimedOut(err) || err.Error() != "release command timed out after 1m0s" {
		t.Fatalf("err = %v", err)
	}
}

func TestStepStreamedFailureLeavesTheReasonInTheStream(t *testing.T) {
	ctx := context.Background()
	var log strings.Builder
	cmd := Command(ctx, "sh", "-c", "echo 'pull access denied'; exit 1")
	cmd.Stdout, cmd.Stderr = &log, &log
	err := Step(ctx, time.Minute, "release command", cmd)
	if err == nil || err.Error() != "release command failed" {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(log.String(), "pull access denied") {
		t.Errorf("stream = %q", log.String())
	}
}

func TestStepStreamedStartFailureSaysWhy(t *testing.T) {
	// A binary that never ran wrote nothing to the stream, so the reason
	// has to be in the error.
	ctx := context.Background()
	var log strings.Builder
	cmd := Command(ctx, "/nonexistent/fog-test-binary")
	cmd.Stdout = &log
	err := Step(ctx, time.Minute, "starting containers", cmd)
	if err == nil || !strings.HasPrefix(err.Error(), "starting containers failed: ") {
		t.Fatalf("err = %v", err)
	}
}

func TestExplainWordsACommandTheCallerRan(t *testing.T) {
	ctx := context.Background()
	err := Explain(ctx, time.Minute, "pg_dumpall", WithOutput(" connection refused\n", errors.New("exit status 1")))
	if err == nil || err.Error() != "pg_dumpall failed: connection refused" {
		t.Fatalf("err = %v", err)
	}
	// A command that failed without a word still says something.
	err = Explain(ctx, time.Minute, "pg_dumpall", WithOutput("", errors.New("exit status 1")))
	if err == nil || err.Error() != "pg_dumpall failed: exit status 1" {
		t.Fatalf("silent failure: err = %v", err)
	}
	if err := Explain(ctx, time.Minute, "x", nil); err != nil {
		t.Fatal(err)
	}
}

func TestStepSuccessIsNil(t *testing.T) {
	ctx := context.Background()
	if err := Step(ctx, time.Minute, "true", Command(ctx, "true")); err != nil {
		t.Fatal(err)
	}
}
