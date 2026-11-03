// Package retention is fogd's one list of what the database forgets, and
// when. Each table's rule used to live beside whichever loop happened to
// run it — main's daily block, Flare's own loop, an hourly closure, the
// tail of the issues scan — so a table nobody had written a loop for
// (consumed key packages, the bot's mirror marks) simply grew forever. A
// new rule is now one entry in Store, or in Chat for the one pass that
// needs more than the store.
package retention

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"gitea.lan/matthew/fog/internal/loop"
	"gitea.lan/matthew/fog/internal/store"
)

// Job is one retention rule: what it is called in the log, how often it
// runs, and the pass itself, which reports how many rows went.
type Job struct {
	Name  string
	Every time.Duration
	Prune func(now time.Time) (int64, error)
}

// bootDelay is when a daily job's first pass runs. Timer-first with a
// daily interval, a box that restarts every day would never reach its first
// tick and never prune; a few minutes after boot is also out of the way of
// startup's own work.
const bootDelay = 4 * time.Minute

const (
	// Half a year of "who did what" — generous, and still tiny rows.
	auditRetention = 180 * 24 * time.Hour
	// Deploy rows kept per app, and how many of them keep their full build
	// log. Older rows stay as cheap history with the log emptied.
	deployKeep    = 50
	deployLogKeep = 10
	// Revisions per app kept regardless of references. 200 covers the
	// 50-row history view and the 200 specs backupsEnabledAt walks;
	// anything a kept deploy, a version row or current_rev names survives
	// beyond it.
	revisionKeep = 200
	// Flare: events (payloads) age out; groups stay much longer — the
	// counters and first/last-seen history are cheap and answer "has this
	// ever happened before". Open groups are never pruned at all.
	errorEventRetention = 90 * 24 * time.Hour
	errorGroupRetention = 365 * 24 * time.Hour
	// A consumed MLS key package is never offered again; a month covers
	// any welcome still in flight that names it.
	keyPackageRetention = 30 * 24 * time.Hour
	// The event stream is a delivery queue, not a history: a week covers a
	// consumer that was down over a long weekend.
	appEventRetention = 7 * 24 * time.Hour
	// An accepted chat invite has done its job; the membership is the
	// record. A month leaves it visible while anyone might ask about it.
	chatInviteRetention = 30 * 24 * time.Hour
	// Resolved Inbox issues are history, useful for a while (did this
	// happen last month too?), then dead weight.
	resolvedIssueRetention = 90 * 24 * time.Hour
	// Compute activity is for seeing how machines are doing lately; a
	// month answers "has this been failing since the update?".
	computeActivityRetention = 30 * 24 * time.Hour
)

func cutoff(now time.Time, age time.Duration) string {
	return now.UTC().Add(-age).Format(time.RFC3339)
}

// Store is the database's retention table. Deletes are batched inside the
// store, so no single pass holds the writer for long.
func Store(st *store.Store) []Job {
	const day = 24 * time.Hour
	return []Job{
		{"audit", day, func(now time.Time) (int64, error) {
			return st.PruneAudit(cutoff(now, auditRetention))
		}},
		// One job, in order: revisions only the trimmed deploys named can
		// go in the same pass as those deploys.
		{"deploy history", day, func(time.Time) (int64, error) {
			var total int64
			for _, step := range []func() (int64, error){
				func() (int64, error) { return st.PruneDeploys(deployKeep) },
				func() (int64, error) { return st.PruneRevisions(revisionKeep) },
				func() (int64, error) { return st.ClearOldDeployLogs(deployLogKeep) },
			} {
				n, err := step()
				total += n
				if err != nil {
					return total, err
				}
			}
			return total, nil
		}},
		{"error events", day, func(now time.Time) (int64, error) {
			return st.PruneErrorEvents(cutoff(now, errorEventRetention))
		}},
		{"error groups", day, func(now time.Time) (int64, error) {
			return st.PruneStaleErrorGroups(cutoff(now, errorGroupRetention))
		}},
		{"key packages", day, func(now time.Time) (int64, error) {
			return st.PruneKeyPackages(cutoff(now, keyPackageRetention))
		}},
		{"bot mirror marks", day, func(time.Time) (int64, error) {
			return st.PruneChatBotSeen()
		}},
		{"app events", time.Hour, func(now time.Time) (int64, error) {
			return st.PruneAppEvents(cutoff(now, appEventRetention))
		}},
		{"sessions", time.Hour, func(time.Time) (int64, error) {
			return st.PruneExpiredSessions()
		}},
		{"gate handshakes", time.Hour, func(time.Time) (int64, error) {
			return st.PruneGate()
		}},
		{"sign-in requests and tokens", time.Hour, func(time.Time) (int64, error) {
			return st.PruneOIDC()
		}},
		{"device sign-ins", time.Hour, func(time.Time) (int64, error) {
			return st.PruneDeviceCodes()
		}},
		{"visits", time.Hour, func(time.Time) (int64, error) {
			return st.PruneVisits()
		}},
		{"disappearing chat messages", time.Hour, func(time.Time) (int64, error) {
			return st.PruneExpiredChatEvents()
		}},
		{"chat invites", time.Hour, func(now time.Time) (int64, error) {
			return st.PruneChatInvites(cutoff(now, chatInviteRetention))
		}},
		{"compute activity", time.Hour, func(now time.Time) (int64, error) {
			return st.PruneComputeActivity(cutoff(now, computeActivityRetention))
		}},
		// Hourly, as when it rode along at the end of each issues scan.
		{"resolved issues", time.Hour, func(now time.Time) (int64, error) {
			return st.PruneResolvedIssues(cutoff(now, resolvedIssueRetention))
		}},
	}
}

// RoomHistory is chat's per-room retention (chat.Service.PruneHistory).
// Each room's policy sets how long it keeps history, and attachment files
// on disk go with their messages, so the pass belongs to the chat service
// rather than to a store query.
type RoomHistory interface {
	PruneHistory(now time.Time) (int64, error)
}

// Chat is the chat service's retention rule.
func Chat(c RoomHistory) []Job {
	return []Job{{"chat room history", time.Hour, c.PruneHistory}}
}

// Run runs every job on its own schedule until ctx ends, and returns once
// every one has stopped — a prune mid-pass finishes before the caller goes
// on to close the store. Daily jobs first run bootDelay after start; more
// frequent ones wait one interval.
func Run(ctx context.Context, log *slog.Logger, jobs []Job) {
	var wg sync.WaitGroup
	for _, j := range jobs {
		first := j.Every
		if j.Every >= 24*time.Hour {
			first = bootDelay
		}
		wg.Go(func() { loop.Every(ctx, first, j.Every, func(context.Context) { runOnce(log, j) }) })
	}
	wg.Wait()
}

func runOnce(log *slog.Logger, j Job) {
	n, err := j.Prune(time.Now())
	if err != nil {
		log.Warn("retention", "job", j.Name, "rows", n, "err", err)
	} else if n > 0 {
		log.Info("retention", "job", j.Name, "rows", n)
	}
}
