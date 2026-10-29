package engine

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// The blocked-plan rule (§6.2, decided 2026-10-29): a plan with done
// tasks that can never land — an implement dependency cancelled or
// failed under them, which is what a dependency that was restarted and
// failed leaves behind — does not close as done. It stays active as
// blocked: the needs-you queue carries one line per stranded task
// naming the dead dependency, one wake per blockage tells the foreman,
// and the one legal move is plan_amend's retarget, which drops or
// replaces the dead dependency so the task lands without running again.
// Plans the old behavior already closed stay closed: no migration, no
// reopen.

// planStrandedTx computes the plan's stranded tasks (§6.2): done,
// landable tasks (the land rule's own eligibility, §7.1: implement and
// approved) that can never land, because an implement dependency of
// theirs was cancelled or failed, or is itself stranded — the blockage
// is transitive over the plan's graph, so a chain stranded at its root
// strands every task above it, and freeing the root frees the rest in
// landing order. The returned map names, per stranded task's node
// index, the root cause's node index: the cancelled or failed task the
// needs-you line and the wake name.
func planStrandedTx(tx *sql.Tx, nodes []amendNode) (map[int]int, error) {
	memo := map[int]int{} // node index -> root index; -1 = not stranded
	var visit func(int) (int, error)
	visit = func(i int) (int, error) {
		if r, ok := memo[i]; ok {
			return r, nil
		}
		memo[i] = -1 // plans are acyclic; this only bounds the recursion
		root := -1
		for _, j := range nodes[i].deps {
			if nodes[j].class != classImplement {
				continue // a dependency that never lands never blocks (§7.1)
			}
			switch nodes[j].state {
			case taskCancelled, taskFailed:
				if root < 0 {
					root = j
				}
			case taskDone:
				settled, err := depLandedForTx(tx, nodes[i].id, nodes[j].id)
				if err != nil {
					return -1, err
				}
				if settled {
					continue
				}
				// Not settled and not dead: it may still land (its own
				// job under way, a gate the user holds) or be stranded
				// itself, which strands this task with its root cause.
				r, err := visit(j)
				if err != nil {
					return -1, err
				}
				if r >= 0 && root < 0 {
					root = r
				}
			}
		}
		memo[i] = root
		return root, nil
	}
	out := map[int]int{}
	for i := range nodes {
		if nodes[i].state != taskDone || nodes[i].class != classImplement {
			continue
		}
		approved, err := taskApprovedTx(tx, nodes[i].id)
		if err != nil {
			return nil, err
		}
		if !approved {
			continue
		}
		root, err := visit(i)
		if err != nil {
			return nil, err
		}
		if root >= 0 {
			out[i] = root
		}
	}
	return out, nil
}

// strandedIndexes lists a stranded map's task indexes in order, so a
// report or a wake names the tasks steadily.
func strandedIndexes(stranded map[int]int) []int {
	idx := make([]int, 0, len(stranded))
	for i := range stranded {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	return idx
}

// evalPlanBlockageTx is the blocked-plan rule's own step (§6.2),
// re-evaluated wherever the plan's closing is: with stranded tasks the
// plan is blocked — it reports true, and the caller leaves it active —
// and one plan_blocked event per blockage reaches the foreman's inbox
// and wakes a turn, naming every stranded task and its root cause. The
// wake is once per blockage: the episode a plan_blocked event opens ends
// only with a plan_unblocked event, written when the last stranded task
// was freed, so clearing and re-blocking wakes again while a mid-run
// re-evaluation alone never does.
func (e *Engine) evalPlanBlockageTx(tx *sql.Tx, project, plan int64, nodes []amendNode, now time.Time) (bool, error) {
	stranded, err := planStrandedTx(tx, nodes)
	if err != nil {
		return false, err
	}
	last, err := lastPlanBlockEventTx(tx, plan)
	if err != nil {
		return false, err
	}
	switch {
	case len(stranded) > 0 && last != evPlanBlocked:
		var lines []string
		for _, i := range strandedIndexes(stranded) {
			r := stranded[i]
			lines = append(lines, fmt.Sprintf("%s cannot land: %s is %s", nodes[i].label(), nodes[r].label(), nodes[r].state))
		}
		if err := inboxEventTx(tx, evPlanBlocked, project, 0, 0,
			eventPayload{Plan: plan, Count: int64(len(lines)), Reason: strings.Join(lines, "; ")}, now); err != nil {
			return true, err
		}
		if err := e.wakeForemanTx(tx, project, now); err != nil {
			return true, err
		}
	case len(stranded) == 0 && last == evPlanBlocked:
		if _, err := appendEventTx(tx, evPlanUnblocked, project, 0, 0,
			eventPayload{Plan: plan}, now); err != nil {
			return false, err
		}
	}
	return len(stranded) > 0, nil
}

// lastPlanBlockEventTx reads the kind of the plan's latest blockage
// event — plan_blocked or plan_unblocked — or "" when it has none: the
// word for whether the plan's current blockage episode was already
// announced.
func lastPlanBlockEventTx(tx *sql.Tx, plan int64) (string, error) {
	var kind string
	err := tx.QueryRow(`SELECT kind FROM events
		WHERE kind IN (?, ?) AND json_extract(payload, '$.plan') = ?
		ORDER BY id DESC LIMIT 1`, evPlanBlocked, evPlanUnblocked, plan).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the blockage events of plan %d: %w", plan, err)
	}
	return kind, nil
}

// stackedOnDeadDepTx reports whether the done task's recorded base is
// the result branch of a cancelled or failed implement dependency it is
// being re-pointed off (§7.1): such a task's branch was built on the
// dead work, and the landing diff — taken against the recorded base, so
// it excludes the base's changes — assumes it. Only a task whose base
// is the land target, or a landed task, may be re-pointed; the returned
// index names the dependency the task is stacked on, -1 when it is not.
func stackedOnDeadDepTx(tx *sql.Tx, nodes []amendNode, i int) (int, error) {
	var baseRef string
	if err := tx.QueryRow("SELECT COALESCE(base_ref, '') FROM tasks WHERE id = ?", nodes[i].id).Scan(&baseRef); err != nil {
		return -1, fmt.Errorf("read the base of task %d: %w", nodes[i].id, err)
	}
	if baseRef == "" {
		return -1, nil
	}
	for _, j := range nodes[i].deps {
		if nodes[j].class != classImplement {
			continue
		}
		switch nodes[j].state {
		case taskCancelled, taskFailed:
			var branch string
			if err := tx.QueryRow("SELECT COALESCE(branch, '') FROM tasks WHERE id = ?", nodes[j].id).Scan(&branch); err != nil {
				return -1, fmt.Errorf("read the branch of task %d: %w", nodes[j].id, err)
			}
			if branch != "" && branch == baseRef {
				return j, nil
			}
		}
	}
	return -1, nil
}

// statusStrandedTx lists the needs-you entries of the blocked plans'
// stranded tasks (§6.2, §10): one line per done task that can never
// land, naming the cancelled or failed dependency that strands it. The
// lines are derived from the plan's own state, so they appear with the
// blockage and leave with its clearing.
func statusStrandedTx(tx *sql.Tx, project int64) ([]NeedsYouItem, error) {
	query := `SELECT p.id FROM plans p JOIN projects pr ON pr.id = p.project_id
		WHERE p.state = 'active' AND pr.archived_at IS NULL`
	args := []any{}
	if project != 0 {
		query += " AND p.project_id = ?"
		args = append(args, project)
	}
	rows, err := tx.Query(query+" ORDER BY p.id", args...)
	if err != nil {
		return nil, fmt.Errorf("list the active plans for blockage: %w", err)
	}
	var plans []int64
	for rows.Next() {
		var plan int64
		if err := rows.Scan(&plan); err != nil {
			rows.Close()
			return nil, fmt.Errorf("list the active plans for blockage: %w", err)
		}
		plans = append(plans, plan)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("list the active plans for blockage: %w", err)
	}
	rows.Close()
	items := []NeedsYouItem{}
	for _, plan := range plans {
		nodes, err := loadAmendNodesTx(tx, plan)
		if err != nil {
			return nil, err
		}
		stranded, err := planStrandedTx(tx, nodes)
		if err != nil {
			return nil, err
		}
		for _, i := range strandedIndexes(stranded) {
			r := stranded[i]
			items = append(items, NeedsYouItem{
				Kind:   "task",
				ID:     nodes[i].id,
				State:  taskDone,
				Title:  nodes[i].title,
				PlanID: plan,
				Reason: oneLine(fmt.Sprintf("done but cannot land: %s is %s; re-point it off the dead dependency (plan_amend retarget) or restart it on a new base by a follow-up task",
					nodes[r].label(), nodes[r].state), needsYouReasonLimit),
			})
		}
	}
	return items, nil
}
