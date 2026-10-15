package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// classPlan is the outcome class of the planner itself (§8, §15 phase 5):
// each plan's completion counts one attempt on it for the project's
// foreman agent. It is measurement only — no dispatch behavior keys off
// it yet: the lease ranks agents by the class of the task's own jobs, and
// no job carries this class.
const classPlan = "plan"

// PlanRetarget re-points one waiting task's dependencies (§6.1): Deps
// replaces the task's whole dependency set, an empty list drops them all.
// Task and Deps name tasks of the plan by name, or by id as a decimal.
type PlanRetarget struct {
	Task string
	Deps []string
}

// PlanAmendment is what the foreman changes in a live plan (§12.2): tasks
// to add (their Deps name tasks of the plan, old or added), tasks to
// cancel, and waiting tasks whose dependencies are re-pointed.
type PlanAmendment struct {
	Add      []PlanTask
	Cancel   []string
	Retarget []PlanRetarget
	// SkipApproved says the user themself approved the cancellations of
	// this amendment: only a user's token can say so, never the foreman's
	// own. Without it a cancelled task is an abandoned one, and the
	// planner's outcome counts it against the plan.
	SkipApproved bool
}

// AmendPlanResult is what AmendPlan did: the plan's state after it, the
// added tasks, the ids cancelled and re-pointed, and every task the
// amendment started (a work job queued), added or not. Reasons says why
// the plan waits for approval, when it does.
type AmendPlanResult struct {
	PlanID     int64
	State      string
	Added      []CreatedTask
	Cancelled  []int64
	Retargeted []int64
	Started    []CreatedTask
	Reasons    []string
}

// ErrPlanNotLive refuses an amendment of a plan that is done or cancelled.
var ErrPlanNotLive = errors.New("plan is not live")

// AmendPlan is the foreman's plan_amend (§12.2): add tasks, cancel tasks
// and re-point a waiting task's dependencies, validated whole — names
// unique, deps in the plan and live, no cycle — before anything is
// written. Cancelling a task stops its live jobs. In the same transaction
// the waiting state re-evaluates as creation does (a pending task whose
// dependencies are all done turns ready and queues its work job), the
// approval limit is evaluated again for the amended plan, and a plan the
// amendment closed is finished. One plan_amended event names the change.
// project scopes the plan to one project; 0 names any.
func (e *Engine) AmendPlan(ctx context.Context, project, plan int64, am PlanAmendment) (AmendPlanResult, error) {
	var res AmendPlanResult
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := e.sweepTx(tx, now); err != nil {
			return err
		}
		r, err := e.amendPlanTx(tx, project, plan, am, now)
		if err != nil {
			return err
		}
		res = r
		return nil
	})
	if err != nil {
		return AmendPlanResult{}, fmt.Errorf("amend plan %d: %w", plan, err)
	}
	return res, nil
}

// amendNode is one task of the plan as the amendment sees it: id 0 for a
// task the amendment adds.
type amendNode struct {
	id                      int64
	name, state             string
	spec, check, class      string
	title                   string
	deps                    []int // indexes into the node list
	cancelNow, retargetedBy bool
	skipApproved            bool // a cancelled task whose skip the user approved
}

func (n amendNode) label() string {
	switch {
	case n.name != "" && n.id != 0:
		return fmt.Sprintf("task %d %s", n.id, n.name)
	case n.name != "":
		return n.name
	case n.id != 0:
		return fmt.Sprintf("task %d", n.id)
	}
	return "an added task"
}

func (e *Engine) amendPlanTx(tx *sql.Tx, project, plan int64, am PlanAmendment, now time.Time) (AmendPlanResult, error) {
	if len(am.Add) == 0 && len(am.Cancel) == 0 && len(am.Retarget) == 0 {
		return AmendPlanResult{}, fmt.Errorf("nothing to amend: give tasks to add, cancel or retarget")
	}
	var proj int64
	var state, baseRef string
	err := tx.QueryRow(`SELECT p.project_id, p.state, j.default_branch FROM plans p
		JOIN projects j ON j.id = p.project_id WHERE p.id = ?`, plan).Scan(&proj, &state, &baseRef)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && project != 0 && proj != project) {
		return AmendPlanResult{}, ErrNoPlan
	}
	if err != nil {
		return AmendPlanResult{}, fmt.Errorf("read plan %d: %w", plan, err)
	}
	if state != "active" && state != planAwaitingApproval {
		return AmendPlanResult{}, fmt.Errorf("%w: plan %d is %s; only an active or awaiting plan is amended", ErrPlanNotLive, plan, state)
	}

	nodes, err := loadAmendNodesTx(tx, plan)
	if err != nil {
		return AmendPlanResult{}, err
	}
	existing := len(nodes)
	byID := map[int64]int{}
	byName := map[string]int{}
	for i, n := range nodes {
		byID[n.id] = i
		if n.name != "" {
			byName[n.name] = i
		}
	}
	for i, t := range am.Add {
		if t.Spec == "" {
			return AmendPlanResult{}, fmt.Errorf("added task %d: spec is required: the worker's task, verbatim (§1.4)", i+1)
		}
		if t.Name != "" {
			if j, dup := byName[t.Name]; dup {
				return AmendPlanResult{}, fmt.Errorf("task name %q is already used by %s: names are unique in a plan", t.Name, nodes[j].label())
			}
			byName[t.Name] = len(nodes)
		}
		class := t.Class
		if class == "" {
			class = classImplement
		}
		title := t.Title
		if title == "" {
			title = taskTitleOf(t.Spec)
		}
		nodes = append(nodes, amendNode{name: t.Name, state: taskPending, spec: t.Spec, check: t.Check, class: class, title: title})
	}
	resolve := func(ref string) (int, error) {
		if i, ok := byName[ref]; ok {
			return i, nil
		}
		if id, err := strconv.ParseInt(ref, 10, 64); err == nil {
			if i, ok := byID[id]; ok {
				return i, nil
			}
		}
		return 0, fmt.Errorf("%q names no task of plan %d", ref, plan)
	}
	// liveDeps resolves a dependency list; a dependency that ended failed
	// or cancelled will never be done and is refused.
	liveDeps := func(owner int, refs []string) ([]int, error) {
		var out []int
		seen := map[int]bool{}
		for _, ref := range refs {
			j, err := resolve(ref)
			if err != nil {
				return nil, fmt.Errorf("%s depends on %w", nodes[owner].label(), err)
			}
			if j == owner {
				return nil, fmt.Errorf("%s depends on itself", nodes[owner].label())
			}
			if s := nodes[j].state; s == taskCancelled || s == taskFailed {
				return nil, fmt.Errorf("%s depends on %s, which is %s and will never be done: re-point it or drop the dependency", nodes[owner].label(), nodes[j].label(), s)
			}
			if !seen[j] {
				seen[j] = true
				out = append(out, j)
			}
		}
		return out, nil
	}

	var cancelled []int
	for _, ref := range am.Cancel {
		i, err := resolve(ref)
		if err != nil {
			return AmendPlanResult{}, fmt.Errorf("cancel: %w", err)
		}
		switch {
		case i >= existing:
			return AmendPlanResult{}, fmt.Errorf("cancel: %s is added by this same amendment", nodes[i].label())
		case nodes[i].state == taskDone:
			return AmendPlanResult{}, fmt.Errorf("cancel: %s is done", nodes[i].label())
		case nodes[i].state == taskCancelled || nodes[i].cancelNow:
			continue
		}
		nodes[i].skipApproved = am.SkipApproved
		nodes[i].cancelNow = true
		nodes[i].state = taskCancelled
		cancelled = append(cancelled, i)
	}
	var retargeted []int
	for _, rt := range am.Retarget {
		i, err := resolve(rt.Task)
		if err != nil {
			return AmendPlanResult{}, fmt.Errorf("retarget: %w", err)
		}
		if i >= existing || nodes[i].state != taskPending {
			return AmendPlanResult{}, fmt.Errorf("retarget: %s is %s; only a waiting (pending) task's dependencies are re-pointed", nodes[i].label(), nodes[i].state)
		}
		if nodes[i].retargetedBy {
			return AmendPlanResult{}, fmt.Errorf("retarget: %s is re-pointed twice", nodes[i].label())
		}
		nodes[i].retargetedBy = true
		retargeted = append(retargeted, i)
	}
	// Dependencies resolve after the cancels, against the whole amended
	// node set, so a task cannot wait on one this amendment cancels.
	for _, i := range retargeted {
		var refs []string
		for _, rt := range am.Retarget {
			if j, _ := resolve(rt.Task); j == i {
				refs = rt.Deps
			}
		}
		deps, err := liveDeps(i, refs)
		if err != nil {
			return AmendPlanResult{}, fmt.Errorf("retarget: %w", err)
		}
		nodes[i].deps = deps
	}
	for k, t := range am.Add {
		i := existing + k
		deps, err := liveDeps(i, t.Deps)
		if err != nil {
			return AmendPlanResult{}, err
		}
		nodes[i].deps = deps
	}

	// The whole graph again, as plan_create validates: no cycle. Names
	// and deps were checked above; the search runs on node indexes, so no
	// user name can collide with anything.
	graph := make([][]int, len(nodes))
	for i, n := range nodes {
		graph[i] = n.deps
	}
	if c := findCycle(graph); c != nil {
		names := make([]string, len(c))
		for k, idx := range c {
			names[k] = nodes[idx].label()
		}
		return AmendPlanResult{}, fmt.Errorf("dependency cycle: %s", strings.Join(names, " -> "))
	}

	// The approval limit (§6.3) over the plan as amended, the cancelled
	// tasks left out.
	settings, err := projectApprovalTx(tx, proj)
	if err != nil {
		return AmendPlanResult{}, err
	}
	var kept []PlanTask
	for _, n := range nodes {
		if n.state != taskCancelled {
			kept = append(kept, PlanTask{Spec: n.spec, Check: n.check, Class: n.class})
		}
	}
	verdict := settings.evaluate(kept)
	newState := state
	flipped := state == "active" && len(am.Add) > 0 && verdict.Needed
	if flipped {
		newState = planAwaitingApproval
	}

	// Write. Cancels first: a cancelled task's live jobs stop with it.
	res := AmendPlanResult{PlanID: plan, State: newState}
	nowStr := fmtTime(now)
	for _, i := range cancelled {
		if err := e.cancelTaskJobsTx(tx, nodes[i].id, now); err != nil {
			return AmendPlanResult{}, err
		}
		if _, err := tx.Exec(`UPDATE tasks SET state = ?, skip_approved = ?, updated_at = ? WHERE id = ?`,
			taskCancelled, nodes[i].skipApproved, nowStr, nodes[i].id); err != nil {
			return AmendPlanResult{}, fmt.Errorf("cancel task %d: %w", nodes[i].id, err)
		}
		res.Cancelled = append(res.Cancelled, nodes[i].id)
	}
	for k := range am.Add {
		i := existing + k
		r, err := tx.Exec(`INSERT INTO tasks (plan_id, name, title, spec, check_cmd, class, state, base_ref, caps, created_at, updated_at)
			VALUES (?, ?, ?, ?, NULLIF(?, ''), ?, ?, ?, '{}', ?, ?)`,
			plan, nodes[i].name, nodes[i].title, nodes[i].spec, nodes[i].check, nodes[i].class, taskPending, baseRef, nowStr, nowStr)
		if err != nil {
			return AmendPlanResult{}, fmt.Errorf("create the task: %w", err)
		}
		if nodes[i].id, err = r.LastInsertId(); err != nil {
			return AmendPlanResult{}, fmt.Errorf("create the task: last insert id: %w", err)
		}
		res.Added = append(res.Added, CreatedTask{TaskID: nodes[i].id, Name: nodes[i].name})
	}
	writeDeps := func(i int) error {
		for _, j := range nodes[i].deps {
			if _, err := tx.Exec(`INSERT INTO task_deps (plan_id, task_id, depends_on_task_id) VALUES (?, ?, ?)`,
				plan, nodes[i].id, nodes[j].id); err != nil {
				return fmt.Errorf("record the dependencies: %w", err)
			}
		}
		return nil
	}
	for _, i := range retargeted {
		if _, err := tx.Exec(`DELETE FROM task_deps WHERE task_id = ?`, nodes[i].id); err != nil {
			return AmendPlanResult{}, fmt.Errorf("drop the dependencies of task %d: %w", nodes[i].id, err)
		}
		// The row update marks the plan for the mirror: a dependency
		// delete has no trigger of its own.
		if _, err := tx.Exec(`UPDATE tasks SET updated_at = ? WHERE id = ?`, nowStr, nodes[i].id); err != nil {
			return AmendPlanResult{}, fmt.Errorf("re-point task %d: %w", nodes[i].id, err)
		}
		if err := writeDeps(i); err != nil {
			return AmendPlanResult{}, err
		}
		res.Retargeted = append(res.Retargeted, nodes[i].id)
	}
	for k := range am.Add {
		if err := writeDeps(existing + k); err != nil {
			return AmendPlanResult{}, err
		}
	}
	if newState != state {
		if _, err := tx.Exec(`UPDATE plans SET state = ?, updated_at = ? WHERE id = ?`, newState, nowStr, plan); err != nil {
			return AmendPlanResult{}, fmt.Errorf("hold plan %d for approval: %w", plan, err)
		}
	} else if _, err := tx.Exec(`UPDATE plans SET updated_at = ? WHERE id = ?`, nowStr, plan); err != nil {
		return AmendPlanResult{}, fmt.Errorf("touch plan %d: %w", plan, err)
	}
	if newState == planAwaitingApproval && verdict.Needed {
		res.Reasons = verdict.reasons(settings.Classes)
		if _, err := appendEventTx(tx, evPlanAwaitingApproval, proj, 0, 0, eventPayload{
			Plan:    plan,
			Count:   int64(len(kept)),
			Classes: verdict.Classes,
			Paths:   verdict.Paths,
			Reason:  strings.Join(res.Reasons, "; "),
		}, now); err != nil {
			return AmendPlanResult{}, err
		}
	}

	// The waiting state re-evaluates as creation does: in an active plan,
	// every pending task whose dependencies are all done starts.
	if newState == "active" {
		started, err := e.readyPlanTasksTx(tx, proj, plan, now)
		if err != nil {
			return AmendPlanResult{}, err
		}
		res.Started = started
		for k := range res.Added {
			for _, s := range started {
				if s.TaskID == res.Added[k].TaskID {
					res.Added[k].JobID = s.JobID
				}
			}
		}
	}

	var change []string
	if len(res.Added) > 0 {
		names := make([]string, len(res.Added))
		for k, a := range res.Added {
			names[k] = fmt.Sprintf("task %d", a.TaskID)
			if a.Name != "" {
				names[k] += " " + a.Name
			}
		}
		change = append(change, "added "+strings.Join(names, ", "))
	}
	if len(res.Cancelled) > 0 {
		change = append(change, "cancelled "+joinIDs(res.Cancelled))
	}
	for _, i := range retargeted {
		to := make([]string, len(nodes[i].deps))
		for k, j := range nodes[i].deps {
			to[k] = nodes[j].label()
		}
		if len(to) == 0 {
			to = []string{"nothing"}
		}
		change = append(change, fmt.Sprintf("re-pointed %s to wait on %s", nodes[i].label(), strings.Join(to, ", ")))
	}
	if flipped {
		change = append(change, "held for approval")
	}
	if _, err := appendEventTx(tx, evPlanAmended, proj, 0, 0, eventPayload{
		Plan:   plan,
		Count:  int64(len(res.Added) + len(res.Cancelled) + len(res.Retargeted)),
		Status: newState,
		Reason: strings.Join(change, "; "),
	}, now); err != nil {
		return AmendPlanResult{}, err
	}

	if newState == "active" {
		closed, err := e.finishPlanIfClosedTx(tx, proj, plan, 0, 0, now)
		if err != nil {
			return AmendPlanResult{}, err
		}
		if closed {
			res.State = planDone
		}
	}
	return res, nil
}

func joinIDs(ids []int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = fmt.Sprintf("task %d", id)
	}
	return strings.Join(parts, ", ")
}

// loadAmendNodesTx reads a plan's tasks, by id, with their dependencies
// as indexes into the same list.
func loadAmendNodesTx(tx *sql.Tx, plan int64) ([]amendNode, error) {
	rows, err := tx.Query(`SELECT id, name, state, title, spec, COALESCE(check_cmd, ''), class, skip_approved
		FROM tasks WHERE plan_id = ? ORDER BY id`, plan)
	if err != nil {
		return nil, fmt.Errorf("read the tasks of plan %d: %w", plan, err)
	}
	var nodes []amendNode
	for rows.Next() {
		var n amendNode
		if err := rows.Scan(&n.id, &n.name, &n.state, &n.title, &n.spec, &n.check, &n.class, &n.skipApproved); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read the tasks of plan %d: %w", plan, err)
		}
		nodes = append(nodes, n)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("read the tasks of plan %d: %w", plan, err)
	}
	rows.Close()
	idx := map[int64]int{}
	for i, n := range nodes {
		idx[n.id] = i
	}
	edges, err := tx.Query(`SELECT task_id, depends_on_task_id FROM task_deps WHERE plan_id = ? ORDER BY task_id, depends_on_task_id`, plan)
	if err != nil {
		return nil, fmt.Errorf("read the dependencies of plan %d: %w", plan, err)
	}
	defer edges.Close()
	for edges.Next() {
		var a, b int64
		if err := edges.Scan(&a, &b); err != nil {
			return nil, fmt.Errorf("read the dependencies of plan %d: %w", plan, err)
		}
		i, ok1 := idx[a]
		j, ok2 := idx[b]
		if ok1 && ok2 {
			nodes[i].deps = append(nodes[i].deps, j)
		}
	}
	return nodes, edges.Err()
}

// cancelTaskJobsTx stops every live job of a task, so a cancelled task
// leaves no worker running or queued.
func (e *Engine) cancelTaskJobsTx(tx *sql.Tx, task int64, now time.Time) error {
	rows, err := tx.Query(`SELECT id FROM jobs WHERE task_id = ? ORDER BY id`, task)
	if err != nil {
		return fmt.Errorf("read the jobs of task %d: %w", task, err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("read the jobs of task %d: %w", task, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("read the jobs of task %d: %w", task, err)
	}
	rows.Close()
	for _, id := range ids {
		job, err := loadJobTx(tx, id)
		if err != nil {
			return err
		}
		if job == nil || isTerminal(job.Status) {
			continue
		}
		if _, err := e.cancelTx(tx, job, "task cancelled by plan_amend", now); err != nil {
			return err
		}
	}
	return nil
}

// readyPlanTasksTx starts every pending task of an active plan whose
// dependencies are all done — none counts — turning it ready with its
// first work job queued, as creation and approval do.
func (e *Engine) readyPlanTasksTx(tx *sql.Tx, project, plan int64, now time.Time) ([]CreatedTask, error) {
	rows, err := tx.Query(`SELECT t.id, t.name, t.spec FROM tasks t
		WHERE t.plan_id = ? AND t.state = ?
		AND NOT EXISTS (SELECT 1 FROM task_deps o JOIN tasks x ON x.id = o.depends_on_task_id
			WHERE o.task_id = t.id AND x.state <> ?)
		ORDER BY t.id`, plan, taskPending, taskDone)
	if err != nil {
		return nil, fmt.Errorf("read the ready tasks of plan %d: %w", plan, err)
	}
	type wave struct {
		id         int64
		name, spec string
	}
	var first []wave
	for rows.Next() {
		var w wave
		if err := rows.Scan(&w.id, &w.name, &w.spec); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read the ready tasks of plan %d: %w", plan, err)
		}
		first = append(first, w)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("read the ready tasks of plan %d: %w", plan, err)
	}
	rows.Close()
	var out []CreatedTask
	for _, w := range first {
		if _, err := tx.Exec(`UPDATE tasks SET state = ?, updated_at = ? WHERE id = ?`,
			taskReady, fmtTime(now), w.id); err != nil {
			return nil, fmt.Errorf("ready task %d: %w", w.id, err)
		}
		sr, err := e.queueTaskWorkTx(tx, project, w.id, w.spec, now)
		if err != nil {
			return nil, err
		}
		out = append(out, CreatedTask{TaskID: w.id, Name: w.name, JobID: sr.JobID})
	}
	return out, nil
}

// finishPlanIfClosedTx finishes an active plan with no live work left
// (§6.2): every task is done, failed or cancelled, or pending behind one
// that will never be done. Those permanently waiting tasks are the plan's
// open end, and the plan_done event — the foreman's done report — names
// them, with any failed task, so the user sees what never ran. The plan
// also counts one attempt on the "plan" class for the foreman's agent:
// a success when every task ended done or was cancelled with the user's
// approval (skip_approved), a failure when a task failed out, was
// cancelled without that approval, or never ran. It reports whether it
// finished the plan.
func (e *Engine) finishPlanIfClosedTx(tx *sql.Tx, project, plan, task, job int64, now time.Time) (bool, error) {
	var state string
	if err := tx.QueryRow("SELECT state FROM plans WHERE id = ?", plan).Scan(&state); err != nil {
		return false, fmt.Errorf("read plan %d: %w", plan, err)
	}
	if state != "active" {
		return false, nil
	}
	nodes, err := loadAmendNodesTx(tx, plan)
	if err != nil {
		return false, err
	}
	var failed, waiting []string
	for _, n := range nodes {
		switch n.state {
		case taskDone:
		case taskCancelled:
			// Only a skip the user approved is a skip: the foreman's own
			// cancellation is an abandoned task, whatever state it was in
			// (a restarted failure is ready, too).
			if !n.skipApproved {
				failed = append(failed, fmt.Sprintf("%s (cancelled without the user's approval)", n.label()))
			}
		case taskFailed:
			failed = append(failed, n.label())
		case taskPending:
			why := make([]string, 0, len(nodes))
			for _, j := range n.deps {
				if nodes[j].state != taskDone {
					why = append(why, fmt.Sprintf("%s %s", nodes[j].label(), nodes[j].state))
				}
			}
			waiting = append(waiting, fmt.Sprintf("%s (waits on %s)", n.label(), strings.Join(why, ", ")))
		default:
			return false, nil // live work remains
		}
	}
	if _, err := tx.Exec(`UPDATE plans SET state = ?, updated_at = ? WHERE id = ?`,
		planDone, fmtTime(now), plan); err != nil {
		return false, fmt.Errorf("finish plan %d: %w", plan, err)
	}
	if err := recordPlanOutcomeTx(tx, project, len(failed) == 0 && len(waiting) == 0, now); err != nil {
		return false, err
	}
	var report []string
	if len(failed) > 0 {
		report = append(report, "failed: "+strings.Join(failed, ", "))
	}
	if len(waiting) > 0 {
		report = append(report, "never ran: "+strings.Join(waiting, "; "))
	}
	if err := inboxEventTx(tx, evPlanDone, project, task, job,
		eventPayload{Status: planDone, Plan: plan, Reason: strings.Join(report, "; ")}, now); err != nil {
		return false, err
	}
	return true, e.wakeForemanTx(tx, project, now)
}

// recordPlanOutcomeTx counts one finished plan on the outcome record's
// plan class (§8) for the project's foreman: the agent a runner
// advertises for the project's foreman harness and model. A project with
// no foreman chosen, or whose foreman no runner advertises, has nobody
// to score and records nothing. Measurement only: no dispatch behavior
// keys off the plan class yet.
func recordPlanOutcomeTx(tx *sql.Tx, project int64, success bool, now time.Time) error {
	var harness, model string
	if err := tx.QueryRow("SELECT foreman_harness, foreman_model FROM projects WHERE id = ?", project).
		Scan(&harness, &model); err != nil {
		return fmt.Errorf("read the foreman of project %d: %w", project, err)
	}
	if harness == "" || model == "" {
		return nil
	}
	var agent int64
	err := tx.QueryRow("SELECT id FROM agents WHERE harness = ? AND model = ? ORDER BY id LIMIT 1", harness, model).Scan(&agent)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the foreman's agent: %w", err)
	}
	successes := 0
	if success {
		successes = 1
	}
	if _, err := tx.Exec(`INSERT INTO outcomes (project_id, agent_id, class, attempts, successes, updated_at)
		VALUES (?, ?, ?, 1, ?, ?)
		ON CONFLICT(project_id, agent_id, class) DO UPDATE SET
			attempts = attempts + 1, successes = successes + excluded.successes, updated_at = excluded.updated_at`,
		project, agent, classPlan, successes, fmtTime(now)); err != nil {
		return fmt.Errorf("record the outcome of the plan: %w", err)
	}
	return nil
}
