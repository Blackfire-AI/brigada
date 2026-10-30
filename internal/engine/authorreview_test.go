package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

const wantAuthorOnlyReviewer = "no reviewer: the only agent offering review is the author; offer another agent to the runner (--agent harness=…) or review by hand"

func TestReviewNeverLeasesToAuthor(t *testing.T) {
	for _, tt := range []struct {
		name, authorFamily, reviewerFamily string
		clearFamily                        bool
	}{
		{"unknown author and unknown reviewer", "", "", false},
		{"unknown author and known reviewer", "", "google", false},
		{"known author and other family", "anthropic", "google", false},
		// A missing family exclusion must never reopen self-review, even
		// when the author's own family is still known.
		{"known author with empty exclusion", "anthropic", "anthropic", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// review.family=require keeps today's rule (issue #126): the
			// author never reviews its own work, whatever the family.
			e := newEnvCfg(t, func(c *Config) { c.Review.Family = ReviewFamilyRequire })
			author := e.agented("author", tt.authorFamily, "implement", "review")
			review := e.runToReview(author)
			if tt.clearFamily {
				e.exec("UPDATE jobs SET exclude_family = '' WHERE id = ?", review)
			}
			if g := e.leaseAny(author); g != nil {
				t.Fatalf("the author leased its own review: %+v", g)
			}
			if g := e.leaseAny(e.runner1); g != nil {
				t.Fatalf("an agent-less runner bypassed review placement: %+v", g)
			}
			reviewer := e.agented("reviewer", tt.reviewerFamily, "review")
			if g := e.leaseAny(reviewer); g == nil || g.JobID != review {
				t.Fatalf("another agent leased %+v, want review %d", g, review)
			}
		})
	}
}

func TestReviewRetryNeverLeasesToAuthorOrFirstReviewer(t *testing.T) {
	for _, tt := range []struct {
		name, authorFamily, secondFamily string
		clearFamily                      bool
	}{
		{"unknown author and unknown reviewer", "", "", false},
		{"unknown author and known reviewer", "", "google", false},
		{"known author", "anthropic", "google", false},
		{"known author with empty exclusion", "anthropic", "anthropic", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			author := e.agented("author", tt.authorFamily, "implement", "review")
			first := e.agented("first", "google", "review")
			second := e.agented("second", tt.secondFamily, "review")
			review := e.runToReview(author)
			if tt.clearFamily {
				e.exec("UPDATE jobs SET exclude_family = '' WHERE id = ?", review)
			}
			e.runReview(first, `{}`)
			retry := e.int("SELECT id FROM jobs WHERE task_id = ? AND kind = 'review' ORDER BY id DESC LIMIT 1", e.task)
			if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'review'", e.task); n != 2 {
				t.Fatalf("review jobs = %d, want the first and its retry", n)
			}
			if id := e.int("SELECT exclude_agent FROM jobs WHERE id = ?", retry); id != e.agentOf(first) {
				t.Fatalf("retry excludes agent %d, want first reviewer", id)
			}
			if got, want := e.str("SELECT exclude_family FROM jobs WHERE id = ?", retry), e.str("SELECT exclude_family FROM jobs WHERE id = ?", review); got != want {
				t.Fatalf("retry excludes family %q, want %q", got, want)
			}
			for _, runner := range []int64{author, first} {
				if g := e.leaseAny(runner); g != nil {
					t.Fatalf("author or first reviewer leased retry: %+v", g)
				}
			}
			g := e.leaseAny(second)
			if g == nil || g.JobID != retry {
				t.Fatalf("second reviewer leased %+v, want retry %d", g, retry)
			}
			e.complete(g, Completion{Result: `{}`})
			if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskNeedsForeman {
				t.Fatalf("state = %s, want needs_foreman after the retry", state)
			}
		})
	}
}

func TestReviewRetryDoesNotCountUnknownFamilyAuthorAsReviewer(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "", "implement", "review")
	first := e.agented("first", "", "review")
	e.runToReview(author)
	e.runReview(first, `{}`)
	// The author, though it offers review and shares the unknown
	// family, is never the retry's other reviewer: with none, the
	// exclusion lifts and the first reviewer takes the retry instead.
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskReviewing {
		t.Fatalf("state = %s, want reviewing behind the lifted retry", state)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'review'", e.task); n != 2 {
		t.Fatalf("review jobs = %d, want the first and its lifted retry", n)
	}
	if g := e.leaseAny(author); g != nil {
		t.Fatalf("the author leased the retry: %+v", g)
	}
	e.runReview(first, `{}`)
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskNeedsForeman {
		t.Fatalf("state = %s, want needs_foreman after the lifted retry", state)
	}
	if p, ok := e.eventPayloadOf(evReviewNoVerdict); !ok || !strings.Contains(p.Reason, "the only available reviewer twice gave no verdict") {
		t.Fatalf("no-verdict event = %+v (%v), want the twice-no-verdict cause", p, ok)
	}
}

func TestReviewExcludesLatestFixAuthor(t *testing.T) {
	e := newEnv(t)
	original := e.agented("original", "", "implement", "review")
	reviewer := e.agented("reviewer", "", "review")
	fixer := e.agented("fixer", "", "implement", "review")
	e.runToReview(original)
	e.runReview(reviewer, `{"verdict":"changes","findings":"fix this"}`)
	// Model a fix handed to another agent instead of resuming the original
	// author's session: the latest fix, rather than the initial work job,
	// must determine whose work the next review judges.
	e.exec("UPDATE jobs SET require_agent = NULL, harness = '', mode = 'fresh' WHERE task_id = ? AND kind = 'fix'", e.task)
	g := e.leaseAny(fixer)
	if g == nil || g.Kind != KindFix {
		t.Fatalf("fixer leased %+v, want the fix job", g)
	}
	e.complete(g, doneOn("ignored", passCheck))
	if g := e.leaseAny(fixer); g != nil {
		t.Fatalf("the fix author leased its own review: %+v", g)
	}
	if g := e.leaseAny(original); g == nil || g.Kind != KindReview {
		t.Fatalf("the original author leased %+v, want a review of the other agent's fix", g)
	}
}

func TestAuthorOnlyReviewIsUnservable(t *testing.T) {
	e := newEnvCfg(t, func(c *Config) { c.QueueTTL = time.Minute })
	author := e.agented("author", "", "implement", "review")
	review := e.runToReview(author)
	// Cooling must not make the author's own review look temporarily held.
	e.exec("UPDATE agents SET cooling_until = ? WHERE id = ?", fmtTime(e.clock.Now().Add(time.Hour)), e.agentOf(author))
	if item, ok := e.needsYouTask(e.task); !ok || item.Reason != wantAuthorOnlyReviewer {
		t.Fatalf("needs-you item = %+v (%v), want the author-only explanation", item, ok)
	}
	if p, ok := e.eventPayloadOf(evReviewUnservable); !ok || p.Reason != wantAuthorOnlyReviewer {
		t.Fatalf("review_unservable event = %+v (%v), want the author-only explanation", p, ok)
	}
	reviewer := e.agented("reviewer", "", "review")
	// A different agent of unknown family is structurally sufficient,
	// even if it is temporarily unavailable.
	for _, health := range []string{"ready", "broken"} {
		e.exec("UPDATE agents SET health = ?, cooling_until = ? WHERE runner_id = ?", health, fmtTime(e.clock.Now().Add(time.Hour)), reviewer)
		if item, ok := e.needsYouTask(e.task); ok {
			t.Fatalf("needs-you item remains with another %s reviewer: %+v", health, item)
		}
	}
	e.exec("UPDATE agents SET classes = '[]' WHERE runner_id = ?", reviewer)
	e.clock.Advance(2 * time.Minute)
	rep := e.mustSweep()
	if len(rep.QueueTimedOut) != 1 || rep.QueueTimedOut[0] != review {
		t.Fatalf("queue timeouts = %v, want review %d", rep.QueueTimedOut, review)
	}
	if p, ok := e.eventPayloadOf(evTaskFailed); !ok || p.Reason != wantAuthorOnlyReviewer {
		t.Fatalf("task_failed event = %+v (%v), want the author-only explanation", p, ok)
	}
}

func TestBranchReviewExcludesUnknownFamilyAuthor(t *testing.T) {
	e := newEnvCfg(t, func(c *Config) {
		c.DefaultReviewer = "author"
		c.BranchTrailers = func(context.Context, string, string, string) (string, error) { return "", nil }
	})
	author := e.agented("author", "", "implement", "review")
	branch := e.authored(author)
	e.eng.cfg.DefaultReviewer = ""
	if _, err := e.reviewBranch(branch); !errors.Is(err, ErrNoReviewer) || !strings.Contains(err.Error(), wantAuthorOnlyReviewer) {
		t.Fatalf("branch review without default error = %v, want the author-only explanation", err)
	}
	e.eng.cfg.DefaultReviewer = "author"
	if _, err := e.reviewBranch(branch); !errors.Is(err, ErrNoReviewer) || !strings.Contains(err.Error(), wantAuthorOnlyReviewer) {
		t.Fatalf("branch review error = %v, want the author-only explanation", err)
	}
	// Another runner may offer the same default harness and unknown family;
	// it may review this branch even though the author may not.
	other := e.agented("other", "", "review")
	e.exec("UPDATE agents SET harness = 'author' WHERE runner_id = ?", other)
	res, err := e.reviewBranch(branch)
	if err != nil {
		t.Fatalf("review the branch with another agent: %v", err)
	}
	if g := e.lease(author, []string{"author"}, nil); g != nil {
		t.Fatalf("the branch author leased its own review: %+v", g)
	}
	if g := e.lease(other, []string{"author"}, nil); g == nil || g.JobID != res.JobID {
		t.Fatalf("another agent leased %+v, want branch review %d", g, res.JobID)
	}
}

func TestQueuedBranchReviewOnlyAuthorIsUnservable(t *testing.T) {
	e := newEnvCfg(t, func(c *Config) { c.QueueTTL = time.Minute })
	author := e.agented("author", "", "implement", "review")
	branch := e.authored(author)
	// A queued branch review must remain protected even when submitted
	// directly, without ReviewBranch's admission check or a family rule.
	params := e.work()
	params.Key, params.TaskID, params.Kind, params.Branch = "branch-review", 0, KindReview, branch
	review := e.submit(params)
	if g := e.leaseAny(author); g != nil {
		t.Fatalf("the branch author leased its own review: %+v", g)
	}
	e.clock.Advance(2 * time.Minute)
	rep := e.mustSweep()
	if len(rep.QueueTimedOut) != 1 || rep.QueueTimedOut[0] != review.JobID {
		t.Fatalf("queue timeouts = %v, want branch review %d", rep.QueueTimedOut, review.JobID)
	}
	if result := e.str("SELECT result FROM jobs WHERE id = ?", review.JobID); !strings.Contains(result, wantAuthorOnlyReviewer) {
		t.Fatalf("branch review result = %s, want the author-only explanation", result)
	}
}
