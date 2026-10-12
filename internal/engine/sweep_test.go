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
	// The queue TTL clock restarts at the expiry, the moment the job
	// went back to waiting.
	if queuedAt := e.str("SELECT queued_at FROM jobs WHERE id = ?", g.JobID); queuedAt != fmtTime(e.clock.Now().Add(-time.Second)) {
		t.Errorf("queued_at = %q, want the expiry moment", queuedAt)
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
			e.clock.Advance(tc.cap - time.Minute)
			if rep := e.mustSweep(); len(rep.HardCapped) != 0 {
				t.Fatalf("sweep capped a minute early: %v", rep.HardCapped)
			}

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
		})
		r := e.submit(e.foremanTurn())
		if g := e.leaseAny(e.runner1); g == nil {
			t.Fatal("nothing to lease")
		}
		e.clock.Advance(6 * time.Minute)
		rep := e.mustSweep()
		if !slices.Contains(rep.HardCapped, r.JobID) {
			t.Fatalf("sweep report hard-capped = %v, want the foreman job %d", rep.HardCapped, r.JobID)
		}
		if result := e.str("SELECT result FROM jobs WHERE id = ?", r.JobID); !strings.Contains(result, "5m") {
			t.Errorf("result = %q, want the failure to name the 5m cap", result)
		}
	})

	t.Run("heartbeat loses the lease its own call capped", func(t *testing.T) {
		e := newEnv(t)
		r := e.submit(e.work())
		g := e.leaseAny(e.runner1)
		e.clock.Advance(46 * time.Minute)
		ans, _, err := e.eng.Heartbeat(e.ctx, g.LeaseOwner, g.JobID, g.LeaseID)
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
		if ans, _, _ := e.eng.Heartbeat(e.ctx, g.LeaseOwner, g.JobID, g.LeaseID); ans != AnsGone {
			t.Errorf("later heartbeat = %s; want gone", ans)
		}
	})
}

func TestSweepQueueTTL(t *testing.T) {
	e := newEnv(t)
	r := e.submit(e.work())

	e.clock.Advance(9*time.Minute + 59*time.Second)
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
