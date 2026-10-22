package engine

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/store"
)

// TestLeasePollSurvivesACancelledRequest covers the lease-path bug of
// the first public-main CI run (39825296139): a runner that walks away
// mid-poll cancels the request context, which rolled the poll's
// transaction back mid-flight, and every statement after that —
// wherever the cancellation landed among the poll's steps — failed
// with "sql: transaction has already been committed or rolled back"
// and open rows with "sql: rows are closed". The poll's transaction
// now outlives the cancellation and ends by its own code, so the poll
// answers as if the runner had stayed; a grant a gone runner never
// collects simply lapses and is swept back to queued, as it always was.
func TestLeasePollSurvivesACancelledRequest(t *testing.T) {
	e := newEnv(t)
	job := e.submit(e.work())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.eng.inLeaseTx = func(tx *sql.Tx) {
		cancel()
		// Play the request's cancellation landing mid-poll: wait until
		// a request-bound transaction has really been rolled back —
		// before the fix the poll's next statement fails from there —
		// under a bound a detached transaction never reaches.
		for i := 0; i < 200; i++ {
			if _, err := tx.Exec("SELECT 1"); err != nil {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}
	g, err := e.eng.Lease(ctx, e.runner1, nil, nil)
	if err != nil {
		t.Fatalf("lease through a cancelled request: %v", err)
	}
	if g == nil || g.JobID != job.JobID {
		t.Fatalf("grant = %+v, want job %d", g, job.JobID)
	}
}

// TestLeasePollsRunUnderContention runs many lease polls and writes at
// once on one file-backed store — a pool of connections over WAL, real
// write locking, the shape CI load gives the engine — and asks only
// what the lease path promises: every poll answers, none fails, and
// each queued job is leased exactly once, whatever the interleaving.
// The store's busy timeout absorbs the lock queues, and a poll that
// still meets SQLITE_BUSY backs off and retries inside Lease instead of
// turning contention into a failed poll.
func TestLeasePollsRunUnderContention(t *testing.T) {
	e := newEnvAt(t, filepath.Join(t.TempDir(), "lease-contention.db"), nil)
	const jobs, workers, rounds = 8, 6, 5
	for i := 0; i < jobs; i++ {
		task := e.addTask("{}")
		p := e.work()
		p.TaskID = task
		p.Key = keyOf(task)
		e.submit(p)
	}
	// The competing writers run beside the polls, on their own
	// transactions; one that meets SQLITE_BUSY backs off itself, the
	// same lease the poll's own retry is held to.
	write := func() error {
		for attempt := 0; ; attempt++ {
			err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
				_, err := tx.Exec("UPDATE projects SET updated_at = updated_at WHERE id = ?", e.project)
				return err
			})
			if err == nil || !store.IsBusy(err) || attempt >= 8 {
				return err
			}
			time.Sleep(time.Millisecond << uint(attempt))
		}
	}
	var leased atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			owner := e.runner1
			if w%2 == 1 {
				owner = e.runner2
			}
			for i := 0; i < rounds; i++ {
				g, err := e.eng.Lease(e.ctx, owner, nil, nil)
				if err != nil {
					t.Errorf("lease poll: %v", err)
					return
				}
				if g != nil {
					leased.Add(1)
				}
				if err := write(); err != nil {
					t.Errorf("write beside the polls: %v", err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	if got := leased.Load(); got != jobs {
		t.Errorf("%d jobs leased, want exactly %d: each queued job leased once", got, jobs)
	}
}
