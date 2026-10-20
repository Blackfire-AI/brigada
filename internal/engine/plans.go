package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Blackfire-AI/brigada/internal/adapter"
	"github.com/Blackfire-AI/brigada/internal/prompt"
)

// PlanTask is one task of a plan as its creator names it (§6.1): the
// title, the spec verbatim (§1.4), the check the runner runs after a
// done (§5.5), and the class. Name is the short handle the plan's author
// gives it so others can depend on it, and Deps the names of the tasks of
// the same plan it waits for (§6.1). Caps and per-task repos arrive with
// the plan tools of phase 3.
type PlanTask struct {
	Title  string
	Name   string // '' = unnamed: nothing can depend on it
	Spec   string
	Check  string
	Class  string // '' = implement
	Tier   string // '' = standard
	Effort string // '' = the agent's own level
	// ReviewTier and ReviewEffort are the task's own tier and effort for
	// its reviews (§7, §8); '' = the engine default (review.tier,
	// review.effort).
	ReviewTier   string
	ReviewEffort string
	// Caps overrides the engine's round caps for this task, key by key
	// (fix_rounds, review_rounds, restart; 0 to 10); nil keeps them all.
	Caps map[string]int
	Deps []string
	// Base names the one of Deps whose result branch this task's work
	// starts from; '' = the only dep when there is exactly one, the
	// continued dep when Continue names one, else the project's default
	// branch.
	Base string
	// Continue names the one of Deps whose author's session this task's
	// first work job resumes (§6.1): the worker that wrote the dep's
	// approved result starts the new task warm, in its own session.
	// '' = start fresh like any other task.
	Continue string
	// ExtraRepos are the read-only repositories the task is given (§5.8).
	// nil = inherit the project default; non-nil, even empty, replaces it.
	ExtraRepos []ExtraRepo
}

// CreatedTask is one task CreatePlan made: its id, its name, and the
// work job queued for it — zero for a task that waits on dependencies.
type CreatedTask struct {
	TaskID int64
	Name   string
	JobID  int64
}

// CreatePlanResult is what CreatePlan did: the plan, its tasks, and the
// first ready task's work job's own submit answer — created or, should
// a live job already hold the key, pending. TaskID, JobID and Run are
// that first ready task's: for the one-task plan, the task.
type CreatePlanResult struct {
	Answer Answer
	PlanID int64
	TaskID int64
	JobID  int64
	Run    int
	Tasks  []CreatedTask
	// State is the plan's: "active", or planAwaitingApproval when the
	// approval limit holds it (§6.3), with Reasons — one phrase each —
	// saying why. A held plan queued no job: TaskID, JobID and Run are
	// zero.
	State   string
	Reasons []string
}

// CreatePlan is the foreman's plan_create (§6.2, §12.2): one plan
// holding its tasks (§6.1), active from the start unless the project's
// approval limit (§6.3) holds it for the user's yes — then no job is
// queued and the plan_awaiting_approval event says why. A task with no dependencies is ready
// and its first work job is queued on the task's key, its prompt
// composed here from the versioned template plus the spec verbatim
// (§1.4, §5.3); a task with dependencies waits, pending, until the last
// of them is done. The graph is validated whole before anything is
// written: a dependency naming no task of the plan, a self-dependency
// or a cycle refuses the plan. Plan, tasks, edges, jobs, the
// plan_created event and the task_spec messages land in one
// transaction, so a caller never sees a plan half made.
func (e *Engine) CreatePlan(ctx context.Context, project int64, title string, tasks []PlanTask) (CreatePlanResult, error) {
	var res CreatePlanResult
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := e.sweepTx(tx, now); err != nil {
			return err
		}
		r, err := e.createPlanTx(tx, project, title, tasks, now)
		if err != nil {
			return fmt.Errorf("create plan %q: %w", title, err)
		}
		res = r
		return nil
	})
	return res, err
}

// validatePlanGraph checks a plan's names and dependencies (§6.1) and
// returns each task's dependencies as indexes into tasks, duplicates
// folded, each task's base as an index into tasks (-1 = none named), and
// each task's continued dep the same way. Names are unique where set;
// every dep names a task of the plan and not its own; a base or a
// continued dep is one of the task's own deps; the continued dep is the
// task's base (§6.1): a base naming another dep is refused, and a base
// none names defaults to the continued dep; the graph has no cycle, the
// refusal naming the cycle's tasks.
func validatePlanGraph(tasks []PlanTask) ([][]int, []int, []int, error) {
	byName := map[string]int{}
	for i, t := range tasks {
		if t.Name == "" {
			continue
		}
		if j, dup := byName[t.Name]; dup {
			return nil, nil, nil, fmt.Errorf("task name %q is used by tasks %d and %d: names are unique in a plan", t.Name, j+1, i+1)
		}
		byName[t.Name] = i
	}
	label := func(i int) string {
		if tasks[i].Name != "" {
			return tasks[i].Name
		}
		return fmt.Sprintf("task %d", i+1)
	}
	deps := make([][]int, len(tasks))
	bases := make([]int, len(tasks))
	continues := make([]int, len(tasks))
	for i, t := range tasks {
		bases[i] = -1
		continues[i] = -1
		seen := map[int]bool{}
		for _, d := range t.Deps {
			j, ok := byName[d]
			switch {
			case !ok:
				return nil, nil, nil, fmt.Errorf("%s depends on %q, which names no task of this plan", label(i), d)
			case j == i:
				return nil, nil, nil, fmt.Errorf("%s depends on itself", label(i))
			}
			if !seen[j] {
				seen[j] = true
				deps[i] = append(deps[i], j)
			}
		}
		if t.Base != "" {
			j, ok := byName[t.Base]
			if !ok || !seen[j] {
				return nil, nil, nil, fmt.Errorf("%s has base %q, which is not one of its deps: a base is the name of a task it depends on", label(i), t.Base)
			}
			bases[i] = j
		}
		if t.Continue != "" {
			j, ok := byName[t.Continue]
			if !ok || !seen[j] {
				return nil, nil, nil, fmt.Errorf("%s continues %q, which is not one of its deps: a continued dep is the name of a task it depends on", label(i), t.Continue)
			}
			continues[i] = j
			// The continued dep is the base (§6.1): the work starts
			// from the branch of the dep whose session it resumes.
			switch {
			case t.Base == "":
				bases[i] = j
			case bases[i] != j:
				return nil, nil, nil, fmt.Errorf("%s has base %q but continues %q: a continuing task's base is its continued dep", label(i), t.Base, t.Continue)
			}
		}
	}
	if c := findCycle(deps); c != nil {
		names := make([]string, len(c))
		for k, idx := range c {
			names[k] = label(idx)
		}
		return nil, nil, nil, fmt.Errorf("dependency cycle: %s", strings.Join(names, " -> "))
	}
	return deps, bases, continues, nil
}

// findCycle searches a dependency graph (each node's dependencies as
// indexes) for a cycle and returns its nodes, the first repeated at the
// end; nil when the graph is acyclic.
func findCycle(deps [][]int) []int {
	// Depth-first search: state 1 = on the current path, 2 = finished
	// with no cycle through it.
	state := make([]int, len(deps))
	var path []int
	var visit func(i int) []int
	visit = func(i int) []int {
		state[i] = 1
		path = append(path, i)
		for _, j := range deps[i] {
			switch state[j] {
			case 1:
				for k, p := range path {
					if p == j {
						return append(append([]int{}, path[k:]...), j)
					}
				}
			case 0:
				if c := visit(j); c != nil {
					return c
				}
			}
		}
		path = path[:len(path)-1]
		state[i] = 2
		return nil
	}
	for i := range deps {
		if state[i] != 0 {
			continue
		}
		if c := visit(i); c != nil {
			return c
		}
	}
	return nil
}

func (e *Engine) createPlanTx(tx *sql.Tx, project int64, title string, tasks []PlanTask, now time.Time) (CreatePlanResult, error) {
	if err := activeProjectTx(tx, project); err != nil {
		return CreatePlanResult{}, err
	}
	if title == "" {
		return CreatePlanResult{}, fmt.Errorf("plan title is required")
	}
	if len(tasks) == 0 {
		return CreatePlanResult{}, fmt.Errorf("a plan takes at least one task")
	}
	for i, t := range tasks {
		if err := validateTaskTierEffort(t); err != nil {
			return CreatePlanResult{}, fmt.Errorf("task %d %q: %w", i+1, t.Name, err)
		}
		if err := validateTaskCaps(t.Caps); err != nil {
			return CreatePlanResult{}, fmt.Errorf("task %d %q: %w", i+1, t.Name, err)
		}
		if t.Spec == "" {
			return CreatePlanResult{}, fmt.Errorf("task %d: spec is required: the worker's task, verbatim (§1.4)", i+1)
		}
	}
	deps, bases, continues, err := validatePlanGraph(tasks)
	if err != nil {
		return CreatePlanResult{}, err
	}
	// The worktree starts from the project's default branch: a plan's
	// tasks work the project's repo (§6.1), and per-task repos are the
	// plan phase's to add.
	var baseRef string
	err = tx.QueryRow("SELECT default_branch FROM projects WHERE id = ?", project).Scan(&baseRef)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return CreatePlanResult{}, fmt.Errorf("no such project %d", project)
	case err != nil:
		return CreatePlanResult{}, fmt.Errorf("read project %d: %w", project, err)
	}
	nowStr := fmtTime(now)

	// The approval limit (§6.3), after validation and before any work
	// is queued.
	settings, err := projectApprovalTx(tx, project)
	if err != nil {
		return CreatePlanResult{}, err
	}
	tasks = settings.withDefaultCheck(tasks)
	// The extra repositories (§5.8) resolve once, here: each task's own
	// list canonicalized, the effective list (its own or the project
	// default) recorded with the task.
	extraDef, err := projectExtraReposTx(tx, project)
	if err != nil {
		return CreatePlanResult{}, err
	}
	tasks, extraRecs, err := resolveTaskExtras(extraDef, tasks, func(i int) string { return fmt.Sprintf("task %d", i+1) })
	if err != nil {
		return CreatePlanResult{}, err
	}
	verdict := settings.evaluate(tasks)
	planState := "active"
	if verdict.Needed {
		planState = planAwaitingApproval
	}

	plan, err := tx.Exec(`INSERT INTO plans (project_id, title, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)`, project, title, planState, nowStr, nowStr)
	if err != nil {
		return CreatePlanResult{}, fmt.Errorf("create the plan: %w", err)
	}
	planID, err := plan.LastInsertId()
	if err != nil {
		return CreatePlanResult{}, fmt.Errorf("create the plan: last insert id: %w", err)
	}
	if _, err := appendEventTx(tx, evPlanCreated, project, 0, 0,
		eventPayload{Answer: string(AnsCreated)}, now); err != nil {
		return CreatePlanResult{}, err
	}

	res := CreatePlanResult{PlanID: planID, State: planState, Tasks: make([]CreatedTask, len(tasks))}
	if verdict.Needed {
		res.Reasons = verdict.reasons(settings.Classes)
		if _, err := appendEventTx(tx, evPlanAwaitingApproval, project, 0, 0, eventPayload{
			Plan:    planID,
			Count:   int64(len(tasks)),
			Classes: verdict.Classes,
			Paths:   verdict.Paths,
			Reason:  strings.Join(res.Reasons, "; "),
		}, now); err != nil {
			return CreatePlanResult{}, err
		}
	}

	// The tasks (§6.1): ready at once when they wait for nothing, on the
	// project's repo and base ref; pending while a dependency is open —
	// and, in a plan awaiting approval, all pending: the approval readies
	// the first wave.
	setupRec, err := taskProjectSetupTx(tx, project)
	if err != nil {
		return CreatePlanResult{}, err
	}
	for i, t := range tasks {
		class := t.Class
		if class == "" {
			class = "implement"
		}
		taskTitle := t.Title
		if taskTitle == "" {
			taskTitle = taskTitleOf(t.Spec)
		}
		state := taskReady
		if len(deps[i]) > 0 || verdict.Needed {
			state = taskPending
		}
		r, err := tx.Exec(`INSERT INTO tasks (plan_id, name, title, spec, check_cmd, class, state, base_ref, caps, setup, extra_repos, tier, effective_tier, effort, effective_effort, review_tier, review_effort, created_at, updated_at)
			VALUES (?, ?, ?, ?, NULLIF(?, ''), ?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), ?, ?, ?, ?, ?, ?, ?, ?)`,
			planID, t.Name, taskTitle, t.Spec, t.Check, class, state, baseRef, taskCapsJSON(t.Caps), setupRec, extraRecs[i], taskTier(t.Tier), taskTier(t.Tier), t.Effort, t.Effort, t.ReviewTier, t.ReviewEffort, nowStr, nowStr)
		if err != nil {
			return CreatePlanResult{}, fmt.Errorf("create the task: %w", err)
		}
		taskID, err := r.LastInsertId()
		if err != nil {
			return CreatePlanResult{}, fmt.Errorf("create the task: last insert id: %w", err)
		}
		if err := stampTaskInstructionsTx(tx, project, taskID, now); err != nil {
			return CreatePlanResult{}, err
		}
		res.Tasks[i] = CreatedTask{TaskID: taskID, Name: t.Name}
	}
	for i, ds := range deps {
		for _, j := range ds {
			if _, err := tx.Exec(`INSERT INTO task_deps (plan_id, task_id, depends_on_task_id, is_base, is_continue) VALUES (?, ?, ?, ?, ?)`,
				planID, res.Tasks[i].TaskID, res.Tasks[j].TaskID, bases[i] == j, continues[i] == j); err != nil {
				return CreatePlanResult{}, fmt.Errorf("record the dependencies: %w", err)
			}
		}
	}

	if verdict.Needed {
		return res, nil
	}

	// The first work job of every ready task (§1.4). The first of them
	// answers for the plan.
	first := true
	for i, t := range tasks {
		if len(deps[i]) > 0 {
			continue
		}
		sr, err := e.queueTaskWorkTx(tx, project, res.Tasks[i].TaskID, t.Spec, now)
		if err != nil {
			return CreatePlanResult{}, err
		}
		res.Tasks[i].JobID = sr.JobID
		if first {
			first = false
			res.Answer, res.TaskID, res.JobID, res.Run = sr.Answer, res.Tasks[i].TaskID, sr.JobID, sr.Run
		}
	}
	return res, nil
}

// queueTaskWorkTx queues a task's first work job on the task's key, its
// prompt composed here (§1.4): only the engine builds prompts, from the
// versioned template plus the spec verbatim. submitTx appends the
// job_created event and the task_spec message, as it does for the debug
// submit. A task that continues one of its deps (§6.1) gets session mode
// on the dep's author instead, resuming its session
// (continuePlacementTx).
func (e *Engine) queueTaskWorkTx(tx *sql.Tx, project, taskID int64, spec string, now time.Time) (SubmitResult, error) {
	if err := resolveBaseTx(tx, taskID, now); err != nil {
		return SubmitResult{}, err
	}
	check, err := taskCheckTx(tx, taskID)
	if err != nil {
		return SubmitResult{}, err
	}
	instructions, err := taskInstructionsTx(tx, taskID)
	if err != nil {
		return SubmitResult{}, err
	}
	params := SubmitParams{
		Key:       fmt.Sprintf("task:%d", taskID),
		ProjectID: project,
		TaskID:    taskID,
		Kind:      KindWork,
		Mode:      ModeFresh,
		Prompt:    prompt.ComposeWorkerPrompt(spec, check, "", prompt.WithInstructions(instructions)),
	}
	if cont, err := e.continuePlacementTx(tx, taskID, now); err != nil {
		return SubmitResult{}, err
	} else if cont != nil {
		// The key's session becomes the dep author's (§4.2): the lease
		// reads the resume id from it, and its runner affinity keeps the
		// job on the runner that holds the session (§8), beside the agent
		// pin.
		if err := upsertSessionTx(tx, params.Key, cont.runner, cont.session, now); err != nil {
			return SubmitResult{}, err
		}
		params.Mode = ModeSession
		params.Harness = cont.harness
		params.RequireAgent = cont.agent
		params.Prompt = prompt.ComposeContinuePrompt(spec, check, prompt.WithInstructions(instructions))
	}
	return e.submitTx(tx, params, now)
}

// resolveBaseTx writes a task's base_ref when its work is queued (§6.1):
// the final result branch of the dependency it stacks on, as the engine
// recorded it on that task (a restart's -r<n> name included), so the
// worktree starts from the dependency's work and the review's diff
// base…branch shows only the task's own change. The stacked dependency
// is the one named base, else the continued dep (§6.1), else the only
// one; with several and neither named, or with none, the row keeps the
// project's default branch it was created with. A dependency that
// recorded no branch (a task that pushed none) leaves the default branch
// too. Every path that readies a task — creation, release, approval,
// amendment — queues through here.
func resolveBaseTx(tx *sql.Tx, taskID int64, now time.Time) error {
	var branch string
	err := tx.QueryRow(`SELECT COALESCE(x.branch, '') FROM task_deps d JOIN tasks x ON x.id = d.depends_on_task_id
		WHERE d.task_id = ? AND (d.is_base = 1 OR d.is_continue = 1 OR (SELECT COUNT(*) FROM task_deps o WHERE o.task_id = d.task_id) = 1)
		ORDER BY d.is_base DESC, d.is_continue DESC LIMIT 1`, taskID).Scan(&branch)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && branch == "") {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the base of task %d: %w", taskID, err)
	}
	if _, err := tx.Exec(`UPDATE tasks SET base_ref = ?, updated_at = ? WHERE id = ?`, branch, fmtTime(now), taskID); err != nil {
		return fmt.Errorf("write the base of task %d: %w", taskID, err)
	}
	return nil
}

// releaseDependentsTx frees the tasks that waited on a task now done
// (§6.1): each pending dependent whose dependencies are all done turns
// ready and has its first work job queued, in the completion's own
// transaction. A dependent with another dependency still open keeps
// waiting, and so does one whose dependency failed or parked at
// needs_foreman: that dependency never reaches done, and the foreman
// decides the dependent's fate.
func (e *Engine) releaseDependentsTx(tx *sql.Tx, project, done int64, now time.Time) error {
	rows, err := tx.Query(`SELECT t.id, t.spec FROM task_deps d JOIN tasks t ON t.id = d.task_id
		JOIN plans p ON p.id = t.plan_id AND p.state = 'active'
		WHERE d.depends_on_task_id = ? AND t.state = ?
		AND NOT EXISTS (SELECT 1 FROM task_deps o JOIN tasks x ON x.id = o.depends_on_task_id
			WHERE o.task_id = t.id AND x.state <> ?)
		ORDER BY t.id`, done, taskPending, taskDone)
	if err != nil {
		return fmt.Errorf("read the dependents of task %d: %w", done, err)
	}
	type freed struct {
		id   int64
		spec string
	}
	var free []freed
	for rows.Next() {
		var f freed
		if err := rows.Scan(&f.id, &f.spec); err != nil {
			rows.Close()
			return fmt.Errorf("read the dependents of task %d: %w", done, err)
		}
		free = append(free, f)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("read the dependents of task %d: %w", done, err)
	}
	rows.Close()
	for _, f := range free {
		if _, err := tx.Exec(`UPDATE tasks SET state = ?, updated_at = ? WHERE id = ?`,
			taskReady, fmtTime(now), f.id); err != nil {
			return fmt.Errorf("ready task %d: %w", f.id, err)
		}
		if _, err := e.queueTaskWorkTx(tx, project, f.id, f.spec, now); err != nil {
			return err
		}
	}
	return nil
}

func taskTier(tier string) string {
	if tier == "" {
		return adapter.TierStandard
	}
	return tier
}

func validateTaskTierEffort(t PlanTask) error {
	if err := adapter.ValidateTier(t.Tier); err != nil {
		return err
	}
	if err := adapter.ValidateTaskEffort(t.Effort); err != nil {
		return err
	}
	return validateReviewTierEffort(t.ReviewTier, t.ReviewEffort)
}

// validateReviewTierEffort checks a task's own review tier and review
// effort; unset keeps the engine default.
func validateReviewTierEffort(tier, effort string) error {
	if err := adapter.ValidateTier(tier); err != nil {
		return fmt.Errorf("review %w", err)
	}
	if err := adapter.ValidateTaskEffort(effort); err != nil {
		return fmt.Errorf("review %w", err)
	}
	return nil
}
