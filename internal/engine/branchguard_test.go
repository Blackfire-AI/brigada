package engine

import (
	"strings"
	"testing"
)

// branchGuardRefusal is the runner's completion of a worktree the branch
// guard refused (§5.2).
var branchGuardRefusal = Completion{
	Error:     "branch guard: the worktree is on branch side, not the task branch brigada/task-1",
	ErrorKind: ErrorKindBranchGuard,
}

// TestBranchGuardNeverCountsTowardTheFailureStreak: a branch guard
// refusal is no verdict on the agent's health, so it neither counts as
// one of the last N failures nor masks the streak between real ones.
func TestBranchGuardNeverCountsTowardTheFailureStreak(t *testing.T) {
	e := newEnv(t)
	e.offerForeman(e.runner1)
	real := Completion{Error: "the harness is on fire"}

	agent := failTurns(e, real, branchGuardRefusal, branchGuardRefusal)
	if h := e.str("SELECT health FROM agents WHERE id = ?", agent); h != "ready" {
		t.Fatalf("health after a failure and branch guard refusals = %q, want ready", h)
	}
	failTurns(e, real)
	if h, why := e.str("SELECT health FROM agents WHERE id = ?", agent), e.str("SELECT health_reason FROM agents WHERE id = ?", agent); h != "broken" || why != brokenFailures {
		t.Errorf("health after two real failures with refusals between = %q (%q), want broken(%q)", h, why, brokenFailures)
	}
}

// TestBranchGuardParksWithoutARetry: a work job or fix round the branch
// guard refused is not retried; the task parks at needs_foreman naming
// what the guard found, and no restart or tier bump is spent.
func TestBranchGuardParksWithoutARetry(t *testing.T) {
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
			if ans := e.complete(g, branchGuardRefusal); ans != AnsOK {
				t.Fatalf("complete = %s", ans)
			}
			if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskNeedsForeman {
				t.Fatalf("task state = %s, want needs_foreman", state)
			}
			if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND status = 'queued'", e.task); n != 0 {
				t.Errorf("queued jobs = %d, want none: a branch guard refusal is not retried", n)
			}
			if n := e.countEvents(evTaskPublishRetry); n != 0 {
				t.Errorf("task_publish_retry events = %d, want none", n)
			}
			p, ok := e.eventPayloadOf(evTaskFailed)
			if !ok || !strings.Contains(p.Reason, "branch guard") || !strings.Contains(p.Reason, "on branch side") {
				t.Errorf("task_failed payload = %+v (%t), want the guard's words", p, ok)
			}
			if h := e.str("SELECT health FROM agents WHERE id = (SELECT agent_id FROM jobs WHERE id = ?)", g.JobID); h != "ready" {
				t.Errorf("author health = %q, want ready", h)
			}
			assertPublishRetrySpentNothing(e)
		})
	}
}
