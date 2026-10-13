package engine

import (
	"testing"

	"github.com/Blackfire-AI/brigada/internal/caps"
)

// TestForemanTurnLeasedBeforeOlderTaskJobs: a pending foreman turn
// outranks every queued task job (§8.1), so with room everywhere the
// turn is leased first even when older task jobs sit ahead of it in
// the queue, and the task jobs keep their oldest-first order behind it.
func TestForemanTurnLeasedBeforeOlderTaskJobs(t *testing.T) {
	e := newEnv(t)
	first := e.submit(e.workOn(e.addTask("{}")))
	second := e.submit(e.workOn(e.addTask("{}")))
	turn := e.submit(e.foremanTurn())

	g := e.leaseAny(e.runner1)
	if g == nil || g.JobID != turn.JobID {
		t.Fatalf("lease = %+v, want the foreman turn %d before the older task jobs", g, turn.JobID)
	}
	if g.Kind != KindForeman || g.TaskID != 0 {
		t.Errorf("foreman grant = %+v; want kind foreman and no task", g)
	}
	g = e.leaseAny(e.runner1)
	if g == nil || g.JobID != first.JobID {
		t.Fatalf("second lease = %+v, want the older task job %d", g, first.JobID)
	}
	g = e.leaseAny(e.runner1)
	if g == nil || g.JobID != second.JobID {
		t.Fatalf("third lease = %+v, want task job %d", g, second.JobID)
	}
}

// TestForemanTurnTakesTheFreedGlobalSlot: at a full global cap with a
// foreman turn waiting, the slot a live job frees goes to the turn,
// not to the older queued task job (§8.1).
func TestForemanTurnTakesTheFreedGlobalSlot(t *testing.T) {
	e := newEnvCfg(t, func(c *Config) { c.Caps.Global = limitOf(2) })
	fill1 := e.submit(e.work())
	fill2 := e.submit(e.workOn(e.addTask("{}")))
	g1 := e.leaseAny(e.runner1)
	g2 := e.leaseAny(e.runner2)
	if g1 == nil || g1.JobID != fill1.JobID || g2 == nil || g2.JobID != fill2.JobID {
		t.Fatalf("filling leases = %+v, %+v; want jobs %d, %d", g1, g2, fill1.JobID, fill2.JobID)
	}
	older := e.submit(e.workOn(e.addTask("{}")))
	turn := e.submit(e.foremanTurn())

	if g := e.leaseAny(e.runner1); g != nil {
		t.Fatalf("a lease through the full global cap took job %d", g.JobID)
	}
	e.assertHeld(turn.JobID, caps.LayerGlobal)
	e.assertHeld(older.JobID, caps.LayerGlobal)

	if ans := e.complete(g1, done()); ans != AnsOK {
		t.Fatalf("free a global slot: %s", ans)
	}
	if g := e.leaseAny(e.runner1); g == nil || g.JobID != turn.JobID {
		t.Fatalf("lease after a slot freed = %+v, want the foreman turn %d, not the older task job %d", g, turn.JobID, older.JobID)
	}
}

// TestForemanTurnTakesTheFreedAccountSlot: a foreman turn held at a
// full account waits for that account's next slot — the older task job
// on the same account does not take it — while a task job through
// another account with room still leases in the same poll (§8.1).
func TestForemanTurnTakesTheFreedAccountSlot(t *testing.T) {
	e := newEnvCfg(t, func(c *Config) {
		c.Caps.Account = map[string]int{"work": 1, "personal": 1}
	})
	e.seedAgent(e.runner1, "claude", "claude", "sonnet", "work")
	e.seedAgent(e.runner1, "kimi", "kimi", "k2", "personal")
	e.seedAgent(e.runner2, "claude", "claude", "sonnet", "work")

	fill := e.submit(e.work()) // through runner 2's work agent
	g := e.leaseAny(e.runner2)
	if g == nil || g.JobID != fill.JobID {
		t.Fatalf("filling lease = %+v, want job %d", g, fill.JobID)
	}
	older := e.submit(e.workOn(e.addTask("{}"))) // waits on the work account
	turn := e.submit(e.foremanTurn())            // so does the turn
	other := e.workOn(e.addTask("{}"))
	other.Harness = "kimi" // through the personal account
	otherJob := e.submit(other)

	got := e.lease(e.runner1, []string{"claude", "kimi"}, nil)
	if got == nil || got.JobID != otherJob.JobID {
		t.Fatalf("same-poll lease = %+v, want the personal-account task job %d", got, otherJob.JobID)
	}
	e.assertHeld(turn.JobID, caps.LayerAccount)
	e.assertHeld(older.JobID, caps.LayerAccount)

	if ans := e.complete(g, done()); ans != AnsOK {
		t.Fatalf("free the work account: %s", ans)
	}
	if g := e.leaseAny(e.runner1); g == nil || g.JobID != turn.JobID {
		t.Fatalf("lease after the account freed = %+v, want the foreman turn %d, not the older task job %d", g, turn.JobID, older.JobID)
	}
}

// TestForemanTurnTakesTheFreedRunnerSlot: a foreman turn held at the
// runner's own full cap takes the slot the runner frees, ahead of the
// older queued task job (§8.1).
func TestForemanTurnTakesTheFreedRunnerSlot(t *testing.T) {
	e := newEnvCfg(t, func(c *Config) { c.Caps.Runner = limitOf(1) })
	fill := e.submit(e.work())
	g := e.leaseAny(e.runner1)
	if g == nil || g.JobID != fill.JobID {
		t.Fatalf("filling lease = %+v, want job %d", g, fill.JobID)
	}
	older := e.submit(e.workOn(e.addTask("{}")))
	turn := e.submit(e.foremanTurn())

	if got := e.leaseAny(e.runner1); got != nil {
		t.Fatalf("a lease through the full runner cap took job %d", got.JobID)
	}
	e.assertHeld(turn.JobID, caps.LayerRunner)
	e.assertHeld(older.JobID, caps.LayerRunner)

	if ans := e.complete(g, done()); ans != AnsOK {
		t.Fatalf("free the runner: %s", ans)
	}
	if got := e.leaseAny(e.runner1); got == nil || got.JobID != turn.JobID {
		t.Fatalf("lease after the runner freed = %+v, want the foreman turn %d, not the older task job %d", got, turn.JobID, older.JobID)
	}
}

// TestForemanTurnIgnoresPlanAndModelCaps: a foreman turn counts toward
// neither the plan nor the model cap (§8.1), so with both full — by the
// task job that fills them — a turn still leases end to end.
func TestForemanTurnIgnoresPlanAndModelCaps(t *testing.T) {
	e := newEnvCfg(t, func(c *Config) {
		c.Caps.Plan = limitOf(1)
		c.Caps.Model = map[string]int{"sonnet": 1}
	})
	e.seedAgent(e.runner1, "claude", "claude", "sonnet", "work")
	running := e.submit(e.work())
	g := e.leaseAny(e.runner1)
	if g == nil || g.JobID != running.JobID {
		t.Fatalf("filling lease = %+v, want job %d", g, running.JobID)
	}

	turn := e.submit(e.foremanTurn())
	gf := e.leaseAny(e.runner1)
	if gf == nil || gf.JobID != turn.JobID {
		t.Fatalf("foreman lease = %+v, want the turn %d through the full plan and model caps", gf, turn.JobID)
	}
}
