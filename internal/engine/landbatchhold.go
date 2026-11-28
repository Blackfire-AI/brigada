package engine

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// Batch gate holds (§7.1 "Deciding a batch hold", §6.3 "Releasing
// holds"). Before the first squash the land_ready command's exit 1 holds
// each member subject to it, every member with its own hold, re-check and
// needs-you line; an approval or a passing re-check releases one member
// alone, exactly once, into one next batch with its gate skipped
// (markLandSkipReadyTx). At the push phase the command runs once for the
// proposed push: its exit 1 holds that one push, and every waiting
// landing the push covers — the waiting commits beneath the pushed one —
// waits with the same reason on its own line, nothing re-squashed or
// newly recorded. Releasing one held landing pushes its recorded prefix
// alone and never a later commit.

// coveredWaitingTx lists the waiting landings a push of the task's
// recorded commit carries (§7.1): the project's landings on the same
// target recorded before it and still waiting, in landing order, which
// sit beneath it on the waiting stack. A landing a live batch reserves is
// its batch's.
func coveredWaitingTx(tx *sql.Tx, taskID int64) ([]int64, error) {
	rows, err := tx.Query(`SELECT c.task_id FROM landings l JOIN landings c
			ON c.project_id = l.project_id AND c.target_branch = l.target_branch AND c.id < l.id
		WHERE l.task_id = ? AND c.pushed = 'waiting' AND NOT `+reservedLiveBatch("c.task_id")+`
		ORDER BY c.id`, taskID)
	if err != nil {
		return nil, fmt.Errorf("read the landings beneath task %d: %w", taskID, err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("read the landings beneath task %d: %w", taskID, err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// holdCoveredPushesTx holds the waiting landings a held push covers
// (§7.1): each takes the push's reason on its own needs-you line, with no
// re-check of its own — the held push's re-check speaks for the one push.
// A landing already decided — held, rejected, or released with a push job
// that skips the gate — is left as it is; an ordinary push of it still
// queued is the same proposed push, so it is cancelled rather than run
// the command again. A push already leased goes its own way. Nothing for
// a squash-phase hold, which has no landing record.
func (e *Engine) holdCoveredPushesTx(tx *sql.Tx, project, taskID int64, reason string, now time.Time) error {
	if l, err := landingTx(tx, taskID); err != nil || l == nil || l.Pushed != "waiting" {
		return err
	}
	covered, err := coveredWaitingTx(tx, taskID)
	if err != nil {
		return err
	}
	for _, id := range covered {
		var decided int
		if err := tx.QueryRow("SELECT COUNT(*) FROM land_holds WHERE task_id = ?", id).Scan(&decided); err != nil {
			return fmt.Errorf("read the land hold of task %d: %w", id, err)
		}
		if decided > 0 {
			continue
		}
		live, err := liveJobTx(tx, fmt.Sprintf("task:%d", id))
		if err != nil {
			return err
		}
		if live != nil {
			if live.Kind != KindLand || live.Status != "queued" || live.SkipLandReady {
				continue
			}
			if _, err := e.cancelTx(tx, live, fmt.Sprintf("land_ready held the push of task %d, which carries this one", taskID), now); err != nil {
				return err
			}
		}
		if err := e.holdLandTx(tx, project, id, reason, now); err != nil {
			return err
		}
	}
	return nil
}

// carryCoveredPushesTx records the waiting landings a push carried (§7.1):
// pushing the task's recorded commit pushed every waiting commit beneath
// it, so each such landing is pushed now, its push hold — held by the
// same push — released with it, without a new squash or a push of its
// own. A landing whose own push is leased settles by that push; one
// still queued is cancelled, its commit already on the target. Later
// commits stay waiting.
func (e *Engine) carryCoveredPushesTx(tx *sql.Tx, job *jobRow, target string, now time.Time) error {
	covered, err := coveredWaitingTx(tx, job.TaskID)
	if err != nil {
		return err
	}
	for _, id := range covered {
		live, err := liveJobTx(tx, fmt.Sprintf("task:%d", id))
		if err != nil {
			return err
		}
		if live != nil {
			if live.Status != "queued" {
				continue
			}
			if _, err := e.cancelTx(tx, live, fmt.Sprintf("the push of task %d carried this landing", job.TaskID), now); err != nil {
				return err
			}
		}
		l, err := landingTx(tx, id)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE landings SET pushed = 'pushed' WHERE task_id = ?`, id); err != nil {
			return fmt.Errorf("record the push of task %d: %w", id, err)
		}
		if _, held, err := heldLandHoldTx(tx, id); err != nil {
			return err
		} else if held {
			if _, err := tx.Exec(`DELETE FROM land_holds WHERE task_id = ? AND state = 'held'`, id); err != nil {
				return fmt.Errorf("release the push hold of task %d: %w", id, err)
			}
			kind := evTaskLandRecheckReleased
			if job.SkipLandReady {
				kind = evTaskLandReleased // the user's release of a later landing pushed this one
			}
			if _, err := appendEventTx(tx, kind, job.ProjectID, id, job.ID, eventPayload{Status: taskDone,
				Reason: boundNote(fmt.Sprintf("released with the push of task %d", job.TaskID))}, now); err != nil {
				return err
			}
		}
		if _, err := appendEventTx(tx, evTaskPushed, job.ProjectID, id, job.ID,
			eventPayload{Status: taskDone, Branch: target, Commit: l.Commit, Pushed: "pushed",
				Reason: boundNote(fmt.Sprintf("carried by the push of task %d", job.TaskID))}, now); err != nil {
			return err
		}
		if err := e.releaseDependentsTx(tx, job.ProjectID, id, now); err != nil {
			return err
		}
		if err := e.finishTaskPlanTx(tx, job.ProjectID, id, job.ID, now); err != nil {
			return err
		}
	}
	return nil
}

// landRecheckPassedTx applies a squash-phase re-check's pass (§7.1
// "Deciding a batch hold"): the engine releases the member exactly as an
// approval does — its hold deleted with the engine's own release event,
// the member alone queued once into one next batch with its gate skipped
// — while the other members' holds wait for their own re-check or the
// user. A hold decided meanwhile, or a member settled another way, takes
// nothing.
func (e *Engine) landRecheckPassedTx(tx *sql.Tx, job *jobRow, c Completion, now time.Time) error {
	// The re-check's pass may verify a green baseline (§7.1).
	var rec struct {
		Gate *landGateEvidence `json:"gate"`
	}
	if json.Unmarshal([]byte(c.Result), &rec) == nil && rec.Gate != nil {
		target, err := landJobTargetTx(tx, job.ProjectID, job.TaskID)
		if err != nil {
			return err
		}
		if err := noteLandBaselineTx(tx, job.ProjectID, target, rec.Gate, now); err != nil {
			return err
		}
	}
	var state string
	if err := tx.QueryRow("SELECT state FROM tasks WHERE id = ?", job.TaskID).Scan(&state); err != nil {
		return fmt.Errorf("read task %d: %w", job.TaskID, err)
	}
	reason, held, err := heldLandHoldTx(tx, job.TaskID)
	if err != nil {
		return err
	}
	l, err := landingTx(tx, job.TaskID)
	if err != nil {
		return err
	}
	if state != taskDone || !held || !landReadyHold(reason) || l != nil {
		return e.finishLandPlanTx(tx, job, now)
	}
	if _, err := tx.Exec(`DELETE FROM land_holds WHERE task_id = ? AND state = 'held'`, job.TaskID); err != nil {
		return fmt.Errorf("release the land hold of task %d: %w", job.TaskID, err)
	}
	if _, err := appendEventTx(tx, evTaskLandRecheckReleased, job.ProjectID, job.TaskID, job.ID,
		eventPayload{Status: taskDone, Reason: "land_ready passed on the re-check; the engine released the landing into the next land batch"}, now); err != nil {
		return err
	}
	if _, err := e.queueLandJobTx(tx, job.ProjectID, job.TaskID, true, now); err != nil {
		return err
	}
	return e.finishLandPlanTx(tx, job, now)
}
