package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// The stop-running restart (§5.6): task_restart with stop_running on a
// task whose job is still live. A job no runner holds — queued and never
// leased, or released — is cancelled and the fresh attempt queued at
// once, as a plain restart replaces a live job. A job a runner holds is
// not cancelled: the restart is kept pending on the task, in
// tasks.stop_pending (its note, '' for none), the heartbeat answers
// AnsStopRunning, and the runner stops the worker, pushes its branch and
// completes the job stalled. That completion — or any other terminal end
// of the job — consumes the pending restart: the fresh attempt is queued
// the moment the old job settles, never beside it, so the key never
// holds two live jobs. The restart budget is checked at the request and
// the restart counted once, by the job the consumption queues.

// RestartTaskStopRunning is task_restart with stop_running (§5.6): a task
// with no live job is restarted as RestartTask restarts it, and must be
// parked at needs_foreman for it; a task whose live work, fix or review
// job no runner holds has it cancelled and the fresh attempt queued now;
// a task whose job a runner holds keeps running, its dependents
// untouched, with the restart pending until the job stops. The pending
// case answers AnsPending with JobID naming the stopping job and records
// task_stop_requested; the others answer as RestartTask does. A second
// restart while one is pending, and a spent restart budget, refuse.
func (e *Engine) RestartTaskStopRunning(ctx context.Context, project, taskID int64, note string) (RestartResult, error) {
	var res RestartResult
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := e.sweepTx(tx, now); err != nil {
			return err
		}
		r, err := e.restartStopRunningTx(tx, project, taskID, note, now)
		if err != nil {
			return fmt.Errorf("restart task %d: %w", taskID, err)
		}
		res = r
		return nil
	})
	return res, err
}

func (e *Engine) restartStopRunningTx(tx *sql.Tx, project, taskID int64, note string, now time.Time) (RestartResult, error) {
	if err := ownedTaskTx(tx, project, taskID); err != nil {
		return RestartResult{}, err
	}
	if err := refuseStopPendingTx(tx, taskID); err != nil {
		return RestartResult{}, err
	}
	live, err := liveJobTx(tx, fmt.Sprintf("task:%d", taskID))
	if err != nil {
		return RestartResult{}, err
	}
	if live == nil {
		// Nothing to stop: the plain restart, parked task and all.
		return e.restartTaskTx(tx, project, taskID, note, "", now)
	}
	if live.Kind != KindWork && live.Kind != KindFix && live.Kind != KindReview {
		return RestartResult{}, fmt.Errorf("task %d's live job %d is a %s job; stop_running stops a work, fix or review job", taskID, live.ID, live.Kind)
	}
	if _, capped, err := e.roundCapReached(tx, SubmitParams{
		ProjectID: project, TaskID: taskID, Kind: KindWork, Restart: true,
	}); err != nil {
		return RestartResult{}, err
	} else if capped {
		return RestartResult{}, fmt.Errorf(
			"the task's restart budget is spent; job %d keeps running", live.ID)
	}
	if live.Status == statusQueued && live.LeaseID == "" {
		// No runner holds the job — never leased, or released: the plain
		// restart's submit cancels it and queues the fresh attempt now.
		r, err := e.queueTaskRestartTx(tx, project, taskID, note, 0, false, time.Time{}, now)
		if err != nil {
			return RestartResult{}, err
		}
		if _, err := appendEventTx(tx, evTaskRestarted, project, taskID, r.JobID,
			eventPayload{Run: int64(r.Run), Reason: boundNote(note)}, now); err != nil {
			return RestartResult{}, err
		}
		return r, nil
	}
	// A runner holds the job (a lapsed lease included: its runner may
	// still revive it, and the next lease's heartbeat answers the same).
	if _, err := tx.Exec(`UPDATE tasks SET stop_pending = ?, updated_at = ? WHERE id = ?`,
		note, fmtTime(now), taskID); err != nil {
		return RestartResult{}, fmt.Errorf("keep the pending restart of task %d: %w", taskID, err)
	}
	if _, err := appendEventTx(tx, evTaskStopRequested, project, taskID, live.ID,
		eventPayload{Job: live.ID, Reason: boundNote(note)}, now); err != nil {
		return RestartResult{}, err
	}
	return RestartResult{Answer: AnsPending, JobID: live.ID}, nil
}

// ownedTaskTx validates a task the stop-running restart acts on: a task
// of project, in an active project. Its state is the live job's to judge.
func ownedTaskTx(tx *sql.Tx, project, taskID int64) error {
	var owner int64
	err := tx.QueryRow(`SELECT p.project_id FROM tasks t JOIN plans p ON p.id = t.plan_id WHERE t.id = ?`, taskID).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("no such task %d", taskID)
	}
	if err != nil {
		return fmt.Errorf("read task %d: %w", taskID, err)
	}
	if owner != project {
		return fmt.Errorf("task %d belongs to project %d, not %d", taskID, owner, project)
	}
	return activeProjectTx(tx, owner)
}

// stopPendingTx reads the task's pending stop-running restart: its note
// and whether one is pending at all.
func stopPendingTx(tx *sql.Tx, taskID int64) (string, bool, error) {
	var note sql.NullString
	if err := tx.QueryRow("SELECT stop_pending FROM tasks WHERE id = ?", taskID).Scan(&note); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("read the pending restart of task %d: %w", taskID, err)
	}
	return note.String, note.Valid, nil
}

// refuseStopPendingTx refuses any restart of a task that already has a
// stop-running restart pending: the stopping job's end queues it.
func refuseStopPendingTx(tx *sql.Tx, taskID int64) error {
	_, pending, err := stopPendingTx(tx, taskID)
	if err != nil {
		return err
	}
	if pending {
		return fmt.Errorf("task %d already has a stop-running restart pending: its fresh attempt queues once the running job stops", taskID)
	}
	return nil
}

// clearStopPendingTx drops the task's pending stop-running restart: a
// cancel of its job, of the task, or its retirement leaves nothing to
// restart.
func clearStopPendingTx(tx *sql.Tx, taskID int64, now time.Time) error {
	if _, err := tx.Exec(`UPDATE tasks SET stop_pending = NULL, updated_at = ? WHERE id = ? AND stop_pending IS NOT NULL`,
		fmtTime(now), taskID); err != nil {
		return fmt.Errorf("clear the pending restart of task %d: %w", taskID, err)
	}
	return nil
}

// stopRunningTx reports whether a heartbeat on job answers AnsStopRunning:
// a work, fix or review job on its task's key whose task carries a
// pending stop-running restart.
func stopRunningTx(tx *sql.Tx, job *jobRow) (bool, error) {
	if job.TaskID == 0 || job.Key != fmt.Sprintf("task:%d", job.TaskID) {
		return false, nil
	}
	if job.Kind != KindWork && job.Kind != KindFix && job.Kind != KindReview {
		return false, nil
	}
	_, pending, err := stopPendingTx(tx, job.TaskID)
	return pending, err
}

// stopSeed is the branch a consumed restart seeds its fresh attempt from:
// the branch the work or fix run's result block names — what the stopped
// run pushed — and nothing for a review, which pushes nothing, or a
// completion with no block.
func stopSeed(job *jobRow, c Completion) string {
	if job.Kind != KindWork && job.Kind != KindFix {
		return ""
	}
	wr, ok := parseWorkerResult(c.Result)
	if !ok {
		return ""
	}
	return wr.Branch
}

// consumeStopPendingTx consumes the task's pending stop-running restart
// when its job has ended (§5.6): the job that ended is already settled —
// the key holds no live job — so the restart's submit replaces nothing.
// The fresh attempt is queued exactly as a restart's (the landing
// forgotten, the base rebuilt) with the persisted note, seeded from seed
// when non-empty; the task goes ready, task_restarted records it, and
// nothing parks or wakes the foreman. It reports false, leaving the
// ending to its usual path, when nothing is pending, the key is still
// live, or the restart budget was spent after all (the stop's request
// checked it; a refusal here would fail the completion itself).
func (e *Engine) consumeStopPendingTx(tx *sql.Tx, project, taskID int64, seed string, now time.Time) (bool, error) {
	if taskID == 0 {
		return false, nil
	}
	note, pending, err := stopPendingTx(tx, taskID)
	if err != nil || !pending {
		return false, err
	}
	live, err := liveJobTx(tx, fmt.Sprintf("task:%d", taskID))
	if err != nil || live != nil {
		return false, err
	}
	if err := clearStopPendingTx(tx, taskID, now); err != nil {
		return false, err
	}
	if _, capped, err := e.roundCapReached(tx, SubmitParams{
		ProjectID: project, TaskID: taskID, Kind: KindWork, Restart: true,
	}); err != nil || capped {
		return false, err
	}
	r, err := e.queueSeededRestartTx(tx, project, taskID, note, 0, false, time.Time{}, seed, now)
	if err != nil {
		return false, err
	}
	if _, err := appendEventTx(tx, evTaskRestarted, project, taskID, r.JobID,
		eventPayload{Status: taskReady, Run: int64(r.Run), Reason: boundNote(note), Branch: seed}, now); err != nil {
		return false, err
	}
	return true, nil
}
