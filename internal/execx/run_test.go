package execx

import (
	"context"
	"errors"
	"os/exec"
	"testing"
)

func TestOutputTrimsAndSucceeds(t *testing.T) {
	out, err := Output(context.Background(), "sh", "-c", "echo '  hi  '")
	if err != nil || out != "hi" {
		t.Fatalf("Output = %q, %v", out, err)
	}
}

func TestRunFailureReadsAsWhatTheToolSaid(t *testing.T) {
	_, err := Output(context.Background(), "sh", "-c", "echo 'no such volume' >&2; exit 3")
	if err == nil || err.Error() != "no such volume" {
		t.Fatalf("err = %v", err)
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 3 {
		t.Errorf("does not unwrap to the exit error: %v", err)
	}
}

func TestRunSilentFailureIsTheExecError(t *testing.T) {
	_, err := Output(context.Background(), "sh", "-c", "exit 2")
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("err = %v", err)
	}
	if err.Error() != ee.Error() {
		t.Errorf("err = %q, want the exit error's own text", err)
	}
}
