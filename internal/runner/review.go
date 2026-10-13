package runner

import (
	"context"
	"errors"
	"fmt"

	"github.com/Blackfire-AI/brigada/internal/adapter"
	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
)

// A review job on the runner (§7): the worktree sits at the task's
// result branch, fetched from the remote, and the reviewer only reads —
// no branch is made, nothing is pushed, no check runs (the task's check
// already spoke, at the work round this review judges). What the run
// ends saying is the verdict block, and the completion carries it
// verbatim: the worker result-block rule (§5.4) binds the work kinds,
// not the review — a review's block is the verdict block of §7.

// isReviewJob reports whether the grant is a review job (§7): the one
// worker kind whose run reads a change instead of making one.
func isReviewJob(grant *api.JobGrant) bool {
	return engine.Kind(grant.Kind) == engine.KindReview
}

// emptyVerdictBlock is what a review completes with when the reviewer's
// final message held no fenced JSON block (§7): a completion must carry
// exactly one of result or error (§12.3), so an absent verdict is
// spelled as an empty JSON object. The engine reads it as no verdict —
// review_no_verdict — never as the run's own failure; what the reviewer
// actually said stays in the transcript.
const emptyVerdictBlock = "{}"

// finishReview turns the dead reviewer's record into the engine's
// completion (§7): the last fenced JSON block of the final message,
// delivered as the result verbatim. The engine judges the verdict
// itself — approve, changes, or nothing usable — so nothing is decoded
// or rewritten here, and a message without a block completes with the
// empty verdict. As everywhere, a CLI error the stream or stderr named
// beats whatever the reviewer said, and a command that never started is
// the runner's own failure to report.
func (r *jobRun) finishReview(ctx context.Context, ad adapter.Adapter, exitErr error) {
	var cliErr *adapter.Event
	r.mu.Lock()
	switch {
	case r.cliError != nil:
		e := *r.cliError
		cliErr = &e
	case r.stderrError != nil && exitErr != nil:
		e := *r.stderrError
		cliErr = &e
	}
	r.mu.Unlock()
	if cliErr != nil {
		r.finishWithError(ctx, fmt.Sprintf("reviewer CLI failed (%s): %s", cliErr.ErrorKind, cliErr.Text))
		return
	}
	if nw := (&noWorkerError{}); errors.As(exitErr, &nw) {
		r.finishWithError(ctx, "start the reviewer: "+nw.err.Error())
		return
	}
	verdict := emptyVerdictBlock
	if block, ok := lastFencedJSON(ad.ResultText(r.snapshotEvents())); ok {
		verdict = block
	}
	comp := api.Completion{Result: verdict, SessionID: r.sessionIDOf()}
	if err := r.saveTranscript(ctx); err != nil {
		r.cfg.Log.WarnContext(ctx, "save transcript", "job_id", r.grant.JobID, "err", err)
	}
	r.deliver(ctx, comp)
}
