package engine

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

func TestSubmitCreatesAndNumbersRuns(t *testing.T) {
	e := newEnv(t)

	r := e.submit(e.work())
	if r.Answer != AnsCreated || r.Run != 1 {
		t.Fatalf("first submit = %s run %d; want created run 1", r.Answer, r.Run)
	}
	if status := e.str("SELECT status FROM jobs WHERE id = ?", r.JobID); status != statusQueued {
		t.Errorf("new job status = %q, want queued", status)
	}
	if n := e.int("SELECT followup FROM jobs WHERE id = ?", r.JobID); n != 0 {
		t.Errorf("new job followup = %d, want 0: creating clears the key's flag", n)
	}
	if queuedAt := e.str("SELECT queued_at FROM jobs WHERE id = ?", r.JobID); queuedAt == "" {
		t.Error("new job has no queued_at for the queue TTL to measure from")
	}

	// The foreman's key works the same way, with no task attached.
	f := e.submit(e.foremanTurn())
	if f.Answer != AnsCreated || f.Run != 1 {
		t.Fatalf("foreman submit = %s run %d; want created run 1", f.Answer, f.Run)
	}
	if kind := e.str("SELECT kind FROM jobs WHERE id = ?", f.JobID); kind != string(KindForeman) {
		t.Errorf("foreman job kind = %q, want foreman", kind)
	}
}

func TestSubmitCoalescesBehindLiveJob(t *testing.T) {
	for _, tc := range []struct{ name, status string }{
		{"queued", statusQueued},
		{"leased", statusLeased},
		{"running", statusRunning},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			first := e.submit(e.work())
			switch tc.status {
			case statusLeased:
				if g := e.leaseAny(e.runner1); g == nil || g.JobID != first.JobID {
					t.Fatal("lease did not take the submitted job")
				}
			case statusRunning:
				e.exec("UPDATE jobs SET status = 'running' WHERE id = ?", first.JobID)
			}

			second := e.submit(e.work())
			if second.Answer != AnsPending || second.JobID != first.JobID {
				t.Fatalf("submit behind a %s job = %+v; want pending pointing at job %d",
					tc.status, second, first.JobID)
			}
			if got := e.int("SELECT COUNT(*) FROM jobs WHERE key = (SELECT key FROM jobs WHERE id = ?)", first.JobID); got != 1 {
				t.Errorf("jobs for the key = %d, want 1: nothing is created behind a live job", got)
			}
			if n := e.int("SELECT followup FROM jobs WHERE id = ?", first.JobID); n != 1 {
				t.Errorf("live job followup = %d, want 1: the duplicate submit set the flag", n)
			}

			// Once the key is free again, the next submit creates again,
			// with the run number advanced and the flag cleared.
			e.exec("UPDATE jobs SET status = 'done' WHERE id = ?", first.JobID)
			third := e.submit(e.work())
			if third.Answer != AnsCreated || third.Run != 2 || third.JobID == first.JobID {
				t.Fatalf("submit after the key freed = %+v; want created run 2 on a new job", third)
			}
			if n := e.int("SELECT followup FROM jobs WHERE id = ?", third.JobID); n != 0 {
				t.Errorf("new job followup = %d, want 0", n)
			}
		})
	}
}

// raceSubmits fires n concurrent submits of the same work job at e and
// returns the one job they raced to create. Every submit must come back
// created or pending — exactly one created, the rest pending behind it —
// never an error: the loser of the one-live-job-per-key race coalesces.
func raceSubmits(t *testing.T, e *env, n int) SubmitResult {
	t.Helper()
	results := make([]SubmitResult, n)
	var errs []error
	var mu sync.Mutex
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			res, err := e.eng.Submit(e.ctx, e.work())
			mu.Lock()
			defer mu.Unlock()
			results[i] = res
			if err != nil {
				errs = append(errs, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("concurrent submits returned errors: %v", errs[0])
	}
	var created SubmitResult
	createdCount := 0
	for _, res := range results {
		switch res.Answer {
		case AnsCreated:
			createdCount++
			created = res
		case AnsPending:
		default:
			t.Fatalf("submit answered %s; want created or pending", res.Answer)
		}
	}
	if createdCount != 1 {
		t.Fatalf("created jobs = %d, want exactly 1", createdCount)
	}
	for _, res := range results {
		if res.Answer == AnsPending && res.JobID != created.JobID {
			t.Errorf("pending submit pointed at job %d; want the live job %d", res.JobID, created.JobID)
		}
	}
	return created
}

func TestConcurrentSubmitsCoalesce(t *testing.T) {
	e := newEnv(t)
	created := raceSubmits(t, e, 8)
	if got := e.int("SELECT COUNT(*) FROM jobs"); got != 1 {
		t.Errorf("jobs after the race = %d, want 1", got)
	}
	if n := e.int("SELECT followup FROM jobs WHERE id = ?", created.JobID); n != 1 {
		t.Errorf("live job followup = %d, want 1", n)
	}
}

// The same race on a file-backed store: the pool hands the goroutines
// several real connections to one database, so they contend on the
// unique live-job index across connections and the losers take the
// pending path from other connections, not just after the winner on a
// shared one.
func TestConcurrentSubmitsCoalesceOnFileStore(t *testing.T) {
	e := newEnvAt(t, filepath.Join(t.TempDir(), "jobs.db"), nil)
	created := raceSubmits(t, e, 8)
	if got := e.int("SELECT COUNT(*) FROM jobs"); got != 1 {
		t.Errorf("jobs after the race = %d, want 1", got)
	}
	if n := e.int("SELECT followup FROM jobs WHERE id = ?", created.JobID); n != 1 {
		t.Errorf("live job followup = %d, want 1", n)
	}
}

func TestSubmitValidatesKindAndTask(t *testing.T) {
	e := newEnv(t)

	p := e.work()
	p.TaskID = 0
	if _, err := e.eng.Submit(e.ctx, p); err == nil {
		t.Error("work job without a task succeeded; want refusal")
	}
	f := e.foremanTurn()
	f.TaskID = e.task
	if _, err := e.eng.Submit(e.ctx, f); err == nil {
		t.Error("foreman turn with a task succeeded; want refusal")
	}
	noKey := e.work()
	noKey.Key = ""
	if _, err := e.eng.Submit(e.ctx, noKey); err == nil {
		t.Error("submit without a key succeeded; want refusal")
	}
}

func TestSubmitRoundCaps(t *testing.T) {
	for _, tc := range []struct {
		kind Kind
		cap  int
	}{
		{KindFix, 3},
		{KindReview, 3},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			e := newEnv(t)
			for i := 1; i <= tc.cap; i++ {
				p := e.work()
				p.Kind = tc.kind
				p.Mode = ModeSession
				r := e.submit(p)
				if r.Answer != AnsCreated {
					t.Fatalf("round %d submit = %s; want created", i, r.Answer)
				}
				g := e.leaseAny(e.runner1)
				if g == nil {
					t.Fatalf("round %d: nothing to lease", i)
				}
				if ans := e.complete(g, done()); ans != AnsOK {
					t.Fatalf("round %d complete = %s; want ok", i, ans)
				}
			}
			p := e.work()
			p.Kind = tc.kind
			r := e.submit(p)
			if r.Answer != AnsCapped {
				t.Fatalf("submit past the cap = %s; want capped", r.Answer)
			}
			if got := e.int("SELECT COUNT(*) FROM jobs WHERE kind = ?", string(tc.kind)); got != int64(tc.cap) {
				t.Errorf("%s jobs = %d, want %d: a capped submit creates nothing", tc.kind, got, tc.cap)
			}

			// Work jobs carry no round cap: the key keeps cycling. Each
			// cycle completes with the worker asking its question (§3),
			// which parks the task without queueing anything behind it —
			// a done would queue the task's review job on the same key
			// (§7), and coalescing behind it is a different test.
			for i := 0; i < tc.cap+2; i++ {
				r := e.submit(e.work())
				if r.Answer != AnsCreated {
					t.Fatalf("work submit %d = %s; want created", i, r.Answer)
				}
				g := e.leaseAny(e.runner1)
				if g == nil || e.complete(g, Completion{Result: `{"status":"needs_help","question":"stuck"}`}) != AnsOK {
					t.Fatalf("work cycle %d failed", i)
				}
			}
		})
	}
}

func TestSubmitTaskCapOverrides(t *testing.T) {
	e := newEnv(t)
	task := e.addTask(`{"fix": 1}`)
	p := SubmitParams{
		Key:       fmt.Sprintf("task:%d", task),
		ProjectID: e.project,
		TaskID:    task,
		Kind:      KindFix,
		Mode:      ModeSession,
		Prompt:    "fix it",
	}
	if r := e.submit(p); r.Answer != AnsCreated {
		t.Fatalf("first fix = %s; want created", r.Answer)
	}
	if g := e.leaseAny(e.runner1); g == nil || e.complete(g, done()) != AnsOK {
		t.Fatal("first fix did not run to completion")
	}
	if r := e.submit(p); r.Answer != AnsCapped {
		t.Errorf("second fix against a fix cap of 1 = %s; want capped", r.Answer)
	}

	// The override is per task: the default task still allows three.
	q := e.work()
	q.Kind = KindFix
	if r := e.submit(q); r.Answer != AnsCreated {
		t.Errorf("fix on the default-caps task = %s; want created", r.Answer)
	}
}

func TestSubmitRestartRounds(t *testing.T) {
	e := newEnv(t)
	first := e.submit(e.work())
	if g := e.leaseAny(e.runner1); g == nil {
		t.Fatal("nothing to lease")
	}

	// A restart cancels the live job and takes the key (§5.6).
	restart := e.work()
	restart.Restart = true
	restart.Prompt = "try again with this instruction"
	r := e.submit(restart)
	if r.Answer != AnsCreated || r.JobID == first.JobID {
		t.Fatalf("restart submit = %+v; want a created new job", r)
	}
	if status := e.str("SELECT status FROM jobs WHERE id = ?", first.JobID); status != statusCancelled {
		t.Errorf("replaced job status = %q, want cancelled", status)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND restart = 1", e.task); n != 1 {
		t.Errorf("restart jobs = %d, want 1", n)
	}
	if g := e.leaseAny(e.runner1); g == nil || g.JobID != r.JobID || e.complete(g, done()) != AnsOK {
		t.Fatal("the restart job did not run")
	}

	// One more restart fits the default cap of 2; the third is capped.
	if r := e.submit(restart); r.Answer != AnsCreated {
		t.Fatalf("second restart = %s; want created", r.Answer)
	}
	if g := e.leaseAny(e.runner1); g == nil || e.complete(g, done()) != AnsOK {
		t.Fatal("the second restart job did not run")
	}
	if r := e.submit(restart); r.Answer != AnsCapped {
		t.Errorf("third restart = %s, want capped against the default of 2", r.Answer)
	}
	// The original, two restarts, and the two review jobs the restarts'
	// done completions queued (§7) — the second restart replaced the
	// first review job with its own, as a restart replaces whatever is
	// live on the key (§5.6).
	if got := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ?", e.task); got != 5 {
		t.Errorf("task jobs = %d, want 5 (the original, two restarts and two reviews)", got)
	}
}

// A restart is its own budget (§14): a restarted fix or review counts
// against the restart cap alone, never against the cap of the kind it
// restarts, and the plain fix/review rounds still get their full
// allowance after the restarts.
func TestSubmitRestartedFixAndReviewCountAgainstRestartCapOnly(t *testing.T) {
	for _, tc := range []struct {
		kind       Kind
		plainCap   int
		restartCap int
	}{
		{KindFix, 3, 2},
		{KindReview, 3, 2},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			e := newEnv(t)
			round := func(restart bool) SubmitParams {
				p := e.work()
				p.Kind = tc.kind
				p.Mode = ModeSession
				p.Restart = restart
				return p
			}
			run := func(p SubmitParams) {
				t.Helper()
				r := e.submit(p)
				if r.Answer != AnsCreated {
					t.Fatalf("submit = %s; want created", r.Answer)
				}
				g := e.leaseAny(e.runner1)
				if g == nil || e.complete(g, done()) != AnsOK {
					t.Fatal("the round did not run to completion")
				}
			}

			// One plain round, then restarts to the restart cap: the
			// restarts must not consume the plain rounds' budget.
			run(round(false))
			for i := 0; i < tc.restartCap; i++ {
				run(round(true))
			}
			if r := e.submit(round(true)); r.Answer != AnsCapped {
				t.Fatalf("one restart too many = %s; want capped against the restart cap", r.Answer)
			}

			// The plain cap is untouched by the restarts: the remaining
			// plain rounds still fit, and only then is the cap spent.
			for i := 1; i < tc.plainCap; i++ {
				run(round(false))
			}
			if r := e.submit(round(false)); r.Answer != AnsCapped {
				t.Fatalf("plain round past the %s cap = %s; want capped", tc.kind, r.Answer)
			}
			if got := e.int("SELECT COUNT(*) FROM jobs WHERE kind = ?", string(tc.kind)); got != int64(tc.plainCap+tc.restartCap) {
				t.Errorf("%s jobs = %d, want %d (%d plain and %d restarts)",
					tc.kind, got, tc.plainCap+tc.restartCap, tc.plainCap, tc.restartCap)
			}
		})
	}
}
