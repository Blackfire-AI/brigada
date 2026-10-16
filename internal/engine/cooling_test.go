package engine

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// pinnedFix submits a fix round pinned to the author's agent (§8
// session affinity).
func (e *env) pinnedFix(agent int64) SubmitResult {
	e.t.Helper()
	p := e.workOn(e.addTask("{}"))
	p.Kind = KindFix
	p.RequireAgent = agent
	return e.submit(p)
}

// coolUntil cools the agent until d from now.
func (e *env) coolUntil(agent int64, d time.Duration) time.Time {
	e.t.Helper()
	until := e.clock.Now().Add(d)
	e.exec("UPDATE agents SET cooling_until = ? WHERE id = ?", fmtTime(until), agent)
	return until
}

func (e *env) heldLayer(job int64) string {
	e.t.Helper()
	return e.str("SELECT COALESCE(cap_held_layer, '') FROM jobs WHERE id = ?", job)
}

// TestPinnedFixWaitsForCoolingAgent: a fix job pinned to a cooling agent
// is held on the cooling layer past the queue TTL, the status names the
// agent and the end of the cooling, and once it ends the job is leased.
func TestPinnedFixWaitsForCoolingAgent(t *testing.T) {
	e := newEnv(t)
	codex := e.addAgent(e.runner1, "codex", "gpt-5", "work")
	until := e.coolUntil(codex, 31*time.Minute)
	fix := e.pinnedFix(codex)

	for i := 0; i < 3; i++ { // 30 minutes in 10-minute steps: three TTLs
		e.clock.Advance(10 * time.Minute)
		if g := e.lease(e.runner1, nil, nil); g != nil {
			t.Fatalf("leased job %d while its agent cools", g.JobID)
		}
		if st := e.str("SELECT status FROM jobs WHERE id = ?", fix.JobID); st != statusQueued {
			t.Fatalf("fix job status = %q after %d minutes, want queued", st, 10*(i+1))
		}
	}
	if got := e.heldLayer(fix.JobID); got != layerCooling {
		t.Fatalf("hold layer = %q, want %q", got, layerCooling)
	}
	if layers := e.capHeldLayers(fix.JobID); len(layers) != 1 || layers[0] != layerCooling {
		t.Errorf("job_cap_held events = %v, want exactly [cooling]", layers)
	}
	info, err := e.eng.Status(e.ctx, 0)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(info.Held) != 1 {
		t.Fatalf("status held = %+v, want one job", info.Held)
	}
	h := info.Held[0]
	if h.ID != fix.JobID || h.Layer != layerCooling || h.Agent != "codex/gpt-5" || h.Until != fmtTime(until) {
		t.Errorf("held = %+v, want job %d cooling on codex/gpt-5 until %s", h, fix.JobID, fmtTime(until))
	}

	e.clock.Advance(2 * time.Minute) // past the end of the cooling
	g := e.lease(e.runner1, nil, nil)
	if g == nil || g.JobID != fix.JobID {
		t.Fatalf("lease = %+v, want the pinned fix job %d once the cooling ended", g, fix.JobID)
	}
	if got := e.jobAgent(fix.JobID); got != codex {
		t.Errorf("the fix round ran on agent %d, want the pinned %d", got, codex)
	}
}

// TestCoolingHoldTTLCountsFromTheEnd: when the cooling ends and nobody
// leases, the TTL counts from that moment, not from queued_at.
func TestCoolingHoldTTLCountsFromTheEnd(t *testing.T) {
	e := newEnv(t)
	codex := e.addAgent(e.runner1, "codex", "gpt-5", "work")
	e.coolUntil(codex, 30*time.Minute)
	fix := e.pinnedFix(codex)

	for i := 0; i < 3; i++ {
		e.clock.Advance(10 * time.Minute)
		e.mustSweep()
	}
	// The cooling ended exactly now. No runner polls; a sweep lifts the hold.
	e.clock.Advance(time.Second)
	e.mustSweep()
	if got := e.heldLayer(fix.JobID); got != "" {
		t.Fatalf("hold layer after the cooling ended = %q, want it lifted", got)
	}
	e.clock.Advance(9*time.Minute + 58*time.Second)
	if rep := e.mustSweep(); slices.Contains(rep.QueueTimedOut, fix.JobID) {
		t.Fatalf("parked 9m58s after the cooling ended (queued 40m ago): %v", rep.QueueTimedOut)
	}
	e.clock.Advance(3 * time.Second)
	if rep := e.mustSweep(); !slices.Contains(rep.QueueTimedOut, fix.JobID) {
		t.Fatalf("timed out = %v, want job %d parked a TTL after the cooling ended", rep.QueueTimedOut, fix.JobID)
	}
}

// TestAllQualifyingAgentsCoolingHolds: an unpinned job whose every
// qualifying agent cools is held, naming the agent that frees first; a
// single agent out of cooling ends the hold.
func TestAllQualifyingAgentsCoolingHolds(t *testing.T) {
	e := newEnv(t)
	a1 := e.addAgent(e.runner1, "codex", "gpt-5", "acct-a")
	a2 := e.addAgent(e.runner1, "claude", "sonnet", "acct-b")
	e.coolUntil(a1, 50*time.Minute)
	soon := e.coolUntil(a2, 20*time.Minute)
	job := e.submit(e.workOn(e.addTask("{}")))

	e.clock.Advance(15 * time.Minute)
	e.lease(e.runner1, nil, nil) // the runner polls, so it is not stale
	if got := e.heldLayer(job.JobID); got != layerCooling {
		t.Fatalf("hold layer = %q, want %q", got, layerCooling)
	}
	info, err := e.eng.Status(e.ctx, 0)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(info.Held) != 1 || info.Held[0].Agent != "claude/sonnet" || info.Held[0].Until != fmtTime(soon) {
		t.Fatalf("status held = %+v, want claude/sonnet until %s", info.Held, fmtTime(soon))
	}

	e.clock.Advance(6 * time.Minute) // a2 is free again
	g := e.lease(e.runner1, nil, nil)
	if g == nil || g.JobID != job.JobID {
		t.Fatalf("lease = %+v, want job %d on the agent that finished cooling", g, job.JobID)
	}
	if got := e.jobAgent(job.JobID); got != a2 {
		t.Errorf("job ran on agent %d, want %d", got, a2)
	}
}

// TestPinnedJobOnBrokenAgentParksNamingIt: a broken pin has no end in
// sight, so it is not held; the TTL parks it and the reason names the
// pinned agent and its state.
func TestPinnedJobOnBrokenAgentParksNamingIt(t *testing.T) {
	e := newEnv(t)
	codex := e.addAgent(e.runner1, "codex", "gpt-5", "work")
	e.exec("UPDATE agents SET health = 'broken', health_reason = 'auth' WHERE id = ?", codex)
	fix := e.pinnedFix(codex)

	e.clock.Advance(10*time.Minute + time.Second)
	rep := e.mustSweep()
	if !slices.Contains(rep.QueueTimedOut, fix.JobID) {
		t.Fatalf("timed out = %v, want job %d parked", rep.QueueTimedOut, fix.JobID)
	}
	result := e.str("SELECT result FROM jobs WHERE id = ?", fix.JobID)
	for _, want := range []string{"queue TTL exceeded", "pinned to codex/gpt-5", "broken", "auth"} {
		if !strings.Contains(result, want) {
			t.Errorf("result = %q, want it to contain %q", result, want)
		}
	}
}
