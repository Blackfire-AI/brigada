package engine

import (
	"database/sql"
	"strings"
	"testing"
	"time"
)

// reviewStack registers an author and three reviewers of other families
// and drives the task to its first review.
func reviewStack(t *testing.T) (e *env, rev [3]int64) {
	t.Helper()
	e = newEnv(t)
	author := e.agented("author", "a", "implement")
	rev[0] = e.agented("first", "b", "review")
	rev[1] = e.agented("second", "c", "review")
	rev[2] = e.agented("third", "d", "review")
	e.runToReview(author)
	return e, rev
}

func (e *env) spentRetry(job int64) bool {
	e.t.Helper()
	var spent bool
	err := e.st.View(e.ctx, func(tx *sql.Tx) error {
		j, err := loadJobTx(tx, job)
		if err != nil {
			return err
		}
		spent, err = reviewRetrySpentTx(tx, j)
		return err
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return spent
}

func TestLimitThenNoVerdictRetryIsQueuedNotParked(t *testing.T) {
	e, rev := reviewStack(t)
	g := e.leaseAny(rev[0])
	e.complete(g, Completion{Error: "usage limit", ErrorKind: "limit"})
	if n := e.countEvents(evReviewRetry); n != 1 {
		t.Fatalf("review_retry events after the limit = %d, want 1", n)
	}
	if p, _ := e.eventPayloadOf(evReviewRetry); !strings.Contains(p.Reason, "reviewer unavailable (limit): first/b-1; requeued") {
		t.Errorf("requeue reason = %q", p.Reason)
	}
	g2 := e.leaseAny(rev[1])
	if g2 == nil || g2.Kind != KindReview {
		t.Fatalf("requeued grant = %+v, want a review", g2)
	}
	e.complete(g2, Completion{Result: "{}"})
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskReviewing {
		t.Fatalf("task state = %s, want reviewing with the retry queued", state)
	}
	if n := e.countEvents(evReviewRetry); n != 2 {
		t.Errorf("review_retry events = %d, want 2", n)
	}
	if n := e.countEvents(evReviewNoVerdict); n != 0 {
		t.Errorf("review_no_verdict events = %d, want none", n)
	}
	if g3 := e.leaseAny(rev[2]); g3 == nil || g3.Kind != KindReview {
		t.Errorf("retry grant = %+v, want a review on the third reviewer", g3)
	}
}

func TestAuthFailureReviewIsRequeued(t *testing.T) {
	e, rev := reviewStack(t)
	e.complete(e.leaseAny(rev[0]), Completion{Error: "login expired", ErrorKind: "auth"})
	if p, ok := e.eventPayloadOf(evReviewRetry); !ok || !strings.Contains(p.Reason, "reviewer unavailable (auth): first/b-1; requeued") {
		t.Errorf("retry event = %+v, %v", p, ok)
	}
	if g := e.leaseAny(rev[1]); g == nil || g.Kind != KindReview {
		t.Errorf("requeued grant = %+v, want a review", g)
	}
}

// TestModelRejectedThenNoVerdictRetryIsQueuedNotParked: a review whose
// reviewer's model was rejected never answered, so it does not use up
// the cycle's one no-verdict retry.
func TestModelRejectedThenNoVerdictRetryIsQueuedNotParked(t *testing.T) {
	e, rev := reviewStack(t)
	e.complete(e.leaseAny(rev[0]), Completion{Error: "The 'x' model is not supported for this account", ErrorKind: "model"})
	if p, ok := e.eventPayloadOf(evReviewRetry); !ok || !strings.Contains(p.Reason, "reviewer unavailable (model)") {
		t.Fatalf("retry event = %+v, %v", p, ok)
	}
	g2 := e.leaseAny(rev[1])
	if g2 == nil || g2.Kind != KindReview {
		t.Fatalf("requeued grant = %+v, want a review", g2)
	}
	e.complete(g2, Completion{Result: "{}"})
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskReviewing {
		t.Fatalf("task state = %s, want reviewing with the retry queued", state)
	}
	if n := e.countEvents(evReviewNoVerdict); n != 0 {
		t.Errorf("review_no_verdict events = %d, want none", n)
	}
}

func TestSetupFailureReviewIsRequeued(t *testing.T) {
	e, rev := reviewStack(t)
	e.complete(e.leaseAny(rev[0]), Completion{Error: "setup failed", ErrorKind: ErrorKindSetup})
	if n := e.countEvents(evReviewRetry); n != 1 {
		t.Errorf("review_retry events = %d, want 1", n)
	}
}

func TestTwoNoVerdictReviewsStillPark(t *testing.T) {
	e, rev := reviewStack(t)
	e.complete(e.leaseAny(rev[0]), Completion{Result: "{}"})
	e.complete(e.leaseAny(rev[1]), Completion{Result: "{}"})
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskNeedsForeman {
		t.Errorf("task state = %s, want needs_foreman", state)
	}
	if n := e.countEvents(evReviewNoVerdict); n != 1 {
		t.Errorf("review_no_verdict events = %d, want 1", n)
	}
}

func TestRequeuedReviewWaitsWhileQualifyingAgentsCool(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "a", "implement")
	first := e.agented("first", "b", "review")
	second := e.agented("second", "c", "review")
	e.runToReview(author)
	e.complete(e.leaseAny(first), Completion{Error: "usage limit", ErrorKind: "limit"})
	until := fmtTime(e.clock.Now().Add(time.Hour))
	e.exec("UPDATE agents SET cooling_until = ?", until)
	if g := e.leaseAny(second); g != nil {
		t.Fatalf("leased %+v while every reviewer cools", g)
	}
	e.clock.Advance(2 * time.Hour)
	if g := e.leaseAny(second); g == nil || g.Kind != KindReview {
		t.Errorf("grant after the cooling = %+v, want the requeued review", g)
	}
}

func TestRetryCountIgnoresUnavailableFailure(t *testing.T) {
	e, rev := reviewStack(t)
	g := e.leaseAny(rev[0])
	e.complete(g, Completion{Error: "usage limit", ErrorKind: "limit"})
	// The limited review and its requeued job: neither is an unusable answer.
	if e.spentRetry(g.JobID) {
		t.Error("retry spent by a limit failure")
	}
	e.complete(e.leaseAny(rev[1]), Completion{Error: "usage limit", ErrorKind: "limit"})
	// Two unavailable failures, then one unusable answer: still its one retry.
	e.complete(e.leaseAny(rev[2]), Completion{Result: "{}"})
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskReviewing {
		t.Errorf("task state = %s, want reviewing with the retry queued", state)
	}
	if n := e.countEvents(evReviewNoVerdict); n != 0 {
		t.Errorf("review_no_verdict events = %d, want none", n)
	}
}

func TestJobErrorKindRecordedAtCompletion(t *testing.T) {
	for _, kind := range []string{"limit", "auth", ErrorKindSetup, "other", "runner_lost", ""} {
		e := newEnv(t)
		author := e.agented("author", "a", "implement")
		e.submit(e.work())
		g := e.leaseAny(author)
		e.complete(g, Completion{Error: "boom", ErrorKind: kind})
		if got := e.str("SELECT error_kind FROM jobs WHERE id = ?", g.JobID); got != kind {
			t.Errorf("error_kind for %q = %q", kind, got)
		}
	}
	e := newEnv(t)
	author := e.agented("author", "a", "implement")
	e.submit(e.work())
	g := e.leaseAny(author)
	e.complete(g, doneOn("task/1-1", passCheck))
	if got := e.str("SELECT error_kind FROM jobs WHERE id = ?", g.JobID); got != "" {
		t.Errorf("error_kind of a done job = %q, want empty", got)
	}
}
