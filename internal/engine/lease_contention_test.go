package engine

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

// TestLeaseFinishesOnACancelledRequest covers the poll's transaction
// living past its request: a runner that goes away mid-poll, or a
// caller cancelled before the poll began, must not roll the
// transaction back with statements still to run — the poll runs to its
// commit, or every later step of it fails on a transaction already
// ended ("sql: transaction has already been committed or rolled
// back", as the first public CI run saw across the lease path).
func TestLeaseFinishesOnACancelledRequest(t *testing.T) {
	e := newEnv(t)
	e.submit(e.work())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	g, err := e.eng.Lease(ctx, e.runner1, nil, nil)
	if err != nil {
		t.Fatalf("lease on a cancelled request: %v", err)
	}
	if g == nil || g.JobID == 0 {
		t.Fatalf("no grant on a cancelled request: %+v", g)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE status = 'leased'"); n != 1 {
		t.Fatalf("%d jobs leased, want the one grant the poll committed", n)
	}
	// The store stays consistent behind the committed grant: a live
	// poll finds nothing queued and hands nothing out.
	if g := e.leaseAny(e.runner1); g != nil {
		t.Fatalf("a live poll leased %+v behind the committed grant", g)
	}
}

// TestLeaseConcurrentUnderContention runs lease polls and submits
// against one file store at once: several goroutines polling while
// several write, the crossing that turns a slow store into SQLITE_BUSY
// and a cancelled request into a half-run transaction. Every job must
// be handed out exactly once — one live lease per key — with no error
// and no busy failure, and the store must drain clean behind them.
func TestLeaseConcurrentUnderContention(t *testing.T) {
	e := newEnvAt(t, filepath.Join(t.TempDir(), "store.db"), nil)
	const tasks = 8
	submits := make([]SubmitParams, tasks)
	for i := range submits {
		task := e.addTask("{}")
		submits[i] = SubmitParams{
			Key:       fmt.Sprintf("task:%d", task),
			ProjectID: e.project,
			TaskID:    task,
			Kind:      KindWork,
			Mode:      ModeFresh,
			Prompt:    "the composed prompt",
		}
	}

	var mu sync.Mutex
	granted := map[int64]int{}
	var errs []error
	record := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		errs = append(errs, err)
	}

	const pollers, writers, rounds = 4, 2, 10
	var wg sync.WaitGroup
	for p := 0; p < pollers; p++ {
		runner := e.runner1
		if p%2 == 1 {
			runner = e.runner2
		}
		wg.Add(1)
		go func(runner int64) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				g, err := e.eng.Lease(context.Background(), runner, nil, nil)
				if err != nil {
					record(fmt.Errorf("poll as runner %d: %w", runner, err))
					return
				}
				if g != nil {
					mu.Lock()
					granted[g.JobID]++
					mu.Unlock()
				}
			}
		}(runner)
	}
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(offset int) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				if _, err := e.eng.Submit(context.Background(), submits[(offset+i)%tasks]); err != nil {
					record(fmt.Errorf("submit %s: %w", submits[(offset+i)%tasks].Key, err))
					return
				}
			}
		}(w)
	}
	wg.Wait()
	if len(errs) > 0 {
		for _, err := range errs {
			t.Error(err)
		}
		t.FailNow()
	}

	// The queue drains: every submitted job is handed out exactly once,
	// and nothing stays queued behind the contention.
	for {
		g, err := e.eng.Lease(context.Background(), e.runner1, nil, nil)
		if err != nil {
			t.Fatalf("drain poll: %v", err)
		}
		if g == nil {
			break
		}
		mu.Lock()
		granted[g.JobID]++
		mu.Unlock()
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE status = 'queued'"); n != 0 {
		t.Fatalf("%d jobs still queued after the drain", n)
	}
	for id, n := range granted {
		if n != 1 {
			t.Fatalf("job %d granted %d times while live, want once", id, n)
		}
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE status = 'leased'"); int64(len(granted)) != n {
		t.Fatalf("%d jobs leased, %d grants counted", n, len(granted))
	}
}
