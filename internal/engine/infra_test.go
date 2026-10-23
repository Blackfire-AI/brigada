package engine

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// infraFail leases the task's newest job and completes it as an
// infrastructure failure, the runner's word for its own plumbing: a
// fetch of the remote that could not be done.
func infraFail(e *env, runner int64) {
	e.t.Helper()
	g := e.leaseAny(runner)
	if g == nil {
		e.t.Fatal("nothing to lease")
	}
	if ans := e.complete(g, Completion{
		Error:     "fetch the mirror of https://github.com/org/repo.git: exit status 128: fatal: unable to access 'https://github.com/org/repo.git/': Could not resolve host: github.com",
		ErrorKind: ErrorKindInfra,
	}); ans != AnsOK {
		e.t.Fatalf("complete = %s", ans)
	}
}

// waitOutInfraRetry advances the clock to the queued retry's
// not-before, the stand-in for a network that stays down while the
// backoff waits it out.
func (e *env) waitOutInfraRetry(key string) {
	e.t.Helper()
	nb := e.str("SELECT not_before FROM jobs WHERE key = ? AND status = 'queued' ORDER BY id DESC LIMIT 1", key)
	if nb == "" {
		e.t.Fatal("no queued retry to wait out")
	}
	at, err := parseTime(nb)
	if err != nil {
		e.t.Fatalf("parse the retry's not-before %q: %v", nb, err)
	}
	if d := at.Sub(e.clock.Now()); d > 0 {
		e.elapse(d)
	}
}

// TestInfraFailureIsRetriedAsInfrastructure pins the retry itself: a
// fetch that could not be done completes as an infrastructure error and
// is requeued on the same key under the infra backoff (§5.1) — the task
// keeps its state, no restart or fix round is spent, no foreman wake,
// and the agent's health is not charged.
func TestInfraFailureIsRetriedAsInfrastructure(t *testing.T) {
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
	// The retry is the same question on the same key, held out of
	// leasing for the first infra delay, spending nothing.
	retry := e.int("SELECT id FROM jobs WHERE key = ? AND status = 'queued'", g.Key)
	if retry == 0 {
		t.Fatal("no infra retry queued")
	}
	if nb := e.str("SELECT not_before FROM jobs WHERE id = ?", retry); nb != fmtTime(e.clock.Now().Add(infraDelayFor(1))) {
		t.Errorf("retry not_before = %s, want now + %s", nb, infraDelayFor(1))
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskRunning {
		t.Errorf("task state = %s, want running: the task keeps its state through the retry", state)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND restart = 1", e.task); n != 0 {
		t.Errorf("restart jobs = %d, want none: an infra retry is not a restart", n)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'fix'", e.task); n != 0 {
		t.Errorf("fix jobs = %d, want none", n)
	}
	if n := e.countEvents(evTaskFailed); n != 0 {
		t.Errorf("task_failed events = %d, want none while the window lasts", n)
	}
	if got := e.foremanJobs(); got != 0 {
		t.Errorf("foreman turns = %d, want none: no wake is spent on an infra retry", got)
	}
	if h := e.str("SELECT health FROM agents WHERE id = ?", e.agentOf(author)); h != "ready" {
		t.Errorf("agent health = %s, want ready", h)
	}
	if p, ok := e.eventPayloadOf(evTaskInfraRetry); !ok || !strings.Contains(p.Reason, "Could not resolve host") {
		t.Errorf("task_infra_retry payload = %+v (%t), want the failure's words", p, ok)
	}
}

// TestInfraFailureRetryWaitsForTheDelay pins the not-before hold: the
// retry the classification queues is not leasable at once — the network
// is not up in the minute after the failure — but only once the first
// infra delay has passed.
func TestInfraFailureRetryWaitsForTheDelay(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "fam", "implement")
	e.submit(e.work())
	infraFail(e, author)
	if g := e.leaseAny(author); g != nil {
		t.Fatalf("leased %+v before the retry delay elapsed, want nothing", g)
	}
	e.elapse(infraDelayFor(1))
	if g := e.leaseAny(author); g == nil || g.Kind != KindWork {
		t.Errorf("grant after the delay = %+v, want the retried work", g)
	}
}

// TestInfraRetryDelayGrowsPerConsecutiveFailure pins the growing pause:
// the first infrastructure failure waits 1m, the next 2m, then 5m and
// 15m — a longer outage is not hammered, and never on the seconds-scale
// table the setup retries use (§5.7).
func TestInfraRetryDelayGrowsPerConsecutiveFailure(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "fam", "implement")
	e.submit(e.work())
	for i := 1; i <= 4; i++ {
		infraFail(e, author)
		want := fmtTime(e.clock.Now().Add(infraDelayFor(i)))
		if nb := e.str("SELECT not_before FROM jobs WHERE key = ? AND status = 'queued' ORDER BY id DESC LIMIT 1", e.work().Key); nb != want {
			t.Errorf("retry %d not_before = %s, want now + %s", i, nb, infraDelayFor(i))
		}
		e.waitOutInfraRetry(e.work().Key)
	}
	if infraDelayFor(5) != infraDelayFor(4) {
		t.Errorf("infraDelayFor(5) = %s, want the 15m cap", infraDelayFor(5))
	}
}

// TestInfraRetriesOutlastASeventyMinuteOutage pins the hours-scale
// window (§5.1): a seventy-minute outage spends no retry for good, parks
// nothing and wakes nobody — each failure requeues the job until the
// network returns.
func TestInfraRetriesOutlastASeventyMinuteOutage(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "fam", "implement")
	e.submit(e.work())
	key := e.work().Key
	start := e.clock.Now()
	for {
		infraFail(e, author)
		if e.clock.Now().Sub(start) >= 70*time.Minute {
			break
		}
		e.waitOutInfraRetry(key)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskRunning {
		t.Fatalf("task state after a 70-minute outage = %s, want running", state)
	}
	if n := e.countEvents(evTaskFailed); n != 0 {
		t.Errorf("task_failed events = %d, want none: the outage parked nothing", n)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND restart = 1", e.task); n != 0 {
		t.Errorf("restart jobs = %d, want none", n)
	}
	if got := e.foremanJobs(); got != 0 {
		t.Errorf("foreman turns = %d, want none", got)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE key = ? AND status = 'queued'", key); n != 1 {
		t.Errorf("queued retries after the outage = %d, want 1", n)
	}
	// The outage ends: the waiting retry runs and the task proceeds.
	e.waitOutInfraRetry(key)
	g := e.leaseAny(author)
	if g == nil || g.Kind != KindWork {
		t.Fatalf("grant after the outage = %+v, want the retried work", g)
	}
	if ans := e.complete(g, doneOn("task/1-1", passCheck)); ans != AnsOK {
		t.Fatalf("complete the retried work: %s", ans)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskReviewing {
		t.Fatalf("task state after the retried work = %s, want reviewing", state)
	}
}

// TestInfraRetriesSpentParkNamingTheOutage pins the window's end (§5.1):
// once a run of infra failures has filled it, the next failure parks the
// task at needs_foreman and the needs-you line names the outage, not a
// task failure.
func TestInfraRetriesSpentParkNamingTheOutage(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "fam", "implement")
	e.submit(e.work())
	key := e.work().Key
	start := e.clock.Now()
	for {
		infraFail(e, author)
		if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state == taskNeedsForeman {
			break
		}
		if e.clock.Now().Sub(start) > infraRetryWindow+2*infraDelayFor(len(infraRetryDelays)) {
			t.Fatal("the outage outlasted the window but the task never parked")
		}
		e.waitOutInfraRetry(key)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskNeedsForeman {
		t.Fatalf("task state = %s, want needs_foreman", state)
	}
	p, ok := e.eventPayloadOf(evTaskFailed)
	if !ok || !strings.Contains(p.Reason, "the network outage outlasted the "+capText(infraRetryWindow)+" infra retry window") ||
		!strings.Contains(p.Reason, "Could not resolve host") {
		t.Errorf("task_failed reason = %q, want the outage named with the failure's words", p.Reason)
	}
	// The park is the one wake the outage costs.
	if got := e.foremanJobs(); got != 1 {
		t.Errorf("foreman turns after the park = %d, want 1", got)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND restart = 1", e.task); n != 0 {
		t.Errorf("restart jobs = %d, want none even at the park", n)
	}
}

// TestInfraFixRoundRetrySpendsNoRound pins the fix side (§5.1, §3): a
// fix round whose fetch failed is requeued — the author's session and
// agent, the round's own prompt — under the infra backoff, spending no
// round and parking nothing while the window lasts.
func TestInfraFixRoundRetrySpendsNoRound(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	fix := driveFixRound(e, author, roundFix)
	if ans := e.complete(fix, Completion{
		Error:     "fetch the worktree's base: Could not resolve host: github.com",
		ErrorKind: ErrorKindInfra,
	}); ans != AnsOK {
		t.Fatalf("complete the fix round: %s", ans)
	}
	retry := e.int("SELECT id FROM jobs WHERE task_id = ? AND kind = 'fix' AND status = 'queued'", e.task)
	if retry == 0 {
		t.Fatal("no infra retry of the fix round queued")
	}
	if round := e.str("SELECT round FROM jobs WHERE id = ?", retry); round != "" {
		t.Errorf("retry round = %q, want none: the round was never attempted", round)
	}
	if mode := e.str("SELECT mode FROM jobs WHERE id = ?", retry); mode != string(ModeSession) {
		t.Errorf("retry mode = %s, want session", mode)
	}
	if agent := e.int("SELECT COALESCE(require_agent, 0) FROM jobs WHERE id = ?", retry); agent != e.agentOf(author) {
		t.Errorf("retry pin = %d, want the author's %d", agent, e.agentOf(author))
	}
	if got, want := e.str("SELECT prompt FROM jobs WHERE id = ?", retry),
		e.str("SELECT prompt FROM jobs WHERE id = ?", fix.JobID); got != want {
		t.Errorf("retry prompt = %q, want the round's own %q", got, want)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskRunning {
		t.Errorf("task state = %s, want running behind the retry", state)
	}
	if n := e.countEvents(evTaskFailed); n != 0 {
		t.Errorf("task_failed events = %d, want none", n)
	}
	// The retry runs the round after the backoff.
	e.elapse(infraDelayFor(1))
	g := e.authorLease(author)
	if g == nil || g.JobID != retry {
		t.Fatalf("grant after the delay = %+v, want the retried fix %d", g, retry)
	}
}

// TestForemanInfraPrepFailuresDoNotBreakTheAgent pins the foreman side
// (§8, §4.2): a foreman turn whose checkout fetch could not be done
// completes as an infrastructure error, and two such prep failures in a
// row never count toward the failure streak, so the foreman's agent
// stays ready and the wake keeps answering. Before the fix, two of them
// marked claude/sonnet broken under the default streak, after which
// every turn timed out in the queue until `brigada agent reset`.
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
		// The re-wake waits out the infra delay before the next lease.
		e.elapse(infraDelayFor(i + 1))
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

// TestForemanWakeStaysArmedThroughAnOutage pins the armed wake (§5.1):
// infra failures do not count toward the wake stall, so the wake keeps
// answering through an outage far past the stall's three attempts. When
// the retry window is spent the wake stops, one event names the outage
// and the needs-you queue carries it (§10).
func TestForemanWakeStaysArmedThroughAnOutage(t *testing.T) {
	e := newEnv(t)
	e.park("which file?")
	start := e.clock.Now()
	turns := 0
	for {
		g := e.leaseForemanRaw()
		if g == nil {
			t.Fatal("the wake was not armed: nothing to lease")
		}
		turns++
		if ans := e.complete(g, Completion{
			Error:     "prepare the foreman checkout: could not resolve host",
			ErrorKind: ErrorKindInfra,
		}); ans != AnsOK {
			t.Fatalf("complete the infra turn: %s", ans)
		}
		if e.clock.Now().Sub(start) > infraRetryWindow {
			break
		}
		if turns > 100 {
			t.Fatal("the wake never stopped inside a window-sized outage")
		}
		e.waitOutInfraRetry(e.foremanTurn().Key)
	}
	if turns <= foremanWakeStallThreshold {
		t.Errorf("foreman turns before the wake gave up = %d, want more than the stall's %d", turns, foremanWakeStallThreshold)
	}
	if n := e.countEvents(evForemanWakeStalled); n != 0 {
		t.Errorf("foreman_wake_stalled events = %d, want none: the outage, not the stall, stopped the wake", n)
	}
	if got := e.liveForeman(); got != 0 {
		t.Fatalf("live foreman turns after the spent window = %d, want none", got)
	}
	if n := e.countEvents(evForemanOutageSpent); n != 1 {
		t.Errorf("foreman_outage_spent events = %d, want 1", n)
	}
	info, err := e.eng.Status(e.ctx, e.project)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	var found bool
	for _, it := range info.NeedsYou {
		if it.Kind == "foreman" && it.State == "outage" {
			found = true
			if !strings.Contains(it.Reason, "the network outage outlasted the "+capText(infraRetryWindow)+" infra retry window") {
				t.Errorf("needs-you reason = %q, want the outage named", it.Reason)
			}
		}
	}
	if !found {
		t.Errorf("needs-you queue = %+v, want the foreman's outage entry", info.NeedsYou)
	}
	// The events the turns carried stay pending, like any stalled wake's.
	if want := []string{"pending:" + fmt.Sprint(e.int("SELECT id FROM jobs WHERE kind = 'foreman' ORDER BY id DESC LIMIT 1"))}; !equalStrings(e.inboxStates(), want) {
		t.Errorf("inbox after the spent outage = %v, want %v", e.inboxStates(), want)
	}
	// The outage ends and a fresh judgment event wakes the foreman again:
	// the turn succeeds, and the needs-you entry retires with it.
	e.clock.Advance(time.Minute)
	second := e.addTask("{}")
	e.submit(SubmitParams{
		Key: fmt.Sprintf("task:%d", second), ProjectID: e.project, TaskID: second,
		Kind: KindWork, Mode: ModeFresh, Prompt: "p",
	})
	e.complete(e.lease(e.runner2, nil, nil), Completion{Result: `{"status":"needs_help","question":"and me?"}`})
	if got := e.liveForeman(); got != 1 {
		t.Fatalf("live foreman turns after the outage ended = %d, want 1", got)
	}
	// The wake after a spent streak still waits the infra cap, so the
	// turn is leasable only after it.
	e.waitOutInfraRetry(e.foremanTurn().Key)
	if ans := e.complete(e.leaseForemanRaw(), Completion{Result: "back", SessionID: "fs-1"}); ans != AnsOK {
		t.Fatalf("complete the turn after the outage: %s", ans)
	}
	info, err = e.eng.Status(e.ctx, e.project)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	for _, it := range info.NeedsYou {
		if it.Kind == "foreman" && it.State == "outage" {
			t.Errorf("needs-you still carries the outage entry after a turn answered: %+v", it)
		}
	}
}

// TestForemanWakeWaitsAfterAFailedTurnAndResetsAfterASuccess pins the
// foreman side of fix D (§4.2): a failed turn's re-wake waits a growing
// delay instead of firing at once — an infra failure on the infra table
// (§5.1), any other failure on the short one — and a done turn resets
// the streak so the next failure waits the first step again.
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
	if nb := e.str("SELECT not_before FROM jobs WHERE kind = 'foreman' AND status = 'queued'"); nb != fmtTime(e.clock.Now().Add(infraDelayFor(1))) {
		t.Errorf("re-wake not_before = %s, want now + %s", nb, infraDelayFor(1))
	}
	if got := e.lease(runner, []string{"claude"}, nil); got != nil {
		t.Fatalf("leased %+v before the re-wake delay, want nothing", got)
	}
	// The delay grows across consecutive failed turns.
	e.elapse(infraDelayFor(1))
	g = e.leaseForemanRaw()
	if ans := e.complete(g, infra); ans != AnsOK {
		t.Fatalf("complete the second failure = %s", ans)
	}
	if nb := e.str("SELECT not_before FROM jobs WHERE kind = 'foreman' AND status = 'queued'"); nb != fmtTime(e.clock.Now().Add(infraDelayFor(2))) {
		t.Errorf("second re-wake not_before = %s, want now + %s", nb, infraDelayFor(2))
	}
	// A done turn consumes the events and resets the streak.
	e.elapse(infraDelayFor(2))
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
	if nb := e.str("SELECT not_before FROM jobs WHERE kind = 'foreman' AND status = 'queued'"); nb != fmtTime(e.clock.Now().Add(infraDelayFor(1))) {
		t.Errorf("re-wake after the reset = %s, want now + %s", nb, infraDelayFor(1))
	}
	// A failure that is not infra keeps the short table: the two
	// classes never share a backoff. The pending event has been carried
	// by two failed turns now, so the short table's second step applies.
	e.elapse(infraDelayFor(1))
	g = e.leaseForemanRaw()
	if ans := e.complete(g, Completion{Error: "the harness is on fire"}); ans != AnsOK {
		t.Fatalf("complete the plain failure = %s", ans)
	}
	if nb := e.str("SELECT not_before FROM jobs WHERE kind = 'foreman' AND status = 'queued'"); nb != fmtTime(e.clock.Now().Add(retryDelayFor(2))) {
		t.Errorf("re-wake after a plain failure = %s, want now + %s", nb, retryDelayFor(2))
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
