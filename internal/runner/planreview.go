package runner

import (
	"context"
	"fmt"
	"strings"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/prompt"
)

// A plan review job on the runner (issue #50): a review of a plan, not
// of a change. The worktree sits at the current head of the project's
// land target (its default branch when none is set) — fetched fresh — and the reviewer only reads: no
// branch is made, nothing is pushed, no check runs. The plan and the
// summary of the other plans ride in the prompt, which the engine
// composed; what the run ends saying is the verdict block, extracted and
// completed exactly as a code review's is (finishReview).

// isPlanReviewJob reports whether the grant is a plan review job.
func isPlanReviewJob(grant *api.JobGrant) bool {
	return engine.Kind(grant.Kind) == engine.KindPlanReview
}

// preparePlanReviewWorkspace adds the plan reviewer's worktree: a
// detached checkout of the grant's base ref, the project's land target
// (its default branch when none is set), as the remote holds it after a
// fresh fetch of the mirror. A land target the remote does not hold yet
// falls back to the grant's default branch (issue #122); the prompt gains
// a note naming the ref actually read.
func preparePlanReviewWorkspace(ctx context.Context, cfg *Config, grant *api.JobGrant) (*workspace, error) {
	if grant.RepoURL == "" {
		return nil, fmt.Errorf("job %d: no repo URL; a plan review must name a repo to read", grant.JobID)
	}
	if grant.BaseRef == "" {
		return nil, fmt.Errorf("job %d: no land target or default branch to check out for the plan review", grant.JobID)
	}
	ref := grant.BaseRef
	w := &workspace{
		mirror:  mirrorPath(cfg.StateDir, grant.RepoURL),
		dir:     worktreeDir(cfg.StateDir, grant),
		repoURL: grant.RepoURL,
		gitEnv:  cfg.GitEnv,
	}
	if grant.DefaultBranch != "" && ref != grant.DefaultBranch {
		out, err := w.git(ctx, "ls-remote", "--heads", grant.RepoURL, "refs/heads/"+ref)
		if err != nil {
			return nil, &infraError{fmt.Errorf("list the branches of %s: %w", redactURL(grant.RepoURL), trimCmdError(out, err))}
		}
		if strings.TrimSpace(string(out)) == "" {
			ref = grant.DefaultBranch
		}
	}
	w.branch = ref
	unlock, err := lockMirrorHolding(ctx, w.mirror, []string{brigadaRef(ref)}, []string{w.dir})
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := w.materializeMirror(ctx, ref); err != nil {
		return nil, err
	}
	if err := w.fetchBranch(ctx, ref); err != nil {
		return nil, err
	}
	grant.Prompt = prompt.WithPlanReviewRef(grant.Prompt, ref, grant.BaseRef, grant.DefaultBranch)
	w.discardLocked(ctx)
	if out, err := w.git(ctx, "--git-dir", w.mirror, "worktree", "add", "--detach", w.dir, w.branch); err != nil {
		w.discardLocked(ctx)
		return nil, fmt.Errorf("add the plan review worktree at %s: %w", w.branch, trimCmdError(out, err))
	}
	holdWorktreeDir(w.dir)
	w.holdsDir = true
	out, err := w.git(ctx, "-C", w.dir, "rev-parse", "HEAD")
	if err != nil {
		w.discardLocked(ctx)
		return nil, fmt.Errorf("read the plan review worktree's commit: %w", trimCmdError(out, err))
	}
	w.base = strings.TrimSpace(string(out))
	return w, nil
}
