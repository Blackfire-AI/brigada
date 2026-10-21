package engine

import (
	"testing"
	"time"
)

// agentProbe reads an agent's probe schedule: when the next probe is due
// ("" when none) and the delay it was given, in whole seconds.
func (e *env) agentProbe(id int64) (at string, delay int64) {
	e.t.Helper()
	return e.str("SELECT probe_at FROM agents WHERE id = ?", id),
		e.int("SELECT probe_delay FROM agents WHERE id = ?", id)
}

// dueProbes reads the runner's due probes through the engine.
func (e *env) dueProbes(runner int64) []Probe {
	e.t.Helper()
	out, err := e.eng.DueProbes(e.ctx, runner)
	if err != nil {
		e.t.Fatalf("due probes: %v", err)
	}
	return out
}

// breakStreak drives the agent on runner through a failure streak,
// cancelling the foreman turn each failure wakes so the next lease is
// the agent's own; it returns the agent id, the clock left at the moment
// the agent was marked broken.
func (e *env) breakStreak(runner int64, harness string) int64 {
	e.t.Helper()
	a := e.addAgent(runner, harness, "sonnet", "work")
	for i := 0; i < defaultFailureStreak; i++ {
		e.runPinned(runner, harness, Completion{Error: "worker CLI failed"})
		if turn := e.int("SELECT COALESCE(MAX(id), 0) FROM jobs WHERE kind = 'foreman' AND status = 'queued'"); turn != 0 {
			e.cancel(turn)
		}
		if i < defaultFailureStreak-1 {
			e.clock.Advance(time.Second)
		}
	}
	return a
}

// TestProbeClearsAStreakBrokenAgent is §8 "Health": a failure streak
// marks the agent broken and schedules a probe a few minutes out; once
// due, a passing probe clears the broken state with a recovered event.
func TestProbeClearsAStreakBrokenAgent(t *testing.T) {
	e := newEnv(t)
	a := e.breakStreak(e.runner1, "claude")

	if h, why, _ := e.agentState(a); h != "broken" || why != "repeated failures" {
		t.Fatalf("agent after the streak: %q (%q), want broken(repeated failures)", h, why)
	}
	at, delay := e.agentProbe(a)
	if want := fmtTime(e.clock.Now().Add(e.eng.cfg.ProbeDelay)); at != want {
		t.Errorf("probe_at = %q, want %q", at, want)
	}
	if want := int64(e.eng.cfg.ProbeDelay / time.Second); delay != want {
		t.Errorf("probe_delay = %d, want %d", delay, want)
	}

	// The probe is not due until its time arrives.
	if got := e.dueProbes(e.runner1); len(got) != 0 {
		t.Fatalf("due probes before the delay = %v, want none", got)
	}
	e.clock.Advance(e.eng.cfg.ProbeDelay + time.Second)
	if got := e.dueProbes(e.runner1); len(got) != 1 || got[0].AgentID != a {
		t.Fatalf("due probes = %v, want the broken agent %d", got, a)
	}

	// A passing probe clears the broken state with an event.
	if err := e.eng.ReportProbe(e.ctx, e.runner1, a, true); err != nil {
		t.Fatalf("report probe: %v", err)
	}
	if h, why, _ := e.agentState(a); h != "ready" || why != "" {
		t.Errorf("agent after a passing probe: %q (%q), want ready", h, why)
	}
	if at, _ := e.agentProbe(a); at != "" {
		t.Errorf("probe_at after a passing probe = %q, want cleared", at)
	}
	var recovered bool
	for _, r := range e.eventRows() {
		if r.kind == evAgentRecovered && r.payload.Agent == a && r.payload.Reason == "repeated failures" {
			recovered = true
		}
	}
	if !recovered {
		t.Errorf("no agent_recovered event naming agent %d for repeated failures", a)
	}
}

// TestFailingProbeBacksOffGrowingCapped is §8 "Health": each failing
// probe doubles the delay to the next, capped, so the agent is retried
// later and later but never left for longer than the cap.
func TestFailingProbeBacksOffGrowingCapped(t *testing.T) {
	e := newEnv(t)
	a := e.breakStreak(e.runner1, "claude")

	start := int64(e.eng.cfg.ProbeDelay / time.Second)
	cap := int64(e.eng.cfg.ProbeMaxDelay / time.Second)
	cur := start
	// Six failures: 300s → 600 → 1200 → 2400 → 3600 (capped) → 3600.
	for i := 0; i < 6; i++ {
		if err := e.eng.ReportProbe(e.ctx, e.runner1, a, false); err != nil {
			t.Fatalf("report probe %d: %v", i+1, err)
		}
		next := cur * 2
		if next > cap {
			next = cap
		}
		at, delay := e.agentProbe(a)
		if delay != next {
			t.Errorf("probe_delay after %d failing probes = %d, want %d", i+1, delay, next)
		}
		if want := fmtTime(e.clock.Now().Add(time.Duration(next) * time.Second)); at != want {
			t.Errorf("probe_at after %d failing probes = %q, want %q", i+1, at, want)
		}
		cur = next
	}
	if h, why, _ := e.agentState(a); h != "broken" || why != "repeated failures" {
		t.Errorf("agent after failing probes = %q (%q), want still broken", h, why)
	}
	if got := e.int("SELECT COUNT(*) FROM events WHERE kind = ?", evAgentProbeFailed); got != 6 {
		t.Errorf("agent_probe_failed events = %d, want 6", got)
	}
}

// TestAuthBrokenAgentIsNeverProbed is §8 "Health": an auth failure
// breaks the agent without a probe; it is never listed and a probe
// report never clears it — only the user's reset does.
func TestAuthBrokenAgentIsNeverProbed(t *testing.T) {
	e := newEnv(t)
	a := e.addAgent(e.runner1, "claude", "sonnet", "work")
	e.runPinned(e.runner1, "claude", Completion{Error: "not logged in", ErrorKind: "auth"})

	if h, why, _ := e.agentState(a); h != "broken" || why != "auth" {
		t.Fatalf("agent after the auth failure: %q (%q), want broken(auth)", h, why)
	}
	if at, _ := e.agentProbe(a); at != "" {
		t.Errorf("auth-broken agent has probe_at %q, want none scheduled", at)
	}

	// However long the wait, an auth-broken agent is never due a probe.
	e.elapse(time.Hour)
	if got := e.dueProbes(e.runner1); len(got) != 0 {
		t.Fatalf("due probes for the auth-broken agent = %v, want none", got)
	}

	// A probe report does not clear it either.
	if err := e.eng.ReportProbe(e.ctx, e.runner1, a, true); err != nil {
		t.Fatalf("report probe: %v", err)
	}
	if h, why, _ := e.agentState(a); h != "broken" || why != "auth" {
		t.Errorf("auth-broken agent after a probe report: %q (%q), want still broken(auth)", h, why)
	}

	// The user's reset still clears it.
	if n, err := e.eng.ResetAgents(e.ctx, "claude", "sonnet"); err != nil || n != 1 {
		t.Fatalf("reset: n = %d, err = %v; want 1", n, err)
	}
	if h, why, _ := e.agentState(a); h != "ready" || why != "" {
		t.Errorf("auth-broken agent after reset: %q (%q), want ready", h, why)
	}
}

// TestResetClearsTheProbeSchedule is §8: the user's reset clears the
// probe schedule a failure streak set, beside the broken state itself.
func TestResetClearsTheProbeSchedule(t *testing.T) {
	e := newEnv(t)
	a := e.breakStreak(e.runner1, "claude")
	if _, delay := e.agentProbe(a); delay == 0 {
		t.Fatalf("the broken agent has no probe scheduled")
	}
	if _, err := e.eng.ResetAgents(e.ctx, "claude", "sonnet"); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if h, why, _ := e.agentState(a); h != "ready" || why != "" {
		t.Errorf("agent after reset: %q (%q), want ready", h, why)
	}
	if at, delay := e.agentProbe(a); at != "" || delay != 0 {
		t.Errorf("probe schedule after reset = %q %d, want cleared", at, delay)
	}
}
