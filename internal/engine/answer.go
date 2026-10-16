package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Blackfire-AI/brigada/internal/prompt"
)

// parkingEventKinds are the event kinds that park a task at
// needs_foreman (§3). The latest of them for a parked task is the event
// that parked it: the two kinds that also land without parking — a
// task_check_failed the rules answered with a fix round, a
// review_changes they answered the same way — are never the latest of
// the set for a task that is parked, because nothing runs on a parked
// task to write them, and a review that spends the review rounds parks
// the task under task_out_of_rounds rather than its own changes event.
// Answer reads the set to learn what parked the task it was given.
var parkingEventKinds = []string{
	evTaskNeedsHelp, evTaskFailed, evTaskCheckFailed, evTaskOutOfRounds, evReviewNoVerdict,
}

// AnswerResult is what Answer did: the questions row it filled — the
// worker's question and the foreman's answer together (§11) — and the
// job it queued, the fix job that resumes the author's session.
type AnswerResult struct {
	Answer     Answer
	QuestionID int64
	JobID      int64
	Run        int
}

// RestartResult is what RestartTask did: the fresh work job it queued
// on the task's key.
type RestartResult struct {
	Answer Answer
	JobID  int64
	Run    int
}

// Answer is the foreman's answer (§3, §12.2) for a task parked at
// needs_foreman by its worker's question: the question and the answer
// are kept together in questions, and the job that carries the answer
// resumes the author's session on the author's agent — the placement a
// fix round pins (§3) — its prompt the worker template with the spec
// and the answer verbatim in its own delimiters, introduced as the
// foreman's answer to the question (§1.4, §5.3). The resume is no fix
// round: it spends none of the task's budgets, so even a task out of
// fix rounds can be answered. The task returns to ready for the job's
// lease to take, and the task_answered event names the questions row —
// the question's id and no prompt (§9). Question, row, job, event: one
// transaction, so a caller never sees an answer whose job failed to
// appear.
func (e *Engine) Answer(ctx context.Context, project, taskID int64, text string) (AnswerResult, error) {
	var res AnswerResult
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := e.sweepTx(tx, now); err != nil {
			return err
		}
		r, err := e.answerTx(tx, project, taskID, text, now)
		if err != nil {
			return fmt.Errorf("answer task %d: %w", taskID, err)
		}
		res = r
		return nil
	})
	return res, err
}

func (e *Engine) answerTx(tx *sql.Tx, project, taskID int64, text string, now time.Time) (AnswerResult, error) {
	if text == "" {
		return AnswerResult{}, fmt.Errorf("answer text is required")
	}
	question, err := parkedQuestionTx(tx, project, taskID)
	if err != nil {
		return AnswerResult{}, err
	}
	// The exchange is one row (§11): the worker's question as the parking
	// event kept it, the foreman's answer as it was given, both verbatim,
	// answered the moment the answer acts.
	qrow, err := tx.Exec(`INSERT INTO questions (project_id, task_id, question, answer, answered_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		project, nullIfZero(taskID), question, text, fmtTime(now), fmtTime(now))
	if err != nil {
		return AnswerResult{}, fmt.Errorf("keep the question and its answer: %w", err)
	}
	questionID, err := qrow.LastInsertId()
	if err != nil {
		return AnswerResult{}, fmt.Errorf("keep the question and its answer: last insert id: %w", err)
	}
	spec, err := taskSpecTx(tx, taskID)
	if err != nil {
		return AnswerResult{}, err
	}
	check, err := taskCheckTx(tx, taskID)
	if err != nil {
		return AnswerResult{}, err
	}
	harness, agent, err := authorPlacementTx(tx, taskID)
	if err != nil {
		return AnswerResult{}, err
	}
	key := fmt.Sprintf("task:%d", taskID)
	sr, err := e.submitTx(tx, SubmitParams{
		Key:          key,
		ProjectID:    project,
		TaskID:       taskID,
		Kind:         KindFix,
		Mode:         ModeSession,
		Prompt:       prompt.ComposeAnswerPrompt(spec, check, text),
		Harness:      harness,
		RequireAgent: agent,
		Round:        roundNone,
	}, now)
	if err != nil {
		return AnswerResult{}, err
	}
	if sr.Answer != AnsCreated {
		// A parked task's key is free — the completion that parked it freed
		// the key inside its own transaction — so no live job can hold this
		// submit and no budget applies to roundNone. An event without its
		// job would log an answer that never reached its worker.
		return AnswerResult{}, fmt.Errorf("queue the answer's job: submit %s answered %s", key, sr.Answer)
	}
	if _, err := tx.Exec(`UPDATE tasks SET state = ?, updated_at = ? WHERE id = ?`,
		taskReady, fmtTime(now), taskID); err != nil {
		return AnswerResult{}, fmt.Errorf("return task %d to ready: %w", taskID, err)
	}
	if _, err := appendEventTx(tx, evTaskAnswered, project, taskID, sr.JobID,
		eventPayload{QuestionID: questionID, Run: int64(sr.Run)}, now); err != nil {
		return AnswerResult{}, err
	}
	return AnswerResult{Answer: sr.Answer, QuestionID: questionID, JobID: sr.JobID, Run: sr.Run}, nil
}

// RestartTask is the foreman's task_restart (§3, §5.6, §12.2) for a
// task parked at needs_foreman for any reason: a fresh work job — a new
// attempt, fresh mode, its worktree built anew from the task's base —
// flagged restart, spending one of the task's restart budget. The new
// attempt carries its rounds unspent: the fix-round count is scoped to
// the fix jobs after the task's latest restart (§14), so the restart
// resets it. The foreman's note rides the prompt verbatim as the
// worker's feedback when there is one (§1.4, §5.3) and nothing rides
// when there is not. With the restart budget spent the call refuses and
// the task stays parked. Otherwise the task returns to ready and the
// task_restarted event lands, in the one transaction that queued the
// job.
func (e *Engine) RestartTask(ctx context.Context, project, taskID int64, note string) (RestartResult, error) {
	var res RestartResult
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := e.sweepTx(tx, now); err != nil {
			return err
		}
		r, err := e.restartTaskTx(tx, project, taskID, note, now)
		if err != nil {
			return fmt.Errorf("restart task %d: %w", taskID, err)
		}
		res = r
		return nil
	})
	return res, err
}

func (e *Engine) restartTaskTx(tx *sql.Tx, project, taskID int64, note string, now time.Time) (RestartResult, error) {
	if err := parkedTaskTx(tx, project, taskID); err != nil {
		return RestartResult{}, err
	}
	// The budget first, plainly: a spent restart budget refuses here,
	// with the task untouched, rather than answering capped inside the
	// submit after the event log has said its piece (§12.1's clear
	// refusal, not a capped answer the caller must decode).
	if _, capped, err := e.roundCapReached(tx, SubmitParams{
		ProjectID: project, TaskID: taskID, Kind: KindWork, Restart: true,
	}); err != nil {
		return RestartResult{}, err
	} else if capped {
		return RestartResult{}, fmt.Errorf(
			"the task's restart budget is spent; the task stays parked at needs_foreman")
	}
	r, err := e.queueTaskRestartTx(tx, project, taskID, note, 0, now)
	if err != nil {
		return RestartResult{}, err
	}
	if _, err := appendEventTx(tx, evTaskRestarted, project, taskID, r.JobID,
		eventPayload{Run: int64(r.Run), Reason: boundNote(note)}, now); err != nil {
		return RestartResult{}, err
	}
	return r, nil
}

// queueTaskRestartTx is shared by the foreman's restart and the automatic
// retry (§3, §5.6). The caller checks the budget and records the decision;
// an automatic retry pins the new attempt to a different eligible agent.
func (e *Engine) queueTaskRestartTx(tx *sql.Tx, project, taskID int64, note string, agent int64, now time.Time) (RestartResult, error) {
	spec, err := taskSpecTx(tx, taskID)
	if err != nil {
		return RestartResult{}, err
	}
	check, err := taskCheckTx(tx, taskID)
	if err != nil {
		return RestartResult{}, err
	}
	key := fmt.Sprintf("task:%d", taskID)
	sr, err := e.submitTx(tx, SubmitParams{
		Key:          key,
		ProjectID:    project,
		TaskID:       taskID,
		Kind:         KindWork,
		Mode:         ModeFresh,
		Prompt:       prompt.ComposeWorkerPrompt(spec, check, note),
		Restart:      true,
		RequireAgent: agent,
	}, now)
	if err != nil {
		return RestartResult{}, err
	}
	if sr.Answer != AnsCreated {
		return RestartResult{}, fmt.Errorf("queue the restart's job: submit %s answered %s", key, sr.Answer)
	}
	if _, err := tx.Exec(`UPDATE tasks SET state = ?, updated_at = ? WHERE id = ?`,
		taskReady, fmtTime(now), taskID); err != nil {
		return RestartResult{}, fmt.Errorf("return task %d to ready: %w", taskID, err)
	}
	return RestartResult{Answer: sr.Answer, JobID: sr.JobID, Run: sr.Run}, nil
}

// parkedQuestionTx validates the task the foreman would answer and
// returns the worker's question: the task must belong to project, be
// parked at needs_foreman, and be parked by its worker's question —
// answering a failed check or an out-of-rounds park makes no sense;
// those want RestartTask. Anything else is the caller's mistake and
// refuses with the reason named.
func parkedQuestionTx(tx *sql.Tx, project, taskID int64) (string, error) {
	if err := parkedTaskTx(tx, project, taskID); err != nil {
		return "", err
	}
	var kind, payload string
	kinds := "'" + strings.Join(parkingEventKinds, "', '") + "'"
	err := tx.QueryRow(`SELECT kind, payload FROM events
		WHERE task_id = ? AND kind IN (`+kinds+`)
		ORDER BY id DESC LIMIT 1`, taskID).Scan(&kind, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("task %d is parked with no parking event", taskID)
	}
	if err != nil {
		return "", fmt.Errorf("read what parked task %d: %w", taskID, err)
	}
	if kind != evTaskNeedsHelp {
		return "", fmt.Errorf("task %d is parked by %s, not by a question: answering it makes no sense, restart it instead", taskID, kind)
	}
	var p eventPayload
	if err := json.Unmarshal([]byte(payload), &p); err != nil || p.Question == "" {
		return "", fmt.Errorf("the question of task %d did not survive its event", taskID)
	}
	return p.Question, nil
}

// parkedTaskTx validates the task both foreman tools act on: a task of
// project, parked at needs_foreman. A task of another project, a task
// in any other state, a task that is not there — each refuses with the
// reason named.
func parkedTaskTx(tx *sql.Tx, project, taskID int64) error {
	var owner int64
	var state string
	err := tx.QueryRow(`SELECT p.project_id, t.state
		FROM tasks t JOIN plans p ON p.id = t.plan_id WHERE t.id = ?`, taskID).Scan(&owner, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("no such task %d", taskID)
	}
	if err != nil {
		return fmt.Errorf("read task %d: %w", taskID, err)
	}
	if owner != project {
		return fmt.Errorf("task %d belongs to project %d, not %d", taskID, owner, project)
	}
	if state != taskNeedsForeman {
		return fmt.Errorf("task %d is %s, not parked waiting for the foreman", taskID, state)
	}
	return nil
}
