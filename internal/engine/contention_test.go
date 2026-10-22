package engine

import (
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/store"
)

// TestLeasePollsReadersAndUploadsShareTheStore runs the calls a busy
// engine serves — lease polls from two runners, the runner resolution a
// runner endpoint begins with (RunnerID), the job status read, a
// heartbeat and artifact uploads for the held lease — against one
// file-backed, write-ahead-logged store at once, the way the race
// detector meets them on a small CI runner. Nothing may fail, and busy
// in particular must not: reads run deferred (Store.View) so they hold
// no write lock, and the calls that write run on their own contexts so
// a crossing waits the busy timeout out instead of dying with the
// request.
func TestLeasePollsReadersAndUploadsShareTheStore(t *testing.T) {
	dir := t.TempDir()
	e := newEnvAt(t, filepath.Join(dir, "store.db"), func(c *Config) {
		c.DataDir = dir
	})
	e.submit(e.work())
	g := e.leaseAny(e.runner1)

	// Each worker makes a fixed number of calls, not a timed burst: the
	// load is the same on a fast machine and a slow one, and no call is
	// cancelled mid-transaction at shutdown (a cancelled context rolls a
	// running transaction back under its statements' feet, which is the
	// tx-after-end failure this test is not about).
	const calls = 150
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	worker := func(name string, fn func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range calls {
				if err := fn(); err != nil {
					errs <- fmt.Errorf("%s: %w", name, err)
					return
				}
			}
		}()
	}
	var uploads atomic.Int64
	worker("lease poll runner-1", func() error {
		_, err := e.eng.Lease(e.ctx, e.runner1, nil, nil)
		return err
	})
	worker("lease poll runner-2", func() error {
		_, err := e.eng.Lease(e.ctx, e.runner2, nil, nil)
		return err
	})
	worker("resolve runner", func() error {
		_, err := e.eng.RunnerID(e.ctx, "runner-1")
		return err
	})
	worker("read job", func() error {
		_, err := e.eng.Job(e.ctx, g.JobID)
		return err
	})
	worker("heartbeat", func() error {
		_, _, err := e.eng.Heartbeat(e.ctx, g.LeaseOwner, g.JobID, g.LeaseID, Progress{})
		return err
	})
	// Each upload carries fresh content, so the write path — the fence
	// checks, the blob, the row — runs every time, not just the
	// idempotent already.
	worker("upload artifact", func() error {
		data := fmt.Appendf(nil, "check log line %d\n", uploads.Add(1))
		_, _, err := e.eng.UploadArtifact(e.ctx, g.LeaseOwner, g.JobID, g.LeaseID, ArtifactCheckLog, data)
		return err
	})
	wg.Wait()
	close(errs)
	for err := range errs {
		if store.IsBusy(err) {
			t.Errorf("the store went busy under mixed load: %v", err)
			continue
		}
		t.Errorf("a call failed under mixed load: %v", err)
	}
}
