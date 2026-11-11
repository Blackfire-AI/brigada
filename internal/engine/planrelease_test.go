package engine

import (
	"errors"
	"testing"
)

// TestRuleHoldIsTheUsersToRelease (§6.3): a task an amendment's rule
// holds alone refuses the foreman's restart and answer — no foreman tool
// passes a plan gate — and brigada plan approve on the active plan
// releases it into the ready wave with a task_plan_released event.
func TestRuleHoldIsTheUsersToRelease(t *testing.T) {
	e := newEnv(t)
	e.setSettings(`{"default_check": "go test ./..."}`)
	res := e.createPlan([]PlanTask{{Name: "a", Spec: "a"}})
	e.assertRuns(res, 1)
	am := e.amend(res.PlanID, PlanAmendment{Add: []PlanTask{{Name: "other", Spec: "d", Check: "make release"}}})
	if len(am.Held) != 1 {
		t.Fatalf("amendment = %+v, want the other-check task held", am)
	}
	held := am.Held[0].TaskID

	if _, err := e.eng.RestartTask(e.ctx, e.project, held, "the user agreed"); !errors.Is(err, ErrTaskRuleHeld) {
		t.Fatalf("restart of a rule-held task: err = %v, want ErrTaskRuleHeld", err)
	}
	if got := e.taskState(held); got != taskNeedsForeman {
		t.Fatalf("held task state after the refused restart = %q, want needs_foreman", got)
	}

	d, err := e.eng.ApprovePlan(e.ctx, e.project, res.PlanID)
	if err != nil {
		t.Fatalf("approve the active plan's hold: %v", err)
	}
	if d.State != "active" || len(d.Tasks) != 1 || d.Tasks[0].TaskID != held || d.Tasks[0].JobID == 0 {
		t.Errorf("decision = %+v, want the held task queued and the plan active", d)
	}
	if got := e.taskState(held); got != taskReady {
		t.Errorf("released task state = %q, want ready", got)
	}
	if n := e.countEvents(evTaskPlanReleased); n != 1 {
		t.Errorf("task_plan_released events = %d, want 1", n)
	}
	// Nothing is held now: another approval has nothing to act on.
	if _, err := e.eng.ApprovePlan(e.ctx, e.project, res.PlanID); !errors.Is(err, ErrPlanNotAwaiting) {
		t.Errorf("second approval: err = %v, want ErrPlanNotAwaiting", err)
	}
}

// TestReleasedTaskIsTheForemansAgain: once released and started, a task
// that parks for another reason is the foreman's to restart again — the
// hold ended with its start.
func TestReleasedTaskIsTheForemansAgain(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	e.setSettings(`{"default_check": "go test ./..."}`)
	res := e.createPlan([]PlanTask{{Name: "a", Spec: "a"}})
	e.assertRuns(res, 1)
	e.runWork(author, doneOn("task/1-1", passCheck))
	am := e.amend(res.PlanID, PlanAmendment{Add: []PlanTask{{Name: "other", Spec: "d", Check: "make release"}}})
	held := am.Held[0].TaskID
	if _, err := e.eng.ApprovePlan(e.ctx, e.project, res.PlanID); err != nil {
		t.Fatal(err)
	}
	g := e.leaseAny(author)
	if g == nil || g.TaskID != held {
		t.Fatalf("grant = %+v, want the released task's work job", g)
	}
	if ans := e.complete(g, Completion{Error: "the harness is on fire"}); ans != AnsOK {
		t.Fatalf("complete: %s", ans)
	}
	if got := e.taskState(held); got != taskNeedsForeman {
		t.Fatalf("task state after the failure = %q, want needs_foreman", got)
	}
	if _, err := e.eng.RestartTask(e.ctx, e.project, held, "try again"); err != nil {
		t.Errorf("restart after a failure of the released task: %v, want it allowed", err)
	}
}

// TestApproveActivePlanWithoutHoldsIsRefused: an active plan with no
// task held at the plan gate is not awaiting the user.
func TestApproveActivePlanWithoutHoldsIsRefused(t *testing.T) {
	e := newEnv(t)
	res := e.createPlan([]PlanTask{{Name: "a", Spec: "a"}})
	e.assertRuns(res, 1)
	if _, err := e.eng.ApprovePlan(e.ctx, e.project, res.PlanID); !errors.Is(err, ErrPlanNotAwaiting) {
		t.Errorf("approve an active plan with no hold: err = %v, want ErrPlanNotAwaiting", err)
	}
}

// TestBudgetHoldReleasedByApproval: a budget hold is released by the
// user's approval too; while the spend still passes the budget the
// released task is held again at once and the decision says so, and a
// raised budget lets the next approval queue it.
func TestBudgetHoldReleasedByApproval(t *testing.T) {
	e := newEnv(t)
	e.addAgent(e.runner1, "claude", "sonnet", "work")
	e.setSettings(`{"approval_budget": "1000"}`)
	res := e.createPlan([]PlanTask{
		{Name: "a", Spec: "a", Class: "answer"},
		{Name: "b", Spec: "b", Class: "answer", Deps: []string{"a"}},
	})
	e.assertRuns(res, 1)
	g := e.leaseAny(e.runner1)
	if g == nil {
		t.Fatal("no lease")
	}
	if ans := e.complete(g, budgetUsage(600, 500, nil)); ans != AnsOK {
		t.Fatal(ans)
	}
	b := res.Tasks[1].TaskID
	if _, err := e.eng.RestartTask(e.ctx, e.project, b, ""); !errors.Is(err, ErrTaskRuleHeld) {
		t.Fatalf("restart of a budget-held task: err = %v, want ErrTaskRuleHeld", err)
	}
	d, err := e.eng.ApprovePlan(e.ctx, e.project, res.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Tasks) != 0 || len(d.Held) != 1 || d.Held[0].TaskID != b {
		t.Fatalf("decision over the budget = %+v, want b held again", d)
	}
	e.setSettings(`{"approval_budget": "5000"}`)
	d, err = e.eng.ApprovePlan(e.ctx, e.project, res.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Tasks) != 1 || d.Tasks[0].TaskID != b || len(d.Held) != 0 {
		t.Errorf("decision under the raised budget = %+v, want b queued", d)
	}
}
