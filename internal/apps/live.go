package apps

import (
	"bytes"
	"context"
	"errors"
	"sort"
	"sync"
)

// liveDeploy tracks an in-flight deploy so the UI can stream progress
// instead of waiting for the terminal write at the end. It counts nested
// starts: an update wraps its whole run (snapshot, pull, deploy) in one
// operation, and the deploy inside it joins that operation rather than
// resetting it — the app reads as busy from first click to swapped
// containers, with one continuous log.
type liveDeploy struct {
	mu        sync.Mutex
	depth     int
	stage     string
	log       []byte
	truncated bool // earlier output was dropped to stay under maxLiveLog

	// cancel interrupts the outermost operation, when it was started with a
	// context to interrupt; nil for operations that can't be called off
	// generically (a restore has its own cancel, which also disarms).
	cancel      context.CancelFunc
	cancelledBy string // who called the operation off; "" if nobody did
}

// maxLiveLog bounds the in-memory deploy log. A docker build of a large image
// can emit tens of megabytes; the UI only ever renders the tail, and this
// buffer is also what gets written into SQLite and read back on every app-page
// load, so keeping all of it cost memory, database size, and page weight for
// output nobody reads. The tail is the part that explains a failure.
const maxLiveLog = 256 << 10

const truncationNotice = "[… earlier output trimmed to keep this log bounded …]\n"

func (l *liveDeploy) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.append(p)
	return len(p), nil
}

// append adds to the log, trimming from the front once it outgrows the cap.
// Caller holds the lock.
func (l *liveDeploy) append(p []byte) {
	l.log = append(l.log, p...)
	if len(l.log) <= maxLiveLog {
		return
	}
	// Copy down rather than reslicing: a plain reslice keeps the whole grown
	// backing array alive, which is the leak this cap exists to prevent.
	drop := len(l.log) - maxLiveLog
	if i := bytes.IndexByte(l.log[drop:], '\n'); i >= 0 {
		drop += i + 1 // resume on a line boundary
	}
	l.log = append(l.log[:0], l.log[drop:]...)
	l.truncated = true
}

func (l *liveDeploy) setStage(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.stage = s
	l.append([]byte("==> " + s + "\n"))
}

func (l *liveDeploy) start() {
	l.begin(context.Background(), false)
}

// startCtx is start for work that can be called off: the outermost start
// derives a cancellable context from ctx and keeps its cancel for CancelOp.
// A nested start returns ctx unchanged — it already descends from the
// outer operation's context, so cancelling that reaches it.
func (l *liveDeploy) startCtx(ctx context.Context) context.Context {
	return l.begin(ctx, true)
}

// begin is start and startCtx: cancellable says whether the outermost
// operation keeps a cancel for CancelOp.
func (l *liveDeploy) begin(ctx context.Context, cancellable bool) context.Context {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.depth++
	if l.depth == 1 {
		ctx = l.reset(ctx, cancellable)
	}
	return ctx
}

// tryStart begins an operation only if none is in flight — how an update
// claims exclusive use of an app.
func (l *liveDeploy) tryStart(ctx context.Context, cancellable bool) (context.Context, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.depth > 0 {
		return ctx, false
	}
	l.depth = 1
	return l.reset(ctx, cancellable), true
}

// reset readies the tracker for a new outermost operation. Caller holds the
// lock. Only a cancellable operation keeps a cancel; CancelOp refuses the
// rest.
func (l *liveDeploy) reset(ctx context.Context, cancellable bool) context.Context {
	l.stage = "starting"
	l.log = l.log[:0]
	l.truncated = false
	l.cancelledBy = ""
	l.cancel = nil
	if cancellable {
		ctx, l.cancel = context.WithCancel(ctx)
	}
	return ctx
}

// busy reports whether an operation is in flight, so state cleanup can leave
// live entries alone.
func (l *liveDeploy) busy() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.depth > 0
}

func (l *liveDeploy) finish() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.depth > 0 {
		l.depth--
	}
	if l.depth == 0 && l.cancel != nil {
		l.cancel()
		l.cancel = nil
	}
}

// String returns the log, prefixed with a notice when output was dropped —
// a truncated log that doesn't say so invites the operator to conclude the
// build simply started where the text starts.
func (l *liveDeploy) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.truncated {
		return truncationNotice + string(l.log)
	}
	return string(l.log)
}

// DeploySnapshot is what the UI polls while a deploy runs.
type DeploySnapshot struct {
	Running bool   `json:"running"`
	Stage   string `json:"stage,omitempty"`
	Log     string `json:"log,omitempty"`
}

func (e *Engine) liveFor(name string) *liveDeploy {
	v, _ := e.liveDeploys.LoadOrStore(name, &liveDeploy{})
	return v.(*liveDeploy)
}

// CurrentDeploy returns the in-flight deploy state for an app (zero value
// when idle).
func (e *Engine) CurrentDeploy(name string) DeploySnapshot {
	v, ok := e.liveDeploys.Load(name)
	if !ok {
		return DeploySnapshot{}
	}
	l := v.(*liveDeploy)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.depth == 0 {
		return DeploySnapshot{}
	}
	// Cut the buffer before converting it: the log runs to 256 KB and this
	// is polled every second or two. One byte past the tail lets LogTail see
	// whether the cut landed on a line boundary, exactly as on the full log.
	const tail = 4000
	b := l.log
	if len(b) > tail+1 {
		b = b[len(b)-tail-1:]
	}
	return DeploySnapshot{Running: true, Stage: l.stage, Log: LogTail(string(b), tail)}
}

// DeployStage is the in-flight operation's stage, or "" when the app is
// idle — CurrentDeploy without copying the log, for callers that only name
// the stage.
func (e *Engine) DeployStage(name string) string {
	v, ok := e.liveDeploys.Load(name)
	if !ok {
		return ""
	}
	l := v.(*liveDeploy)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.depth == 0 {
		return ""
	}
	return l.stage
}

// Deploying reports whether a deploy-shaped operation (a deploy, or an
// update's snapshot/pull/deploy run) is in flight for the app.
func (e *Engine) Deploying(name string) bool {
	v, ok := e.liveDeploys.Load(name)
	return ok && v.(*liveDeploy).busy()
}

// DeployingApps names every app with a deploy-shaped operation in flight,
// sorted. Fog's own self-update refuses while this is non-empty: the
// restart kills the goroutine driving the deploy mid-run.
func (e *Engine) DeployingApps() []string {
	var names []string
	e.liveDeploys.Range(func(k, v any) bool {
		if lv, ok := v.(*liveDeploy); ok && lv.busy() {
			names = append(names, k.(string))
		}
		return true
	})
	sort.Strings(names)
	return names
}

// BeginOp claims the app for a deploy-shaped operation and shows `stage` as
// its live progress; false means one is already in flight. Deploys started
// inside the operation join it instead of resetting it. Pair with EndOp.
// The operation can't be called off with CancelOp; see BeginCancellableOp.
func (e *Engine) BeginOp(name, stage string) bool {
	_, ok := e.beginOp(context.Background(), name, stage, false)
	return ok
}

// BeginCancellableOp is BeginOp for work an operator may call off: run the
// operation under the returned context, and CancelOp cancels it.
func (e *Engine) BeginCancellableOp(ctx context.Context, name, stage string) (context.Context, bool) {
	return e.beginOp(ctx, name, stage, true)
}

func (e *Engine) beginOp(ctx context.Context, name, stage string, cancellable bool) (context.Context, bool) {
	lv := e.liveFor(name)
	ctx, ok := lv.tryStart(ctx, cancellable)
	if !ok {
		return ctx, false
	}
	lv.setStage(stage)
	return ctx, true
}

// CancelledError is what a deploy-shaped operation returns once an operator
// has called it off — whatever the interrupted step itself reported
// ("context canceled", a timeout-shaped message) is not the reason.
type CancelledError struct {
	By string
}

func (e *CancelledError) Error() string { return "cancelled by " + e.By }

// ErrNothingToCancel is CancelOp's answer for an app with no operation in
// flight.
var ErrNothingToCancel = errors.New("nothing is in progress for this app")

// CancelOp calls off the app's in-flight deploy, update, or rollback. It only
// signals: the operation unwinds on its own goroutine, and the app reads as
// busy until it has. What already happened stays happened — a deploy
// cancelled mid-swap stops where it is, like a timed-out one.
func (e *Engine) CancelOp(name, actor string) error {
	v, ok := e.liveDeploys.Load(name)
	if !ok {
		return ErrNothingToCancel
	}
	l := v.(*liveDeploy)
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case l.depth == 0:
		return ErrNothingToCancel
	case l.cancel == nil:
		return errors.New("this operation can't be cancelled from here")
	case l.cancelledBy != "":
		return nil // already unwinding
	}
	l.cancelledBy = actor
	l.stage = "cancelling"
	l.append([]byte("==> cancelled by " + actor + "\n"))
	l.cancel()
	return nil
}

// CancelledBy names who called off the app's in-flight operation, or ""
// if nobody has. Read it before EndOp: the next operation resets it.
func (e *Engine) CancelledBy(name string) string {
	v, ok := e.liveDeploys.Load(name)
	if !ok {
		return ""
	}
	l := v.(*liveDeploy)
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.cancelledBy
}

// JoinOp marks the app busy for the duration of an operation, joining an
// enclosing one if it exists rather than competing with it — a pre-update
// snapshot is part of its update's progress, not a rival operation. Unlike
// BeginOp it never refuses, so it suits work that has already been authorized
// by whoever holds the outer claim. Call the returned function when done.
func (e *Engine) JoinOp(name, stage string) func() {
	lv := e.liveFor(name)
	lv.start()
	lv.setStage(stage)
	return lv.finish
}

// OpStage updates the live progress stage of an operation begun with BeginOp.
func (e *Engine) OpStage(name, stage string) {
	e.liveFor(name).setStage(stage)
}

// EndOp releases the claim taken by BeginOp.
func (e *Engine) EndOp(name string) {
	e.liveFor(name).finish()
}
