package engine

import (
	"strings"
	"testing"
	"time"
)

// TestRetryExclusionHoldsWhileAnotherReviewerIsReady: the retry's
// exclusion of the first reviewer is a preference (§7, decided
// 2026-10-20) that holds while another qualifying reviewer is ready:
// the first reviewer's runner does not see the retry, the other
// reviewer's takes it, and its pick event carries no lift.
func TestRetryExclusionHoldsWhileAnotherReviewerIsReady(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	first := e.agented("first", "google", "review")
	second := e.agented("second", "moonshot", "review")
	e.runToReview(author)
	e.runReview(first, `{}`)
	retry := e.int("SELECT id FROM jobs WHERE task_id = ? AND kind = 'review' ORDER BY id DESC LIMIT 1", e.task)

	if g := e.leaseAny(first); g != nil {
		t.Fatalf("the excluded reviewer leased the retry while another reviewer was ready: %+v", g)
	}
	g := e.leaseAny(second)
	if g == nil || g.JobID != retry {
		t.Fatalf("the other reviewer leased %+v, want the retry job %d", g, retry)
	}
	if evs := e.pickEvents(retry); len(evs) != 1 || evs[0].ExclusionLifted {
		t.Errorf("pick events = %+v, want one without the lift", evs)
	}
}

// TestRetryExclusionLiftsWhenTheOnlyOtherReviewerCools: with the
// only other qualifying reviewer cooling, the excluded
// reviewer qualifies again, takes the retry at once, and its pick event
// records the lifted exclusion.
func TestRetryExclusionLiftsWhenTheOnlyOtherReviewerCools(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	first := e.agented("first", "google", "review")
	second := e.agented("second", "moonshot", "review")
	e.runToReview(author)
	e.runReview(first, `{}`)
	retry := e.int("SELECT id FROM jobs WHERE task_id = ? AND kind = 'review' ORDER BY id DESC LIMIT 1", e.task)

	e.coolUntil(e.agentOf(second), 4*24*time.Hour) // a multi-day limit
	if g := e.leaseAny(second); g != nil {
		t.Fatalf("the cooling reviewer leased the retry: %+v", g)
	}
	g := e.leaseAny(first)
	if g == nil || g.JobID != retry {
		t.Fatalf("the excluded reviewer leased %+v, want its lifted retry %d", g, retry)
	}
	if got := e.jobAgent(retry); got != e.agentOf(first) {
		t.Errorf("the lifted retry ran on agent %d, want the excluded %d", got, e.agentOf(first))
	}
	if evs := e.pickEvents(retry); len(evs) != 1 || !evs[0].ExclusionLifted {
		t.Errorf("pick events = %+v, want one lift recorded", evs)
	}
}

// TestRetryExclusionLiftsOnAShortCoolingReviewer: another reviewer
// cooling for minutes is unavailable too, so the exclusion lifts at
// once and the excluded reviewer takes its retry without waiting for
// the reset.
func TestRetryExclusionLiftsOnAShortCoolingReviewer(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	first := e.agented("first", "google", "review")
	second := e.agented("second", "moonshot", "review")
	e.runToReview(author)
	e.runReview(first, `{}`)
	retry := e.int("SELECT id FROM jobs WHERE task_id = ? AND kind = 'review' ORDER BY id DESC LIMIT 1", e.task)

	e.coolUntil(e.agentOf(second), 5*time.Minute)
	if g := e.leaseAny(second); g != nil {
		t.Fatalf("the cooling reviewer leased the retry: %+v", g)
	}
	g := e.leaseAny(first)
	if g == nil || g.JobID != retry {
		t.Fatalf("the excluded reviewer leased %+v, want its lifted retry %d", g, retry)
	}
	if evs := e.pickEvents(retry); len(evs) != 1 || !evs[0].ExclusionLifted {
		t.Errorf("pick events = %+v, want one lift recorded", evs)
	}
}

// TestLiftedRetryCoolingHoldNamesTheExcludedAgent: when the excluded
// reviewer is the soonest reset, the cooling hold names it — the agent
// the job really waits for — and once it cools out it takes the retry.
func TestLiftedRetryCoolingHoldNamesTheExcludedAgent(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	first := e.agented("first", "google", "review")
	second := e.agented("second", "moonshot", "review")
	e.runToReview(author)
	e.runReview(first, `{}`)
	retry := e.int("SELECT id FROM jobs WHERE task_id = ? AND kind = 'review' ORDER BY id DESC LIMIT 1", e.task)

	soonest := e.coolUntil(e.agentOf(first), 30*time.Minute)
	e.coolUntil(e.agentOf(second), 4*24*time.Hour)
	if g := e.leaseAny(first); g != nil {
		t.Fatalf("leased %+v while the excluded agent still cools", g)
	}
	e.mustSweep()
	if got := e.heldLayer(retry); got != layerCooling {
		t.Fatalf("hold layer = %q, want %q", got, layerCooling)
	}
	info, err := e.eng.Status(e.ctx, 0)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(info.Held) != 1 || info.Held[0].ID != retry ||
		info.Held[0].Agent != "first/google-1" || info.Held[0].Until != fmtTime(soonest) {
		t.Fatalf("status held = %+v, want job %d cooling on first/google-1 until %s",
			info.Held, retry, fmtTime(soonest))
	}

	e.clock.Advance(31 * time.Minute) // the excluded agent cools out; the other still cools for days
	g := e.leaseAny(first)
	if g == nil || g.JobID != retry {
		t.Fatalf("the excluded reviewer leased %+v, want its lifted retry %d", g, retry)
	}
}

// TestLiftedRetryTwiceNoVerdictParksTheTask: the lift is once — a
// lifted retry that again ends with no verdict from the same agent
// parks the task, saying the only available reviewer twice gave no
// verdict, and no third review is queued.
func TestLiftedRetryTwiceNoVerdictParksTheTask(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	first := e.agented("first", "google", "review")
	second := e.agented("second", "moonshot", "review")
	e.runToReview(author)
	e.runReview(first, `{}`)
	e.coolUntil(e.agentOf(second), 4*24*time.Hour)

	e.runReview(first, `I read it again, still fine`)
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskNeedsForeman {
		t.Fatalf("task state after the lifted retry's no verdict = %q, want needs_foreman", state)
	}
	p, ok := e.eventPayloadOf(evReviewNoVerdict)
	if !ok {
		t.Fatalf("no review_no_verdict event; have %v", e.eventKinds())
	}
	if !strings.Contains(p.Reason, "the only available reviewer twice gave no verdict") {
		t.Errorf("no-verdict reason = %q, want the twice-no-verdict cause", p.Reason)
	}
	if !strings.Contains(p.Reason, "first/google-1: no verdict") {
		t.Errorf("no-verdict reason = %q, want both reviews of the cycle named", p.Reason)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'review'", e.task); n != 2 {
		t.Errorf("review jobs = %d, want the first and its lifted retry alone", n)
	}
}

// TestLiftedExclusionNeverReachesTheAuthorsFamily: the family rule is
// never relaxed — with no reviewer but the author's own agent, the
// lift opens nothing and the task parks on the first no-verdict.
func TestLiftedExclusionNeverReachesTheAuthorsFamily(t *testing.T) {
	e := newEnvCfg(t, func(c *Config) { c.Review.Family = ReviewFamilyRequire })
	author := e.agented("author", "anthropic", "implement", "review")
	first := e.agented("first", "google", "review")
	e.runToReview(author)
	e.runReview(first, `{}`)
	// The author's own agent, of the excluded family, offers review:
	// still nothing for it to lease, lifted or not.
	if g := e.leaseAny(author); g != nil {
		t.Fatalf("the author's family leased the lifted retry: %+v", g)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskReviewing {
		t.Fatalf("task state = %q, want reviewing behind the lifted retry", state)
	}
	e.runReview(first, `{}`)
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskNeedsForeman {
		t.Fatalf("task state = %q, want needs_foreman after the lifted retry", state)
	}
}
