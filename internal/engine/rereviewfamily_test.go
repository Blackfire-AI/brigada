package engine

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Issue #126: re-reviews and session isolation under review.family=prefer.

// The placement sentences the grant's prompt states, as internal/prompt
// writes them: enough of each to tell them apart.
const (
	saysCrossFamily = "You were placed cross-family"
	saysSameFamily  = "You were placed same-family"
	saysSameAgent   = "You run on the author's own agent and model"
)

// TestPreferReReviewResumesASameFamilyReviewer: under prefer a re-review
// resumes the previous reviewer's session though it shares the author's
// family, and the grant's prompt says it was placed same-family.
func TestPreferReReviewResumesASameFamilyReviewer(t *testing.T) {
	f := changesRoundWith(t, nil, "anthropic", nil) // the prefer default
	e := f.e
	if got := e.pickPlacement(f.review); got != placeSameFamily {
		t.Fatalf("first review placement = %q, want %q", got, placeSameFamily)
	}
	f.fixDone("h2")
	j := f.secondReview()
	if j.Mode != ModeSession || j.RequireAgent != e.agentOf(f.reviewer) {
		t.Fatalf("re-review mode/require = %s/%d, want the session pinned to the same-family reviewer", j.Mode, j.RequireAgent)
	}
	g := f.reviewerLease()
	if g == nil || g.JobID != j.ID || g.ResumeSessionID != reviewerSession || g.ReviewSince != "h1" {
		t.Fatalf("re-review grant = %+v, want the reviewer's session %q since h1", g, reviewerSession)
	}
	if !strings.Contains(g.Prompt, saysSameFamily) || !strings.Contains(g.Prompt, "back for a re-review") {
		t.Errorf("re-review prompt does not state the same-family placement:\n%s", g.Prompt)
	}
	if got := e.pickPlacement(j.ID); got != placeSameFamily {
		t.Errorf("re-review placement = %q, want %q", got, placeSameFamily)
	}
}

// sameAgentFixture is a task whose one agent both authors and reviews:
// the single-agent setup prefer's last family step serves.
type sameAgentFixture struct {
	e      *env
	author int64
}

// sameAgentChanges runs the work and its first review on the one agent;
// the review, in a session of its own, asks for changes and ends on
// session (empty: unknown).
func sameAgentChanges(t *testing.T, session string) *sameAgentFixture {
	t.Helper()
	e := newEnv(t) // the prefer default
	f := &sameAgentFixture{e: e, author: e.agented("author", "anthropic", "implement", "review")}
	e.submit(e.work())
	g := e.authorLease(f.author)
	first := headResult("task/1-1", "added the thing", "h1")
	first.SessionID = authorSession
	if ans := e.complete(g, first); ans != AnsOK {
		t.Fatalf("complete the work: %s", ans)
	}
	rg := e.authorLease(f.author)
	if rg == nil || rg.Kind != KindReview {
		t.Fatalf("the author's agent leased %+v, want its review", rg)
	}
	f.wantFreshGrant(rg)
	if ans := e.complete(rg, Completion{Result: `{"verdict":"changes","findings":"the loop leaks"}`, SessionID: session}); ans != AnsOK {
		t.Fatalf("complete the review: %s", ans)
	}
	return f
}

// wantFreshGrant checks a review grant on the author's own agent: a
// fresh session — never the author's — and the same-agent placement in
// the full review prompt.
func (f *sameAgentFixture) wantFreshGrant(g *LeaseGrant) {
	f.e.t.Helper()
	if g.Mode != ModeFresh || g.ResumeSessionID != "" {
		f.e.t.Errorf("same-agent review grant mode/resume = %s/%q, want a fresh session, not the author's", g.Mode, g.ResumeSessionID)
	}
	if !strings.Contains(g.Prompt, saysSameAgent) || strings.Contains(g.Prompt, saysCrossFamily) || !strings.Contains(g.Prompt, "<task-spec>") {
		f.e.t.Errorf("same-agent review prompt does not state its placement:\n%s", g.Prompt)
	}
	if got := f.e.pickPlacement(g.JobID); got != placeSameAgent {
		f.e.t.Errorf("placement = %q, want %q", got, placeSameAgent)
	}
}

// fixRound leases the fix round, which must resume the author's session,
// runs before (if any) and completes it at head.
func (f *sameAgentFixture) fixRound(head string, before func()) {
	f.e.t.Helper()
	g := f.e.authorLease(f.author)
	if g == nil || g.Kind != KindFix || g.ResumeSessionID != authorSession {
		f.e.t.Fatalf("fix grant = %+v, want the fix round on the author's session %q", g, authorSession)
	}
	if before != nil {
		before()
	}
	c := headResult("task/1-1", "closed the leak", head)
	c.SessionID = authorSession
	if ans := f.e.complete(g, c); ans != AnsOK {
		f.e.t.Fatalf("complete the fix round: %s", ans)
	}
}

// TestSameAgentReReviewResumesTheReviewersOwnSession: a review on the
// author's own agent runs in a fresh session of its own; its re-review
// resumes that review session, never the author's; and the fix rounds
// around it resume the author's, which the key keeps.
func TestSameAgentReReviewResumesTheReviewersOwnSession(t *testing.T) {
	f := sameAgentChanges(t, reviewerSession)
	e := f.e
	f.fixRound("h2", nil)

	g := e.authorLease(f.author)
	if g == nil || g.Kind != KindReview || g.Mode != ModeSession {
		t.Fatalf("leased %+v, want the re-review in session mode", g)
	}
	if g.ResumeSessionID != reviewerSession || g.ReviewSince != "h1" {
		t.Errorf("re-review resume/since = %q/%q, want the review's own session %q since h1", g.ResumeSessionID, g.ReviewSince, reviewerSession)
	}
	if !strings.Contains(g.Prompt, "back for a re-review") || !strings.Contains(g.Prompt, saysSameAgent) ||
		!strings.Contains(g.Prompt, "no access to the author's session") {
		t.Errorf("re-review prompt does not state the same-agent placement:\n%s", g.Prompt)
	}
	if got := e.pickPlacement(g.JobID); got != placeSameAgent {
		t.Errorf("re-review placement = %q, want %q", got, placeSameAgent)
	}
	if ans := e.complete(g, Completion{Result: `{"verdict":"changes","findings":"still leaks"}`, SessionID: "hs-reviewer-2"}); ans != AnsOK {
		t.Fatalf("complete the re-review: %s", ans)
	}
	if sid := e.str("SELECT harness_session_id FROM sessions WHERE key = ?", g.Key); sid != authorSession {
		t.Errorf("key session = %q, want the author's %q", sid, authorSession)
	}
	// The next fix round still resumes the author's session, and the
	// re-review after it the reviewer's latest.
	f.fixRound("h3", nil)
	g3 := e.authorLease(f.author)
	if g3 == nil || g3.Kind != KindReview || g3.ResumeSessionID != "hs-reviewer-2" || g3.ReviewSince != "h2" {
		t.Errorf("second re-review grant = %+v, want session hs-reviewer-2 since h2", g3)
	}
}

// TestSameAgentReReviewFallsBackFresh: when the reviewer's own session
// cannot be resumed — unknown, the agent cooling, or the CLI's
// resume_failed — the review on the author's own agent runs fresh, in a
// session of its own, and never picks up the author's.
func TestSameAgentReReviewFallsBackFresh(t *testing.T) {
	t.Run("session unknown", func(t *testing.T) {
		f := sameAgentChanges(t, "")
		f.fixRound("h2", nil)
		if p, _ := f.e.eventPayloadOf(evTaskReviewing); !strings.Contains(p.Reason, "session id is unknown") {
			t.Errorf("task_reviewing reason = %q, want the unknown session", p.Reason)
		}
		g := f.e.authorLease(f.author)
		if g == nil || g.Kind != KindReview {
			t.Fatalf("leased %+v, want the fresh review", g)
		}
		f.wantFreshGrant(g)
	})
	t.Run("cooling", func(t *testing.T) {
		f := sameAgentChanges(t, reviewerSession)
		agent := f.e.agentOf(f.author)
		f.fixRound("h2", func() {
			f.e.exec("UPDATE agents SET cooling_until = ? WHERE id = ?", fmtTime(f.e.clock.Now().Add(30*time.Minute)), agent)
		})
		if p, _ := f.e.eventPayloadOf(evTaskReviewing); !strings.Contains(p.Reason, "is cooling until") {
			t.Errorf("task_reviewing reason = %q, want the cooling", p.Reason)
		}
		f.e.exec("UPDATE agents SET cooling_until = NULL WHERE id = ?", agent)
		g := f.e.authorLease(f.author)
		if g == nil || g.Kind != KindReview {
			t.Fatalf("leased %+v, want the fresh review", g)
		}
		f.wantFreshGrant(g)
	})
	t.Run("resume_failed", func(t *testing.T) {
		f := sameAgentChanges(t, reviewerSession)
		f.fixRound("h2", nil)
		g := f.e.authorLease(f.author)
		if g == nil || g.Kind != KindReview || g.ResumeSessionID != reviewerSession {
			t.Fatalf("leased %+v, want the re-review on the review's session", g)
		}
		f.e.complete(g, Completion{Error: "reviewer CLI failed (resume_failed): no conversation found", ErrorKind: "resume_failed"})
		again := f.e.authorLease(f.author)
		if again == nil || again.Kind != KindReview || again.JobID == g.JobID {
			t.Fatalf("leased %+v, want the requeued fresh review", again)
		}
		f.wantFreshGrant(again)
	})
}

// TestPreferPlanReviewRetryRunsTheFamilySteps: a plan review's no-verdict
// retry still excludes the first reviewer under prefer, and the family
// steps place it over the remaining agents: the foreman's own family's
// agent takes it, its prompt saying so. With no agent left the plan is
// held, the reason naming the excluded reviewer as the only one.
func TestPreferPlanReviewRetryRunsTheFamilySteps(t *testing.T) {
	t.Run("same family takes the retry", func(t *testing.T) {
		e := newPlanReviewEnv(t)
		e.offerForeman(e.runner1)
		e.offerReviewer()
		sibling := e.agented("sibling", "claude", "review")
		e.createPlan(nTasks(2))
		first := e.lease(e.runner2, nil, nil)
		if first == nil || first.Kind != KindPlanReview || !strings.Contains(first.Prompt, "You were placed cross-family") {
			t.Fatalf("the other family's agent leased %+v, want the plan review stating cross-family", first)
		}
		e.reviewVerdict(first, verdictApprove, "LGTM") // no substantive findings: no verdict
		retry := e.leaseAny(sibling)
		if retry == nil || retry.Kind != KindPlanReview {
			t.Fatalf("the foreman's family's agent leased %+v, want the retry", retry)
		}
		if got := e.int("SELECT COALESCE(exclude_agent, 0) FROM jobs WHERE id = ?", retry.JobID); got == 0 {
			t.Error("the retry excludes no agent, want the first reviewer")
		}
		if got := e.pickPlacement(retry.JobID); got != placeSameFamily {
			t.Errorf("placement = %q, want %q", got, placeSameFamily)
		}
		if !strings.Contains(retry.Prompt, "You were placed same-family") || strings.Contains(retry.Prompt, "You were placed cross-family") {
			t.Errorf("the retry's prompt does not state the same-family placement:\n%s", retry.Prompt)
		}
	})
	t.Run("only the excluded reviewer", func(t *testing.T) {
		e := newPlanReviewEnv(t)
		e.offerForeman(e.runner1)
		e.offerReviewer()
		e.createPlan(nTasks(2))
		e.reviewVerdict(e.lease(e.runner2, nil, nil), verdictApprove, "LGTM")
		prompt := e.str("SELECT prompt FROM jobs WHERE kind = 'foreman' ORDER BY id DESC")
		if !strings.Contains(prompt, "the only available reviewer was the one excluded") {
			t.Errorf("the woken turn's prompt lacks the reason:\n%s", prompt)
		}
	})
}

// TestPreferBranchReviewDefaultReviewerFirst: under prefer a branch of an
// unknown family goes to the default reviewer's harness when it is ready,
// and to any other review-class agent when it is not; require keeps the
// default reviewer alone.
func TestPreferBranchReviewDefaultReviewerFirst(t *testing.T) {
	setup := func(t *testing.T, family string) (*env, int64) {
		e := newEnvCfg(t, func(c *Config) {
			c.Review.Family = family
			c.DefaultReviewer = "preferred"
			c.BranchTrailers = func(context.Context, string, string, string) (string, error) { return "", nil }
		})
		return e, e.agented("other", "google", "review")
	}
	t.Run("ready", func(t *testing.T) {
		e, _ := setup(t, ReviewFamilyPrefer)
		e.agented("preferred", "moonshot", "review")
		res, err := e.reviewBranch("feature/x")
		if err != nil {
			t.Fatalf("review the branch: %v", err)
		}
		if res.Source != reviewSourceDefault || e.str("SELECT harness FROM jobs WHERE id = ?", res.JobID) != "preferred" {
			t.Errorf("result %+v, want the default reviewer's harness", res)
		}
	})
	t.Run("unavailable", func(t *testing.T) {
		e, other := setup(t, ReviewFamilyPrefer)
		res, err := e.reviewBranch("feature/x")
		if err != nil {
			t.Fatalf("review the branch with the default reviewer missing: %v", err)
		}
		if res.Source != "" {
			t.Errorf("source = %q, want none: the default reviewer was not taken", res.Source)
		}
		g := e.leaseAny(other)
		if g == nil || g.JobID != res.JobID {
			t.Fatalf("another review-class agent leased %+v, want the branch review %d", g, res.JobID)
		}
		if got := e.pickPlacement(res.JobID); got != placeUnknown {
			t.Errorf("placement = %q, want %q", got, placeUnknown)
		}
		if !strings.Contains(g.Prompt, "The author's model family is not known") || strings.Contains(g.Prompt, saysCrossFamily) {
			t.Errorf("the prompt does not say the author's family is unknown:\n%s", g.Prompt)
		}
	})
	t.Run("require", func(t *testing.T) {
		e, _ := setup(t, ReviewFamilyRequire)
		if _, err := e.reviewBranch("feature/x"); err == nil || !strings.Contains(err.Error(), "default reviewer's harness") {
			t.Errorf("error = %v, want today's default-reviewer refusal", err)
		}
	})
}
