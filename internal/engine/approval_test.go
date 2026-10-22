package engine

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// nTasks is n implement tasks with distinct specs, no dependencies.
func nTasks(n int) []PlanTask {
	tasks := make([]PlanTask, n)
	for i := range tasks {
		tasks[i] = PlanTask{Spec: fmt.Sprintf("do thing %d", i+1)}
	}
	return tasks
}

func (e *env) setSettings(json string) {
	e.t.Helper()
	e.exec("UPDATE projects SET settings = ? WHERE id = ?", json, e.project)
}

func (e *env) createPlan(tasks []PlanTask) CreatePlanResult {
	e.t.Helper()
	res, err := e.eng.CreatePlan(e.ctx, e.project, "a plan to judge", tasks)
	if err != nil {
		e.t.Fatalf("create plan: %v", err)
	}
	return res
}

func (e *env) assertRuns(res CreatePlanResult, jobs int) {
	e.t.Helper()
	if res.State != "active" || len(res.Reasons) != 0 {
		e.t.Errorf("plan = %s %v, want active, no reasons", res.State, res.Reasons)
	}
	if got := e.int("SELECT COUNT(*) FROM jobs WHERE kind = 'work'"); got != int64(jobs) {
		e.t.Errorf("work jobs = %d, want %d", got, jobs)
	}
}

func (e *env) assertAwaits(res CreatePlanResult, want string) {
	e.t.Helper()
	if res.State != planAwaitingApproval {
		e.t.Fatalf("plan state = %q, want %s", res.State, planAwaitingApproval)
	}
	if !strings.Contains(strings.Join(res.Reasons, "; "), want) {
		e.t.Errorf("reasons = %v, want one naming %q", res.Reasons, want)
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != planAwaitingApproval {
		e.t.Errorf("stored plan state = %q", got)
	}
	if got := e.int("SELECT COUNT(*) FROM jobs WHERE kind = 'work'"); got != 0 {
		e.t.Errorf("work jobs = %d, want none queued", got)
	}
	if res.JobID != 0 || res.TaskID != 0 {
		e.t.Errorf("answer names task %d job %d, want none", res.TaskID, res.JobID)
	}
	if got := e.int("SELECT COUNT(*) FROM tasks WHERE plan_id = ? AND state <> 'pending'", res.PlanID); got != 0 {
		e.t.Errorf("%d tasks left pending, want all held", got)
	}
}

// The count rule is off unless the project sets a count: with 5 set,
// five tasks run and six await; with nothing set, six run.
func TestApprovalFiveTasksRunSixAwait(t *testing.T) {
	e := newEnv(t)
	e.setSettings(`{"approval_max_tasks": 5}`)
	e.assertRuns(e.createPlan(nTasks(5)), 5)

	e = newEnv(t)
	e.setSettings(`{"approval_max_tasks": 5}`)
	res := e.createPlan(nTasks(6))
	e.assertAwaits(res, "6 tasks, over the limit of 5")
	if got := e.int("SELECT COUNT(*) FROM events WHERE kind = 'plan_awaiting_approval'"); got != 1 {
		t.Errorf("plan_awaiting_approval events = %d, want 1", got)
	}

	e = newEnv(t)
	e.assertRuns(e.createPlan(nTasks(6)), 6)
}

func TestApprovalClasses(t *testing.T) {
	e := newEnv(t)
	for _, c := range []string{"implement", "review", "answer", ""} {
		e = newEnv(t)
		e.assertRuns(e.createPlan([]PlanTask{{Spec: "x", Class: c}}), 1)
	}
	e = newEnv(t)
	e.assertAwaits(e.createPlan([]PlanTask{{Spec: "x"}, {Spec: "y", Class: "deploy"}}), "class deploy")

	// The set is the project's to change.
	e = newEnv(t)
	e.setSettings(`{"approval_classes": "implement, deploy"}`)
	e.assertRuns(e.createPlan([]PlanTask{{Spec: "x", Class: "deploy"}}), 1)
}

func TestApprovalProtectedPaths(t *testing.T) {
	e := newEnv(t)
	e.setSettings(`{"approval_protected_paths": ["internal/store/migrations/*", ".github/**", "go.mod"]}`)
	e.assertRuns(e.createPlan([]PlanTask{{Spec: "touch internal/engine/plans.go", Check: "go test ./internal/engine"}}), 1)

	for name, task := range map[string]PlanTask{
		"glob in spec":      {Spec: "add internal/store/migrations/0021_x.sql with a column"},
		"glob in check":     {Spec: "harmless", Check: "test -f ./internal/store/migrations/0001_init.sql"},
		"dir glob":          {Spec: "edit `.github/workflows/ci.yml`."},
		"containment":       {Spec: "bump a dependency in go.mod"},
		"quoted, trailing.": {Spec: `edit "go.mod".`},
	} {
		e = newEnv(t)
		e.setSettings(`{"approval_protected_paths": ["internal/store/migrations/*", ".github/**", "go.mod"]}`)
		res := e.createPlan([]PlanTask{task})
		if res.State != planAwaitingApproval || len(res.Reasons) == 0 || !strings.Contains(res.Reasons[0], "protected paths") {
			t.Errorf("%s: plan = %s %v, want held for a protected path", name, res.State, res.Reasons)
		}
	}
	// A glob does not reach a deeper path than it names.
	e = newEnv(t)
	e.setSettings(`{"approval_protected_paths": ["internal/store/migrations/*"]}`)
	e.assertRuns(e.createPlan([]PlanTask{{Spec: "see internal/store/migrations/deep/x.sql"}}), 1)
}

func TestApprovalLimitZeroAndNegative(t *testing.T) {
	// 0 is the default and means off: the count rule holds nothing.
	e := newEnv(t)
	e.setSettings(`{"approval_max_tasks": 0}`)
	e.assertRuns(e.createPlan(nTasks(9)), 9)

	e = newEnv(t)
	e.setSettings(`{"approval_max_tasks": -1, "approval_protected_paths": ["go.mod"]}`)
	tasks := nTasks(9)
	tasks[0].Class = "deploy"
	tasks[1].Spec = "edit go.mod"
	e.assertRuns(e.createPlan(tasks), 9)

	e = newEnv(t)
	e.setSettings(`{"approval_max_tasks": 2}`)
	e.assertAwaits(e.createPlan(nTasks(3)), "3 tasks, over the limit of 2")
}

// TestPlanDecisionVersion: a decision naming the plan version the user
// was shown is applied only while the plan is still at it; an amendment
// of any kind moves the version and the old decision is refused whole.
// Version 0 names none and checks nothing (the CLI's decisions).
func TestPlanDecisionVersion(t *testing.T) {
	e := newEnv(t)
	e.setSettings(`{"gate_plan": "ask"}`)
	res := e.createPlan(nTasks(1))
	e.assertAwaits(res, "gate_plan is ask")
	if got := e.int("SELECT version FROM plans WHERE id = ?", res.PlanID); got != 1 {
		t.Fatalf("new plan version = %d, want 1", got)
	}
	e.amend(res.PlanID, PlanAmendment{Add: []PlanTask{{Name: "more", Spec: "one more"}}})
	if got := e.int("SELECT version FROM plans WHERE id = ?", res.PlanID); got != 2 {
		t.Fatalf("version after an amendment = %d, want 2", got)
	}
	if _, err := e.eng.ApprovePlanVersion(e.ctx, 0, res.PlanID, 1); !errors.Is(err, ErrPlanVersionStale) {
		t.Fatalf("approve at the old version: %v, want ErrPlanVersionStale", err)
	}
	if _, err := e.eng.RejectPlanVersion(e.ctx, 0, res.PlanID, 1); !errors.Is(err, ErrPlanVersionStale) {
		t.Fatalf("reject at the old version: %v, want ErrPlanVersionStale", err)
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != planAwaitingApproval {
		t.Fatalf("a stale decision moved the plan to %q", got)
	}
	if got := e.int("SELECT COUNT(*) FROM jobs WHERE kind = 'work'"); got != 0 {
		t.Fatalf("a stale decision queued %d work jobs", got)
	}
	dec, err := e.eng.ApprovePlanVersion(e.ctx, e.project, res.PlanID, 2)
	if err != nil || dec.State != "active" {
		t.Fatalf("approve at the current version: %+v, %v", dec, err)
	}

	other := e.createPlan(nTasks(1))
	if _, err := e.eng.RejectPlanVersion(e.ctx, 0, other.PlanID, 0); err != nil {
		t.Fatalf("reject with no version: %v", err)
	}
}

func TestApprovePlanQueuesFirstWaveOnly(t *testing.T) {
	e := newEnv(t)
	e.setSettings(`{"gate_plan": "ask"}`)
	res := e.createPlan([]PlanTask{
		{Name: "a", Spec: "first"},
		{Name: "b", Spec: "second", Deps: []string{"a"}},
		{Name: "c", Spec: "third"},
	})
	e.assertAwaits(res, "gate_plan is ask")

	dec, err := e.eng.ApprovePlan(e.ctx, 0, res.PlanID)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if dec.State != "active" || len(dec.Tasks) != 2 {
		t.Fatalf("decision = %+v, want active with the two dependency-free tasks", dec)
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != "active" {
		t.Errorf("plan state = %q", got)
	}
	states := []string{"ready", "pending", "ready"}
	for i, want := range states {
		if got := e.str("SELECT state FROM tasks WHERE id = ?", res.Tasks[i].TaskID); got != want {
			t.Errorf("task %d state = %q, want %q", i+1, got, want)
		}
	}
	if got := e.int("SELECT COUNT(*) FROM jobs WHERE kind = 'work' AND status = 'queued'"); got != 2 {
		t.Errorf("queued work jobs = %d, want 2", got)
	}
	if got := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ?", res.Tasks[1].TaskID); got != 0 {
		t.Errorf("the dependent task has %d jobs, want none", got)
	}

	// A second yes is refused, and so is a no after it.
	if _, err := e.eng.ApprovePlan(e.ctx, 0, res.PlanID); err == nil {
		t.Error("approving an active plan succeeded")
	}
	if _, err := e.eng.RejectPlan(e.ctx, 0, res.PlanID); err == nil {
		t.Error("rejecting an active plan succeeded")
	}
	if _, err := e.eng.ApprovePlan(e.ctx, 0, 9999); err == nil {
		t.Error("approving a missing plan succeeded")
	}
	if _, err := e.eng.ApprovePlan(e.ctx, e.project+1, res.PlanID); err == nil {
		t.Error("approving across projects succeeded")
	}
}

func TestRejectPlanCancelsTasks(t *testing.T) {
	e := newEnv(t)
	e.setSettings(`{"approval_max_tasks": 1}`)
	res := e.createPlan(nTasks(2))
	e.assertAwaits(res, "2 tasks")

	dec, err := e.eng.RejectPlan(e.ctx, 0, res.PlanID)
	if err != nil {
		t.Fatalf("reject: %v", err)
	}
	if dec.State != "cancelled" {
		t.Errorf("decision = %+v", dec)
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != "cancelled" {
		t.Errorf("plan state = %q", got)
	}
	if got := e.int("SELECT COUNT(*) FROM tasks WHERE plan_id = ? AND state = 'cancelled'", res.PlanID); got != 2 {
		t.Errorf("cancelled tasks = %d, want 2", got)
	}
	if got := e.int("SELECT COUNT(*) FROM jobs WHERE kind = 'work'"); got != 0 {
		t.Errorf("work jobs = %d, want none", got)
	}
	if _, err := e.eng.ApprovePlan(e.ctx, 0, res.PlanID); err == nil {
		t.Error("approving a rejected plan succeeded")
	}
	st, err := e.eng.Status(e.ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.NeedsYou) != 0 {
		t.Errorf("needs you = %+v, want the rejected plan gone", st.NeedsYou)
	}
}

func TestAwaitingPlanInNeedsYouWithReasons(t *testing.T) {
	e := newEnv(t)
	e.setSettings(`{"approval_max_tasks": 5, "approval_protected_paths": ["go.mod"]}`)
	tasks := nTasks(6)
	tasks[2].Class = "deploy"
	tasks[3].Check = "git diff go.mod"
	res := e.createPlan(tasks)

	st, err := e.eng.Status(e.ctx, e.project)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.NeedsYou) != 1 {
		t.Fatalf("needs you = %+v, want the awaiting plan", st.NeedsYou)
	}
	got := st.NeedsYou[0]
	if got.Kind != "plan" || got.ID != res.PlanID || got.State != planAwaitingApproval {
		t.Errorf("entry = %+v", got)
	}
	for _, want := range []string{"6 tasks, over the limit of 5", "class deploy", "protected paths: go.mod"} {
		if !strings.Contains(got.Reason, want) {
			t.Errorf("reason %q lacks %q", got.Reason, want)
		}
	}
	// The awaiting plan lists in full beside the seeded active one:
	// every live plan shows, awaiting approval or not.
	if len(st.Plans) != 2 || st.Plans[0].ID != e.plan || st.Plans[1].ID != res.PlanID {
		t.Errorf("plans = %+v, want the seeded active plan and the awaiting one", st.Plans)
	}
	if awaiting := st.Plans[1]; awaiting.State != planAwaitingApproval || len(awaiting.Tasks) != 6 {
		t.Errorf("the awaiting plan = %+v, want its six tasks listed", awaiting)
	}
	// The event payload is the source: it carries the structure too.
	payload := e.str("SELECT payload FROM events WHERE kind = 'plan_awaiting_approval'")
	for _, want := range []string{`"classes":["deploy"]`, `"paths":["go.mod"]`, fmt.Sprintf(`"plan":%d`, res.PlanID), `"count":6`} {
		if !strings.Contains(payload, want) {
			t.Errorf("payload %s lacks %s", payload, want)
		}
	}
}
