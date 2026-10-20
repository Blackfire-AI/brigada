package engine

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Blackfire-AI/brigada/internal/prompt"
)

// A re-review (§7) is the review that follows a fix round which answered a
// changes verdict: it resumes the session of the reviewer that wrote the
// verdict, on the same agent, and is shown the fix round alone. Every way
// it cannot be had falls back to the fresh review a first review is.

// pickResumed is the pick the job_agent_picked and task_reviewing events
// record for a re-review: the agent and the session were the previous
// review's, nothing was ranked.
const pickResumed = "resumed"

// priorReview is the review a re-review answers: the task's latest changes
// verdict after its latest work job.
type priorReview struct {
	agent   int64  // the agent the review ran on; 0 for an agent-less runner
	session string // the harness session the review ended on; "" when unknown
	head    string // the branch head the review judged, from its author's result; "" when unknown
}

// storedResult is the shape completeTx keeps a job's result in: the
// worker's or reviewer's block as a string beside the session id.
type storedResult struct {
	Result    string `json:"result"`
	SessionID string `json:"session_id"`
}

// headField is the one field of a worker's result block a re-review reads:
// the head the runner pushed, which it sets itself.
type headField struct {
	Head string `json:"head"`
}

// priorChangesReviewTx finds the review a re-review would answer: the
// newest review of the task that ended done with a changes verdict and
// came after the task's latest work job. A work job — the first attempt,
// a restart, a tier bump, an automatic retry — begins a new attempt, whose
// first review starts cold. Reviews with no verdict, or that never ran,
// are no one's findings and are passed over. Nil when there is none.
func priorChangesReviewTx(tx *sql.Tx, taskID int64) (*priorReview, error) {
	var lastWork int64
	if err := tx.QueryRow("SELECT COALESCE(MAX(id), 0) FROM jobs WHERE task_id = ? AND kind = 'work'", taskID).Scan(&lastWork); err != nil {
		return nil, fmt.Errorf("read the latest work job of task %d: %w", taskID, err)
	}
	type row struct {
		id, agent int64
		result    string
	}
	rows, err := tx.Query(`SELECT id, COALESCE(agent_id, 0), COALESCE(result, '') FROM jobs
		WHERE task_id = ? AND kind = 'review' AND status = ? AND id > ? ORDER BY id DESC`, taskID, statusDone, lastWork)
	if err != nil {
		return nil, fmt.Errorf("list the reviews of task %d: %w", taskID, err)
	}
	var reviews []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.agent, &r.result); err != nil {
			rows.Close()
			return nil, fmt.Errorf("list the reviews of task %d: %w", taskID, err)
		}
		reviews = append(reviews, r)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("list the reviews of task %d: %w", taskID, err)
	}
	for _, r := range reviews {
		var stored storedResult
		var v verdictBlock
		if json.Unmarshal([]byte(r.result), &stored) != nil || json.Unmarshal([]byte(stored.Result), &v) != nil || v.Verdict != verdictChanges {
			continue
		}
		// The head the review judged is the one its author job pushed: the
		// latest work or fix job before it.
		var authored string
		err := tx.QueryRow(`SELECT COALESCE(result, '') FROM jobs
			WHERE task_id = ? AND kind IN ('work', 'fix') AND id < ? ORDER BY id DESC LIMIT 1`, taskID, r.id).Scan(&authored)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("read the author job before review %d: %w", r.id, err)
		}
		var head headField
		var author storedResult
		if json.Unmarshal([]byte(authored), &author) == nil {
			_ = json.Unmarshal([]byte(author.Result), &head)
		}
		return &priorReview{agent: r.agent, session: stored.SessionID, head: head.Head}, nil
	}
	return nil, nil
}

// resumedReview is the placement of a re-review.
type resumedReview struct {
	agent   int64
	harness string
	since   string // the head the previous review judged
}

// resumedReviewTx decides whether the review that follows the author job
// is a re-review (§7). It is when the job is a fix round and a changes
// verdict stands before it, and the reviewer of that verdict can still be
// had: its session id and the head it judged are known, its agent is
// ready or cooling for no more than longCooling, and it still passes the
// review rules against the author of this round — a different family, the
// review class, not the author itself. why says, for the fresh review that
// results when a verdict stood but the reviewer could not be had, what
// stopped the resume; it is empty when nothing could have been resumed.
func (e *Engine) resumedReviewTx(tx *sql.Tx, job *jobRow, excludeFamily string, now time.Time) (rr *resumedReview, why string, err error) {
	if job.Kind != KindFix || job.TaskID == 0 {
		return nil, "", nil
	}
	prior, err := priorChangesReviewTx(tx, job.TaskID)
	if err != nil || prior == nil {
		return nil, "", err
	}
	switch {
	case prior.agent == 0:
		// An agent-less runner, or an agent a re-register has since dropped.
		return nil, "the previous reviewer's agent is gone", nil
	case prior.session == "":
		return nil, "the previous reviewer's session id is unknown", nil
	case prior.head == "":
		return nil, "the head the previous review judged is unknown", nil
	}
	var family, harness, classes, health, cooling string
	err = tx.QueryRow("SELECT family, harness, classes, health, COALESCE(cooling_until, '') FROM agents WHERE id = ?", prior.agent).
		Scan(&family, &harness, &classes, &health, &cooling)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "the previous reviewer's agent is gone", nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("read agent %d: %w", prior.agent, err)
	}
	switch {
	case health != "ready":
		return nil, "the previous reviewer's agent is " + health, nil
	case cooling > fmtTime(now.Add(longCooling)):
		return nil, "the previous reviewer's agent is cooling until " + cooling, nil
	case !reviewAgentQualifies(prior.agent, family, classes, excludeFamily, 0, job.AgentID):
		return nil, "the previous reviewer's agent no longer qualifies for this change", nil
	}
	return &resumedReview{agent: prior.agent, harness: harness, since: prior.head}, "", nil
}

// reReviewPrompt composes the re-review's prompt from the fix round's
// result: its summary, and the check the runner ran on the round.
func reReviewPrompt(wr workerResultFields, checkJSON, base, branch, since string) string {
	return prompt.ComposeReReviewPrompt(wr.Summary, checkJSON, base, branch, since)
}

// freshReviewPromptTx is the prompt of a fresh review of the task as it
// stands: the one a retry or a requeue of a re-review asks, since the
// re-review's own prompt is meant for the session it resumes. It is
// composed as queueReviewTx composes a first review's, from the task's
// latest work or fix job.
func freshReviewPromptTx(tx *sql.Tx, taskID int64) (string, error) {
	var authorPrompt, checkJSON string
	if err := tx.QueryRow(`SELECT prompt, COALESCE(check_result, '') FROM jobs
		WHERE task_id = ? AND kind IN ('work', 'fix') ORDER BY id DESC LIMIT 1`, taskID).Scan(&authorPrompt, &checkJSON); err != nil {
		return "", fmt.Errorf("read the author job of task %d: %w", taskID, err)
	}
	var branch string
	if err := tx.QueryRow("SELECT COALESCE(branch, '') FROM tasks WHERE id = ?", taskID).Scan(&branch); err != nil {
		return "", fmt.Errorf("read the branch of task %d: %w", taskID, err)
	}
	spec, base, err := taskSpecAndBaseTx(tx, taskID)
	if err != nil {
		return "", err
	}
	instructions, err := taskInstructionsTx(tx, taskID)
	if err != nil {
		return "", err
	}
	return prompt.ComposeReviewPrompt(spec, prompt.WorkerFeedback(authorPrompt), checkJSON, base, branch, prompt.WithInstructions(instructions)), nil
}

// retryPromptTx is the prompt a retry or a requeue of the review job
// carries: the job's own, which asks the same question of the next
// reviewer — except for a re-review, whose prompt speaks to a session no
// other agent has, so the next reviewer gets the fresh review instead.
func retryPromptTx(tx *sql.Tx, job *jobRow) (string, error) {
	if job.Mode != ModeSession {
		return job.Prompt, nil
	}
	return freshReviewPromptTx(tx, job.TaskID)
}

// reReviewLeaseTx reads what a re-review's lease needs from the store: the
// session of the review it resumes and the head that review judged. Zero
// values when the task has no such review — the lease then resumes
// nothing.
func reReviewLeaseTx(tx *sql.Tx, j *jobRow) (session, since string, err error) {
	prior, err := priorChangesReviewTx(tx, j.TaskID)
	if err != nil || prior == nil {
		return "", "", err
	}
	return prior.session, prior.head, nil
}
