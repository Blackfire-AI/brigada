package engine

import (
	"slices"
	"testing"
	"time"
)

// leaseNew submits a fresh work job on a new task and leases it to the
// runner, returning the grant.
func (e *env) leaseNew(runner int64) *LeaseGrant {
	e.t.Helper()
	res := e.submit(e.workOn(e.addTask("{}")))
	g := e.leaseAny(runner)
	if g == nil || g.JobID != res.JobID {
		e.t.Fatalf("lease = %+v, want job %d", g, res.JobID)
	}
	return g
}

// jobAgent is the agent id the job's lease recorded.
func (e *env) jobAgent(job int64) int64 {
	e.t.Helper()
	return e.int("SELECT COALESCE(agent_id, 0) FROM jobs WHERE id = ?", job)
}

// pickEvents are the job's agent-pick events.
func (e *env) pickEvents(job int64) []eventPayload {
	e.t.Helper()
	var out []eventPayload
	for _, r := range e.eventRows() {
		if r.kind == evJobAgentPicked && r.job == job {
			out = append(out, r.payload)
		}
	}
	return out
}

func (e *env) record(agent int64, attempts, successes int) {
	e.t.Helper()
	e.exec(`INSERT INTO outcomes (project_id, agent_id, class, attempts, successes, updated_at)
		VALUES (?, ?, 'implement', ?, ?, ?)`, e.project, agent, attempts, successes, seedTS)
}

func TestLeaseRanksByScore(t *testing.T) {
	e := newEnv(t)
	a1 := e.addAgent(e.runner1, "claude", "sonnet", "work")
	a2 := e.addAgent(e.runner1, "kimi", "k2", "home")
	e.record(a1, 4, 1)
	e.record(a2, 4, 4)

	g := e.leaseNew(e.runner1)
	if got := e.jobAgent(g.JobID); got != a2 {
		t.Errorf("picked agent %d, want the better record %d", got, a2)
	}
	evs := e.pickEvents(g.JobID)
	if len(evs) != 1 {
		t.Fatalf("pick events = %v, want one", evs)
	}
	ev := evs[0]
	if ev.Agent != a2 || ev.Pick != pickSampled || ev.Score != Score(4, 4) || len(ev.PassedOver) != 0 {
		t.Errorf("pick event = %+v, want sampled agent %d at score %v", ev, a2, Score(4, 4))
	}
	if ev.Weight == nil || *ev.Weight != 1 || ev.Draw <= 0 || len(ev.Candidates) != 2 || ev.Candidates[0].Agent != a2 {
		t.Errorf("pick event = %+v, want weight 1, a draw and both candidates, %d first", ev, a2)
	}
}

func TestLeaseColdStartKeepsIDOrder(t *testing.T) {
	e := newEnv(t)
	a1 := e.addAgent(e.runner1, "claude", "sonnet", "work")
	e.addAgent(e.runner1, "kimi", "k2", "home")

	g := e.leaseNew(e.runner1)
	if got := e.jobAgent(g.JobID); got != a1 {
		t.Errorf("picked agent %d, want the first by id %d", got, a1)
	}
	if evs := e.pickEvents(g.JobID); len(evs) != 1 || evs[0].Score != 0.5 {
		t.Errorf("pick events = %+v, want one at the prior 0.5", evs)
	}
}

// Equal records, so the cost weight alone orders the two: the cheaper
// agent goes first even though the dearer one has the lower id (§8).
func TestLeaseRanksByCostWeight(t *testing.T) {
	e := newEnvCfg(t, func(c *Config) {
		c.Caps.Cost = map[string]int{"claude/sonnet": 5, "kimi/k2": 1}
	})
	e.addAgent(e.runner1, "claude", "sonnet", "work")
	a2 := e.addAgent(e.runner1, "kimi", "k2", "home")
	g := e.leaseNew(e.runner1)
	if got := e.jobAgent(g.JobID); got != a2 {
		t.Errorf("picked agent %d, want the cheaper %d", got, a2)
	}
	evs := e.pickEvents(g.JobID)
	if len(evs) != 1 || evs[0].Weight == nil || *evs[0].Weight != 1 || len(evs[0].Candidates) != 2 || evs[0].Candidates[1].Weight != 5 {
		t.Errorf("pick events = %+v, want weight 1 chosen over a candidate of weight 5", evs)
	}
}

// A weight of 0 ranks before every positive weight, whatever the records.
func TestLeaseZeroWeightFirst(t *testing.T) {
	e := newEnvCfg(t, func(c *Config) {
		c.Caps.Cost = map[string]int{"kimi/k2": 0}
	})
	e.addAgent(e.runner1, "claude", "sonnet", "work")
	a2 := e.addAgent(e.runner1, "kimi", "k2", "home")
	e.record(a2, 8, 0)
	g := e.leaseNew(e.runner1)
	if got := e.jobAgent(g.JobID); got != a2 {
		t.Errorf("picked agent %d, want the free %d", got, a2)
	}
}

// The cap walk still passes a full agent over for the next one in the
// sampled order, and names it.
func TestLeaseWalkSkipsFullAgentInSampledOrder(t *testing.T) {
	e := newEnvCfg(t, func(c *Config) {
		c.Caps.Account = map[string]int{"home": 1}
		c.Caps.Cost = map[string]int{"kimi/k2": 0}
	})
	a1 := e.addAgent(e.runner1, "claude", "sonnet", "work")
	a2 := e.addAgent(e.runner1, "kimi", "k2", "home")
	if got := e.jobAgent(e.leaseNew(e.runner1).JobID); got != a2 {
		t.Fatalf("first pick = %d, want the free agent %d", got, a2)
	}
	second := e.leaseNew(e.runner1)
	if got := e.jobAgent(second.JobID); got != a1 {
		t.Errorf("second pick = %d, want %d with the free agent's account full", got, a1)
	}
	if evs := e.pickEvents(second.JobID); len(evs) != 1 || !slices.Equal(evs[0].PassedOver, []int64{a2}) {
		t.Errorf("pick events = %+v, want %d passed over", evs, a2)
	}
}

func TestLeaseSkipsIneligibleAgents(t *testing.T) {
	e := newEnv(t)
	broken := e.addAgent(e.runner1, "claude", "sonnet", "work")
	cooling := e.addAgent(e.runner1, "kimi", "k2", "home")
	ready := e.addAgent(e.runner1, "gemini", "gemini-2.5", "free")
	e.exec("UPDATE agents SET health = 'broken' WHERE id = ?", broken)
	e.exec("UPDATE agents SET cooling_until = ? WHERE id = ?", fmtTime(e.clock.Now().Add(time.Hour)), cooling)

	g := e.leaseNew(e.runner1)
	if got := e.jobAgent(g.JobID); got != ready {
		t.Errorf("picked agent %d, want the ready one %d", got, ready)
	}

	// Cooling that has expired is eligible again, and wins on id.
	e.exec("UPDATE agents SET cooling_until = ? WHERE id = ?", fmtTime(e.clock.Now().Add(-time.Minute)), cooling)
	e.exec("UPDATE agents SET health = 'ready' WHERE id = ?", broken)
	e.exec("UPDATE agents SET health = 'broken' WHERE id = ?", ready)
	g = e.leaseNew(e.runner1)
	if got := e.jobAgent(g.JobID); got != broken {
		t.Errorf("picked agent %d, want %d", got, broken)
	}
	e.exec("UPDATE agents SET health = 'broken' WHERE id = ?", broken)
	g = e.leaseNew(e.runner1)
	if got := e.jobAgent(g.JobID); got != cooling {
		t.Errorf("picked agent %d, want the expired-cooling agent %d", got, cooling)
	}

	// Nothing eligible: the runner does not get the job.
	e.exec("UPDATE agents SET health = 'broken' WHERE id = ?", cooling)
	e.submit(e.workOn(e.addTask("{}")))
	if g := e.leaseAny(e.runner1); g != nil {
		t.Errorf("a runner with no eligible agent leased job %d", g.JobID)
	}
}

func TestLeaseRequireAgentIgnoresRanking(t *testing.T) {
	draws := 0
	e := newEnvCfg(t, func(c *Config) { c.Rand = func() float64 { draws++; return 0.5 } })
	a1 := e.addAgent(e.runner1, "claude", "sonnet", "work")
	a2 := e.addAgent(e.runner1, "claude", "opus", "work")
	e.record(a1, 4, 0)
	e.record(a2, 4, 4)

	task := e.addTask("{}")
	p := e.workOn(task)
	p.Kind = KindFix
	p.RequireAgent = a1
	res := e.submit(p)
	g := e.leaseAny(e.runner1)
	if g == nil || g.JobID != res.JobID {
		t.Fatalf("lease = %+v, want job %d", g, res.JobID)
	}
	if got := e.jobAgent(g.JobID); got != a1 {
		t.Errorf("picked agent %d, want the required %d", got, a1)
	}
	if draws != 0 {
		t.Errorf("a pinned lease consumed %d random draws, want none", draws)
	}
	evs := e.pickEvents(g.JobID)
	if len(evs) != 1 || evs[0].Pick != pickPinned || evs[0].Agent != a1 || evs[0].Draw != 0 || len(evs[0].Candidates) != 0 || len(evs[0].PassedOver) != 0 {
		t.Errorf("pick events = %+v, want one pinned pick of %d with no draw or candidates", evs, a1)
	}
}

func TestLeaseDefersToNextBestWithCapRoom(t *testing.T) {
	e := newEnvCfg(t, func(c *Config) {
		c.Caps.Account = map[string]int{"work": 1, "home": 1}
	})
	a1 := e.addAgent(e.runner1, "claude", "sonnet", "work")
	a2 := e.addAgent(e.runner1, "kimi", "k2", "home")
	e.record(a1, 4, 4)
	e.record(a2, 4, 2)

	first := e.leaseNew(e.runner1)
	if got := e.jobAgent(first.JobID); got != a1 {
		t.Fatalf("first pick = %d, want the best %d", got, a1)
	}

	// The best agent's account is full: the next-best with room takes it.
	second := e.leaseNew(e.runner1)
	if got := e.jobAgent(second.JobID); got != a2 {
		t.Errorf("second pick = %d, want the next-best with room %d", got, a2)
	}
	evs := e.pickEvents(second.JobID)
	if len(evs) != 1 || evs[0].Agent != a2 || !slices.Equal(evs[0].PassedOver, []int64{a1}) {
		t.Errorf("pick events = %+v, want agent %d passing over %d", evs, a2, a1)
	}

	// No eligible agent has room: the job is held at the cap.
	held := e.submit(e.workOn(e.addTask("{}")))
	if g := e.leaseAny(e.runner1); g != nil {
		t.Fatalf("leased job %d with every account full", g.JobID)
	}
	e.assertHeld(held.JobID, "account")
	if evs := e.pickEvents(held.JobID); len(evs) != 0 {
		t.Errorf("held job has pick events %+v", evs)
	}
}
