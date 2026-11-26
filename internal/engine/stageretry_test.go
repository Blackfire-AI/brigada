package engine

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// The placement pipeline's stage-retry acceptance (issue #263, criteria
// 4-5): a reviewer that stalls or answers unusably retries the unchanged
// head on another eligible reviewer without new implementation work or
// spent rounds; the attempt's exclusion set holds throughout; the
// retries are bounded, and exhaustion follows the waiting rules — a
// cooling reviewer is a wait, a missing one the foreman's park.

// TestReviewerStallRetriesTheUnchangedHead: a stalled review records a
// reliability failure on the review class and retries the same question
// of the unchanged head on another reviewer — no new work or fix job,
// no implementation or fix round spent — and the retry's approve
// finishes the task.
func TestReviewerStallRetriesTheUnchangedHead(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	first := e.agented("first", "google", "review")
	second := e.agented("second", "meta", "review")
	review := e.runToReview(author)

	g := e.leaseAny(first)
	if g == nil || g.JobID != review {
		t.Fatalf("the first reviewer leased %+v, want the review %d", g, review)
	}
	if ans := e.complete(g, Completion{Result: `{"status":"stalled","summary":"the review stalled"}`, Outcome: OutcomeStalled}); ans != AnsOK {
		t.Fatalf("complete the stalled review: %s", ans)
	}
	// The stall is a reliability failure of the review class: one
	// attempt, no success, on the stalled reviewer's record.
	if a, s := e.outcomeOf(e.agentOf(first), "review"); a != 1 || s != 0 {
		t.Errorf("stalled reviewer's outcome = %d/%d, want 1/0", a, s)
	}
	retry := e.int("SELECT id FROM jobs WHERE task_id = ? AND kind = 'review' ORDER BY id DESC LIMIT 1", e.task)
	if retry == review {
		t.Fatal("no retry review job was queued behind the stall")
	}
	if got, want := e.str("SELECT prompt FROM jobs WHERE id = ?", retry), e.str("SELECT prompt FROM jobs WHERE id = ?", review); got != want {
		t.Errorf("retry prompt differs from the stalled review's own:\n%q\nwant\n%q", got, want)
	}
	if got, want := e.str("SELECT exclude_agents FROM jobs WHERE id = ?", retry), fmt.Sprintf("[%d]", e.agentOf(first)); got != want {
		t.Errorf("retry exclude_agents = %s, want the stalled reviewer %s", got, want)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskReviewing {
		t.Errorf("task state behind the retry = %q, want reviewing", state)
	}
	// Only the failed stage retries: no new implementation attempt, no
	// fix round, nothing of the task's rounds spent.
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'work'", e.task); n != 1 {
		t.Errorf("work jobs = %d, want the original alone", n)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'fix'", e.task); n != 0 {
		t.Errorf("fix jobs = %d, want none: the stall spends no round", n)
	}
	if _, ok := e.eventPayloadOf(evReviewNoVerdict); ok {
		t.Error("a review_no_verdict event parked a task the retry carries")
	}
	e.runReview(second, `{"verdict":"approve","findings":"`+substantiveFindings+`"}`)
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskDone {
		t.Errorf("task state after the retry's approve = %q, want done", state)
	}
}

// TestMissingVerdictRetriesWithoutSpendingRounds: a review with no
// usable verdict retries the unchanged head on another reviewer, the
// failed one excluded for the whole attempt; the task's implementation
// and fix rounds are untouched.
func TestMissingVerdictRetriesWithoutSpendingRounds(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	first := e.agented("first", "google", "review")
	second := e.agented("second", "meta", "review")
	review := e.runToReview(author)
	e.runReview(first, `{}`)

	retry := e.int("SELECT id FROM jobs WHERE task_id = ? AND kind = 'review' ORDER BY id DESC LIMIT 1", e.task)
	if retry == review {
		t.Fatal("no retry review job was queued behind the missing verdict")
	}
	if got, want := e.str("SELECT prompt FROM jobs WHERE id = ?", retry), e.str("SELECT prompt FROM jobs WHERE id = ?", review); got != want {
		t.Errorf("retry prompt differs from the review's own:\n%q\nwant\n%q", got, want)
	}
	if got, want := e.str("SELECT exclude_agents FROM jobs WHERE id = ?", retry), fmt.Sprintf("[%d]", e.agentOf(first)); got != want {
		t.Errorf("retry exclude_agents = %s, want the first reviewer %s", got, want)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind IN ('work', 'fix')", e.task); n != 1 {
		t.Errorf("author jobs = %d, want the original work alone: no new implementation attempt", n)
	}
	if p, ok := e.eventPayloadOf(evReviewRetry); !ok || !strings.Contains(p.Reason, "no verdict") {
		t.Errorf("review_retry payload = %+v (%t), want the missing verdict named", p, ok)
	}
	if g := e.leaseAny(first); g != nil {
		t.Fatalf("the failed reviewer leased its own retry: %+v", g)
	}
	e.runReview(second, `{"verdict":"approve","findings":"`+substantiveFindings+`"}`)
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskDone {
		t.Errorf("task state after the retry's approve = %q, want done", state)
	}
}

// TestExclusionsHoldThroughoutTheLogicalAttempt: each retry carries the
// whole set of the attempt's failed agents, and none of them leases
// again — not even when the only remaining reviewer cools and the wait
// grows long. The exclusion never lifts within the attempt.
func TestExclusionsHoldThroughoutTheLogicalAttempt(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	first := e.agented("first", "google", "review")
	second := e.agented("second", "meta", "review")
	third := e.agented("third", "openai", "review")
	e.runToReview(author)
	e.runReview(first, `{}`)
	retry1 := e.int("SELECT id FROM jobs WHERE task_id = ? AND kind = 'review' ORDER BY id DESC LIMIT 1", e.task)
	if got, want := e.str("SELECT exclude_agents FROM jobs WHERE id = ?", retry1), fmt.Sprintf("[%d]", e.agentOf(first)); got != want {
		t.Fatalf("first retry exclude_agents = %s, want %s", got, want)
	}
	e.runReview(second, `{}`)
	retry2 := e.int("SELECT id FROM jobs WHERE task_id = ? AND kind = 'review' ORDER BY id DESC LIMIT 1", e.task)
	if retry2 == retry1 {
		t.Fatal("no second retry queued behind the second unusable verdict")
	}
	if got, want := e.str("SELECT exclude_agents FROM jobs WHERE id = ?", retry2), fmt.Sprintf("[%d,%d]", e.agentOf(first), e.agentOf(second)); got != want {
		t.Fatalf("second retry exclude_agents = %s, want the whole failed set %s", got, want)
	}
	// The only remaining reviewer cools for days: the excluded two do
	// not qualify again however long the wait — the job waits instead.
	e.coolUntil(e.agentOf(third), 4*24*time.Hour)
	for _, runner := range []int64{first, second} {
		if g := e.leaseAny(runner); g != nil {
			t.Fatalf("an excluded reviewer leased the retry while the third cools: %+v", g)
		}
	}
	e.mustSweep()
	if got := e.heldLayer(retry2); got != layerCooling {
		t.Errorf("hold layer = %q, want %q: the retry waits for the cooling reviewer", got, layerCooling)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskReviewing {
		t.Errorf("task state = %q, want reviewing behind the cooling wait", state)
	}
	e.clock.Advance(5 * 24 * time.Hour)
	g := e.leaseAny(third)
	if g == nil || g.JobID != retry2 {
		t.Fatalf("the third reviewer leased %+v, want the second retry %d", g, retry2)
	}
}

// TestStageRetryExhaustionParksTheTask: the bound is maxStageAgents
// distinct agents per logical attempt — the third unusable verdict
// exhausts it, the task parks at needs_foreman with the no-verdict
// event, and no fourth review is queued.
func TestStageRetryExhaustionParksTheTask(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	first := e.agented("first", "google", "review")
	second := e.agented("second", "meta", "review")
	third := e.agented("third", "openai", "review")
	e.runToReview(author)
	for _, runner := range []int64{first, second, third} {
		e.runReview(runner, `{}`)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskNeedsForeman {
		t.Fatalf("task state after the bound = %q, want needs_foreman", state)
	}
	p, ok := e.eventPayloadOf(evReviewNoVerdict)
	if !ok || !strings.HasPrefix(p.Reason, fmt.Sprintf("task %d is parked (needs_foreman): its review retry is spent", e.task)) {
		t.Fatalf("no-verdict payload = %+v (%t), want the spent-retry park", p, ok)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'review'", e.task); n != maxStageAgents {
		t.Errorf("review jobs = %d, want the bound of %d", n, maxStageAgents)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind IN ('work', 'fix')", e.task); n != 1 {
		t.Errorf("author jobs = %d, want the original work alone: exhaustion creates no implementation attempt", n)
	}
}

// TestExhaustedReadyReviewersWaitNotPark: with the only remaining
// reviewer cooling, the retry waits for its reset — the cooling hold,
// not the park — and takes the retry once it cools out.
func TestExhaustedReadyReviewersWaitNotPark(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	first := e.agented("first", "google", "review")
	second := e.agented("second", "meta", "review")
	e.runToReview(author)
	e.runReview(first, `{}`)
	retry := e.int("SELECT id FROM jobs WHERE task_id = ? AND kind = 'review' ORDER BY id DESC LIMIT 1", e.task)
	e.coolUntil(e.agentOf(second), 30*time.Minute)
	if g := e.leaseAny(second); g != nil {
		t.Fatalf("the cooling reviewer leased the retry: %+v", g)
	}
	if g := e.leaseAny(first); g != nil {
		t.Fatalf("the excluded reviewer leased the retry: %+v", g)
	}
	e.mustSweep()
	if got := e.heldLayer(retry); got != layerCooling {
		t.Errorf("hold layer = %q, want %q", got, layerCooling)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskReviewing {
		t.Fatalf("task state = %q, want reviewing: a cooling reviewer is a wait, not a park", state)
	}
	if _, ok := e.eventPayloadOf(evReviewNoVerdict); ok {
		t.Error("a review_no_verdict event parked a task whose reviewer only cools")
	}
	e.clock.Advance(31 * time.Minute)
	if g := e.leaseAny(second); g == nil || g.JobID != retry {
		t.Fatalf("the cooled-out reviewer leased %+v, want the retry %d", g, retry)
	}
}
