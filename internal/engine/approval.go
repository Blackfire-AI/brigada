package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// The approval limit (§6.3): a plan the user has not pre-approved starts
// on its own only below a size and risk limit; above it the plan waits,
// in plan state planAwaitingApproval, for the user's yes.
//
// The plan state's stored word is "pending_approval": the schema's CHECK
// on plans.state and the design name it so, and SQLite cannot widen a
// CHECK without rebuilding the table the tasks cascade from. The event
// that announces it is plan_awaiting_approval.
const planAwaitingApproval = "pending_approval"

// Plan states the user's decision sets.
const planCancelled = "cancelled"

// The shipped approval limit (§6.3's proposal).
const defaultApprovalMaxTasks = 5

var defaultApprovalClasses = []string{"implement", "review", "answer"}

// Project setting keys, in the projects.settings JSON (§6.3 "Configurable
// per project"):
//
//	approval_max_tasks          int; more tasks than this need approval.
//	                            Absent = 5. 0 approvals every plan (never
//	                            runs on its own); negative disables the
//	                            approval entirely (everything runs).
//	approval_classes            list of classes (or a comma-separated
//	                            string) that run without approval. Absent
//	                            or empty = implement,review,answer.
//	approval_protected_paths    list of glob patterns; a task whose spec
//	                            or check mentions a matching path needs
//	                            approval. Absent = none.
type approvalSettings struct {
	MaxTasks       *int       `json:"approval_max_tasks"`
	Classes        stringList `json:"approval_classes"`
	ProtectedPaths stringList `json:"approval_protected_paths"`
	DefaultCheck   string     `json:"default_check"`
}

// withDefaultCheck returns the tasks with every omitted check set to the
// project's default check (§6.1), resolved now so the task stores it and
// a later change of the default leaves it alone. No default: unchanged.
func (s approvalSettings) withDefaultCheck(tasks []PlanTask) []PlanTask {
	if s.DefaultCheck == "" {
		return tasks
	}
	out := make([]PlanTask, len(tasks))
	copy(out, tasks)
	for i := range out {
		if out[i].Check == "" {
			out[i].Check = s.DefaultCheck
		}
	}
	return out
}

// stringList reads a JSON array of strings, or one comma-separated
// string, as the settings' list values.
type stringList []string

func (l *stringList) UnmarshalJSON(b []byte) error {
	var one string
	if json.Unmarshal(b, &one) == nil {
		*l = nil
		for _, s := range strings.Split(one, ",") {
			if s = strings.TrimSpace(s); s != "" {
				*l = append(*l, s)
			}
		}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*l = nil
	for _, s := range many {
		if s = strings.TrimSpace(s); s != "" {
			*l = append(*l, s)
		}
	}
	return nil
}

// approvalVerdict is what the limit made of a plan: Needed, and the
// reasons it gives — the task count when it is over the limit, the
// classes outside the allowed set, the protected paths its specs and
// checks mention.
type approvalVerdict struct {
	Needed  bool
	Tasks   int
	Limit   int
	Always  bool // the limit is 0: every plan waits
	Classes []string
	Paths   []string
}

// reasons renders the verdict as short phrases, one per reason.
func (v approvalVerdict) reasons(allowed []string) []string {
	var out []string
	if v.Always {
		out = append(out, "every plan needs approval in this project (approval_max_tasks 0)")
	} else if v.Limit >= 0 && v.Tasks > v.Limit {
		out = append(out, fmt.Sprintf("%d tasks, over the limit of %d", v.Tasks, v.Limit))
	}
	if len(v.Classes) > 0 {
		out = append(out, fmt.Sprintf("class %s outside %s", strings.Join(v.Classes, ", "), strings.Join(allowed, ",")))
	}
	if len(v.Paths) > 0 {
		out = append(out, "protected paths: "+strings.Join(v.Paths, ", "))
	}
	return out
}

// projectApprovalTx reads the project's approval settings with the
// defaults applied.
func projectApprovalTx(tx *sql.Tx, project int64) (approvalSettings, error) {
	var raw string
	if err := tx.QueryRow("SELECT settings FROM projects WHERE id = ?", project).Scan(&raw); err != nil {
		return approvalSettings{}, fmt.Errorf("read the settings of project %d: %w", project, err)
	}
	var s approvalSettings
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &s); err != nil {
			return approvalSettings{}, fmt.Errorf("parse the settings of project %d: %w", project, err)
		}
	}
	if s.MaxTasks == nil {
		d := defaultApprovalMaxTasks
		s.MaxTasks = &d
	}
	if len(s.Classes) == 0 {
		s.Classes = defaultApprovalClasses
	}
	return s, nil
}

// evaluate applies the limit to a plan's tasks (§6.3).
func (s approvalSettings) evaluate(tasks []PlanTask) approvalVerdict {
	v := approvalVerdict{Tasks: len(tasks), Limit: *s.MaxTasks}
	if v.Limit < 0 {
		return v // approval disabled: everything runs
	}
	if v.Limit == 0 {
		v.Needed, v.Always = true, true
	} else if v.Tasks > v.Limit {
		v.Needed = true
	}
	allowed := map[string]bool{}
	for _, c := range s.Classes {
		allowed[c] = true
	}
	seenClass, seenPath := map[string]bool{}, map[string]bool{}
	for _, t := range tasks {
		class := t.Class
		if class == "" {
			class = classImplement
		}
		if !allowed[class] && !seenClass[class] {
			seenClass[class] = true
			v.Classes = append(v.Classes, class)
			v.Needed = true
		}
		for _, p := range matchProtected(s.ProtectedPaths, t.Spec+"\n"+t.Check) {
			if !seenPath[p] {
				seenPath[p] = true
				v.Paths = append(v.Paths, p)
				v.Needed = true
			}
		}
	}
	return v
}

// matchProtected returns the patterns that match a path the text
// mentions. The documented choice, per pattern:
//
//   - a pattern with no glob character (* ? [) matches by simple
//     containment: the text contains it;
//   - a pattern with glob characters is tried with filepath.Match against
//     every path-like word of the text (split on whitespace, quotes and
//     brackets; a leading "./" dropped), so "internal/store/*.sql"
//     matches "internal/store/migrations.sql" but not a deeper path;
//     a pattern ending in "/**" matches everything under its directory.
func matchProtected(patterns []string, text string) []string {
	if len(patterns) == 0 {
		return nil
	}
	words := strings.FieldsFunc(text, func(r rune) bool {
		switch r {
		case ' ', '\t', '\n', '\r', '"', '\'', '`', ',', ';', ':', '(', ')', '[', ']', '{', '}', '<', '>', '=':
			return true
		}
		return false
	})
	for i, w := range words {
		w = strings.TrimPrefix(w, "./")
		words[i] = strings.TrimRight(w, ".")
	}
	var hit []string
	for _, p := range patterns {
		if !strings.ContainsAny(p, "*?[") {
			if strings.Contains(text, p) {
				hit = append(hit, p)
			}
			continue
		}
		dir := ""
		if strings.HasSuffix(p, "/**") {
			dir = strings.TrimSuffix(p, "**")
		}
		for _, w := range words {
			if dir != "" && strings.HasPrefix(w, dir) {
				hit = append(hit, p)
				break
			}
			if ok, err := filepath.Match(p, w); err == nil && ok {
				hit = append(hit, p)
				break
			}
		}
	}
	return hit
}

// Refusals the plan decisions give the caller.
var (
	ErrNoPlan          = errors.New("no such plan")
	ErrPlanNotAwaiting = errors.New("plan is not awaiting approval")
)

// PlanDecision is what approving or rejecting a plan did: the plan's
// new state and, for an approval, the first wave's tasks with the work
// job queued for each.
type PlanDecision struct {
	PlanID int64
	State  string
	Tasks  []CreatedTask
}

// ApprovePlan is the user's yes (§6.3): the plan turns active and its
// first wave — the tasks with no dependencies — turns ready with its
// work jobs queued, exactly as an active plan's would have been, all in
// one transaction. project scopes the plan to one project; 0 names any.
// The foreman never calls it: confirmation is the user's.
func (e *Engine) ApprovePlan(ctx context.Context, project, plan int64) (PlanDecision, error) {
	var res PlanDecision
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := e.sweepTx(tx, now); err != nil {
			return err
		}
		proj, err := awaitingPlanTx(tx, project, plan)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE plans SET state = 'active', updated_at = ? WHERE id = ?`, fmtTime(now), plan); err != nil {
			return fmt.Errorf("activate plan %d: %w", plan, err)
		}
		// The first wave: the pending tasks waiting for nothing open (a
		// plan an amendment held may have tasks whose dependencies are
		// already done).
		first, err := e.readyPlanTasksTx(tx, proj, plan, now)
		if err != nil {
			return err
		}
		res = PlanDecision{PlanID: plan, State: "active", Tasks: first}
		if err := inboxEventTx(tx, evPlanApproved, proj, 0, 0,
			eventPayload{Plan: plan, Status: "active", Count: int64(len(first))}, now); err != nil {
			return err
		}
		// An amendment may have left nothing to run (every task done,
		// cancelled or stuck behind one): the approved plan is finished.
		closed, err := e.finishPlanIfClosedTx(tx, proj, plan, 0, 0, now)
		if err != nil {
			return err
		}
		if closed {
			res.State = planDone
		}
		return e.wakeForemanTx(tx, proj, now)
	})
	if err != nil {
		return PlanDecision{}, fmt.Errorf("approve plan %d: %w", plan, err)
	}
	return res, nil
}

// RejectPlan is the user's no (§6.3): the plan and its tasks are
// cancelled; nothing was queued, so no job needs stopping. The foreman
// hears of it through its inbox.
func (e *Engine) RejectPlan(ctx context.Context, project, plan int64) (PlanDecision, error) {
	var res PlanDecision
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := e.sweepTx(tx, now); err != nil {
			return err
		}
		proj, err := awaitingPlanTx(tx, project, plan)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE plans SET state = ?, updated_at = ? WHERE id = ?`, planCancelled, fmtTime(now), plan); err != nil {
			return fmt.Errorf("cancel plan %d: %w", plan, err)
		}
		// A plan an amendment held may have tasks already running.
		nodes, err := loadAmendNodesTx(tx, plan)
		if err != nil {
			return err
		}
		for _, n := range nodes {
			if n.state != taskDone && n.state != taskCancelled {
				if err := e.cancelTaskJobsTx(tx, n.id, now); err != nil {
					return err
				}
			}
		}
		if _, err := tx.Exec(`UPDATE tasks SET state = ?, updated_at = ? WHERE plan_id = ? AND state NOT IN (?, ?)`,
			taskCancelled, fmtTime(now), plan, taskDone, taskCancelled); err != nil {
			return fmt.Errorf("cancel the tasks of plan %d: %w", plan, err)
		}
		if err := recordPlanOutcomeTx(tx, proj, false, now); err != nil {
			return err
		}
		res = PlanDecision{PlanID: plan, State: planCancelled}
		if err := inboxEventTx(tx, evPlanRejected, proj, 0, 0,
			eventPayload{Plan: plan, Status: planCancelled}, now); err != nil {
			return err
		}
		return e.wakeForemanTx(tx, proj, now)
	})
	if err != nil {
		return PlanDecision{}, fmt.Errorf("reject plan %d: %w", plan, err)
	}
	return res, nil
}

// awaitingPlanTx finds the plan and checks it waits for approval,
// returning its project.
func awaitingPlanTx(tx *sql.Tx, project, plan int64) (int64, error) {
	var proj int64
	var state string
	err := tx.QueryRow("SELECT project_id, state FROM plans WHERE id = ?", plan).Scan(&proj, &state)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && project != 0 && proj != project) {
		return 0, fmt.Errorf("%w: %d", ErrNoPlan, plan)
	}
	if err != nil {
		return 0, fmt.Errorf("read plan %d: %w", plan, err)
	}
	if state != planAwaitingApproval {
		return 0, fmt.Errorf("%w: plan %d is %s", ErrPlanNotAwaiting, plan, state)
	}
	return proj, nil
}
