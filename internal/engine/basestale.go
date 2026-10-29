package engine

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// The stale-base rule (§6.1, §7.1, issue #124): no task reaches done or
// lands on a base other than its stacked dependency's final result
// branch. A dependency that leaves done — a landing that failed — and is
// restarted after its dependents started finishes again from a new
// -r<n> branch, and every started, unlanded dependent stacked on a
// superseded round of it is held: parked at needs_foreman when its key
// is free, and refused at the land gate otherwise and ever after, until
// the foreman restarts it and the rebuild re-resolves its base to the
// dependency's new result. One wake per rebuild (the dep_rebuilt event)
// names the dependency and every stale task, the one-wake-per-cause
// shape of issue #116; the land-path refusal (task_base_stale) parks and
// wakes per task, as a failure about the task's own content does.

// stackedDepTx reads the dependency a task's base is stacked on (§6.1):
// resolveBaseTx's own selection — the dep edge named base, else the
// continued dep, else the only dep — with that dependency's current
// recorded result branch. dep 0 when the task stacks on nothing: no
// dependencies, several with neither named, or a cross-plan dep no edge
// row records.
func stackedDepTx(tx *sql.Tx, taskID int64) (dep int64, branch string, err error) {
	err = tx.QueryRow(`SELECT x.id, COALESCE(x.branch, '') FROM task_deps d JOIN tasks x ON x.id = d.depends_on_task_id
		WHERE d.task_id = ? AND (d.is_base = 1 OR d.is_continue = 1 OR (SELECT COUNT(*) FROM task_deps o WHERE o.task_id = d.task_id) = 1)
		ORDER BY d.is_base DESC, d.is_continue DESC LIMIT 1`, taskID).Scan(&dep, &branch)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", nil
	}
	if err != nil {
		return 0, "", fmt.Errorf("read the stacked dependency of task %d: %w", taskID, err)
	}
	return dep, branch, nil
}

// depRoundNames reports whether ref is one of dependency dep's own
// result branches — its first attempt's brigada/task-<id> or a restart's
// brigada/task-<id>-r<n> — so a base that names one is the dependency's
// work, and a different current branch means the round it names was
// superseded.
func depRoundNames(ref string, dep int64) bool {
	first := fmt.Sprintf("brigada/task-%d", dep)
	return ref == first || strings.HasPrefix(ref, first+"-r")
}

// staleBaseTx reports whether the task's recorded base is a superseded
// result branch of the dependency it stacks on (§6.1, issue #124): the
// dependency's current branch differs from the base and the base names
// one of its rounds. A base that names nothing of the dependency — the
// land target a dep that pushed no branch leaves, another branch — is
// never stale, and neither is a dependency that recorded no branch.
func staleBaseTx(tx *sql.Tx, taskID int64) (dep int64, baseRef, depBranch string, stale bool, err error) {
	var base string
	if err := tx.QueryRow("SELECT COALESCE(base_ref, '') FROM tasks WHERE id = ?", taskID).Scan(&base); err != nil {
		return 0, "", "", false, fmt.Errorf("read the base of task %d: %w", taskID, err)
	}
	dep, branch, err := stackedDepTx(tx, taskID)
	if err != nil || dep == 0 || branch == "" || base == branch || !depRoundNames(base, dep) {
		return 0, "", "", false, err
	}
	return dep, base, branch, true, nil
}

// staleBaseReason words a stale-base hold (issue #124): the superseded
// base and the dependency's current branch both named, so the foreman
// reads what to rebuild on. wants names what the hold refused — the
// task's own landing, or nothing for the sweep that parks it outright.
func staleBaseReason(dep int64, baseRef, depBranch, wants string) string {
	reason := fmt.Sprintf("the base %s is a superseded result of task %d, now on %s", baseRef, dep, depBranch)
	if wants != "" {
		reason = wants + ": " + reason
	}
	return reason + "; restart the task to rebuild on the new base"
}

// parkStaleBaseTx parks a task on a stale base (issue #124):
// needs_foreman under no land cause, any held land-gate hold beside it
// dropped — its landing is refused anyway — and the task_base_stale
// event naming both refs. inbox sends the event to the foreman's wake
// with the park; log keeps it to the history when a wake of the cause
// already carries the task, as the sweep's does.
func (e *Engine) parkStaleBaseTx(tx *sql.Tx, project, taskID, dep int64, baseRef, depBranch, wants string, inbox bool, now time.Time) error {
	if _, err := tx.Exec(`UPDATE tasks SET state = ?, land_env_cause = '', updated_at = ? WHERE id = ?`,
		taskNeedsForeman, fmtTime(now), taskID); err != nil {
		return fmt.Errorf("park task %d on its stale base: %w", taskID, err)
	}
	if _, err := tx.Exec(`DELETE FROM land_holds WHERE task_id = ? AND state = 'held'`, taskID); err != nil {
		return fmt.Errorf("drop the land hold of task %d: %w", taskID, err)
	}
	payload := eventPayload{Status: taskNeedsForeman, Branch: depBranch,
		Reason: boundNote(staleBaseReason(dep, baseRef, depBranch, wants))}
	if inbox {
		if err := inboxEventTx(tx, evTaskBaseStale, project, taskID, 0, payload, now); err != nil {
			return err
		}
		return e.wakeForemanTx(tx, project, now)
	}
	_, err := appendEventTx(tx, evTaskBaseStale, project, taskID, 0, payload, now)
	return err
}

// staleDependent is one dependent the sweep holds on its dependency's
// superseded round (issue #124): parked at needs_foreman when live is
// false, its landing refused later when true.
type staleDependent struct {
	id    int64
	title string
	live  bool
}

// sweepStaleDependentsTx runs the stale-base rule where a dependency's
// new result becomes final (issue #124): the completion that records
// the branch the task is now done from. Every started, unlanded
// dependent stacked on a superseded round of dep parks at needs_foreman
// — one with a live job keeps running and is only named here, for the
// land rule refuses its landing when it gets there — and one wake, the
// dep_rebuilt event, names the dependency and every stale task. A
// pending dependent is untouched: its base resolves to the new branch
// when it is released, and a landed one has nothing left to hold.
func (e *Engine) sweepStaleDependentsTx(tx *sql.Tx, project, dep int64, now time.Time) error {
	var depBranch, depTitle string
	if err := tx.QueryRow("SELECT COALESCE(branch, ''), COALESCE(title, '') FROM tasks WHERE id = ?", dep).
		Scan(&depBranch, &depTitle); err != nil {
		return fmt.Errorf("read task %d: %w", dep, err)
	}
	if depBranch == "" {
		return nil // nothing recorded: no round any base could supersede
	}
	rows, err := tx.Query(`SELECT t.id, COALESCE(t.base_ref, ''), COALESCE(t.title, '')
		FROM task_deps d JOIN tasks t ON t.id = d.task_id
		WHERE d.depends_on_task_id = ?
			AND t.state IN (?, ?, ?, ?, ?, ?, ?)
			AND NOT EXISTS (SELECT 1 FROM landings l WHERE l.task_id = t.id)
		ORDER BY t.id`, dep,
		taskReady, taskRunning, taskChecking, taskReviewing, taskNeedsForeman, taskNeedsUser, taskDone)
	if err != nil {
		return fmt.Errorf("read the dependents of task %d: %w", dep, err)
	}
	type candidate struct {
		id    int64
		base  string
		title string
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.base, &c.title); err != nil {
			rows.Close()
			return fmt.Errorf("read the dependents of task %d: %w", dep, err)
		}
		if c.base != depBranch && depRoundNames(c.base, dep) {
			candidates = append(candidates, c)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("read the dependents of task %d: %w", dep, err)
	}
	rows.Close()
	var held []staleDependent
	for _, c := range candidates {
		live, err := liveJobTx(tx, fmt.Sprintf("task:%d", c.id))
		if err != nil {
			return err
		}
		held = append(held, staleDependent{id: c.id, title: c.title, live: live != nil})
		if live != nil {
			// Running or reviewing: the park could not hold it, and the
			// land rule refuses the landing its completion reaches.
			continue
		}
		if err := e.parkStaleBaseTx(tx, project, c.id, dep, c.base, depBranch, "", false, now); err != nil {
			return err
		}
	}
	if len(held) == 0 {
		return nil
	}
	parts := make([]string, 0, len(held))
	for _, s := range held {
		what := "parked"
		if s.live {
			what = "held from landing"
		}
		parts = append(parts, fmt.Sprintf("task %d (%s) %s", s.id, oneLine(s.title, 60), what))
	}
	if err := inboxEventTx(tx, evDepRebuilt, project, dep, 0, eventPayload{
		Status: taskDone, Branch: depBranch,
		Cause: fmt.Sprintf("dep:%d:%s", dep, depBranch),
		Reason: boundNote(fmt.Sprintf("task %d (%s) finished again from %s after a restart; on its stale base: %s; restart each to rebuild on the new base",
			dep, oneLine(depTitle, 60), depBranch, strings.Join(parts, ", "))),
	}, now); err != nil {
		return err
	}
	return e.wakeForemanTx(tx, project, now)
}
