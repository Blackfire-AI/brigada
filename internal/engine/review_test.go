package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const reviewRepo = "https://example.invalid/brigada.git"

// authored leases a work job of the fixture's task to the author's
// runner, so the job record holds the author's agent and the branch the
// engine named; it returns that branch. The job is left running: its
// completion would queue the task's own review, which these tests are
// not about.
func (e *env) authored(author int64) string {
	e.t.Helper()
	e.submit(e.work())
	g := e.leaseAny(author)
	if g == nil {
		e.t.Fatal("the work job was not leased to the author")
	}
	return branchOf(e.task)
}

func (e *env) reviewBranch(branch string) (ReviewBranchResult, error) {
	e.t.Helper()
	return e.eng.ReviewBranch(e.ctx, reviewRepo, branch)
}

// TestReviewBranchTakesTheAuthorFamilyFromTheLastWorkJob: the review of
// a branch excludes the family of the agent of the branch's last work
// job; a same-family reviewer never sees the job, another family's gets
// a task-less grant that reads the branch against the project's default
// branch, and its substantive approve lands on the outcome record and in
// the history with the branch.
func TestReviewBranchTakesTheAuthorFamilyFromTheLastWorkJob(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	sibling := e.agented("sibling", "anthropic", "review")
	reviewer := e.agented("reviewer", "openai", "review")
	branch := e.authored(author)

	res, err := e.reviewBranch(branch)
	if err != nil {
		t.Fatalf("review the branch: %v", err)
	}
	if res.Answer != AnsCreated || res.JobID == 0 || res.Family != "anthropic" || res.Source != reviewSourceJob {
		t.Fatalf("result = %+v, want a created job excluding anthropic from the job record", res)
	}
	if got := e.str("SELECT exclude_family FROM jobs WHERE id = ?", res.JobID); got != "anthropic" {
		t.Errorf("exclude_family = %q, want anthropic", got)
	}
	if got := e.str("SELECT COALESCE(branch, '') FROM jobs WHERE id = ?", res.JobID); got != branch {
		t.Errorf("job branch = %q, want %q", got, branch)
	}
	if e.int("SELECT COUNT(*) FROM jobs WHERE id = ? AND task_id IS NULL AND kind = 'review'", res.JobID) != 1 {
		t.Error("the review job should be a review with no task")
	}
	if g := e.leaseAny(sibling); g != nil {
		t.Fatalf("a same-family agent leased the review: %+v", g)
	}
	g := e.leaseAny(reviewer)
	if g == nil || g.JobID != res.JobID {
		t.Fatalf("the other family's agent did not lease the review: %+v", g)
	}
	if g.Kind != KindReview || g.TaskID != 0 || g.ReviewBranch != branch || g.BaseRef != "main" ||
		g.RepoURL != reviewRepo || g.Branch != "" || g.CheckCmd != "" {
		t.Errorf("grant = %+v, want a task-less review of %s against main", g, branch)
	}
	if !strings.Contains(g.Prompt, "git diff main..."+branch) {
		t.Errorf("prompt does not carry the diff command: %q", g.Prompt)
	}

	e.complete(g, Completion{Result: `{"verdict":"approve","findings":"` + substantiveFindings + `"}`})
	if attempts, successes := e.outcomeOf(e.agentOf(reviewer), "review"); attempts != 1 || successes != 1 {
		t.Errorf("reviewer outcome = %d/%d, want 1/1", attempts, successes)
	}
	p, ok := e.eventPayloadOf(evReviewVerdict)
	if !ok || p.Status != verdictApprove || p.Branch != branch {
		t.Errorf("review_verdict event = %+v (found %v), want approve on %s", p, ok, branch)
	}
	if p, ok := e.eventPayloadOf(evJobCreated); !ok || p.Branch != branch {
		t.Errorf("job_created event = %+v, want the review's branch %s", p, branch)
	}
	// The task is untouched: a standalone review moves no task.
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state == taskDone {
		t.Errorf("the task moved to %s", state)
	}
}

// TestReviewBranchRepeatCoalesces: a second review of the branch while
// the first is live answers pending with the live job's id.
func TestReviewBranchRepeatCoalesces(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	e.agented("reviewer", "openai", "review")
	branch := e.authored(author)
	first, err := e.reviewBranch(branch)
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	second, err := e.reviewBranch(branch)
	if err != nil {
		t.Fatalf("review again: %v", err)
	}
	if second.Answer != AnsPending || second.JobID != first.JobID {
		t.Errorf("second = %+v, want pending on job %d", second, first.JobID)
	}
}

// TestReviewBranchWithoutWorkJobReadsTheTrailer: a branch no work job
// made takes its author's family from the Co-Authored-By trailers of
// its commits, matched against the registered agents.
func TestReviewBranchWithoutWorkJobReadsTheTrailer(t *testing.T) {
	var asked struct{ repo, base, branch string }
	e := newEnvCfg(t, func(c *Config) {
		c.BranchTrailers = func(_ context.Context, repo, base, branch string) (string, error) {
			asked.repo, asked.base, asked.branch = repo, base, branch
			return "Claude Sonnet 5.5 <noreply@anthropic.com>\n", nil
		}
	})
	same := e.agented("same", "claude", "review")
	other := e.agented("other", "openai", "review")

	res, err := e.reviewBranch("feature/x")
	if err != nil {
		t.Fatalf("review the branch: %v", err)
	}
	if res.Family != "claude" || res.Source != reviewSourceTrailer {
		t.Errorf("result = %+v, want family claude from the trailer", res)
	}
	if asked.repo != reviewRepo || asked.base != "main" || asked.branch != "feature/x" {
		t.Errorf("trailers asked for %+v", asked)
	}
	if g := e.leaseAny(same); g != nil {
		t.Errorf("the trailer's family leased the review: %+v", g)
	}
	if g := e.leaseAny(other); g == nil || g.ReviewBranch != "feature/x" {
		t.Errorf("the other family did not lease the review: %+v", g)
	}
}

// TestReviewBranchFallsBackToTheDefaultReviewer: with no work job and no
// trailer, and again when the work job's agent has no known family, the
// config's default reviewer takes the review with no family rule.
func TestReviewBranchFallsBackToTheDefaultReviewer(t *testing.T) {
	e := newEnvCfg(t, func(c *Config) {
		c.DefaultReviewer = "preferred"
		c.BranchTrailers = func(context.Context, string, string, string) (string, error) { return "", nil }
	})
	author := e.agented("author", "anthropic", "implement")
	e.agented("other", "openai", "review")
	preferred := e.agented("preferred", "google", "review")

	res, err := e.reviewBranch("feature/y")
	if err != nil {
		t.Fatalf("review a branch without a work job: %v", err)
	}
	if res.Source != reviewSourceDefault || res.Family != "" {
		t.Errorf("result = %+v, want the default reviewer", res)
	}
	if got := e.str("SELECT harness FROM jobs WHERE id = ?", res.JobID); got != "preferred" {
		t.Errorf("job harness = %q, want preferred", got)
	}
	if g := e.lease(preferred, []string{"preferred"}, nil); g == nil || g.JobID != res.JobID {
		t.Errorf("the default reviewer did not lease the review: %+v", g)
	}

	// A work job whose agent's family is unknown follows the same path.
	branch := e.authored(author)
	e.exec("UPDATE agents SET family = '' WHERE id = ?", e.agentOf(author))
	res, err = e.reviewBranch(branch)
	if err != nil {
		t.Fatalf("review a branch of an unknown family: %v", err)
	}
	if res.Source != reviewSourceDefault || res.Family != "" {
		t.Errorf("result = %+v, want the default reviewer for an unknown family", res)
	}
}

// TestReviewBranchWithoutEligibleReviewerErrors: no agent of another
// family that is ready and offers the review class is a plain error, and
// so is an unknown family with no default reviewer.
func TestReviewBranchWithoutEligibleReviewerErrors(t *testing.T) {
	e := newEnvCfg(t, func(c *Config) {
		c.BranchTrailers = func(context.Context, string, string, string) (string, error) { return "", nil }
	})
	author := e.agented("author", "anthropic", "implement", "review")
	other := e.agented("other", "openai", "implement")
	branch := e.authored(author)

	_, err := e.reviewBranch(branch)
	// The author is the only review provider, so the refusal now names
	// self-review rather than the less specific family rule.
	if !errors.Is(err, ErrNoReviewer) || !strings.Contains(err.Error(), wantAuthorOnlyReviewer) {
		t.Fatalf("error = %v, want the author-only explanation", err)
	}
	// A broken agent of another family that offers review is no help.
	e.exec("UPDATE agents SET classes = '[\"review\"]', health = 'broken' WHERE id = ?", e.agentOf(other))
	if _, err := e.reviewBranch(branch); !errors.Is(err, ErrNoReviewer) {
		t.Fatalf("error = %v, want no eligible reviewer for a broken agent", err)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE kind = 'review'"); n != 0 {
		t.Errorf("%d review jobs queued by refused reviews", n)
	}

	_, err = e.reviewBranch("feature/unknown")
	if !errors.Is(err, ErrNoReviewer) || !strings.Contains(err.Error(), "default_reviewer") {
		t.Fatalf("error = %v, want no family and no default reviewer", err)
	}
	if _, err := e.reviewBranch(" "); !errors.Is(err, ErrInvalidReview) {
		t.Errorf("blank branch error = %v, want invalid review", err)
	}
}

// A standalone review has no task snapshot, so it uses the current role
// texts rather than the legacy project setting or an earlier task's text.
func TestReviewBranchUsesCurrentRoleInstructions(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	e.agented("reviewer", "openai", "review")
	e.setRole(RoleWorker, "old worker")
	branch := e.authored(author)
	e.set(map[string]string{SettingInstructions: "legacy setting"})
	e.setRole(RoleWorker, "current worker")
	e.setRole(RoleReviewer, "current reviewer")
	res, err := e.reviewBranch(branch)
	if err != nil {
		t.Fatal(err)
	}
	p := e.jobPrompt(res.JobID)
	for _, want := range []string{"<worker-instructions>\ncurrent worker\n</worker-instructions>", "<reviewer-instructions>\ncurrent reviewer\n</reviewer-instructions>"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q: %s", want, p)
		}
	}
	if strings.Contains(p, "legacy setting") || strings.Contains(p, "old worker") {
		t.Errorf("prompt contains stale guidance: %s", p)
	}
	var meta promptMeta
	if err := json.Unmarshal([]byte(e.str("SELECT prompt_meta FROM jobs WHERE id = ?", res.JobID)), &meta); err != nil {
		t.Fatal(err)
	}
	if meta.Instructions != instructionsDigest("current worker") || meta.Reviewer != instructionsDigest("current reviewer") {
		t.Errorf("prompt metadata = %+v", meta)
	}
}
