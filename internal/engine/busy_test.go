package engine

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/store"
)

// The busy-retry tests are deterministic: no test holds a real lock,
// which the five-second busy timeout would ride out anyway. Instead the
// test hook faultBusyTx plays the store refusing the transaction the
// write lock at the end of its work, as a busy at COMMIT looks to the
// caller, and the bounded retry answers it.

// errBusy is the driver's busy error text (internal/store/busy_test.go):
// the store refusing the write lock.
var errBusy = errors.New("database is locked (5) (SQLITE_BUSY)")

// TestSweepSucceedsAfterOneBusyAttempt covers the retry on the sweep's
// maintenance transaction: one attempt refused busy, the next commits,
// and only the committed attempt's report returns.
func TestSweepSucceedsAfterOneBusyAttempt(t *testing.T) {
	e := newEnv(t)
	e.submit(e.work())
	g := e.leaseAny(e.runner1)
	e.clock.Advance(e.eng.cfg.LeaseTTL + time.Second) // the lease lapses: the sweep has work to commit
	calls := 0
	e.eng.faultBusyTx = func() error {
		calls++
		if calls == 1 {
			return errBusy
		}
		return nil
	}
	rep, err := e.eng.Sweep(e.ctx)
	if err != nil {
		t.Fatalf("sweep after one busy attempt: %v", err)
	}
	if calls != 2 {
		t.Fatalf("the transaction ran %d times, want the failed attempt and the retry", calls)
	}
	if len(rep.Expired) != 1 || rep.Expired[0] != g.JobID {
		t.Fatalf("report = %+v, want the expired job %d: only the committed attempt's report returns", rep, g.JobID)
	}
	if status := e.str("SELECT status FROM jobs WHERE id = ?", g.JobID); status != statusQueued {
		t.Fatalf("status = %q, want queued: the retry's sweep committed", status)
	}
}

// TestHeartbeatAnswersOKAfterOneBusyAttempt covers the retry on the
// heartbeat's own transaction: one attempt refused busy, the next commits
// and answers ok, keeping the lease the failed attempt would have cost.
func TestHeartbeatAnswersOKAfterOneBusyAttempt(t *testing.T) {
	e := newEnv(t)
	e.submit(e.work())
	g := e.leaseAny(e.runner1)
	calls := 0
	e.eng.faultBusyTx = func() error {
		calls++
		if calls == 1 {
			return errBusy
		}
		return nil
	}
	ans, expires, err := e.eng.Heartbeat(e.ctx, g.LeaseOwner, g.JobID, g.LeaseID, Progress{})
	if err != nil {
		t.Fatalf("heartbeat after one busy attempt: %v", err)
	}
	if calls != 2 {
		t.Fatalf("the transaction ran %d times, want the failed attempt and the retry", calls)
	}
	if ans != AnsOK {
		t.Fatalf("answer = %s, want ok", ans)
	}
	if want := e.clock.Now().Add(e.eng.cfg.LeaseTTL); !expires.Equal(want) {
		t.Fatalf("expiry = %v, want %v: only the committed attempt's expiry returns", expires, want)
	}
	if got := e.str("SELECT lease_expires_at FROM jobs WHERE id = ?", g.JobID); got != fmtTime(expires) {
		t.Fatalf("stored expiry = %q, want %q: the retry's beat committed", got, fmtTime(expires))
	}
}

// TestSweepSuspensionSurvivesABusyRollback covers the bookkeeping the
// retry must keep: a sweep that detected a suspension and was then
// refused busy rolled back, so its tick is not consumed — the retry
// measures the same pre-sleep gap, re-detects the suspension and commits
// the engine_suspended event the failed attempt's rollback erased.
func TestSweepSuspensionSurvivesABusyRollback(t *testing.T) {
	e := newEnv(t)
	e.submit(e.work()) // a queued job whose deadlines the suspension restarts
	e.mustSweep()      // a tick before the sleep: the suspension is a gap from here
	e.clock.Advance(2 * time.Hour)
	calls := 0
	e.eng.faultBusyTx = func() error {
		calls++
		if calls == 1 {
			return errBusy
		}
		return nil
	}
	if _, err := e.eng.Sweep(e.ctx); err != nil {
		t.Fatalf("sweep past a busy first attempt: %v", err)
	}
	if calls != 2 {
		t.Fatalf("the transaction ran %d times, want the failed attempt and the retry", calls)
	}
	if n := e.int("SELECT COUNT(*) FROM events WHERE kind = ?", evEngineSuspended); n != 1 {
		t.Fatalf("engine_suspended events = %d, want the one the committed attempt re-detected", n)
	}
	if queuedAt := e.str("SELECT queued_at FROM jobs WHERE task_id = ?", e.task); queuedAt != fmtTime(e.clock.Now()) {
		t.Fatalf("queued_at = %q, want restarted at the wake %q: the retry re-detected the suspension",
			queuedAt, fmtTime(e.clock.Now()))
	}
}

// TestSweepAndHeartbeatStayBusyAfterBoundedAttempts covers the bound: a
// transaction refused busy on every attempt fails with the busy error
// after the retry's bounded attempts — the store's own bound, read here
// by running one always-busy fn through the same RetryBusy — with
// nothing committed.
func TestSweepAndHeartbeatStayBusyAfterBoundedAttempts(t *testing.T) {
	bound := 0
	store.RetryBusy(context.Background(), func() error {
		bound++
		return errBusy
	})
	t.Run("sweep", func(t *testing.T) {
		e := newEnv(t)
		e.submit(e.work())
		g := e.leaseAny(e.runner1)
		e.clock.Advance(e.eng.cfg.LeaseTTL + time.Second) // work the rolled-back attempts would commit
		calls := 0
		e.eng.faultBusyTx = func() error { calls++; return errBusy }
		rep, err := e.eng.Sweep(e.ctx)
		if !store.IsBusy(err) {
			t.Fatalf("sweep error = %v, want the busy it could not ride out", err)
		}
		if calls != bound {
			t.Fatalf("the transaction ran %d times, want the retry's %d bounded attempts", calls, bound)
		}
		if len(rep.Expired) != 0 || len(rep.HardCapped) != 0 || len(rep.QueueTimedOut) != 0 {
			t.Fatalf("report = %+v, want none: no attempt committed, so none of its values return", rep)
		}
		if status := e.str("SELECT status FROM jobs WHERE id = ?", g.JobID); status != statusLeased {
			t.Fatalf("status = %q, want leased: no attempt committed", status)
		}
	})
	t.Run("heartbeat", func(t *testing.T) {
		e := newEnv(t)
		e.submit(e.work())
		g := e.leaseAny(e.runner1)
		before := e.str("SELECT lease_expires_at FROM jobs WHERE id = ?", g.JobID)
		calls := 0
		e.eng.faultBusyTx = func() error { calls++; return errBusy }
		ans, expires, err := e.eng.Heartbeat(e.ctx, g.LeaseOwner, g.JobID, g.LeaseID, Progress{})
		if !store.IsBusy(err) {
			t.Fatalf("heartbeat error = %v, want the busy it could not ride out", err)
		}
		if calls != bound {
			t.Fatalf("the transaction ran %d times, want the retry's %d bounded attempts", calls, bound)
		}
		if ans != "" || !expires.IsZero() {
			t.Fatalf("answer = %q, expiry = %v, want none: the last attempt rolled back, and only a committed beat's values return", ans, expires)
		}
		if after := e.str("SELECT lease_expires_at FROM jobs WHERE id = ?", g.JobID); after != before {
			t.Fatalf("lease expiry moved from %q to %q: no attempt committed", before, after)
		}
	})
}

// TestNonBusyErrorIsNotRetried covers the boundary: an error that is not
// busy goes back at once, unretried, whatever call it came from, and the
// rolled-back attempt's outputs go back with nothing.
func TestNonBusyErrorIsNotRetried(t *testing.T) {
	boom := errors.New("no such table: jobs")
	t.Run("sweep", func(t *testing.T) {
		e := newEnv(t)
		e.submit(e.work())
		calls := 0
		e.eng.faultBusyTx = func() error { calls++; return boom }
		rep, err := e.eng.Sweep(e.ctx)
		if !errors.Is(err, boom) {
			t.Fatalf("sweep error = %v, want the injected one at once", err)
		}
		if len(rep.Expired) != 0 || len(rep.HardCapped) != 0 || len(rep.QueueTimedOut) != 0 {
			t.Fatalf("report = %+v, want none: the one attempt rolled back, so none of its values return", rep)
		}
		if calls != 1 {
			t.Fatalf("the transaction ran %d times, want the one attempt: a non-busy error is never retried", calls)
		}
	})
	t.Run("heartbeat", func(t *testing.T) {
		e := newEnv(t)
		e.submit(e.work())
		g := e.leaseAny(e.runner1)
		calls := 0
		e.eng.faultBusyTx = func() error { calls++; return boom }
		ans, expires, err := e.eng.Heartbeat(e.ctx, g.LeaseOwner, g.JobID, g.LeaseID, Progress{})
		if !errors.Is(err, boom) {
			t.Fatalf("heartbeat error = %v, want the injected one at once", err)
		}
		if ans != "" || !expires.IsZero() {
			t.Fatalf("answer = %q, expiry = %v, want none: the one attempt rolled back, so none of its values return", ans, expires)
		}
		if calls != 1 {
			t.Fatalf("the transaction ran %d times, want the one attempt: a non-busy error is never retried", calls)
		}
	})
}

// TestConcurrentSweepsDetectOneSuspension covers the commit bookkeeping
// under concurrency, with the interleaving driven rather than raced: the
// afterTxCommit hook parks the first sweep between its commit and its
// tick's recording — the window where the store's write lock is already
// gone — and a second sweep runs at that window on a file-backed store,
// where pooled connections make the concurrency real. The serialized
// bookkeeping holds the second sweep until the first's tick is recorded,
// so the suspension is detected once and the cap clock shifted once;
// without the serialization the second sweep reads the pre-commit tick,
// detects the same suspension again and shifts the deadlines twice. The
// parked world cannot finish the second sweep, so the wait below always
// picks the right branch: no flake in either world, and the assertions
// fail outright in the unserialized one.
func TestConcurrentSweepsDetectOneSuspension(t *testing.T) {
	e := newEnvAt(t, filepath.Join(t.TempDir(), "store.db"), nil)
	e.submit(e.work())
	g := e.leaseAny(e.runner1)
	e.mustSweep() // a tick before the sleep: the suspension is a gap from here
	before := e.str("SELECT first_leased FROM jobs WHERE id = ?", g.JobID)
	e.clock.Advance(2 * time.Hour) // the host sleeps: the next sweep sees the jump

	// The first committing sweep parks between its commit and the
	// recording of its tick: provably committed, provably unrecorded.
	parked := make(chan struct{})
	release := make(chan struct{})
	aDone := make(chan error, 1)
	var first sync.Once
	e.eng.afterTxCommit = func() {
		first.Do(func() {
			close(parked)
			<-release
		})
	}
	go func() {
		_, err := e.eng.Sweep(e.ctx)
		aDone <- err
	}()
	<-parked

	// The second sweep runs in the window. It may finish there only when
	// the bookkeeping does not serialize — the regression — and it can
	// never finish there when it does, so the timeout branch is the
	// serialized world's certain path.
	bDone := make(chan error, 1)
	go func() {
		_, err := e.eng.Sweep(e.ctx)
		bDone <- err
	}()
	select {
	case err := <-bDone: // not serialized: the second sweep slipped past the parked one
		close(release)
		if err != nil {
			t.Fatalf("the second sweep, unserialized, failed: %v", err)
		}
		if err := <-aDone; err != nil {
			t.Fatalf("the first sweep failed: %v", err)
		}
	case <-time.After(2 * time.Second): // serialized: the second sweep waits for the tick
		close(release)
		if err := <-aDone; err != nil {
			t.Fatalf("the first sweep failed: %v", err)
		}
		if err := <-bDone; err != nil {
			t.Fatalf("the second sweep failed: %v", err)
		}
	}
	if count := e.int("SELECT COUNT(*) FROM events WHERE kind = ?", evEngineSuspended); count != 1 {
		t.Fatalf("engine_suspended events = %d, want the one detection: a sweep behind a committed, unrecorded tick re-detected the suspension", count)
	}
	firstAt, err := parseTime(before)
	if err != nil {
		t.Fatalf("first_leased %q: %v", before, err)
	}
	if want := fmtTime(firstAt.Add(2 * time.Hour)); e.str("SELECT first_leased FROM jobs WHERE id = ?", g.JobID) != want {
		t.Fatalf("first_leased = %q, want %q: the sleep shifted the cap clock once, not once per sweep behind the window",
			e.str("SELECT first_leased FROM jobs WHERE id = ?", g.JobID), want)
	}
	// The recorded tick is the committed sweep's, so the gap is spent: a
	// further sweep with the engine awake suspends nothing, and a tick
	// the settlement lost would leave the gap open for it to re-detect.
	e.clock.Advance(time.Minute)
	e.mustSweep()
	if count := e.int("SELECT COUNT(*) FROM events WHERE kind = ?", evEngineSuspended); count != 1 {
		t.Fatalf("engine_suspended events after a wakeful sweep = %d, want still the one: a committed tick was lost", count)
	}
}
