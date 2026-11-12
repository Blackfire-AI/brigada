package engine

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrTaskRuleHeld marks a foreman call refused because a plan rule holds
// the task (§6.3): the hold is the user's to release, with brigada plan
// approve, and no foreman tool passes it.
var ErrTaskRuleHeld = errors.New("task held by a plan rule")

// ruleHeldTaskTx reports whether a plan rule holds the task: of the
// events that settle it, the latest is task_plan_held — not the user's
// release, and not a start since (task_running), which a task held before
// the release existed passed through on the foreman's restart. The caller
// checks the task is parked at needs_foreman.
func ruleHeldTaskTx(tx *sql.Tx, taskID int64) (bool, error) {
	var kind string
	err := tx.QueryRow(`SELECT kind FROM events WHERE task_id = ? AND kind IN (?, ?, ?)
		ORDER BY id DESC LIMIT 1`, taskID, evTaskPlanHeld, evTaskPlanReleased, evTaskRunning).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the holds of task %d: %w", taskID, err)
	}
	return kind == evTaskPlanHeld, nil
}

// ruleHeldTasksTx lists the tasks of plan a plan rule holds alone, by id.
func ruleHeldTasksTx(tx *sql.Tx, plan int64) ([]int64, error) {
	rows, err := tx.Query("SELECT id FROM tasks WHERE plan_id = ? AND state = ? ORDER BY id", plan, taskNeedsForeman)
	if err != nil {
		return nil, fmt.Errorf("read the parked tasks of plan %d: %w", plan, err)
	}
	var parked []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read the parked tasks of plan %d: %w", plan, err)
		}
		parked = append(parked, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("read the parked tasks of plan %d: %w", plan, err)
	}
	rows.Close()
	var held []int64
	for _, id := range parked {
		ok, err := ruleHeldTaskTx(tx, id)
		if err != nil {
			return nil, err
		}
		if ok {
			held = append(held, id)
		}
	}
	return held, nil
}

// releaseHeldTasksTx is the user's yes to the tasks a plan rule holds
// alone in an active plan (§6.3): each returns to pending with a
// task_plan_released event, and the ready rule then queues those whose
// dependencies are done — the plan budget may hold them again, as it
// holds any task about to start.
func (e *Engine) releaseHeldTasksTx(tx *sql.Tx, project int64, held []int64, now time.Time) error {
	for _, id := range held {
		if _, err := tx.Exec("UPDATE tasks SET state = ?, updated_at = ? WHERE id = ?", taskPending, fmtTime(now), id); err != nil {
			return fmt.Errorf("release task %d: %w", id, err)
		}
		if _, err := appendEventTx(tx, evTaskPlanReleased, project, id, 0,
			eventPayload{Status: taskPending}, now); err != nil {
			return err
		}
	}
	return nil
}
