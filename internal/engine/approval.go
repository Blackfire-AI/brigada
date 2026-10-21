package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
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
//	                            approval, and at the land gate a task
//	                            whose recorded changed paths match holds
//	                            the landing (§6.3, §7.1). Absent = none.
//	gate_plan                   auto or ask; ask holds every plan at the
//	                            plan gate (§6.3). Absent = auto.
//	gate_land, gate_push        the land and push gates the land job
//	                            enforces (§6.3, §7.1): gate_land ask
//	                            holds every landing, gate_push decides
//	                            the push after it and also takes never.
//	                            Absent: auto and ask.
//	approval_tiers              list of tiers whose tasks wait at the
//	                            land gate (§6.3). Absent = none.
//	approval_restarted          bool; a task that spent a restart or a
//	                            tier bump waits at the land gate
//	                            (§6.3). Absent = false.
//	land_target, after_land     the landing target branch and the command
//	                            the runner runs after a successful land
//	                            (§7.1). Absent: the default branch, none.
type approvalSettings struct {
	MaxTasks          *int       `json:"approval_max_tasks"`
	Classes           stringList `json:"approval_classes"`
	ProtectedPaths    stringList `json:"approval_protected_paths"`
	GatePlan          string     `json:"gate_plan"`
	GateLand          string     `json:"gate_land"`
	GatePush          string     `json:"gate_push"`
	ApprovalTiers     stringList `json:"approval_tiers"`
	ApprovalRestarted bool       `json:"approval_restarted"`
	LandTarget        string     `json:"land_target"`
	AfterLand         string     `json:"after_land"`
	DefaultCheck      string     `json:"default_check"`
	// ExtraRepos is the project's default list (§5.8): a repository a task
	// names outside it needs approval.
	ExtraRepos []ExtraRepo `json:"extra_repos"`
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
// checks mention, and the plan gate set to ask (§6.3).
type approvalVerdict struct {
	Needed  bool
	Tasks   int
	Limit   int
	Always  bool // the limit is 0: every plan waits
	Gate    bool // gate_plan is ask: every plan waits
	Classes []string
	Paths   []string
	// Extras describes each extra repository a task names outside the
	// project's default list (§5.8), with its ref.
	Extras []string
}

// reasons renders the verdict as short phrases, one per reason.
func (v approvalVerdict) reasons(allowed []string) []string {
	var out []string
	if v.Gate {
		out = append(out, "gate_plan is "+GateAsk)
	}
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
	if len(v.Extras) > 0 {
		out = append(out, "extra repositories outside the project default: "+strings.Join(v.Extras, ", "))
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
	if s.GatePlan == GateAsk {
		// The plan gate itself (§6.3): ask holds every plan, the rules
		// below only add their reasons.
		v.Needed, v.Gate = true, true
	}
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
	seenClass, seenPath, seenExtra := map[string]bool{}, map[string]bool{}, map[string]bool{}
	inDefault := map[string]bool{}
	for _, x := range s.ExtraRepos {
		inDefault[x.Repo] = true
	}
	for _, t := range tasks {
		// A task that names a repository (§5.8) outside the project's
		// default widens what the runner reads with the user's git
		// credentials; one that inherits the default never does.
		for _, x := range t.ExtraRepos {
			if inDefault[x.Repo] {
				continue
			}
			if d := extraRepoDescription(x); !seenExtra[d] {
				seenExtra[d] = true
				v.Extras = append(v.Extras, d)
				v.Needed = true
			}
		}
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
	// ErrPlanVersionStale marks a decision that names a plan version the
	// plan has moved past (§4.6 mode B): an amendment landed after the
	// user looked, so nothing is applied and the plan must be shown again.
	ErrPlanVersionStale = errors.New("plan changed since it was shown")
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
	return e.ApprovePlanVersion(ctx, project, plan, 0)
}

// ApprovePlanVersion is ApprovePlan for the plan version the user was
// shown (§4.6 mode B): a plan amended since is refused whole with
// ErrPlanVersionStale. Version 0 names no version and checks nothing.
func (e *Engine) ApprovePlanVersion(ctx context.Context, project, plan, version int64) (PlanDecision, error) {
	var res PlanDecision
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := e.sweepTx(tx, now); err != nil {
			return err
		}
		proj, err := awaitingPlanTx(tx, project, plan, version)
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
	return e.RejectPlanVersion(ctx, project, plan, 0)
}

// RejectPlanVersion is RejectPlan for the plan version the user was
// shown; see ApprovePlanVersion.
func (e *Engine) RejectPlanVersion(ctx context.Context, project, plan, version int64) (PlanDecision, error) {
	var res PlanDecision
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := e.sweepTx(tx, now); err != nil {
			return err
		}
		proj, err := awaitingPlanTx(tx, project, plan, version)
		if err != nil {
			return err
		}
		if err := e.cancelPlanTx(tx, proj, plan, evPlanRejected, now); err != nil {
			return err
		}
		res = PlanDecision{PlanID: plan, State: planCancelled}
		return e.wakeForemanTx(tx, proj, now)
	})
	if err != nil {
		return PlanDecision{}, fmt.Errorf("reject plan %d: %w", plan, err)
	}
	return res, nil
}

// awaitingPlanTx finds the plan and checks it waits for approval,
// returning its project. A non-zero version must be the plan's current
// one, else the decision is stale (ErrPlanVersionStale).
func awaitingPlanTx(tx *sql.Tx, project, plan, version int64) (int64, error) {
	var proj, have int64
	var state string
	err := tx.QueryRow("SELECT project_id, state, version FROM plans WHERE id = ?", plan).Scan(&proj, &state, &have)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && project != 0 && proj != project) {
		return 0, fmt.Errorf("%w: %d", ErrNoPlan, plan)
	}
	if err != nil {
		return 0, fmt.Errorf("read plan %d: %w", plan, err)
	}
	if err := activeProjectTx(tx, proj); err != nil {
		return 0, err
	}
	if state != planAwaitingApproval {
		return 0, fmt.Errorf("%w: plan %d is %s", ErrPlanNotAwaiting, plan, state)
	}
	if version != 0 && version != have {
		return 0, fmt.Errorf("%w: plan %d is at version %d, not the version %d you were shown; read the plan again and show it to the user", ErrPlanVersionStale, plan, have, version)
	}
	return proj, nil
}

// cancelPlanTx shares task/job cancellation and outcome accounting with rejection.
func (e *Engine) cancelPlanTx(tx *sql.Tx, proj, plan int64, eventKind string, now time.Time) error {
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
	if err := inboxEventTx(tx, eventKind, proj, 0, 0,
		eventPayload{Plan: plan, Status: planCancelled}, now); err != nil {
		return err
	}
	return nil
}
