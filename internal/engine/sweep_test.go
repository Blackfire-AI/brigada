package engine

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestSweepKeepsExpiredLeaseID(t *testing.T) {
	e := newEnv(t)
	e.submit(e.work())
	g := e.leaseAny(e.runner1)
	rep := e.expireLease() // advances past the TTL and sweeps

	if len(rep.Expired) != 1 || rep.Expired[0] != g.JobID {
		t.Fatalf("sweep report expired = %v, want [%d]", rep.Expired, g.JobID)
	}
	if status := e.str("SELECT status FROM jobs WHERE id = ?", g.JobID); status != statusQueued {
		t.Errorf("status = %q, want queued", status)
	}
	if leaseID := e.str("SELECT lease_id FROM jobs WHERE id = ?", g.JobID); leaseID != g.LeaseID {
		t.Errorf("lease_id = %q, want %q kept so the holder can still revive or complete", leaseID, g.LeaseID)
	}
	// The queue TTL clock restarts at the requeue the sweep observes.
	if queuedAt := e.str("SELECT queued_at FROM jobs WHERE id = ?", g.JobID); queuedAt != fmtTime(e.clock.Now()) {
		t.Errorf("queued_at = %q, want the requeue moment", queuedAt)
	}
}

// TestSweepSuspensionKeepsTheLease: a host sleep makes the sweep's wall
// clock jump far past the sweep interval, so the sweep restarts the
// lease's deadline from now instead of requeuing the still-live job
// (§13). The suspension tests live beside the sweep; see
// suspension_test.go for the other deadlines.
func TestSweepSuspensionKeepsTheLease(t *testing.T) {
	e := newEnv(t)
	e.submit(e.work())
	g := e.leaseAny(e.runner1)

	e.clock.Advance(time.Hour)
	rep := e.mustSweep()

	if len(rep.Expired) != 0 {
		t.Fatalf("sweep report expired = %v, want none after a host sleep", rep.Expired)
	}
	if status := e.str("SELECT status FROM jobs WHERE id = ?", g.JobID); status != statusLeased {
		t.Errorf("status = %q, want leased: the sleep did not end the hold", status)
	}
	if expires := e.str("SELECT lease_expires_at FROM jobs WHERE id = ?", g.JobID); expires != fmtTime(e.clock.Now().Add(e.eng.cfg.LeaseTTL)) {
		t.Errorf("lease_expires_at = %q, want %q (restarted from now)", expires, fmtTime(e.clock.Now().Add(e.eng.cfg.LeaseTTL)))
	}
	if n := e.int("SELECT COUNT(*) FROM events WHERE kind = ?", evEngineSuspended); n != 1 {
		t.Errorf("engine_suspended events = %d, want 1", n)
	}
}

func TestSweepHardCaps(t *testing.T) {
	// The lease outlives the caps, so the cap — not an expiry — is what
	// ends the job and the accrual is the cap itself.
	for _, tc := range []struct {
		kind Kind
		cap  time.Duration
	}{
		{KindWork, 45 * time.Minute},
		{KindFix, 45 * time.Minute},
		{KindReview, 25 * time.Minute},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			e := newEnvCfg(t, func(c *Config) { c.LeaseTTL = time.Hour })
			p := e.work()
			p.Kind = tc.kind
			p.Mode = ModeSession
			r := e.submit(p)
			if g := e.leaseAny(e.runner1); g == nil {
				t.Fatal("nothing to lease")
			}

			// A hair under the cap the job is untouched.
			e.elapse(tc.cap - time.Minute)
			if rep := e.mustSweep(); len(rep.HardCapped) != 0 {
				t.Fatalf("sweep capped a minute early: %v", rep.HardCapped)
			}

			// One small wakeful gap past the cap, so the cap fires in this
			// sweep rather than being read as a host sleep (§13).
			e.clock.Advance(2 * time.Minute)
			rep := e.mustSweep()
			if !slices.Contains(rep.HardCapped, r.JobID) {
				t.Fatalf("sweep report hard-capped = %v, want job %d", rep.HardCapped, r.JobID)
			}
			if status := e.str("SELECT status FROM jobs WHERE id = ?", r.JobID); status != statusFailed {
				t.Errorf("status = %q, want failed", status)
			}
			result := e.str("SELECT result FROM jobs WHERE id = ?", r.JobID)
			if !strings.Contains(result, "hard cap") || !strings.Contains(result, capText(tc.cap)) {
				t.Errorf("result = %q, want the failure to name the %s cap", result, capText(tc.cap))
			}
			// Wall time is banked only up to the cap itself.
			if secs := e.int("SELECT leased_seconds FROM jobs WHERE id = ?", r.JobID); secs != int64(tc.cap.Seconds()) {
				t.Errorf("leased_seconds = %d, want %d", secs, int64(tc.cap.Seconds()))
			}
		})
	}

	t.Run("foreman cap from config", func(t *testing.T) {
		e := newEnvCfg(t, func(c *Config) {
			c.HardCaps = map[Kind]time.Duration{KindForeman: 5 * time.Minute}
			c.LeaseTTL = time.Hour // the cap fires, not the expiry
		})
		e.seedAgent(e.runner1, "claude", "claude", "sonnet", "claude") // the project's foreman pair
		r := e.submit(e.foremanTurn())
		if g := e.leaseClaude(e.runner1); g == nil {
			t.Fatal("nothing to lease")
		}
		e.elapse(6 * time.Minute)
		e.mustSweep()
		if status := e.str("SELECT status FROM jobs WHERE id = ?", r.JobID); status != statusFailed {
			t.Fatalf("status = %q, want failed by the 5m foreman cap", status)
		}
		if result := e.str("SELECT result FROM jobs WHERE id = ?", r.JobID); !strings.Contains(result, "5m") {
			t.Errorf("result = %q, want the failure to name the 5m cap", result)
		}
	})

	t.Run("heartbeat loses the lease its own call capped", func(t *testing.T) {
		// The lease must outlive the wakeful run-up, so the hard cap —
		// not the lease expiry or the queue TTL — is what the heartbeat's
		// own sweep does.
		e := newEnvCfg(t, func(c *Config) { c.LeaseTTL = time.Hour })
		r := e.submit(e.work())
		g := e.leaseAny(e.runner1)
		// Wakefully to just under the cap, then one small gap past it, so
		// the cap is this heartbeat's own sweep's work, not a sleep (§13).
		e.elapse(44 * time.Minute)
		e.clock.Advance(2 * time.Minute)
		ans, _, err := e.eng.Heartbeat(e.ctx, g.LeaseOwner, g.JobID, g.LeaseID, Progress{})
		if err != nil {
			t.Fatalf("heartbeat: %v", err)
		}
		if ans != AnsLost {
			t.Errorf("heartbeat = %s; want lost: this call's sweep hard-capped the caller's lease", ans)
		}
		if status := e.str("SELECT status FROM jobs WHERE id = ?", r.JobID); status != statusFailed {
			t.Errorf("status = %q, want failed", status)
		}
		// A later heartbeat on the same failed job is gone, not lost:
		// nothing of that call capped it.
		if ans, _, _ := e.eng.Heartbeat(e.ctx, g.LeaseOwner, g.JobID, g.LeaseID, Progress{}); ans != AnsGone {
			t.Errorf("later heartbeat = %s; want gone", ans)
		}
	})
}

// TestSweepHardCapFromCurrentLease (issue #36): the cap measures from
// the current lease, not the first. A job requeued mid-cap — its lease
// lapsed when a runner or the engine restarted — gets the full cap on
// its next lease, while a job that never loses its lease is still
// killed at the cap (TestSweepHardCaps above).
func TestSweepHardCapFromCurrentLease(t *testing.T) {
	cap45 := DefaultConfig().HardCaps[KindWork]
	ttl := DefaultConfig().LeaseTTL
	e := newEnv(t) // default 90 s LeaseTTL, 45 m work cap
	r := e.submit(e.work())
	e.leaseAny(e.runner1)
	firstAt := e.str("SELECT first_leased FROM jobs WHERE id = ?", r.JobID)
	if firstAt == "" {
		t.Fatal("first_leased = '', want the first lease stamped")
	}

	// The 90 s lease lapses and the sweep requeues the job; it must not
	// count the requeued job against the cap: no lease runs.
	if rep := e.expireLease(); len(rep.HardCapped) != 0 {
		t.Fatalf("sweep capped a job with no lease: %v", rep.HardCapped)
	}
	if status := e.str("SELECT status FROM jobs WHERE id = ?", r.JobID); status != statusQueued {
		t.Fatalf("status = %q, want queued after the lease lapsed", status)
	}

	// The next lease starts the worker from scratch and gets the whole
	// cap again, measured from the re-lease, not the first.
	g := e.leaseAny(e.runner1)
	if g == nil {
		t.Fatal("nothing to lease")
	}
	if got := e.str("SELECT first_leased FROM jobs WHERE id = ?", r.JobID); got != firstAt {
		t.Errorf("first_leased = %q after the re-lease, want %q kept", got, firstAt)
	}
	// The re-lease outlives what is left to test, so its expiry does not
	// end it before the cap does (TestSweepHardCaps covers that side).
	e.exec("UPDATE jobs SET lease_expires_at = ? WHERE id = ?",
		fmtTime(e.clock.Now().Add(cap45+time.Hour)), r.JobID)

	// Past the first lease's whole cap — the re-lease is 91 s in, so
	// 44 m more is 45 m 31 s from the first lease, past its cap — but
	// inside the re-lease's: the sweep and the heartbeat leave it alone.
	e.elapse(44 * time.Minute)
	if rep := e.mustSweep(); len(rep.HardCapped) != 0 {
		t.Fatalf("sweep capped the re-leased job on the first lease's clock: %v", rep.HardCapped)
	}
	if ans := e.heartbeat(g); ans != AnsOK {
		t.Errorf("heartbeat = %s; want ok: inside the re-lease's cap", ans)
	}
	// The beat extended the expiry again; re-pin it past the cap.
	e.exec("UPDATE jobs SET lease_expires_at = ? WHERE id = ?",
		fmtTime(e.clock.Now().Add(cap45)), r.JobID)

	// The full cap after the re-lease still kills it, banking the
	// lapsed lease up to its expiry and the re-lease up to the cap.
	e.clock.Advance(2 * time.Minute) // the re-lease is 46 m in, past its 45 m cap
	rep := e.mustSweep()
	if !slices.Contains(rep.HardCapped, r.JobID) {
		t.Fatalf("sweep report hard-capped = %v, want job %d", rep.HardCapped, r.JobID)
	}
	if status := e.str("SELECT status FROM jobs WHERE id = ?", r.JobID); status != statusFailed {
		t.Errorf("status = %q, want failed", status)
	}
	want := int64(ttl.Seconds()) + int64(cap45.Seconds())
	if secs := e.int("SELECT leased_seconds FROM jobs WHERE id = ?", r.JobID); secs != want {
		t.Errorf("leased_seconds = %d, want %d", secs, want)
	}
}

func TestSweepQueueTTL(t *testing.T) {
	e := newEnv(t)
	r := e.submit(e.work())

	e.elapse(9*time.Minute + 59*time.Second)
	if rep := e.mustSweep(); len(rep.QueueTimedOut) != 0 {
		t.Fatalf("sweep timed out at 9m59s: %v", rep.QueueTimedOut)
	}
	if status := e.str("SELECT status FROM jobs WHERE id = ?", r.JobID); status != statusQueued {
		t.Fatalf("status = %q, want still queued", status)
	}

	e.clock.Advance(2 * time.Second)
	rep := e.mustSweep()
	if !slices.Contains(rep.QueueTimedOut, r.JobID) {
		t.Fatalf("sweep report timed out = %v, want job %d", rep.QueueTimedOut, r.JobID)
	}
	if status := e.str("SELECT status FROM jobs WHERE id = ?", r.JobID); status != statusFailed {
		t.Errorf("status = %q, want failed", status)
	}
	if result := e.str("SELECT result FROM jobs WHERE id = ?", r.JobID); !strings.Contains(result, "queue TTL") {
		t.Errorf("result = %q, want the queue TTL failure", result)
	}
	// The failed job cannot wedge its key: a new submit takes it.
	if next := e.submit(e.work()); next.Answer != AnsCreated || next.Run != 2 {
		t.Errorf("submit after the TTL failure = %+v; want created run 2", next)
	}

	t.Run("busy runner", func(t *testing.T) { testSweepQueueTTLBusyRunner(t) })
	t.Run("busy runner not polling for the harness", func(t *testing.T) { testSweepQueueTTLNarrowedPoll(t) })
}

// testSweepQueueTTLNarrowedPoll: a full runner registered for two
// harnesses but polling for one is no candidate for a job pinned to the
// other — the lease would never give it the job — so that job keeps the
// plain TTL, while a job pinned to the polled harness keeps waiting.
func testSweepQueueTTLNarrowedPoll(t *testing.T) {
	e := newEnvCfg(t, func(c *Config) { c.LeaseTTL = time.Hour })
	e.seedAgent(e.runner1, "claude", "claude", "sonnet", "work")
	e.seedAgent(e.runner1, "codex", "codex", "gpt", "other")
	e.submit(e.work())
	g := e.lease(e.runner1, []string{"claude"}, nil) // polls for claude only
	if g == nil {
		t.Fatal("nothing to lease")
	}
	pinned := func(harness string) SubmitResult {
		p := e.work()
		p.TaskID = e.addTask("{}")
		p.Key = keyOf(p.TaskID)
		p.Harness = harness
		return e.submit(p)
	}
	codex := pinned("codex")
	claude := pinned("claude")

	e.elapse(9 * time.Minute)
	if ans := e.heartbeat(g); ans != AnsOK {
		t.Fatalf("heartbeat = %s; want ok", ans)
	}
	e.elapse(9 * time.Minute)
	if ans := e.heartbeat(g); ans != AnsOK {
		t.Fatalf("heartbeat = %s; want ok", ans)
	}
	status := func(job int64) string { return e.str("SELECT status FROM jobs WHERE id = ?", job) }
	if status(codex.JobID) != statusFailed {
		t.Errorf("codex-pinned job status = %q, want failed: no runner offers codex on its polls", status(codex.JobID))
	}
	if status(claude.JobID) != statusQueued {
		t.Errorf("claude-pinned job status = %q, want queued: its runner is full, not absent", status(claude.JobID))
	}
}

// testSweepQueueTTLBusyRunner: a queued job whose only candidate runner
// is at its advertised capacity (unset = one job, a sequential runner)
// is waiting for a slot, not unserved — it survives the TTL, shows as a
// runner-layer hold, and is parked only after a slot has been free for
// the whole TTL. A job no runner can serve keeps the plain TTL.
func testSweepQueueTTLBusyRunner(t *testing.T) {
	e := newEnvCfg(t, func(c *Config) { c.LeaseTTL = time.Hour })
	e.seedAgent(e.runner1, "claude", "claude", "sonnet", "work")
	e.submit(e.work())
	g := e.leaseAny(e.runner1)
	if g == nil {
		t.Fatal("nothing to lease")
	}
	waiting := e.submit(e.workOn(e.addTask("{}")))
	unservable := e.work()
	unservable.TaskID = e.addTask("{}")
	unservable.Key = keyOf(unservable.TaskID)
	unservable.Harness = "gemini" // no runner offers it
	unservableJob := e.submit(unservable)

	status := func(job int64) string { return e.str("SELECT status FROM jobs WHERE id = ?", job) }
	// step moves time on with the busy runner still beating, so it is not stale.
	step := func(d time.Duration) SweepReport {
		e.elapse(d)
		if ans := e.heartbeat(g); ans != AnsOK {
			t.Fatalf("heartbeat = %s; want ok", ans)
		}
		return e.mustSweep()
	}

	rep := step(9 * time.Minute)
	if layer := e.str("SELECT COALESCE(cap_held_layer, '') FROM jobs WHERE id = ?", waiting.JobID); layer != "runner" {
		t.Fatalf("waiting job hold layer = %q, want runner", layer)
	}
	// The status names it: held at the runner layer, waiting for a slot.
	info, err := e.eng.Status(e.ctx, 0)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(info.Held) != 1 || info.Held[0].ID != waiting.JobID || info.Held[0].Layer != "runner" {
		t.Errorf("status held = %+v, want job %d at the runner layer", info.Held, waiting.JobID)
	}
	rep = step(9 * time.Minute) // 18 minutes since it queued
	if slices.Contains(rep.QueueTimedOut, waiting.JobID) || status(waiting.JobID) != statusQueued {
		t.Fatalf("the job waiting for a runner slot was parked at 18m: %v", rep.QueueTimedOut)
	}
	// (the heartbeat's own sweep may have parked it, so read the status)
	if status(unservableJob.JobID) != statusFailed {
		t.Errorf("unservable job status = %q, want failed: no candidate runner keeps the TTL", status(unservableJob.JobID))
	}
	if rep = step(9 * time.Minute); slices.Contains(rep.QueueTimedOut, waiting.JobID) {
		t.Fatalf("the job waiting for a runner slot was parked at 27m")
	}

	// The slot frees: the hold lifts and the clock restarts there.
	if ans := e.complete(g, done()); ans != AnsOK {
		t.Fatalf("complete = %s; want ok", ans)
	}
	e.mustSweep()
	if layer := e.str("SELECT COALESCE(cap_held_layer, '') FROM jobs WHERE id = ?", waiting.JobID); layer != "" {
		t.Errorf("hold layer after the slot freed = %q, want it lifted", layer)
	}
	e.elapse(9*time.Minute + 59*time.Second)
	if rep := e.mustSweep(); slices.Contains(rep.QueueTimedOut, waiting.JobID) {
		t.Fatalf("parked at 9m59s of a free slot: %v", rep.QueueTimedOut)
	}
	e.clock.Advance(2 * time.Second)
	if rep := e.mustSweep(); !slices.Contains(rep.QueueTimedOut, waiting.JobID) {
		t.Fatalf("sweep timed out = %v, want job %d parked after ten minutes with a free slot", rep.QueueTimedOut, waiting.JobID)
	}
}

// checkSweptTaskParked asserts a sweep-failed task job parked its task at
// needs_foreman, woke the foreman, and left the task restartable.
func checkSweptTaskParked(t *testing.T, e *env) {
	t.Helper()
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskNeedsForeman {
		t.Errorf("task state = %q, want %q", state, taskNeedsForeman)
	}
	if n := e.countEvents(evTaskFailed); n != 1 {
		t.Errorf("task_failed events = %d, want 1", n)
	}
	if n := e.foremanJobs(); n != 1 {
		t.Errorf("foreman jobs = %d, want 1", n)
	}
	if _, err := e.eng.RestartTask(e.ctx, e.project, e.task, "redo"); err != nil {
		t.Fatalf("RestartTask after the sweep failure: %v", err)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskReady {
		t.Errorf("task state after restart = %q, want %q", state, taskReady)
	}
	if n := e.countEvents(evTaskRestarted); n != 1 {
		t.Errorf("task_restarted events = %d, want 1", n)
	}
}

// TestSweepHardCapParksTask: a hard-capped task job parks its task.
func TestSweepHardCapParksTask(t *testing.T) {
	e := newEnvCfg(t, func(c *Config) { c.LeaseTTL = time.Hour }) // the cap fires, not the expiry
	r := e.submit(e.work())
	if g := e.leaseAny(e.runner1); g == nil {
		t.Fatal("nothing to lease")
	}
	e.elapse(46 * time.Minute)
	if status := e.str("SELECT status FROM jobs WHERE id = ?", r.JobID); status != statusFailed {
		t.Fatalf("status = %q, want failed by the hard cap", status)
	}
	checkSweptTaskParked(t, e)
}

// TestSweepQueueTTLParksTask: a queue-TTL-failed task job parks its task.
func TestSweepQueueTTLParksTask(t *testing.T) {
	e := newEnv(t)
	r := e.submit(e.work())
	e.elapse(10*time.Minute + 2*time.Second)
	if status := e.str("SELECT status FROM jobs WHERE id = ?", r.JobID); status != statusFailed {
		t.Fatalf("status = %q, want failed by the queue TTL", status)
	}
	checkSweptTaskParked(t, e)
}

func TestWorkerTimeAccrual(t *testing.T) {
	e := newEnv(t)

	// Job A: one lease kept alive by a heartbeat for 90 s in total. The
	// heartbeat extends the lease but does not restart the accrual.
	a := e.submit(e.work())
	g := e.leaseAny(e.runner1)
	if g == nil {
		t.Fatal("nothing to lease")
	}
	e.clock.Advance(time.Minute)
	if ans := e.heartbeat(g); ans != AnsOK {
		t.Fatalf("heartbeat A = %s; want ok", ans)
	}
	e.clock.Advance(30 * time.Second)
	if ans := e.complete(g, done()); ans != AnsOK {
		t.Fatalf("complete A = %s; want ok", ans)
	}
	if secs := e.int("SELECT leased_seconds FROM jobs WHERE id = ?", a.JobID); secs != 90 {
		t.Errorf("job A leased_seconds = %d, want 90", secs)
	}

	// Job B: a lease that lapses and is completed late. The 90 s of the
	// lapsed lease are banked at expiry; the late complete banks nothing
	// more — no lease was running.
	b := e.submit(e.work())
	g = e.leaseAny(e.runner1)
	if g == nil {
		t.Fatal("nothing to lease")
	}
	e.expireLease()
	e.clock.Advance(29 * time.Second)
	if ans := e.complete(g, done()); ans != AnsOK {
		t.Fatalf("complete B = %s; want ok after expiry", ans)
	}
	if secs := e.int("SELECT leased_seconds FROM jobs WHERE id = ?", b.JobID); secs != 90 {
		t.Errorf("job B leased_seconds = %d, want 90", secs)
	}

	// Job C: one lapsed lease (90 s) plus a second runner's lease
	// completed 30 s in.
	c := e.submit(e.work())
	g = e.leaseAny(e.runner1)
	if g == nil {
		t.Fatal("nothing to lease")
	}
	e.expireLease()
	g = e.leaseAny(e.runner2)
	if g == nil {
		t.Fatal("the other runner could not re-lease")
	}
	e.clock.Advance(30 * time.Second)
	if ans := e.complete(g, done()); ans != AnsOK {
		t.Fatalf("complete C = %s; want ok", ans)
	}
	if secs := e.int("SELECT leased_seconds FROM jobs WHERE id = ?", c.JobID); secs != 90+30 {
		t.Errorf("job C leased_seconds = %d, want %d", secs, 90+30)
	}

	// The per-plan worker budget reads exactly this sum (§14).
	got, err := e.eng.PlanWorkerSeconds(e.ctx, e.plan)
	if err != nil {
		t.Fatalf("plan worker seconds: %v", err)
	}
	if want := 300 * time.Second; got != want {
		t.Errorf("plan worker time = %v, want %v", got, want)
	}

	// A release banks its lease's time too.
	d := e.submit(e.work())
	g = e.leaseAny(e.runner1)
	if g == nil {
		t.Fatal("nothing to lease")
	}
	e.clock.Advance(45 * time.Second)
	if ans := e.release(e.runner1, g, ""); ans != AnsOK {
		t.Fatalf("release D = %s; want ok", ans)
	}
	if secs := e.int("SELECT leased_seconds FROM jobs WHERE id = ?", d.JobID); secs != 45 {
		t.Errorf("job D leased_seconds = %d, want 45", secs)
	}
}
