package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Blackfire-AI/brigada/internal/prompt"
)

// oneTaskRule is the refusal a plan of any other size earns (§6.1):
// dependencies arrive with plans in phase 3, until then a plan is one
// task, so the foreman splits its work into one-task plans.
const oneTaskRule = "one task per plan until plans with dependencies (phase 3)"

// PlanTask is one task of a plan as its creator names it (§6.1): the
// title, the spec verbatim (§1.4), the check the runner runs after a
// done (§5.5), and the class. Dependencies, caps and per-task repos
// arrive with the plan tools of phase 3.
type PlanTask struct {
	Title string
	Spec  string
	Check string
	Class string // '' = implement
}

// CreatePlanResult is what CreatePlan did: the plan, its one task, and
// the first work job's own submit answer — created or, should a live
// job already hold the key, pending.
type CreatePlanResult struct {
	Answer Answer
	PlanID int64
	TaskID int64
	JobID  int64
	Run    int
}

// CreatePlan is the foreman's plan_create (§6.2, §12.2): one plan,
// active from the start — the approval limit arrives with the plan
// phase — holding exactly one ready task whose first work job is
// queued on the task's key, its prompt composed here from the versioned
// template plus the spec verbatim (§1.4, §5.3). Plan, task, job, the
// plan_created event and the task_spec message land in one transaction,
// so a caller never sees a plan whose task or job failed to appear.
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

func (e *Engine) createPlanTx(tx *sql.Tx, project int64, title string, tasks []PlanTask, now time.Time) (CreatePlanResult, error) {
	if title == "" {
		return CreatePlanResult{}, fmt.Errorf("plan title is required")
	}
	if len(tasks) != 1 {
		return CreatePlanResult{}, fmt.Errorf("a plan takes exactly one task, got %d: %s", len(tasks), oneTaskRule)
	}
	t := tasks[0]
	if t.Spec == "" {
		return CreatePlanResult{}, fmt.Errorf("task spec is required: the worker's task, verbatim (§1.4)")
	}
	// The worktree starts from the project's default branch: a plan's
	// tasks work the project's repo (§6.1), and per-task repos are the
	// plan phase's to add.
	var baseRef string
	err := tx.QueryRow("SELECT default_branch FROM projects WHERE id = ?", project).Scan(&baseRef)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return CreatePlanResult{}, fmt.Errorf("no such project %d", project)
	case err != nil:
		return CreatePlanResult{}, fmt.Errorf("read project %d: %w", project, err)
	}
	class := t.Class
	if class == "" {
		class = "implement"
	}
	taskTitle := t.Title
	if taskTitle == "" {
		taskTitle = taskTitleOf(t.Spec)
	}
	nowStr := fmtTime(now)

	plan, err := tx.Exec(`INSERT INTO plans (project_id, title, state, created_at, updated_at)
		VALUES (?, ?, 'active', ?, ?)`, project, title, nowStr, nowStr)
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

	// The task (§6.1): ready at once — its plan has no other task to
	// wait for — on the project's repo and base ref.
	task, err := tx.Exec(`INSERT INTO tasks (plan_id, title, spec, check_cmd, class, state, base_ref, caps, created_at, updated_at)
		VALUES (?, ?, ?, NULLIF(?, ''), ?, 'ready', ?, '{}', ?, ?)`,
		planID, taskTitle, t.Spec, t.Check, class, baseRef, nowStr, nowStr)
	if err != nil {
		return CreatePlanResult{}, fmt.Errorf("create the task: %w", err)
	}
	taskID, err := task.LastInsertId()
	if err != nil {
		return CreatePlanResult{}, fmt.Errorf("create the task: last insert id: %w", err)
	}

	// The first work job, its prompt composed here (§1.4): only the
	// engine builds prompts, from the versioned template plus the spec
	// verbatim. submitTx appends the job_created event and the task_spec
	// message, as it does for the debug submit.
	sr, err := e.submitTx(tx, SubmitParams{
		Key:       fmt.Sprintf("task:%d", taskID),
		ProjectID: project,
		TaskID:    taskID,
		Kind:      KindWork,
		Mode:      ModeFresh,
		Prompt:    prompt.ComposeWorkerPrompt(t.Spec, ""),
	}, now)
	if err != nil {
		return CreatePlanResult{}, err
	}
	return CreatePlanResult{
		Answer: sr.Answer,
		PlanID: planID,
		TaskID: taskID,
		JobID:  sr.JobID,
		Run:    sr.Run,
	}, nil
}
