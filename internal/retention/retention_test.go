package retention

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/store/storetest"
)

// Every rule runs cleanly against a real, freshly migrated database — a
// rule naming a column or table that doesn't exist fails here, not on the
// first daily pass in production.
func TestStoreJobsRunAgainstARealDatabase(t *testing.T) {
	st, err := storetest.Open(filepath.Join(t.TempDir(), "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	names := map[string]bool{}
	for _, j := range Store(st) {
		if names[j.Name] {
			t.Errorf("duplicate job name %q", j.Name)
		}
		names[j.Name] = true
		if j.Every <= 0 {
			t.Errorf("%s: no interval", j.Name)
		}
		if _, err := j.Prune(time.Now()); err != nil {
			t.Errorf("%s: %v", j.Name, err)
		}
	}
}

func TestRunStartsEveryJob(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var a, b atomic.Int32
	returned := make(chan struct{})
	go func() {
		Run(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), []Job{
			{"a", 10 * time.Millisecond, func(time.Time) (int64, error) { a.Add(1); return 1, nil }},
			{"b", 10 * time.Millisecond, func(time.Time) (int64, error) { b.Add(1); return 0, nil }},
		})
		close(returned)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for a.Load() < 2 || b.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("jobs ran a=%d b=%d times", a.Load(), b.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case <-returned:
		t.Fatal("Run returned while its context was live")
	default:
	}
}

// Run returns only once every job has stopped, so fogd's shutdown can wait
// for it: a prune mid-pass when the context ends finishes first, rather
// than running on into a store that has closed.
func TestRunWaitsForItsJobs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	var finished atomic.Bool
	returned := make(chan struct{})
	go func() {
		Run(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), []Job{
			{"slow", time.Millisecond, func(time.Time) (int64, error) {
				if finished.Load() {
					return 0, nil
				}
				close(started)
				time.Sleep(100 * time.Millisecond)
				finished.Store(true)
				return 0, nil
			}},
		})
		close(returned)
	}()
	<-started
	cancel()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context ended")
	}
	if !finished.Load() {
		t.Error("Run returned with a prune still running")
	}
}

// Resolved issues are one of the list's rules now, not the tail of the
// issues scan: a resolved row past the window goes, an open one of any age
// and a recently resolved one stay.
func TestResolvedIssuesAgeOutInTheList(t *testing.T) {
	st, err := storetest.Open(filepath.Join(t.TempDir(), "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now()
	old := now.Add(-resolvedIssueRetention - time.Hour).UTC().Format(time.RFC3339)
	recent := now.Add(-time.Hour).UTC().Format(time.RFC3339)
	for i, row := range [][2]string{{"resolved", old}, {"open", old}, {"resolved", recent}} {
		if _, err := st.DB().Exec(`INSERT INTO issues (kind, subject, title, status, details_hash, first_seen, last_seen)
			VALUES ('k', ?, 't', ?, '', ?, ?)`, i, row[0], row[1], row[1]); err != nil {
			t.Fatal(err)
		}
	}
	job := jobNamed(t, Store(st), "resolved issues")
	if job.Every != time.Hour {
		t.Errorf("resolved issues every %v, want hourly like the scan it rode on", job.Every)
	}
	if n, err := job.Prune(now); err != nil || n != 1 {
		t.Fatalf("pruned %d, %v; want 1", n, err)
	}
}

type fakeRooms struct{ at time.Time }

func (f *fakeRooms) PruneHistory(now time.Time) (int64, error) { f.at = now; return 2, nil }

func TestChatJobRunsRoomHistory(t *testing.T) {
	var rooms fakeRooms
	job := jobNamed(t, Chat(&rooms), "chat room history")
	now := time.Now()
	if n, err := job.Prune(now); err != nil || n != 2 || !rooms.at.Equal(now) {
		t.Fatalf("chat job: %d, %v, ran at %v", n, err, rooms.at)
	}
}

func jobNamed(t *testing.T, jobs []Job, name string) Job {
	t.Helper()
	for _, j := range jobs {
		if j.Name == name {
			return j
		}
	}
	t.Fatalf("no %q job", name)
	return Job{}
}
