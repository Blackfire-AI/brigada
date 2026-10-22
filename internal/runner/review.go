package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/Blackfire-AI/brigada/internal/adapter"
	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/prompt"
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
	return engine.Kind(grant.Kind) == engine.KindReview || isPlanReviewJob(grant)
}

// emptyVerdictBlock is what a review completes with when the reviewer's
// final message held no verdict block (§7): a completion must carry
// exactly one of result or error (§12.3), so an absent verdict is
// spelled as an empty JSON object. The engine reads it as no verdict —
// review_no_verdict — never as the run's own failure; what the reviewer
// actually said stays in the transcript.
const emptyVerdictBlock = "{}"

// verdictFields is the shape of the verdict block (§7) as the engine
// decodes it: a verdict and findings, both strings. The runner checks a
// candidate against it the way the engine will — the whole block, one
// JSON object, these field types — so a block the engine would reject
// never beats an earlier one it would accept.
type verdictFields struct {
	Verdict  string `json:"verdict"`
	Findings string `json:"findings"`
}

// verdictOf finds the verdict block in a reviewer's final message (§7):
// the candidates of blockCandidates in its order — fenced blocks by the
// line rules, last first, then a trailing JSON object, then the old
// substring heuristic — and the first that decodes whole as a verdict
// block naming a verdict wins, so findings that quote a fence no longer
// cut the block in two, and an unrelated or malformed block after the
// verdict does not hide it. The block is returned verbatim: the engine
// judges the verdict itself. A message with no such block is the empty
// verdict.
func verdictOf(text string) string {
	for _, block := range blockCandidates(text) {
		var v verdictFields
		if err := json.Unmarshal([]byte(block), &v); err == nil && v.Verdict != "" {
			return block
		}
	}
	return emptyVerdictBlock
}

// finishReview turns the dead reviewer's record into the engine's
// completion (§7): the verdict block of the final message (verdictOf),
// delivered as the result verbatim. The engine judges the verdict
// itself — approve, changes, or nothing usable — so nothing is rewritten
// here, and a message without a block completes with the empty verdict.
// As everywhere, a CLI error the stream or stderr named beats whatever
// the reviewer said, and a command that never started is the runner's
// own failure to report.
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
		r.finishWithCLIError(ctx, fmt.Sprintf("reviewer CLI failed (%s): %s", cliErr.ErrorKind, cliErr.Text), cliErr)
		return
	}
	if nw := (&noWorkerError{}); errors.As(exitErr, &nw) {
		// As a worker's: the reviewer's CLI never started, so the failure
		// is the environment's (issue #39).
		r.finishWithInfraError(ctx, "start the reviewer: "+nw.err.Error())
		return
	}
	comp := api.Completion{Result: verdictOf(ad.ResultText(r.snapshotEvents())), SessionID: r.sessionIDOf()}
	if err := r.saveTranscript(ctx); err != nil {
		r.cfg.Log.WarnContext(ctx, "save transcript", "job_id", r.grant.JobID, "err", err)
	}
	r.deliver(ctx, comp)
}

// reviewDiffMaxBytes bounds the diff file a reviewer reads (§7): past
// it, a reviewer's context fills with the change before it can judge it.
const reviewDiffMaxBytes = 256 << 10

// reviewGitOutputCap bounds what the runner ever holds of one git
// command's output while it builds the diff file. The diff's size is
// the repo content's to choose — a vendored dataset, a generated file —
// and the runner is long-lived, with other jobs in flight, so output
// past the cap is read and dropped, never buffered.
const reviewGitOutputCap = 32 << 20

// writeReviewDiff writes the change under review into the worktree's
// diff file (prompt.ReviewDiffFile), so a reviewer that may only read
// files — no shell, no git — sees the change the template's diff
// command shows. The diff is base...HEAD, the change against the merge
// base, with whole functions as context so the reviewer rarely needs
// the base's copy of a file. A diff past the size bound falls back to
// the plain three lines of context, and past it still, to the stat of
// every file plus as much of the diff as fits, marked as cut. The file
// sits in the worktree untracked; nothing of a review is ever
// committed or pushed, so it never reaches the change.
func (w *workspace) writeReviewDiff(ctx context.Context, base string) error {
	return w.writeReviewDiffSince(ctx, base, "")
}

// writeReviewDiffSince is writeReviewDiff for a re-review (§7) when since
// names the head the reviewer last judged: the file then holds the fix
// round alone, since..HEAD. A since the branch's history does not hold —
// a rewritten branch, a commit the mirror lacks — leaves the whole change
// in the file under a line saying so, never a diff the prompt misdescribes.
func (w *workspace) writeReviewDiffSince(ctx context.Context, base, since string) error {
	rng := base + "...HEAD"
	var note string
	if since != "" {
		if _, err := w.git(ctx, "-C", w.dir, "merge-base", "--is-ancestor", since, "HEAD"); err == nil {
			rng = since + "..HEAD"
		} else {
			note = fmt.Sprintf("# The reviewed head %s is not in the branch's history, so this is the\n"+
				"# whole change (%s), not the fix round alone.\n\n", since, rng)
		}
	}
	diffArgs := []string{"diff", "--no-color", "--no-ext-diff", "--no-textconv"}
	diff, full, err := w.gitStdout(ctx, reviewGitOutputCap, append(diffArgs, "--function-context", rng)...)
	if err != nil {
		return fmt.Errorf("diff the change under review (%s): %w", rng, err)
	}
	if full || len(diff) > reviewDiffMaxBytes {
		if diff, full, err = w.gitStdout(ctx, reviewGitOutputCap, append(diffArgs, rng)...); err != nil {
			return fmt.Errorf("diff the change under review (%s): %w", rng, err)
		}
	}
	if full || len(diff) > reviewDiffMaxBytes {
		stat, statFull, err := w.gitStdout(ctx, reviewDiffMaxBytes, append(diffArgs, "--stat=200", rng)...)
		if err != nil {
			return fmt.Errorf("stat the change under review (%s): %w", rng, err)
		}
		size := fmt.Sprintf("%d bytes", len(diff))
		if full {
			size = fmt.Sprintf("over %d bytes", reviewGitOutputCap)
		}
		cut := diff[:reviewDiffMaxBytes]
		if i := bytes.LastIndexByte(cut, '\n'); i >= 0 {
			cut = cut[:i+1]
		}
		var b bytes.Buffer
		fmt.Fprintf(&b, "# The diff is %s, past the runner's %d-byte bound: the stat\n"+
			"# below lists the files the change touches, and the diff after it\n"+
			"# is cut. Read the rest of the touched files at the branch.\n\n",
			size, reviewDiffMaxBytes)
		b.Write(stat)
		if statFull {
			b.WriteString("\n# [cut: the stat itself is past the bound]\n")
		}
		b.WriteString("\n")
		b.Write(cut)
		b.WriteString("# [cut: the rest of the diff is past the bound]\n")
		diff = b.Bytes()
	}
	if note != "" {
		diff = append([]byte(note), diff...)
	}
	path := filepath.Join(w.dir, prompt.ReviewDiffFile)
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("the change under review holds %s itself; the runner's diff file would replace it", prompt.ReviewDiffFile)
	}
	if err := os.WriteFile(path, diff, 0o644); err != nil {
		return fmt.Errorf("write the review diff: %w", err)
	}
	return nil
}

// gitStdout runs git in the worktree and returns its standard output
// alone — a diff is data, and a warning on stderr must not land in it —
// keeping at most limit bytes of it. full reports that the output ran
// past the limit; the rest was read and dropped, so git never blocks
// on a full pipe and the runner never holds it.
func (w *workspace) gitStdout(ctx context.Context, limit int, args ...string) (out []byte, full bool, err error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", w.dir}, args...)...)
	cmd.Env = append(append(os.Environ(), "GIT_TERMINAL_PROMPT=0"), w.gitEnv...)
	stdout := &cappedBuffer{limit: limit}
	stderr := &cappedBuffer{limit: 4 << 10}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return nil, false, trimCmdError(stderr.buf.Bytes(), err)
	}
	return stdout.buf.Bytes(), stdout.full, nil
}

// cappedBuffer keeps the first limit bytes written to it and drops the
// rest, remembering that it did. A write never fails, so the writer on
// the other end of the pipe runs to its end.
type cappedBuffer struct {
	buf   bytes.Buffer
	limit int
	full  bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := c.limit - c.buf.Len(); room > 0 {
		if len(p) <= room {
			c.buf.Write(p)
			return len(p), nil
		}
		c.buf.Write(p[:room])
	}
	c.full = true
	return len(p), nil
}
