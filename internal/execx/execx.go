// Package execx wraps os/exec for the one guarantee fogd needs from every
// child process: that cancelling its context actually kills it, along with
// everything it spawned.
//
// This matters more than it sounds. `docker compose`, `docker build`, git, and
// tar all fork grandchildren, and exec.CommandContext signals only the direct
// child. A deploy whose compose invocation wedges on an unreachable registry
// would otherwise ignore its 30-minute budget entirely and hold the app's lock
// until fogd restarts — leaving that app permanently un-deployable,
// un-startable, and un-deletable through the UI.
package execx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// WaitDelay bounds how long a cancelled command's I/O pipes stay open after
// the kill signal, so a child ignoring SIGKILL can't pin the caller forever.
const WaitDelay = 10 * time.Second

// Command is exec.CommandContext with process-group semantics: the child
// leads its own group, and cancelling the context SIGKILLs the whole group.
func Command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		// Negative pid addresses the group, not just the leader.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = WaitDelay
	return cmd
}

// Run runs cmd and returns its combined output, trimmed. When it fails, the
// error reads as that output — what the tool said is what an operator
// needs — and unwraps to the exec error underneath. A command that failed
// without a word reads as the exec error itself.
//
// Callers that run under a deadline check their context first, so a
// timeout reports as one rather than as whatever the killed child printed.
func Run(cmd *exec.Cmd) (string, error) {
	out, err := cmd.CombinedOutput()
	s := strings.TrimSpace(string(out))
	if err != nil {
		return s, WithOutput(s, err)
	}
	return s, nil
}

// WithOutput is err described by output, the way Run describes a failure,
// for a caller that collected the output itself: an *OutputError, or err as
// it is when output is blank.
func WithOutput(output string, err error) error {
	output = strings.TrimSpace(output)
	if err == nil || output == "" {
		return err
	}
	return &OutputError{Output: output, Err: err}
}

// Output is Run for a command that needs nothing set beyond its arguments.
func Output(ctx context.Context, name string, args ...string) (string, error) {
	return Run(Command(ctx, name, args...))
}

// OutputError is a failed command, described by what it printed.
type OutputError struct {
	Output string
	Err    error
}

func (e *OutputError) Error() string { return e.Output }
func (e *OutputError) Unwrap() error { return e.Err }

// Step runs cmd — built on ctx — as one named step of a longer job, and
// owns how its failure reads. A step that ran out of time reads
// "<what> timed out after <budget>", whatever the killed child printed on
// its way out, because a timeout and a refusal need different things from
// the operator. Any other failure reads "<what> failed: <what it said>".
//
// budget is only for the wording: the deadline itself is ctx's, set by the
// caller around the whole job or just this step.
//
// A cmd whose output is already going somewhere — streamed into a live log
// the operator is watching — is run as it is, and a non-zero exit reads
// just "<what> failed": the reason is in the stream, and "exit status 1"
// would add nothing to it. Otherwise the combined output is captured, as
// Run does, and becomes the error's detail.
func Step(ctx context.Context, budget time.Duration, what string, cmd *exec.Cmd) error {
	if cmd.Stdout != nil || cmd.Stderr != nil {
		err := cmd.Run()
		var ee *exec.ExitError
		if err != nil && errors.As(err, &ee) && ctx.Err() == nil {
			return &StepError{What: what, Budget: budget, Err: err, streamed: true}
		}
		return Explain(ctx, budget, what, err)
	}
	_, err := Run(cmd)
	return Explain(ctx, budget, what, err)
}

// Explain is Step's wording for a command the caller had to run itself — one
// read through a pipe, or whose stderr it trimmed — given what the run
// returned. A nil err is nil.
func Explain(ctx context.Context, budget time.Duration, what string, err error) error {
	if err == nil {
		return nil
	}
	return &StepError{What: what, Budget: budget, TimedOut: ctx.Err() != nil, Err: err}
}

// TimedOut reports whether err is a Step that ran out of time, for a caller
// with cleanup that only a timeout calls for.
func TimedOut(err error) bool {
	var se *StepError
	return errors.As(err, &se) && se.TimedOut
}

// StepError is a failed Step.
type StepError struct {
	What     string
	Budget   time.Duration
	TimedOut bool
	Err      error
	streamed bool
}

func (e *StepError) Error() string {
	switch {
	case e.TimedOut:
		return fmt.Sprintf("%s timed out after %s", e.What, e.Budget)
	case e.streamed:
		return e.What + " failed"
	}
	return fmt.Sprintf("%s failed: %v", e.What, e.Err)
}

func (e *StepError) Unwrap() error { return e.Err }
