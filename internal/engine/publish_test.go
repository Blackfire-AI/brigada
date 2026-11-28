package engine

import (
	"database/sql"
	"strings"
	"testing"
)

// failTurns runs foreman turns to a completion each, on one agent.
func failTurns(e *env, cs ...Completion) (agent int64) {
	e.t.Helper()
	for _, c := range cs {
		e.submit(e.foremanTurn())
		g := e.leaseClaude(e.runner1)
		if g == nil {
			e.t.Fatal("the foreman turn was not leased")
		}
		agent = e.int("SELECT agent_id FROM jobs WHERE id = ?", g.JobID)
		if ans := e.complete(g, c); ans != AnsOK {
			e.t.Fatalf("complete: %s", ans)
		}
	}
	return agent
}

// TestPublishFailureNeverCountsTowardTheFailureStreak: a publish failure
// (§5.4) is not the agent's health, so it neither counts as one of the
// last N failures nor masks the streak when it sits between real ones.
func TestPublishFailureNeverCountsTowardTheFailureStreak(t *testing.T) {
	e := newEnv(t)
	e.offerForeman(e.runner1)
	publish := Completion{Error: "publish the branch, lease refused: moved", ErrorKind: ErrorKindPublish}
	real := Completion{Error: "the harness is on fire"}

	agent := failTurns(e, real, publish)
	if h := e.str("SELECT health FROM agents WHERE id = ?", agent); h != "ready" {
		t.Fatalf("health after a failure and a publish failure = %q, want ready: the publish failure is not a second failure", h)
	}
	failTurns(e, publish, publish)
	if h := e.str("SELECT health FROM agents WHERE id = ?", agent); h != "ready" {
		t.Fatalf("health after more publish failures = %q, want ready", h)
	}
	failTurns(e, real)
	if h, why := e.str("SELECT health FROM agents WHERE id = ?", agent), e.str("SELECT health_reason FROM agents WHERE id = ?", agent); h != "broken" || why != brokenFailures {
		t.Errorf("health after two real failures with publish failures between = %q (%q), want broken(%q)", h, why, brokenFailures)
	}
}

// TestPublishFailureEndsTheInfraStreak: a publish failure is not an infra
// failure, so infraStreakTx counts only the infra failures after it.
func TestPublishFailureEndsTheInfraStreak(t *testing.T) {
	e := newEnv(t)
	e.offerForeman(e.runner1)
	infra := Completion{Error: "could not resolve host", ErrorKind: ErrorKindInfra}
	publish := Completion{Error: "publish the branch, lease refused: moved", ErrorKind: ErrorKindPublish}
	failTurns(e, infra, infra, publish, infra)

	var n int
	if err := e.st.View(e.ctx, func(tx *sql.Tx) error {
		var err error
		n, _, err = infraStreakTx(tx, e.str("SELECT key FROM jobs ORDER BY id DESC LIMIT 1"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("infra streak after infra, infra, publish, infra = %d, want 1", n)
	}
}

// leaseRefused is the runner's completion of a push the lease refused
// (§5.4).
var leaseRefused = Completion{
	Error:     "publish the branch, lease refused: stale info: the remote's task/1-1 moved",
	ErrorKind: ErrorKindPublish,
}

// assertPublishRetrySpentNothing checks the publish retry's books: no
// restart, no tier or effort bump, no foreman wake.
func assertPublishRetrySpentNothing(e *env) {
	e.t.Helper()
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND restart = 1", e.task); n != 0 {
		e.t.Errorf("restart jobs = %d, want none: a publish retry is not a restart", n)
	}
	for _, kind := range []string{evTaskRestarted, evTaskTierBumped, evTaskEffortBumped} {
		if n := e.countEvents(kind); n != 0 {
			e.t.Errorf("%s events = %d, want none", kind, n)
		}
	}
}

// TestPublishFailureOfWorkIsRetriedOnce: a work job whose push failed is
// queued again once on its key (§5.4), leasable at once, with the same
// prompt; the task keeps its state and nothing is spent.
func TestPublishFailureOfWorkIsRetriedOnce(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "fam", "implement")
	e.submit(e.work())
	g := e.leaseAny(author)
	if g == nil {
		t.Fatal("nothing to lease")
	}
	if ans := e.complete(g, leaseRefused); ans != AnsOK {
		t.Fatalf("complete = %s", ans)
	}
	retry := e.int("SELECT id FROM jobs WHERE key = ? AND status = 'queued'", g.Key)
	if retry == 0 {
		t.Fatal("no publish retry queued")
	}
	if got, want := e.str("SELECT prompt FROM jobs WHERE id = ?", retry),
		e.str("SELECT prompt FROM jobs WHERE id = ?", g.JobID); got != want {
		t.Errorf("retry prompt = %q, want the job's own %q", got, want)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskRunning {
		t.Errorf("task state = %s, want running behind the retry", state)
	}
	if n := e.countEvents(evTaskFailed); n != 0 {
		t.Errorf("task_failed events = %d, want none", n)
	}
	if got := e.foremanJobs(); got != 0 {
		t.Errorf("foreman turns = %d, want none", got)
	}
	if p, ok := e.eventPayloadOf(evTaskPublishRetry); !ok || !strings.Contains(p.Reason, "lease refused") {
		t.Errorf("task_publish_retry payload = %+v (%t), want the failure's words", p, ok)
	}
	assertPublishRetrySpentNothing(e)
	// No infra backoff: the retry leases at once.
	if g := e.leaseAny(author); g == nil || g.JobID != retry {
		t.Fatalf("grant = %+v, want the retry %d at once", g, retry)
	}
}

// TestPublishFailureOfFixRoundRetrySpendsNoRound: a fix round whose push
// failed is queued again once on the author's session, spending no round.
func TestPublishFailureOfFixRoundRetrySpendsNoRound(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	fix := driveFixRound(e, author, roundFix)
	if ans := e.complete(fix, leaseRefused); ans != AnsOK {
		t.Fatalf("complete the fix round: %s", ans)
	}
	retry := e.int("SELECT id FROM jobs WHERE task_id = ? AND kind = 'fix' AND status = 'queued'", e.task)
	if retry == 0 {
		t.Fatal("no publish retry of the fix round queued")
	}
	if round := e.str("SELECT round FROM jobs WHERE id = ?", retry); round != "" {
		t.Errorf("retry round = %q, want none: the retry spends no round", round)
	}
	if mode := e.str("SELECT mode FROM jobs WHERE id = ?", retry); mode != string(ModeSession) {
		t.Errorf("retry mode = %s, want session", mode)
	}
	if got, want := e.str("SELECT prompt FROM jobs WHERE id = ?", retry),
		e.str("SELECT prompt FROM jobs WHERE id = ?", fix.JobID); got != want {
		t.Errorf("retry prompt = %q, want the round's own %q", got, want)
	}
	if n := e.countEvents(evTaskFailed); n != 0 {
		t.Errorf("task_failed events = %d, want none", n)
	}
	assertPublishRetrySpentNothing(e)
	if g := e.authorLease(author); g == nil || g.JobID != retry {
		t.Fatalf("grant = %+v, want the retried fix %d at once", g, retry)
	}
}

// TestSecondPublishFailureParksNamingThePublish: the retry's own publish
// failure is not retried again; the task parks at needs_foreman with the
// publish failure, the lease refusal and the local branch as its reason,
// and no restart or tier bump is spent.
func TestSecondPublishFailureParksNamingThePublish(t *testing.T) {
	for _, kind := range []Kind{KindWork, KindFix} {
		t.Run(string(kind), func(t *testing.T) {
			e := newEnv(t)
			author := e.agented("author", "anthropic", "implement")
			var g *LeaseGrant
			if kind == KindFix {
				g = driveFixRound(e, author, roundFix)
			} else {
				e.submit(e.work())
				g = e.authorLease(author)
			}
			if ans := e.complete(g, leaseRefused); ans != AnsOK {
				t.Fatalf("complete = %s", ans)
			}
			retry := e.authorLease(author)
			if retry == nil || retry.Key != g.Key {
				t.Fatalf("grant = %+v, want the publish retry on %s", retry, g.Key)
			}
			if ans := e.complete(retry, leaseRefused); ans != AnsOK {
				t.Fatalf("complete the retry = %s", ans)
			}
			if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskNeedsForeman {
				t.Fatalf("task state = %s, want needs_foreman", state)
			}
			if n := e.int("SELECT COUNT(*) FROM jobs WHERE key = ? AND status = 'queued'", g.Key); n != 0 {
				t.Errorf("queued jobs on the key = %d, want none: the one retry is spent", n)
			}
			if n := e.countEvents(evTaskPublishRetry); n != 1 {
				t.Errorf("task_publish_retry events = %d, want 1", n)
			}
			p, ok := e.eventPayloadOf(evTaskFailed)
			if !ok || !strings.Contains(p.Reason, "publish failure") || !strings.Contains(p.Reason, "lease refused") ||
				!strings.Contains(p.Reason, "locally, not on the remote") || strings.Contains(p.Reason, "network outage") {
				t.Errorf("task_failed payload = %+v (%t), want the publish failure named", p, ok)
			}
			assertPublishRetrySpentNothing(e)
		})
	}
}
