package engine

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// TestAmendCancelRecordsReasonPerTask: each cancelled task of an amendment
// gets its own task_cancelled_by_amend event carrying its reason verbatim
// and who asked, beside the one plan_amended summary event.
func TestAmendCancelRecordsReasonPerTask(t *testing.T) {
	e := newEnv(t)
	res := e.twoStep()
	build, ship := res.Tasks[0], res.Tasks[1]

	e.amend(res.PlanID, PlanAmendment{
		Cancel: []PlanCancel{
			{Task: "build", Reason: "  superseded by task 274 "},
			{Task: fmt.Sprint(ship.TaskID), Reason: "the build it needs is gone"},
		},
		Caller: "foreman",
	})
	if n := e.int("SELECT COUNT(*) FROM events WHERE kind = ?", evTaskCancelledByAmend); n != 2 {
		t.Fatalf("task_cancelled_by_amend events = %d, want one per cancelled task", n)
	}
	for task, want := range map[int64]string{build.TaskID: "  superseded by task 274 ", ship.TaskID: "the build it needs is gone"} {
		payload := e.str("SELECT payload FROM events WHERE kind = ? AND task_id = ?", evTaskCancelledByAmend, task)
		var p eventPayload
		if err := json.Unmarshal([]byte(payload), &p); err != nil {
			t.Fatalf("task %d payload %q: %v", task, payload, err)
		}
		if p.Reason != want || p.Caller != "foreman" || p.Status != taskCancelled {
			t.Errorf("task %d payload = %+v, want reason %q, caller foreman, status cancelled", task, p, want)
		}
	}
	if n := e.int("SELECT COUNT(*) FROM events WHERE kind = ?", evPlanAmended); n != 1 {
		t.Errorf("plan_amended events = %d, want the one summary", n)
	}
}

// TestAmendCancelNeedsReason: a cancel with a missing or blank reason
// refuses the whole amendment, naming the task, and changes nothing — not
// a cancel the amendment also carried, nor an added task.
func TestAmendCancelNeedsReason(t *testing.T) {
	e := newEnv(t)
	res := e.twoStep()
	for _, reason := range []string{"", " \t\n"} {
		_, err := e.eng.AmendPlan(e.ctx, e.project, res.PlanID, PlanAmendment{
			Add:    []PlanTask{{Name: "extra", Spec: "more", Class: "answer"}},
			Cancel: []PlanCancel{{Task: "ship", Reason: "fine"}, {Task: "build", Reason: reason}},
		})
		if err == nil || !strings.Contains(err.Error(), "task build needs a reason") {
			t.Fatalf("cancel with reason %q: err = %v, want a refusal naming task build", reason, err)
		}
	}
	for _, task := range res.Tasks {
		if got := e.taskState(task.TaskID); got == taskCancelled {
			t.Errorf("task %d cancelled by a refused amendment", task.TaskID)
		}
	}
	if n := e.int("SELECT COUNT(*) FROM tasks WHERE name = 'extra'"); n != 0 {
		t.Errorf("a refused amendment added %d tasks", n)
	}
	if n := e.int("SELECT COUNT(*) FROM events WHERE kind IN (?, ?)", evTaskCancelledByAmend, evPlanAmended); n != 0 {
		t.Errorf("a refused amendment wrote %d events", n)
	}
}
