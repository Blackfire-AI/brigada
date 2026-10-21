package engine

import (
	"strings"
	"testing"
)

// TestAmendRespec: a pending task's spec is replaced or appended to and the
// change is recorded; a task that has started is refused, and so is a bad
// mode, an empty spec or a second edit of one task.
func TestAmendRespec(t *testing.T) {
	e := newEnv(t)
	res := e.twoStep()
	build, ship := res.Tasks[0], res.Tasks[1]
	before := e.str("SELECT spec FROM tasks WHERE id = ?", ship.TaskID)

	am := e.amend(res.PlanID, PlanAmendment{Respec: []PlanRespec{{Task: "ship", Spec: "also tag it", Mode: RespecAppend}}})
	if len(am.Respecced) != 1 || am.Respecced[0] != ship.TaskID || am.State != "active" {
		t.Fatalf("amendment = %+v, want ship respecified", am)
	}
	if got, want := e.str("SELECT spec FROM tasks WHERE id = ?", ship.TaskID), before+"\n\nalso tag it"; got != want {
		t.Errorf("appended spec = %q, want %q", got, want)
	}
	if got := e.taskState(ship.TaskID); got != taskPending {
		t.Errorf("ship state = %q, want pending", got)
	}

	e.amend(res.PlanID, PlanAmendment{Respec: []PlanRespec{{Task: "ship", Spec: "ship it differently"}}})
	if got := e.str("SELECT spec FROM tasks WHERE id = ?", ship.TaskID); got != "ship it differently" {
		t.Errorf("replaced spec = %q", got)
	}
	if p, ok := e.eventPayloadOf(evPlanAmended); !ok || !strings.Contains(p.Reason, "respecified task") {
		t.Errorf("plan_amended event = %+v, want the respec named", p)
	}

	if got := e.queuedWork(ship.TaskID); got != 0 {
		t.Errorf("ship has %d queued jobs, want none", got)
	}

	for name, rs := range map[string]PlanRespec{
		"started task": {Task: "build", Spec: "x"},
		"bad mode":     {Task: "ship", Spec: "x", Mode: "prepend"},
		"empty spec":   {Task: "ship", Spec: " "},
		"unknown":      {Task: "ghost", Spec: "x"},
	} {
		_, err := e.eng.AmendPlan(e.ctx, e.project, res.PlanID, PlanAmendment{Respec: []PlanRespec{rs}})
		if err == nil {
			t.Errorf("%s: respec succeeded", name)
		}
	}
	_, err := e.eng.AmendPlan(e.ctx, e.project, res.PlanID, PlanAmendment{Respec: []PlanRespec{{Task: "build", Spec: "x"}}})
	if err == nil || !strings.Contains(err.Error(), "follow-up task") {
		t.Errorf("started task err = %v, want the follow-up advice", err)
	}
	_, err = e.eng.AmendPlan(e.ctx, e.project, res.PlanID, PlanAmendment{Respec: []PlanRespec{{Task: "ship", Spec: "a"}, {Task: "ship", Spec: "b"}}})
	if err == nil {
		t.Error("two edits of one task succeeded")
	}
	_, err = e.eng.AmendPlan(e.ctx, e.project, res.PlanID, PlanAmendment{
		Add:    []PlanTask{{Name: "extra", Spec: "x"}},
		Respec: []PlanRespec{{Task: "extra", Spec: "y"}},
	})
	if err == nil || !strings.Contains(err.Error(), "added by this same amendment") {
		t.Errorf("respec of an added task err = %v, want its own message", err)
	}
	if got := e.str("SELECT spec FROM tasks WHERE id = ?", build.TaskID); got == "x" {
		t.Error("a refused amendment changed a spec")
	}
}

// TestAmendRespecHoldsTrippingEdit: an edit that adds a protected path holds
// that task alone, while the plan stays active.
func TestAmendRespecHoldsTrippingEdit(t *testing.T) {
	e := newEnv(t)
	e.setSettings(`{"approval_protected_paths": ["go.mod"]}`)
	res := e.twoStep()
	ship := res.Tasks[1]

	am := e.amend(res.PlanID, PlanAmendment{Respec: []PlanRespec{{Task: "ship", Spec: "edit go.mod", Mode: RespecAppend}}})
	if am.State != "active" || len(am.Held) != 1 || am.Held[0].TaskID != ship.TaskID || !strings.Contains(am.Held[0].Reason, "touches go.mod") {
		t.Fatalf("amendment = %+v, want ship held for go.mod", am)
	}
	if got := e.taskState(ship.TaskID); got != taskNeedsForeman {
		t.Errorf("ship state = %q, want needs_foreman", got)
	}
	if p, ok := e.eventPayloadOf(evTaskPlanHeld); !ok || !strings.Contains(p.Reason, "touches go.mod") {
		t.Errorf("task_plan_held event = %+v, want the rule named", p)
	}
}
