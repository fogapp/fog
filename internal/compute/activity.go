package compute

import (
	"context"
	"time"

	"gitea.lan/matthew/fog/internal/glass"
	"gitea.lan/matthew/fog/internal/store"
)

// Activity is what Settings › Compute › Activity shows: every request an app
// made to the inference pool and every video run it started, the machine
// each went to, and how it ended. A row is written as fog takes the work
// and finished when it ends. Each also goes to Glass, when fogd reports
// there, as a "compute" transaction — and an inference call as an llm
// record too, which is what Glass's Models page reads.

// Outcomes of compute work.
const (
	OutcomeRunning   = "running"
	OutcomeOK        = "ok"
	OutcomeFailed    = "failed"
	OutcomeRefused   = "refused"
	OutcomeLost      = "lost"
	OutcomeDisplaced = "displaced"
	OutcomeStopped   = "stopped"
)

// Kinds of compute work.
const (
	KindInference = "inference"
	KindVideo     = "video"
)

// ActivityRestarted is why a row still running when fogd starts is lost:
// the fogd that handed the work out is gone, and with it what it knew.
const ActivityRestarted = "fogd restarted while it ran"

// tracked is one piece of work as it is recorded: its row, and its
// transaction in Glass (nil when fogd doesn't report there).
type tracked struct {
	row   store.ComputeActivity
	start time.Time
	txn   *glass.Txn
	ctx   context.Context
}

// begin records work fog just took, and starts its transaction under
// parent (the app's own trace, when it passed one).
func (s *Service) begin(parent context.Context, row store.ComputeActivity) *tracked {
	t := &tracked{row: row, start: time.Now()}
	t.row.Outcome = OutcomeRunning
	t.row.At = t.start.UTC().Format(time.RFC3339)
	t.ctx, t.txn = glass.Start(parent, "compute", activityName(row))
	t.txn.Attr("app", row.App)
	t.txn.Attr("pool", row.Pool)
	if row.Machine != "" {
		t.txn.Attr("machine", row.Machine)
	}
	if row.Model != "" {
		t.txn.Attr("model", row.Model)
	}
	if err := s.Registry.Store.AddComputeActivity(&t.row); err != nil {
		s.warn("compute: activity unrecorded", "app", row.App, "err", err)
	}
	return t
}

// finish records how work ended. Lost or failed work is an error in
// Glass; displaced and stopped work did what was asked of it.
func (s *Service) finish(t *tracked, outcome string, status *int, message string) {
	t.row.Outcome, t.row.Status, t.row.Message = outcome, status, message
	t.row.DurationMS = time.Since(t.start).Milliseconds()
	t.row.EndedAt = time.Now().UTC().Format(time.RFC3339)
	if t.row.ID != 0 {
		if err := s.Registry.Store.FinishComputeActivity(&t.row); err != nil {
			s.warn("compute: activity unfinished", "app", t.row.App, "err", err)
		}
	}
	t.txn.Attr("outcome", outcome)
	if status != nil {
		t.txn.Attr("status", *status)
	}
	if outcome == OutcomeFailed || outcome == OutcomeLost {
		t.txn.Fail(outcome)
	}
	t.txn.End(nil)
}

// refused records work fog turned down before it reached a machine.
func (s *Service) refused(parent context.Context, row store.ComputeActivity, status *int, why string) {
	t := s.begin(parent, row)
	t.row.Outcome, t.row.Status, t.row.Message = OutcomeRefused, status, why
	t.row.EndedAt = t.row.At
	if t.row.ID != 0 {
		if err := s.Registry.Store.FinishComputeActivity(&t.row); err != nil {
			s.warn("compute: activity unfinished", "app", row.App, "err", err)
		}
	}
	t.txn.Attr("outcome", OutcomeRefused)
	t.txn.Fail(OutcomeRefused)
	t.txn.End(nil)
}

// activityName is the transaction's name in Glass: few enough distinct
// ones to group by — the endpoint or the class, never an id.
func activityName(row store.ComputeActivity) string {
	if row.What == "" {
		return row.Kind
	}
	return row.Kind + " " + row.What
}

func (s *Service) warn(msg string, args ...any) {
	if s.Log != nil {
		s.Log.Warn(msg, args...)
	}
}
