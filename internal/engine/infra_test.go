package engine

import (
	"testing"
)

// TestInfraFailureIsRetriedAsInfrastructure pins issue #46's fix: a
// fetch that could not be done — right after a wake the network is not
// up and the host does not resolve — completes as an infrastructure
// error and is retried as a transient error (§3), capped like a failed
// setup (§5.7). The task parks only once the retries are out.
func TestInfraFailureIsRetriedAsInfrastructure(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "fam", "implement")
	e.submit(e.work())
	fail := func() *LeaseGrant {
		g := e.leaseAny(author)
		if g == nil {
			t.Fatal("nothing to lease")
		}
		if ans := e.complete(g, Completion{
			Error:     "fetch the mirror of https://github.com/org/repo.git: exit status 128: fatal: unable to access 'https://github.com/org/repo.git/': Could not resolve host: github.com",
			ErrorKind: ErrorKindInfra,
		}); ans != AnsOK {
			t.Fatalf("complete = %s", ans)
		}
		return g
	}
	for i := 1; i <= infraRetries; i++ {
		fail()
		if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskReady {
			t.Fatalf("after infrastructure failure %d: task state = %s, want ready (retried)", i, state)
		}
	}
	if n := e.countEvents(evTaskFailed); n != 0 {
		t.Errorf("task_failed events = %d, want none while retries remain", n)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND restart = 1", e.task); n != infraRetries {
		t.Errorf("retry jobs = %d, want %d", n, infraRetries)
	}
	// The retries are spent: the foreman judges it.
	fail()
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskNeedsForeman {
		t.Errorf("task state = %s, want needs_foreman once the retries are out", state)
	}
	if n := e.countEvents(evTaskFailed); n != 1 {
		t.Errorf("task_failed events = %d, want one", n)
	}
	// Neither agent health nor a fix round is charged: three failed
	// jobs on one agent are past the failure streak.
	if h := e.str("SELECT health FROM agents WHERE id = ?", e.agentOf(author)); h != "ready" {
		t.Errorf("agent health = %s, want ready", h)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'fix'", e.task); n != 0 {
		t.Errorf("fix jobs = %d, want none", n)
	}
}

// TestForemanInfraPrepFailuresDoNotBreakTheAgent pins issue #46's
// foreman side (§8, §4.2): a foreman turn whose checkout fetch could not
// be done — the network not up yet right after a wake — completes as an
// infrastructure error, and two such prep failures in a row never count
// toward the failure streak, so the foreman's agent stays ready and the
// wake keeps answering. Before the fix, two of them marked claude/sonnet
// broken under the default streak, after which every turn timed out in
// the queue until `brigada agent reset`.
func TestForemanInfraPrepFailuresDoNotBreakTheAgent(t *testing.T) {
	e := newEnv(t)
	e.park("which file?")
	agent := int64(0)
	for i := 0; i < 2; i++ {
		g := e.leaseForemanRaw()
		agent = e.int("SELECT agent_id FROM jobs WHERE id = ?", g.JobID)
		if ans := e.complete(g, Completion{
			Error:     "prepare the foreman checkout: fetch https://example.invalid/brigada.git branch main: could not resolve host",
			ErrorKind: ErrorKindInfra,
		}); ans != AnsOK {
			t.Fatalf("complete the infra prep failure %d: %s", i+1, ans)
		}
		if h := e.str("SELECT health FROM agents WHERE id = ?", agent); h != "ready" {
			t.Fatalf("foreman agent after %d infra prep failure(s): health = %s, want ready", i+1, h)
		}
	}
	if n := e.countEvents(evAgentBroken); n != 0 {
		t.Errorf("agent_broken events = %d, want none", n)
	}
	// The wake answered both failures: a third turn is queued for the
	// next attempt, not wedged behind a broken agent.
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE kind = 'foreman' AND status = 'queued'"); n != 1 {
		t.Errorf("queued foreman turns after two infra prep failures = %d, want 1", n)
	}
}

// TestInfraFailuresDoNotAdvanceTheStreak pins the streak query's own
// exclusion (§8): an infrastructure failure sits between real failures
// without padding or masking them. Two infra failures then a real one
// leave the agent ready — the real one is the streak's first count — and
// a second real one breaks it, so the infra kind exempts only itself.
func TestInfraFailuresDoNotAdvanceTheStreak(t *testing.T) {
	e := newEnv(t)
	e.offerForeman(e.runner1)
	agent := int64(0)
	fail := func(c Completion) {
		t.Helper()
		g := e.leaseClaude(e.runner1)
		if g == nil {
			t.Fatal("the foreman turn was not leased")
		}
		agent = e.int("SELECT agent_id FROM jobs WHERE id = ?", g.JobID)
		if ans := e.complete(g, c); ans != AnsOK {
			t.Fatalf("complete: %s", ans)
		}
	}
	infra := Completion{Error: "prepare the foreman checkout: could not resolve host", ErrorKind: ErrorKindInfra}
	for i := 0; i < 2; i++ {
		e.submit(e.foremanTurn())
		fail(infra)
	}
	if h := e.str("SELECT health FROM agents WHERE id = ?", agent); h != "ready" {
		t.Fatalf("health after two infra failures = %q, want ready", h)
	}
	e.submit(e.foremanTurn())
	fail(Completion{Error: "the harness is on fire"})
	if h := e.str("SELECT health FROM agents WHERE id = ?", agent); h != "ready" {
		t.Fatalf("health after two infra failures and one real failure = %q, want ready (streak of one)", h)
	}
	e.submit(e.foremanTurn())
	fail(Completion{Error: "the harness is on fire again"})
	if h, why := e.str("SELECT health FROM agents WHERE id = ?", agent), e.str("SELECT health_reason FROM agents WHERE id = ?", agent); h != "broken" || why != brokenFailures {
		t.Errorf("health after two real failures = %q (%q), want broken(%q)", h, why, brokenFailures)
	}
}
