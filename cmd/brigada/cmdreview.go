package main

import (
	"context"
	"fmt"
	"io"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// runReview is `brigada review --branch X` (§7): a review of a branch
// with no plan, task or foreman behind it, placed by the review.family
// policy — an agent of another family when one is available, and under
// review.family=prefer the author's own family or agent when none is.
// It submits the review job, prints its id and exits; the verdict
// arrives through history, status and notifications like any review's.
func runReview(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("review",
		"brigada review --branch BRANCH [--repo URL] [--no-start]",
		"Review a branch, with an agent from another model family when one is available\n(review.family=prefer, the default; the author's own family or agent otherwise),\nor only from another family with review.family=require in the engine config.")
	stderr = commandStderr(stderr, fs.Name())
	common := addCommon(fs)
	branch := fs.String("branch", "", "the branch to review (required)")
	repo := fs.String("repo", "", "the project's repo URL; empty names the engine's only project")
	noStart := fs.Bool("no-start", false, "do not start the engine on demand; fail when none is running")
	if code, stop := common.parse(stdout, stderr, args); stop {
		return code
	}
	if extra := fs.Args(); len(extra) > 0 || *branch == "" {
		return usagef(stderr, "usage: brigada review --branch BRANCH [--repo URL] [--no-start]")
	}
	rt, code := resolveRuntime(common, stderr)
	if code != 0 {
		return code
	}
	client, code := connect(ctx, rt, "client", *noStart, stderr)
	if code != 0 {
		return code
	}
	return printReview(ctx, client, *repo, *branch, stdout, stderr)
}

// branchReviewer is the client call runReview makes, so the printing is
// testable without an engine.
type branchReviewer interface {
	ReviewBranch(ctx context.Context, repoURL, branch, idemKey string) (engine.ReviewBranchResult, error)
}

// printReview submits the review and prints the job's id.
func printReview(ctx context.Context, client branchReviewer, repo, branch string, stdout, stderr io.Writer) int {
	// A command typed by a person is deliberate each time: no key.
	res, err := client.ReviewBranch(ctx, repo, branch, "")
	if err != nil {
		return fail(stderr, err)
	}
	switch res.Answer {
	case engine.AnsPending:
		fmt.Fprintf(stdout, "review job %d already queued or running for %s\n", res.JobID, branch)
	default:
		fmt.Fprintf(stdout, "review job %d queued for %s\n", res.JobID, branch)
	}
	return 0
}
