package engine

import (
	"testing"
	"time"
)

// The sweep's suspension rule (§13): a host sleep makes the engine's own
// wall clock jump far past the sweep interval between two ticks, so the
// sweep restarts every wall-clock deadline from now instead of acting on
// it. These tests play a sleep as one clock jump and a wakeful engine as
// a series of sweep-interval steps (e.elapse).

// TestSweepSuspensionRestartsQueueTTL: a queued job's wait did not
// include the sleep, so the queue-TTL clock restarts from now.
func TestSweepSuspensionRestartsQueueTTL(t *testing.T) {
	e := newEnv(t)
	r := e.submit(e.work())

	e.clock.Advance(40 * time.Minute)
	e.mustSweep()

	if status := e.str("SELECT status FROM jobs WHERE id = ?", r.JobID); status != statusQueued {
		t.Fatalf("status = %q, want queued: the sleep is not queue wait", status)
	}
	if queuedAt := e.str("SELECT queued_at FROM jobs WHERE id = ?", r.JobID); queuedAt != fmtTime(e.clock.Now()) {
		t.Errorf("queued_at = %q, want restarted at wake %q", queuedAt, fmtTime(e.clock.Now()))
	}

	// A full TTL after the wake, with the engine awake, still parks it.
	e.elapse(10*time.Minute + time.Second)
	if status := e.str("SELECT status FROM jobs WHERE id = ?", r.JobID); status != statusFailed {
		t.Errorf("status = %q, want failed by the queue TTL after a wakeful TTL", status)
	}
}

// TestSweepSuspensionKeepsRunnerRegistration: a runner unseen only
// because the engine slept keeps its agents healthy; the stale window
// restarts at wake.
func TestSweepSuspensionKeepsRunnerRegistration(t *testing.T) {
	e := newEnv(t)
	e.seedAgent(e.runner1, "claude", "claude", "sonnet", "work")
	e.mustSweep() // a tick before the sleep: the suspension is a gap from here

	e.clock.Advance(2 * time.Hour)
	e.mustSweep()

	if h, why, _ := e.agentState(e.int("SELECT id FROM agents WHERE runner_id = ?", e.runner1)); h != "ready" || why != "" {
		t.Errorf("agent health after the sleep = %q (%q), want ready", h, why)
	}
	if seen := e.str("SELECT last_seen_at FROM runners WHERE id = ?", e.runner1); seen != fmtTime(e.clock.Now()) {
		t.Errorf("runner last_seen_at = %q, want restarted at wake %q", seen, fmtTime(e.clock.Now()))
	}

	// A runner genuinely quiet for a full stale window, with the engine
	// awake, still goes stale.
	e.elapse(16 * time.Minute)
	if h, why, _ := e.agentState(e.int("SELECT id FROM agents WHERE runner_id = ?", e.runner1)); h != "broken" || why != "runner stale" {
		t.Errorf("agent health after a wakeful stale window = %q (%q), want broken(runner stale)", h, why)
	}
}

// TestSweepSuspensionKeepsAttach: an attached terminal that slept with
// the engine keeps its hold; the hold's deadline restarts at wake.
func TestSweepSuspensionKeepsAttach(t *testing.T) {
	e := newEnv(t)
	hold := e.attach(false)

	e.clock.Advance(2 * time.Hour)
	e.mustSweep()

	if state, expires := e.holdOf(hold.HoldID); state != "live" {
		t.Errorf("hold state = %q, want live after the sleep", state)
	} else if expires != fmtTime(e.clock.Now().Add(attachHoldTTL)) {
		t.Errorf("hold expires_at = %q, want restarted at wake %q", expires, fmtTime(e.clock.Now().Add(attachHoldTTL)))
	}

	// A hold left to lapse with the engine awake still expires.
	e.elapse(attachHoldTTL + time.Second)
	if state, _ := e.holdOf(hold.HoldID); state != "expired" {
		t.Errorf("hold state = %q, want expired after a wakeful silence", state)
	}
}

// TestSweepSuspensionKeepsLeasePastHardCap: a sleep longer than the
// job's hard cap must not fail the job — the cap clock shifts past the
// gap, so the runner's first heartbeat after wake answers ok and the
// job stays leased (§13).
func TestSweepSuspensionKeepsLeasePastHardCap(t *testing.T) {
	e := newEnv(t) // work jobs are hard-capped at 45m
	e.submit(e.work())
	g := e.leaseAny(e.runner1)

	e.clock.Advance(2 * time.Hour) // an overnight sleep, far past the cap
	if ans := e.heartbeat(g); ans != AnsOK {
		t.Fatalf("heartbeat = %s, want ok after a sleep past the hard cap", ans)
	}
	if status := e.str("SELECT status FROM jobs WHERE id = ?", g.JobID); status != statusLeased {
		t.Errorf("status = %q, want leased", status)
	}

	// A wakeful tick after the wake must not hard-cap the job either.
	e.elapse(time.Minute)
	if status := e.str("SELECT status FROM jobs WHERE id = ?", g.JobID); status != statusLeased {
		t.Errorf("status = %q, want still leased after a wakeful tick", status)
	}
}

// TestSweepSuspensionKeepsQueuedLeasePastHardCap: a job whose lease
// lapsed wakefully — requeued keeping its lease id and first_leased —
// must not be hard-capped after a later sleep. The cap clock shifts past
// the gap for queued jobs too, so the holder's first heartbeat after
// wake revives the lease instead of answering lost (§13, §5.6).
func TestSweepSuspensionKeepsQueuedLeasePastHardCap(t *testing.T) {
	e := newEnv(t) // work jobs are hard-capped at 45m
	e.submit(e.work())
	g := e.leaseAny(e.runner1)

	// The lease lapses wakefully; the sweep requeues it, keeping its
	// lease id and first_leased.
	e.elapse(e.eng.cfg.LeaseTTL + time.Second)
	if status := e.str("SELECT status FROM jobs WHERE id = ?", g.JobID); status != statusQueued {
		t.Fatalf("status = %q, want queued after the wakeful lapse", status)
	}

	// Then the laptop sleeps far past the hard cap.
	e.clock.Advance(2 * time.Hour)

	// The holder's first heartbeat after wake revives the lease, not lost.
	if ans := e.heartbeat(g); ans != AnsOK {
		t.Fatalf("heartbeat = %s, want ok (revive) after the sleep", ans)
	}
	if status := e.str("SELECT status FROM jobs WHERE id = ?", g.JobID); status != statusLeased {
		t.Errorf("status = %q, want leased (revived)", status)
	}
}

// TestSweepRealExpiryStillExpires: time passing through several sweep
// ticks, with no single jump, is not a suspension, so a lapsed lease is
// requeued as ever (§13).
func TestSweepRealExpiryStillExpires(t *testing.T) {
	e := newEnv(t)
	e.submit(e.work())
	g := e.leaseAny(e.runner1)

	e.elapse(e.eng.cfg.LeaseTTL + time.Second)

	if status := e.str("SELECT status FROM jobs WHERE id = ?", g.JobID); status != statusQueued {
		t.Errorf("status = %q, want queued: the lease lapsed over real ticks", status)
	}
	if n := e.int("SELECT COUNT(*) FROM events WHERE kind = ?", evEngineSuspended); n != 0 {
		t.Errorf("engine_suspended events = %d, want none for a wakeful expiry", n)
	}
}
