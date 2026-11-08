// Package jobs is the registry of long-running work a request starts and
// the UI polls — Files copies and snapshots, offsite syncs, locker copies.
// In-memory on purpose: a job's progress is only interesting while fogd
// is the process doing it, and a restart mid-copy leaves the disk (or the
// bucket) in a state the next run inspects, not a row that pretends to
// know. The same shape as apps.liveDeploy.
package jobs

import (
	"context"
	"sort"
	"sync"
	"time"

	"gitea.lan/matthew/fog/internal/glass"
	"gitea.lan/matthew/fog/internal/randx"
)

type Jobs struct {
	mu   sync.Mutex
	jobs map[string]*jobState
	keep int
}

// Job is the polled view.
type Job struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Label      string `json:"label"`
	State      string `json:"state" ts:"\"running\" | \"done\" | \"failed\" | \"cancelled\""` // running | done | failed | cancelled
	Current    string `json:"current,omitempty"`
	Done       int64  `json:"done"`
	Total      int64  `json:"total"`
	Items      int64  `json:"items"`
	Error      string `json:"error,omitempty"`
	StartedAt  string `json:"startedAt"`
	FinishedAt string `json:"finishedAt,omitempty"`
	// Result carries a job-specific payload (a restored path, a count).
	Result any `json:"result,omitempty"`
}

type jobState struct {
	mu     sync.Mutex
	job    Job
	cancel context.CancelFunc
	start  time.Time
	done   chan struct{}
}

// Progress is what a running job reports through.
type Progress struct {
	st *jobState
}

func (p *Progress) Set(current string, done, total int64) {
	p.st.mu.Lock()
	p.st.job.Current, p.st.job.Done, p.st.job.Total = current, done, total
	p.st.mu.Unlock()
}

func (p *Progress) Add(bytes int64, items int64) {
	p.st.mu.Lock()
	p.st.job.Done += bytes
	p.st.job.Items += items
	p.st.mu.Unlock()
}

func (p *Progress) Current(s string) {
	p.st.mu.Lock()
	p.st.job.Current = s
	p.st.mu.Unlock()
}

func (p *Progress) Total(total int64) {
	p.st.mu.Lock()
	p.st.job.Total = total
	p.st.mu.Unlock()
}

func (p *Progress) Result(v any) {
	p.st.mu.Lock()
	p.st.job.Result = v
	p.st.mu.Unlock()
}

func New() *Jobs { return &Jobs{jobs: map[string]*jobState{}, keep: 100} }

// NewProgress returns a Progress attached to no registry — for code paths
// that take one but are being driven directly (tests, one-off calls).
func NewProgress() *Progress { return &Progress{st: &jobState{done: make(chan struct{})}} }

// NewID mints a random 16-hex id — shared with the callers that need ids
// of the same shape for things that are not jobs.
func NewID() string {
	return randx.Hex(8)
}

// Start runs fn on its own goroutine and returns the job immediately. fn's
// error becomes the job's failure; a context cancellation is reported as
// cancelled rather than failed.
func (j *Jobs) Start(kind, label string, fn func(ctx context.Context, p *Progress) error) Job {
	st, ctx := newState(kind, label)
	j.mu.Lock()
	j.jobs[st.job.ID] = st
	j.prune()
	j.mu.Unlock()
	st.run(ctx, fn)
	return st.snapshot()
}

// StartExclusive is Start unless a job of kind, or of any kind in also, is
// already running; then it starts nothing and ok is false. The check and the
// registration happen under one lock, so two callers racing to start the
// same kind can't both get in.
func (j *Jobs) StartExclusive(kind, label string, fn func(ctx context.Context, p *Progress) error, also ...string) (job Job, ok bool) {
	st, ctx := newState(kind, label)
	j.mu.Lock()
	for _, k := range append([]string{kind}, also...) {
		if _, busy := j.currentLocked(k); busy {
			j.mu.Unlock()
			st.cancel()
			return Job{}, false
		}
	}
	j.jobs[st.job.ID] = st
	j.prune()
	j.mu.Unlock()
	st.run(ctx, fn)
	return st.snapshot(), true
}

func newState(kind, label string) (*jobState, context.Context) {
	ctx, cancel := context.WithCancel(context.Background())
	st := &jobState{cancel: cancel, start: time.Now(), done: make(chan struct{})}
	st.job = Job{ID: NewID(), Kind: kind, Label: label, State: "running", StartedAt: st.start.UTC().Format(time.RFC3339)}
	return st, ctx
}

func (st *jobState) run(ctx context.Context, fn func(ctx context.Context, p *Progress) error) {
	go func() {
		defer close(st.done)
		var txnErr error
		defer func() {
			if v := recover(); v != nil {
				st.mu.Lock()
				st.job.State, st.job.Error = "failed", "internal error"
				st.job.FinishedAt = time.Now().UTC().Format(time.RFC3339)
				st.mu.Unlock()
			}
		}()
		// The job as a Glass transaction: a cancellation is not a failure,
		// and a panic (recovered above) is.
		ctx, txn := glass.Start(ctx, "job", st.job.Kind)
		returned := false
		defer func() {
			switch {
			case !returned:
				txn.Fail("panic")
			case ctx.Err() != nil:
				txn.Attr("cancelled", true)
			}
			txn.End(txnErr)
		}()
		err := fn(ctx, &Progress{st: st})
		returned = true
		if ctx.Err() == nil {
			txnErr = err
		}
		st.mu.Lock()
		defer st.mu.Unlock()
		st.job.FinishedAt = time.Now().UTC().Format(time.RFC3339)
		switch {
		case err == nil:
			st.job.State = "done"
		case ctx.Err() != nil:
			st.job.State = "cancelled"
		default:
			st.job.State, st.job.Error = "failed", err.Error()
		}
		st.job.Current = ""
	}()
}

func (st *jobState) snapshot() Job {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.job
}

func (j *Jobs) Get(id string) (Job, bool) {
	j.mu.Lock()
	st, ok := j.jobs[id]
	j.mu.Unlock()
	if !ok {
		return Job{}, false
	}
	return st.snapshot(), true
}

// Wait blocks until the job finishes or ctx ends, and returns its final
// view. Loops that run jobs one after another use it.
func (j *Jobs) Wait(ctx context.Context, id string) (Job, bool) {
	j.mu.Lock()
	st, ok := j.jobs[id]
	j.mu.Unlock()
	if !ok {
		return Job{}, false
	}
	select {
	case <-st.done:
	case <-ctx.Done():
	}
	return st.snapshot(), true
}

// List returns every job, newest first.
func (j *Jobs) List() []Job {
	j.mu.Lock()
	states := make([]*jobState, 0, len(j.jobs))
	for _, st := range j.jobs {
		states = append(states, st)
	}
	j.mu.Unlock()
	sort.Slice(states, func(a, b int) bool { return states[a].start.After(states[b].start) })
	out := make([]Job, 0, len(states))
	for _, st := range states {
		out = append(out, st.snapshot())
	}
	return out
}

func (j *Jobs) Cancel(id string) bool {
	j.mu.Lock()
	st, ok := j.jobs[id]
	j.mu.Unlock()
	if !ok {
		return false
	}
	st.cancel()
	return true
}

// Running reports whether any job of kind is in flight.
func (j *Jobs) Running(kind string) bool {
	_, ok := j.Current(kind)
	return ok
}

// Current returns the in-flight job of kind, if any.
func (j *Jobs) Current(kind string) (Job, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.currentLocked(kind)
}

// currentLocked is Current for a caller holding j.mu.
func (j *Jobs) currentLocked(kind string) (Job, bool) {
	for _, st := range j.jobs {
		if s := st.snapshot(); s.Kind == kind && s.State == "running" {
			return s, true
		}
	}
	return Job{}, false
}

// prune drops the oldest finished jobs beyond keep. Caller holds j.mu.
func (j *Jobs) prune() {
	if len(j.jobs) <= j.keep {
		return
	}
	var finished []*jobState
	for _, st := range j.jobs {
		if st.snapshot().State != "running" {
			finished = append(finished, st)
		}
	}
	sort.Slice(finished, func(a, b int) bool { return finished[a].start.Before(finished[b].start) })
	for _, st := range finished {
		if len(j.jobs) <= j.keep {
			break
		}
		delete(j.jobs, st.snapshot().ID)
	}
}
