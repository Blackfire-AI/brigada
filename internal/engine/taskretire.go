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

// The retire (§6.2): a task that will not land — its work landed
// elsewhere, another task supersedes it, or it sits parked past all
// repair — cannot be cancelled once it is done, the land gate refuses
// it once its hold is gone, and a plan that holds it never closes. Retire
// settles such a task on purpose: whatever its state, short of landed,
// its live jobs stop, its land hold drops, its branch stays untouched,
// and it never lands by any path. Its dependents must go with it or
// already be dead, so a retirement leaves nothing stranded on it, and
// the plan then closes by the usual rule, the done report naming each
// retired task with its reason.

// RetireResult is what Retire did: the task ids it retired, and per plan
// touched the state the plan reached after the close rule ran.
type RetireResult struct {
	Retired []int64       `json:"retired"`
	Plans   []RetiredPlan `json:"plans"`
}

// RetiredPlan is one plan a retirement touched, with its state after it.
type RetiredPlan struct {
	PlanID int64  `json:"plan_id"`
	State  string `json:"state"`
}

// Retire retires each named task of the project that has not landed,
// whatever its state — done, needs_foreman, needs_user, failed, held at
// the land gate, pending, ready, running — with the reason recorded
// verbatim and caller naming who asked, the foreman or the user. A
// landed task is refused, and so is a cancelled or already retired one:
// those are settled their own way. A task built on a retired task — a
// dependent, by dependency or base — blocks the retirement unless it is
// retired in this same call or is already cancelled or retired; every
// other state blocks, a failed dependent included, for its branch or
// its approved result is built on the retired task's branch. The
// refusal names each blocking task with its state, and one refusal
// rejects the whole call: nothing is written. Each retirement stops the
// task's live jobs, drops its land hold and leaves its branch alone;
// the plan then closes by the usual rule (§6.2), the done report naming
// the retired tasks with their reasons. project scopes the call to one
// project; 0 names any, and a task of another project is refused.
func (e *Engine) Retire(ctx context.Context, project int64, tasks []int64, reason, caller string) (RetireResult, error) {
	if len(tasks) == 0 {
		return RetireResult{}, fmt.Errorf("give at least one task to retire")
	}
	if strings.TrimSpace(reason) == "" {
		return RetireResult{}, fmt.Errorf("reason is required: say why the tasks are retired")
	}
	var res RetireResult
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := e.sweepTx(tx, now); err != nil {
			return err
		}
		r, err := e.retireTx(tx, project, tasks, reason, caller, now)
		if err != nil {
			return err
		}
		res = r
		return nil
	})
	if err != nil {
		return RetireResult{}, fmt.Errorf("retire tasks: %w", err)
	}
	return res, nil
}

// retireTask is one task the call retires, as it was read.
type retireTask struct {
	id, plan, project int64
	name              string
}

// label words the task as the refusal and the history name it.
func (t retireTask) label() string {
	if t.name != "" {
		return fmt.Sprintf("task %d %s", t.id, t.name)
	}
	return fmt.Sprintf("task %d", t.id)
}

// retireTx is Retire inside the caller's transaction: everything is
// validated — each task retirable, each dependent covered — before
// anything is written, so a refusal leaves the store as it was.
func (e *Engine) retireTx(tx *sql.Tx, project int64, tasks []int64, reason, caller string, now time.Time) (RetireResult, error) {
	seen := map[int64]bool{}
	var retiring []retireTask
	planProject := map[int64]int64{}
	var order []int64
	for _, id := range tasks {
		if seen[id] {
			continue
		}
		var t retireTask
		var state string
		err := tx.QueryRow(`SELECT p.project_id, t.plan_id, t.state, COALESCE(t.name, '')
			FROM tasks t JOIN plans p ON p.id = t.plan_id WHERE t.id = ?`, id).
			Scan(&t.project, &t.plan, &state, &t.name)
		if errors.Is(err, sql.ErrNoRows) {
			return RetireResult{}, fmt.Errorf("no such task %d", id)
		}
		if err != nil {
			return RetireResult{}, fmt.Errorf("read task %d: %w", id, err)
		}
		t.id = id
		if project != 0 && t.project != project {
			return RetireResult{}, fmt.Errorf("task %d belongs to project %d, not %d", id, t.project, project)
		}
		if err := activeProjectTx(tx, t.project); err != nil {
			return RetireResult{}, err
		}
		if l, err := landingTx(tx, id); err != nil {
			return RetireResult{}, err
		} else if l != nil {
			return RetireResult{}, fmt.Errorf("task %d is landed (%s@%s): a landed task is not retired", id, l.TargetBranch, l.Commit)
		}
		switch state {
		case taskCancelled:
			return RetireResult{}, fmt.Errorf("task %d is cancelled: a cancelled task is not retired", id)
		case taskRetired:
			return RetireResult{}, fmt.Errorf("task %d is retired already", id)
		}
		seen[id] = true
		retiring = append(retiring, t)
		if _, ok := planProject[t.plan]; !ok {
			planProject[t.plan] = t.project
			order = append(order, t.plan)
		}
	}

	// The dependents rule (§6.2), deliberately narrower than cancel's:
	// a dependent of a retired task is covered only by being retired in
	// this same call or by having ended cancelled or retired itself. A
	// failed dependent blocks too — its branch was still built on the
	// retired task's — and so does every other state, pending included,
	// for retire has no re-point. One refusal rejects the whole call.
	for _, t := range retiring {
		rows, err := tx.Query(`SELECT x.id, COALESCE(x.name, ''), x.state FROM task_deps d
			JOIN tasks x ON x.id = d.task_id WHERE d.depends_on_task_id = ? ORDER BY x.id`, t.id)
		if err != nil {
			return RetireResult{}, fmt.Errorf("read the dependents of task %d: %w", t.id, err)
		}
		var offenders []string
		for rows.Next() {
			var id int64
			var name, state string
			if err := rows.Scan(&id, &name, &state); err != nil {
				rows.Close()
				return RetireResult{}, fmt.Errorf("read the dependents of task %d: %w", t.id, err)
			}
			if seen[id] || state == taskCancelled || state == taskRetired {
				continue
			}
			label := retireTask{id: id, name: name}.label()
			offenders = append(offenders, fmt.Sprintf("%s (%s)", label, state))
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return RetireResult{}, fmt.Errorf("read the dependents of task %d: %w", t.id, err)
		}
		rows.Close()
		if len(offenders) > 0 {
			return RetireResult{}, fmt.Errorf("retire: %s has dependents built on its branch: %s; retire each in this same call, or leave them be",
				t.label(), strings.Join(offenders, ", "))
		}
	}

	// Write: jobs stop, the land hold drops, the state moves, and one
	// task_retired event carries the reason verbatim and who asked. The
	// row update marks the plan's file owed, as any task write does.
	res := RetireResult{}
	nowStr := fmtTime(now)
	for _, t := range retiring {
		if err := e.cancelTaskJobsTx(tx, t.id, "task retired", now); err != nil {
			return RetireResult{}, err
		}
		if _, err := tx.Exec(`DELETE FROM land_holds WHERE task_id = ?`, t.id); err != nil {
			return RetireResult{}, fmt.Errorf("drop the land hold of task %d: %w", t.id, err)
		}
		if _, err := tx.Exec(`UPDATE tasks SET state = ?, land_env_cause = '', updated_at = ? WHERE id = ?`,
			taskRetired, nowStr, t.id); err != nil {
			return RetireResult{}, fmt.Errorf("retire task %d: %w", t.id, err)
		}
		if _, err := appendEventTx(tx, evTaskRetired, t.project, t.id, 0,
			eventPayload{Status: taskRetired, Reason: reason, Caller: caller}, now); err != nil {
			return RetireResult{}, err
		}
		res.Retired = append(res.Retired, t.id)
	}

	// The plan closes by the usual rule: a retired task is settled, so
	// what the close reads is the rest of the plan. A plan that is not
	// active keeps its state; the result names each plan's state as it
	// stands after the call. A plan closes under its own project, so a
	// call that spans projects (project 0, the engine's own reach) still
	// reports each plan right.
	for _, plan := range order {
		if _, err := e.finishPlanIfClosedTx(tx, planProject[plan], plan, 0, 0, now); err != nil {
			return RetireResult{}, err
		}
		state := ""
		if err := tx.QueryRow("SELECT state FROM plans WHERE id = ?", plan).Scan(&state); err != nil {
			return RetireResult{}, fmt.Errorf("read plan %d: %w", plan, err)
		}
		res.Plans = append(res.Plans, RetiredPlan{PlanID: plan, State: state})
	}
	return res, nil
}

// taskRetireReasonTx reads the reason a task was retired with: the
// verbatim reason of its latest task_retired event, "" when it has none.
func taskRetireReasonTx(tx *sql.Tx, taskID int64) (string, error) {
	var payload string
	err := tx.QueryRow(`SELECT payload FROM events WHERE task_id = ? AND kind = ?
		ORDER BY id DESC LIMIT 1`, taskID, evTaskRetired).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the retirement of task %d: %w", taskID, err)
	}
	var p eventPayload
	if json.Unmarshal([]byte(payload), &p) != nil {
		return "", nil
	}
	return p.Reason, nil
}
