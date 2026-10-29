package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// newRequireEnv builds an engine with review.family=require (issue #126):
// today's cross-family-only placement.
func newRequireEnv(t *testing.T) *env {
	t.Helper()
	return newEnvCfg(t, func(c *Config) { c.Review.Family = ReviewFamilyRequire })
}

// pickPlacement is the placement kind of the job's newest agent pick.
func (e *env) pickPlacement(job int64) string {
	e.t.Helper()
	evs := e.pickEvents(job)
	if len(evs) == 0 {
		e.t.Fatalf("job %d has no agent pick event", job)
	}
	return evs[len(evs)-1].Placement
}

// TestPreferPicksCrossFamilyFirst: under the prefer default — the key
// absent — a review still goes to another family first, whichever runner
// polls, and its pick records the cross_family placement.
func TestPreferPicksCrossFamilyFirst(t *testing.T) {
	e := newEnv(t) // no review.family key: the prefer default
	author := e.agented("author", "anthropic", "implement")
	sibling := e.agented("sibling", "anthropic", "review")
	reviewer := e.agented("reviewer", "google", "review")
	review := e.runToReview(author)

	// The same-family runner polls first and gets nothing: another
	// family's agent is available elsewhere.
	if g := e.leaseAny(sibling); g != nil {
		t.Fatalf("a same-family agent leased the review before another family's: %+v", g)
	}
	g := e.leaseAny(reviewer)
	if g == nil || g.JobID != review {
		t.Fatalf("the other family's agent leased %+v, want the review %d", g, review)
	}
	if got := e.pickPlacement(review); got != placeCrossFamily {
		t.Errorf("placement = %q, want %q", got, placeCrossFamily)
	}
	if agent := e.jobAgent(review); agent != e.agentOf(reviewer) {
		t.Errorf("review agent = %d, want the other family's %d", agent, e.agentOf(reviewer))
	}
}

// TestPreferFallsBackToTheAuthorsFamily: with no agent of another family
// available — cooling or broken — the review goes to another agent of
// the author's family, and its pick records same_family.
func TestPreferFallsBackToTheAuthorsFamily(t *testing.T) {
	for name, unavailable := range map[string]func(e *env, id int64){
		"cooling": func(e *env, id int64) { e.coolUntil(id, time.Hour) },
		"broken":  func(e *env, id int64) { e.exec("UPDATE agents SET health = 'broken' WHERE id = ?", id) },
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t) // the prefer default
			author := e.agented("author", "anthropic", "implement")
			sibling := e.agented("sibling", "anthropic", "review")
			other := e.agented("other", "google", "review")
			review := e.runToReview(author)
			unavailable(e, e.agentOf(other))

			g := e.leaseAny(sibling)
			if g == nil || g.JobID != review {
				t.Fatalf("the author's family's other agent leased %+v, want the review %d", g, review)
			}
			if got := e.pickPlacement(review); got != placeSameFamily {
				t.Errorf("placement = %q, want %q", got, placeSameFamily)
			}
		})
	}
}

// TestPreferFallsBackToTheAuthorsAgent: with no reviewer but the author
// — a single-agent setup — the author's own agent reviews its work, and
// its pick records same_agent.
func TestPreferFallsBackToTheAuthorsAgent(t *testing.T) {
	e := newEnv(t) // the prefer default, the key left absent
	author := e.agented("author", "anthropic", "implement", "review")
	review := e.runToReview(author)

	g := e.leaseAny(author)
	if g == nil || g.JobID != review {
		t.Fatalf("the author's own agent leased %+v, want its review %d", g, review)
	}
	if got := e.pickPlacement(review); got != placeSameAgent {
		t.Errorf("placement = %q, want %q", got, placeSameAgent)
	}
}

// TestPreferFamilyStepsOutrankTier: a cross-family reviewer on another
// tier beats a same-family reviewer on the tier the review asked for.
func TestPreferFamilyStepsOutrankTier(t *testing.T) {
	e := newEnvCfg(t, func(c *Config) {
		// The task runs standard, so the review asks light; the
		// cross-family agent alone is heavy.
		c.Caps.Tier = map[string]string{"sibling/anthropic-1": "light", "other/google-1": "heavy"}
	})
	author := e.agented("author", "anthropic", "implement")
	sibling := e.agented("sibling", "anthropic", "review")
	other := e.agented("other", "google", "review")
	e.exec("UPDATE tasks SET effective_tier = 'standard' WHERE id = ?", e.task)
	review := e.runToReview(author)

	g := e.leaseAny(sibling)
	if g != nil {
		t.Fatalf("the same-family agent on the asked tier leased the review: %+v", g)
	}
	g = e.leaseAny(other)
	if g == nil || g.JobID != review {
		t.Fatalf("the cross-family agent on another tier leased %+v, want the review %d", g, review)
	}
	if p := e.pickEvents(review); len(p) != 1 || p[0].TierAsked != "light" || p[0].TierServed != "heavy" {
		t.Errorf("pick = %+v, want the light ask served heavy", p)
	}
	if got := e.pickPlacement(review); got != placeCrossFamily {
		t.Errorf("placement = %q, want %q", got, placeCrossFamily)
	}
}

// TestPreferRetryExclusionHoldsOverTheAuthorsFamily: the retry's
// exclusion of the first reviewer holds while the family steps have
// another reviewer — the author's family's own agent included — so the
// retry goes there, not back to the excluded agent.
func TestPreferRetryExclusionHoldsOverTheAuthorsFamily(t *testing.T) {
	e := newEnv(t) // the prefer default
	author := e.agented("author", "anthropic", "implement")
	first := e.agented("first", "google", "review")
	sibling := e.agented("sibling", "anthropic", "review")
	e.runToReview(author)
	e.runReview(first, `{}`)
	retry := e.int("SELECT id FROM jobs WHERE task_id = ? AND kind = 'review' ORDER BY id DESC LIMIT 1", e.task)

	if g := e.leaseAny(first); g != nil {
		t.Fatalf("the excluded reviewer leased the retry while the author's family had one: %+v", g)
	}
	g := e.leaseAny(sibling)
	if g == nil || g.JobID != retry {
		t.Fatalf("the author's family's agent leased %+v, want the retry %d", g, retry)
	}
	if evs := e.pickEvents(retry); len(evs) != 1 || evs[0].ExclusionLifted {
		t.Errorf("pick events = %+v, want one without the lift", evs)
	}
	if got := e.pickPlacement(retry); got != placeSameFamily {
		t.Errorf("placement = %q, want %q", got, placeSameFamily)
	}
}

// TestPreferPlanReviewFallsBackToTheForemansFamily: a plan review under
// prefer is placed by the same steps with the foreman as the author —
// with no other family, the foreman's own family's agent reviews the
// plan and the pick records same_family; under require the plan proceeds
// with today's reason instead.
func TestPreferPlanReviewFallsBackToTheForemansFamily(t *testing.T) {
	t.Run("prefer", func(t *testing.T) {
		e := newPlanReviewEnv(t)
		e.offerForeman(e.runner1)
		// Another agent of the foreman's family (claude), review-capable.
		sibling := e.agented("sibling", "claude", "review")
		res := e.createPlan(nTasks(2))
		if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != planReviewing {
			t.Fatalf("plan state = %q, want %s: the foreman's family can review under prefer", got, planReviewing)
		}
		g := e.leaseAny(sibling)
		if g == nil || g.Kind != KindPlanReview {
			t.Fatalf("the foreman's family's agent leased %+v, want the plan review", g)
		}
		if got := e.pickPlacement(g.JobID); got != placeSameFamily {
			t.Errorf("placement = %q, want %q", got, placeSameFamily)
		}
	})
	t.Run("require", func(t *testing.T) {
		e := newEnvCfg(t, func(c *Config) {
			c.PlanReview = nil
			c.Review.Family = ReviewFamilyRequire
		})
		e.offerForeman(e.runner1)
		e.agented("sibling", "claude", "review")
		res := e.createPlan(nTasks(2))
		if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != "active" {
			t.Fatalf("plan state = %q, want active: require keeps the foreman's family out", got)
		}
		prompt := e.str("SELECT prompt FROM jobs WHERE kind = 'foreman' ORDER BY id DESC")
		if !strings.Contains(prompt, "no agent of a family other than the foreman's offers the review class") {
			t.Errorf("the foreman's turn lacks today's skip reason:\n%s", prompt)
		}
	})
}

// TestPreferBranchReviewPlacedWithoutAFamily: a standalone review whose
// author's family cannot be told — no work job, no known trailer — is
// placed on any ready review-class agent under prefer instead of being
// refused, and its pick records the unknown placement; under require
// today's refusal stands.
func TestPreferBranchReviewPlacedWithoutAFamily(t *testing.T) {
	setup := func(t *testing.T) (*env, int64) {
		e := newEnvCfg(t, func(c *Config) {
			c.BranchTrailers = func(context.Context, string, string, string) (string, error) { return "", nil }
		})
		reviewer := e.agented("reviewer", "google", "review")
		return e, reviewer
	}
	t.Run("prefer", func(t *testing.T) {
		e, reviewer := setup(t)
		res, err := e.reviewBranch("feature/x")
		if err != nil {
			t.Fatalf("review the branch of an unknown family: %v", err)
		}
		if res.Family != "" || res.Source != "" {
			t.Errorf("result = %+v, want no family and no source: nothing named one", res)
		}
		g := e.leaseAny(reviewer)
		if g == nil || g.JobID != res.JobID {
			t.Fatalf("a review-class agent leased %+v, want the branch review %d", g, res.JobID)
		}
		if got := e.pickPlacement(res.JobID); got != placeUnknown {
			t.Errorf("placement = %q, want %q", got, placeUnknown)
		}
	})
	t.Run("require", func(t *testing.T) {
		e, _ := setup(t)
		e.eng.cfg.Review.Family = ReviewFamilyRequire
		_, err := e.reviewBranch("feature/x")
		if !errors.Is(err, ErrNoReviewer) || !strings.Contains(err.Error(), "no default_reviewer is configured") ||
			!strings.Contains(err.Error(), "review.family=require") {
			t.Fatalf("error = %v, want today's refusal naming the missing default reviewer and review.family=require", err)
		}
		if n := e.int("SELECT COUNT(*) FROM jobs WHERE kind = 'review'"); n != 0 {
			t.Errorf("%d review jobs queued by a refused review", n)
		}
	})
}

// TestPreferBranchReviewFallsBackToTheAuthorsFamily: a standalone review
// of a branch whose author's family is known goes to another agent of
// that family when no other family offers review, with same_family
// recorded; require refuses it as today.
func TestPreferBranchReviewFallsBackToTheAuthorsFamily(t *testing.T) {
	t.Run("prefer", func(t *testing.T) {
		e := newEnv(t)
		author := e.agented("author", "anthropic", "implement")
		sibling := e.agented("sibling", "anthropic", "review")
		branch := e.authored(author)
		res, err := e.reviewBranch(branch)
		if err != nil {
			t.Fatalf("review the branch: %v", err)
		}
		g := e.leaseAny(sibling)
		if g == nil || g.JobID != res.JobID {
			t.Fatalf("the author's family's agent leased %+v, want the branch review %d", g, res.JobID)
		}
		if got := e.pickPlacement(res.JobID); got != placeSameFamily {
			t.Errorf("placement = %q, want %q", got, placeSameFamily)
		}
	})
	t.Run("require", func(t *testing.T) {
		e := newRequireEnv(t)
		author := e.agented("author", "anthropic", "implement", "review")
		branch := e.authored(author)
		_, err := e.reviewBranch(branch)
		if !errors.Is(err, ErrNoReviewer) || !strings.Contains(err.Error(), wantAuthorOnlyReviewer) {
			t.Fatalf("error = %v, want the author-only explanation", err)
		}
	})
}

// TestPreferUnservableOnlyWithoutReviewClass: under prefer a review is
// unservable only when no review-class agent at all can serve it — the
// author's own family never leaves it stuck — and the reason names no
// family then.
func TestPreferUnservableOnlyWithoutReviewClass(t *testing.T) {
	e := newEnvCfg(t, func(c *Config) { c.QueueTTL = time.Minute })
	// The author offers no review class, and no other agent exists.
	author := e.agented("author", "anthropic", "implement")
	e.runToReview(author)

	it, ok := e.needsYouTask(e.task)
	if !ok {
		t.Fatal("no needs-you entry for the review no agent can take")
	}
	want := "no reviewer: no agent offers review; offer an agent that does to the runner (--agent harness=…) or review by hand"
	if it.Reason != want {
		t.Errorf("reason = %q, want %q", it.Reason, want)
	}
	// The author's own agent, of a known family, makes it servable the
	// moment it offers review: the ladder's last step is the author.
	e.exec("UPDATE agents SET classes = '[\"implement\",\"review\"]' WHERE runner_id = ?", author)
	if it, ok := e.needsYouTask(e.task); ok {
		t.Errorf("entry %+v remains once the author offers review", it)
	}
}
