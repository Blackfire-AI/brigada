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
		// The retry waits out the growing delay before it may be leased.
		e.clock.Advance(retryDelayFor(i))
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
		// The re-wake waits out the growing delay before the next lease.
		e.clock.Advance(retryDelayFor(i + 1))
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

// TestInfraFailureRetryWaitsForTheDelay pins issue #46's fix D: the
// retry the classification (§8) queues is not leasable at once — the
// network is not up in the second after the failure — but only once the
// first growing delay has passed.
func TestInfraFailureRetryWaitsForTheDelay(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "fam", "implement")
	e.submit(e.work())
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
	// The retry is queued for now + 5s, and a lease before then is empty.
	if nb := e.str("SELECT not_before FROM jobs WHERE task_id = ? AND restart = 1", e.task); nb != fmtTime(e.clock.Now().Add(retryDelayFor(1))) {
		t.Errorf("retry not_before = %s, want %s (now + 5s)", nb, fmtTime(e.clock.Now().Add(retryDelayFor(1))))
	}
	if g := e.leaseAny(author); g != nil {
		t.Fatalf("leased %+v before the retry delay elapsed, want nothing", g)
	}
	e.clock.Advance(retryDelayFor(1))
	if g := e.leaseAny(author); g == nil || g.Kind != KindWork {
		t.Errorf("grant after the delay = %+v, want the retried work", g)
	}
}

// TestInfraRetryDelayGrowsPerConsecutiveFailure pins the growing pause:
// the first infrastructure failure waits 5s, the next 15s — a longer
// outage is not hammered every second.
func TestInfraRetryDelayGrowsPerConsecutiveFailure(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "fam", "implement")
	e.submit(e.work())
	fail := func() {
		t.Helper()
		g := e.leaseAny(author)
		if g == nil {
			t.Fatal("nothing to lease")
		}
		if ans := e.complete(g, Completion{
			Error:     "fetch the mirror: Could not resolve host: github.com",
			ErrorKind: ErrorKindInfra,
		}); ans != AnsOK {
			t.Fatalf("complete = %s", ans)
		}
	}
	fail()
	if nb := e.str("SELECT not_before FROM jobs WHERE task_id = ? AND restart = 1 ORDER BY id DESC LIMIT 1", e.task); nb != fmtTime(e.clock.Now().Add(retryDelayFor(1))) {
		t.Errorf("first retry not_before = %s, want now + 5s", nb)
	}
	e.clock.Advance(retryDelayFor(1))
	fail()
	if nb := e.str("SELECT not_before FROM jobs WHERE task_id = ? AND restart = 1 ORDER BY id DESC LIMIT 1", e.task); nb != fmtTime(e.clock.Now().Add(retryDelayFor(2))) {
		t.Errorf("second retry not_before = %s, want now + 15s", nb)
	}
}

// TestForemanWakeWaitsAfterAFailedTurnAndResetsAfterASuccess pins the
// foreman side of fix D (§4.2): a failed turn's re-wake waits a growing
// delay instead of firing at once, and a done turn resets the streak so
// the next failure waits 5s again.
func TestForemanWakeWaitsAfterAFailedTurnAndResetsAfterASuccess(t *testing.T) {
	e := newEnv(t)
	e.park("which file?")
	if nb := e.str("SELECT not_before FROM jobs WHERE kind = 'foreman' AND status = 'queued'"); nb != "" {
		t.Fatalf("fresh wake not_before = %q, want none", nb)
	}
	g := e.leaseForemanRaw()
	runner := e.int("SELECT id FROM runners WHERE name = 'runner-foreman'")
	infra := Completion{Error: "prepare the foreman checkout: could not resolve host", ErrorKind: ErrorKindInfra}
	if ans := e.complete(g, infra); ans != AnsOK {
		t.Fatalf("complete = %s", ans)
	}
	if nb := e.str("SELECT not_before FROM jobs WHERE kind = 'foreman' AND status = 'queued'"); nb != fmtTime(e.clock.Now().Add(retryDelayFor(1))) {
		t.Errorf("re-wake not_before = %s, want now + 5s", nb)
	}
	if got := e.lease(runner, []string{"claude"}, nil); got != nil {
		t.Fatalf("leased %+v before the re-wake delay, want nothing", got)
	}
	// The delay grows across consecutive failed turns.
	e.clock.Advance(retryDelayFor(1))
	g = e.leaseForemanRaw()
	if ans := e.complete(g, infra); ans != AnsOK {
		t.Fatalf("complete the second failure = %s", ans)
	}
	if nb := e.str("SELECT not_before FROM jobs WHERE kind = 'foreman' AND status = 'queued'"); nb != fmtTime(e.clock.Now().Add(retryDelayFor(2))) {
		t.Errorf("second re-wake not_before = %s, want now + 15s", nb)
	}
	// A done turn consumes the events and resets the streak.
	e.clock.Advance(retryDelayFor(2))
	g = e.leaseForemanRaw()
	if ans := e.complete(g, Completion{Result: "judged", SessionID: "fs-1"}); ans != AnsOK {
		t.Fatalf("complete the done turn = %s", ans)
	}
	e.park("and now?")
	if nb := e.str("SELECT not_before FROM jobs WHERE kind = 'foreman' AND status = 'queued'"); nb != "" {
		t.Errorf("fresh wake after the reset not_before = %q, want none", nb)
	}
	g = e.leaseForemanRaw()
	if ans := e.complete(g, infra); ans != AnsOK {
		t.Fatalf("complete after the reset = %s", ans)
	}
	if nb := e.str("SELECT not_before FROM jobs WHERE kind = 'foreman' AND status = 'queued'"); nb != fmtTime(e.clock.Now().Add(retryDelayFor(1))) {
		t.Errorf("re-wake after the reset = %s, want now + 5s", nb)
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
