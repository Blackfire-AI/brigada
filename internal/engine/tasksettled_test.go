package engine

import (
	"testing"
	"time"
)

// settledAt reads a task's settled_at, "" when it is NULL.
func (e *env) settledAt(task int64) string {
	e.t.Helper()
	return e.str("SELECT COALESCE(settled_at, '') FROM tasks WHERE id = ?", task)
}

// TestSettledAtOnCancelAndRetire: a task settles — it reaches a state
// its branch can never land from again (§13.2) — when a plan amendment
// cancels it, when its plan's close cancels it, or when it is retired,
// and settled_at carries the engine clock of that transition. Every
// other state leaves it NULL.
func TestSettledAtOnCancelAndRetire(t *testing.T) {
	e := newEnv(t)

	// Cancel by the plan's close: the close cancels the plan's live tasks
	// and settles each one.
	e.set(map[string]string{SettingApprovalMaxTasks: "1"})
	held := e.createPlan(nTasks(2))
	e.assertAwaits(held, "2 tasks")
	e.clock.Advance(time.Minute)
	if _, err := e.eng.RejectPlan(e.ctx, 0, held.PlanID); err != nil {
		t.Fatalf("reject: %v", err)
	}
	want := fmtTime(e.clock.Now())
	for _, task := range held.Tasks {
		if got := e.settledAt(task.TaskID); got != want {
			t.Errorf("closed plan's task %d settled_at = %q, want %q", task.TaskID, got, want)
		}
	}

	// Cancel by plan_amend: the cancelled task settles, its kept sibling
	// does not.
	res := e.createPlan([]PlanTask{
		{Name: "a", Spec: "a", Class: "answer"},
		{Name: "keep", Spec: "keep", Class: "answer"},
	})
	a, keep := res.Tasks[0].TaskID, res.Tasks[1].TaskID
	e.clock.Advance(time.Minute)
	e.amend(res.PlanID, PlanAmendment{Cancel: cancels("a"), Caller: "foreman"})
	if got, want := e.settledAt(a), fmtTime(e.clock.Now()); got != want {
		t.Errorf("amend-cancelled task settled_at = %q, want %q", got, want)
	}
	if got := e.settledAt(keep); got != "" {
		t.Errorf("kept task settled_at = %q, want none: it can still land", got)
	}

	// Retire: the retire settles the task the same way.
	e.exec("UPDATE tasks SET state = 'needs_foreman' WHERE id = ?", keep)
	e.clock.Advance(time.Minute)
	if _, err := e.eng.Retire(e.ctx, e.project, []int64{keep}, "landed by hand on main", "user"); err != nil {
		t.Fatalf("retire keep: %v", err)
	}
	if got, want := e.settledAt(keep), fmtTime(e.clock.Now()); got != want {
		t.Errorf("retired task settled_at = %q, want %q", got, want)
	}
}

// TestSettledAtNullForOtherStates: the states a branch can still land
// from never settle the task — not pending, ready, running, failed,
// needs_foreman nor done.
func TestSettledAtNullForOtherStates(t *testing.T) {
	e := newEnv(t)
	res := e.createPlan(nTasks(1))
	task := res.Tasks[0].TaskID
	for _, state := range []string{"pending", "ready", "running", "failed", "needs_foreman", "done"} {
		e.exec("UPDATE tasks SET state = ? WHERE id = ?", state, task)
		if got := e.settledAt(task); got != "" {
			t.Errorf("task %s settled_at = %q, want none: its branch can still land", state, got)
		}
	}
}
