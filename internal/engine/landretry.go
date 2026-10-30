package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// The land retry (#133): a task whose landing failed on something other
// than its content — the push rejected because the target moved, or the
// environment (issue #116) — is parked at needs_foreman, and a restart
// would redo work that is finished and approved. RetryLanding queues the
// landing again instead, through the land rule itself, so every gate
// still applies; the foreman's land_retry tool calls it.

// The outcomes of a land retry, LandRetryResult.Outcome.
const (
	LandRetryQueued    = "queued"     // the land job is queued; JobID names it
	LandRetryHeld      = "held"       // the land gate held the task; Reason names the rule
	LandRetryStaleBase = "stale_base" // the landing was refused on a stale base; the task parked again
	LandRetryRefused   = "refused"    // nothing changed; Reason says why
)

// LandRetryResult is what RetryLanding did: one of the four outcomes, the
// land job a queued retry submitted, and the reason a held, stale-base or
// refused retry carries.
type LandRetryResult struct {
	TaskID  int64  `json:"task_id"`
	Outcome string `json:"outcome"`
	JobID   int64  `json:"job_id,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// landRetryRefusal is a retry refused after the task was read: no row
// changed, and the reason is the result's, not an error.
type landRetryRefusal struct{ reason string }

func (r landRetryRefusal) Error() string { return r.reason }

// RetryLanding queues again the landing of a task parked by a landing
// failure that is not about its content (#133): a task of project, in a
// plan not finished, parked at needs_foreman by a rejected push or by an
// environment failure, with no landing record. The task returns to done
// — off the needs-you queue, and off its environment cause the way a
// restart takes it off (the cause's episode ends once no task it parked
// is still parked) — and the land rule runs as for any done task: the
// land gate and its approval rules, land_ready (the grant never skips
// it) and the push gate all apply. No gate is passed and no restart is
// spent.
//
// The outcomes are four. queued: the land job is submitted, and the
// land_retried event names it. held: a land-gate rule held the task, the
// hold waiting for the user's release as any hold does, and land_retried
// carries the rule with no job. stale_base: the task's base is a
// superseded round of its dependency (issue #124), so the land rule
// refused the landing and parked the task again with its task_base_stale
// event — a recorded outcome, not a refusal, for the task did change
// state and land_retried records it; the way forward is task_restart.
// refused: nothing changed and nothing is recorded — a task of another
// project or a finished plan, one with a landing record (pushed or
// waiting at the push gate: task_restart makes a fresh attempt), one not
// parked at needs_foreman, held at the land gate (the user's), one with
// a live job, or one parked by anything but a rejected push or an
// environment failure — a conflict, a failed land check, a moved
// approved head or a stale base want task_restart.
func (e *Engine) RetryLanding(ctx context.Context, project, taskID int64) (LandRetryResult, error) {
	res := LandRetryResult{TaskID: taskID}
	var refusal landRetryRefusal
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		res, refusal = LandRetryResult{TaskID: taskID}, landRetryRefusal{}
		now := e.cfg.Now()
		if _, err := e.sweepTx(tx, now); err != nil {
			return err
		}
		if err := landRetryEligibleTx(tx, project, taskID); err != nil {
			if errors.As(err, &refusal) {
				return nil // the sweep's own changes still commit
			}
			return err
		}
		r, err := e.retryLandingTx(tx, project, taskID, now)
		if err != nil {
			return fmt.Errorf("retry the landing of task %d: %w", taskID, err)
		}
		res = r
		return nil
	})
	if err != nil {
		return LandRetryResult{TaskID: taskID}, err
	}
	if refusal.reason != "" {
		return LandRetryResult{TaskID: taskID, Outcome: LandRetryRefused, Reason: refusal.reason}, nil
	}
	return res, nil
}

// landRetryEligibleTx checks a land retry before anything changes,
// returning a landRetryRefusal naming what it found, or nil when the
// task is one RetryLanding retries. The checks run in the order that
// names the most telling reason: the landing record before the state, so
// a landed task says so; the parking cause last.
func landRetryEligibleTx(tx *sql.Tx, project, taskID int64) error {
	refuse := func(format string, args ...any) error {
		return landRetryRefusal{reason: fmt.Sprintf(format, args...)}
	}
	var owner int64
	var state, planState, cause string
	err := tx.QueryRow(`SELECT p.project_id, p.state, t.state, COALESCE(t.land_env_cause, '')
		FROM tasks t JOIN plans p ON p.id = t.plan_id WHERE t.id = ?`, taskID).Scan(&owner, &planState, &state, &cause)
	if errors.Is(err, sql.ErrNoRows) {
		return refuse("no such task %d", taskID)
	}
	if err != nil {
		return fmt.Errorf("read task %d: %w", taskID, err)
	}
	if owner != project {
		return refuse("task %d belongs to project %d, not %d", taskID, owner, project)
	}
	if err := activeProjectTx(tx, owner); err != nil {
		return refuse("%s", err.Error())
	}
	if l, err := landingTx(tx, taskID); err != nil {
		return err
	} else if l != nil {
		return refuse("task %d already has a landing record (%s): its landing is not retried; use task_restart for a fresh attempt", taskID, l.Pushed)
	}
	if planState == planDone || planState == planCancelled {
		return refuse("task %d is in a plan that is %s", taskID, planState)
	}
	if state != taskNeedsForeman {
		return refuse("task %d is %s, not parked at needs_foreman by a failed landing", taskID, state)
	}
	var holdState string
	if err := tx.QueryRow("SELECT state FROM land_holds WHERE task_id = ?", taskID).Scan(&holdState); err == nil {
		return refuse("task %d is %s at the land gate: the release is the user's (brigada land approve)", taskID, holdState)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read the land hold of task %d: %w", taskID, err)
	}
	if live, err := liveJobTx(tx, fmt.Sprintf("task:%d", taskID)); err != nil {
		return err
	} else if live != nil {
		return refuse("task %d has a live %s job (job %d)", taskID, live.Kind, live.ID)
	}
	var kind, payload string
	kinds := "'" + strings.Join(parkingEventKinds, "', '") + "'"
	err = tx.QueryRow(`SELECT kind, payload FROM events
		WHERE task_id = ? AND kind IN (`+kinds+`)
		ORDER BY id DESC LIMIT 1`, taskID).Scan(&kind, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return refuse("task %d is parked with no parking event", taskID)
	}
	if err != nil {
		return fmt.Errorf("read what parked task %d: %w", taskID, err)
	}
	if kind == evTaskBaseStale {
		return refuse("task %d is parked on a stale base (%s): use task_restart to rebuild on the new base", taskID, kind)
	}
	if kind != evTaskLandFailed {
		return refuse("task %d is parked by %s, not by a failed landing: use task_restart or answer it", taskID, kind)
	}
	if cause == "" {
		var p eventPayload
		_ = json.Unmarshal([]byte(payload), &p)
		if !landRetryablePush(p) {
			return refuse("task %d is parked by a landing failure about its content (%s): use task_restart", taskID, oneLine(p.Reason, 200))
		}
	}
	// What landableDoneTx reads of a done task, read of the parked one.
	var class string
	if err := tx.QueryRow("SELECT class FROM tasks WHERE id = ?", taskID).Scan(&class); err != nil {
		return fmt.Errorf("read task %d: %w", taskID, err)
	}
	if class != classImplement {
		return refuse("task %d is of class %s, which never lands", taskID, class)
	}
	if ok, err := taskApprovedTx(tx, taskID); err != nil {
		return err
	} else if !ok {
		return refuse("task %d has no approve verdict on its last review", taskID)
	}
	if landed, err := depsLandedTx(tx, taskID); err != nil {
		return err
	} else if !landed {
		return refuse("task %d has a dependency that has not landed", taskID)
	}
	return nil
}

// landRetryablePush reports whether a task_land_failed event with no
// environment cause is a rejected push (§7.1) — the target moved, the
// task's content untouched. The content failures are read first, because
// their words — a conflicting file's name, a check log — may contain the
// push-rejection markers: a conflict, a failed check on the landed
// result, a moved approved head.
func landRetryablePush(p eventPayload) bool {
	if p.CheckOutput != "" {
		return false
	}
	for _, s := range []string{"land conflict:", "the check failed on the landed result"} {
		if strings.HasPrefix(p.Reason, s) {
			return false
		}
	}
	if strings.Contains(p.Reason, "not the approved head") {
		return false
	}
	return pushRejected(p.Reason)
}

// retryLandingTx returns an eligible task to done and runs the land rule
// on it (#133), recording the outcome. The rule's empty outcome cannot
// follow the eligibility checks; it fails the transaction rather than
// leave the task done with nothing queued.
func (e *Engine) retryLandingTx(tx *sql.Tx, project, taskID int64, now time.Time) (LandRetryResult, error) {
	if _, err := tx.Exec(`UPDATE tasks SET state = ?, land_env_cause = '', updated_at = ? WHERE id = ?`,
		taskDone, fmtTime(now), taskID); err != nil {
		return LandRetryResult{}, fmt.Errorf("return task %d to done: %w", taskID, err)
	}
	r, err := e.landRuleTx(tx, taskID, now)
	if err != nil {
		return LandRetryResult{}, err
	}
	status := taskDone
	switch r.Outcome {
	case LandRetryQueued, LandRetryHeld:
	case LandRetryStaleBase:
		status = taskNeedsForeman
		r.Reason += " (task_restart)"
	default:
		return LandRetryResult{}, fmt.Errorf("the land rule queued nothing for task %d", taskID)
	}
	// A parked task that stays done frees its waiting dependents as a
	// finished task's does (§6.1, the settleLandedTx precedent): a
	// dependent added while it sat parked starts now, not never. Only a
	// task still done frees them — a stale base parks it again (issue
	// #124), and no dependent may start work built on the round the
	// rebuild will replace.
	if status == taskDone {
		if err := e.releaseDependentsTx(tx, project, taskID, now); err != nil {
			return LandRetryResult{}, err
		}
	}
	words := r.Outcome
	if r.Reason != "" {
		words += ": " + r.Reason
	}
	if _, err := appendEventTx(tx, evLandRetried, project, taskID, r.JobID,
		eventPayload{Status: status, Reason: boundNote(words)}, now); err != nil {
		return LandRetryResult{}, err
	}
	return LandRetryResult{TaskID: taskID, Outcome: r.Outcome, JobID: r.JobID, Reason: r.Reason}, nil
}
