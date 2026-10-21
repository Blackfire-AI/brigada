package engine

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// amend runs AmendPlan and fails the test on a refusal.
func (e *env) amend(plan int64, am PlanAmendment) AmendPlanResult {
	e.t.Helper()
	res, err := e.eng.AmendPlan(e.ctx, e.project, plan, am)
	if err != nil {
		e.t.Fatalf("amend plan %d: %v", plan, err)
	}
	return res
}

func (e *env) taskState(id int64) string {
	e.t.Helper()
	return e.str("SELECT state FROM tasks WHERE id = ?", id)
}

func (e *env) queuedWork(task int64) int64 {
	e.t.Helper()
	return e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'work' AND status = 'queued'", task)
}

func (e *env) planOutcome(agent int64) (attempts, successes int64) {
	e.t.Helper()
	return e.int("SELECT COALESCE(SUM(attempts), 0) FROM outcomes WHERE agent_id = ? AND class = 'plan'", agent),
		e.int("SELECT COALESCE(SUM(successes), 0) FROM outcomes WHERE agent_id = ? AND class = 'plan'", agent)
}

// TestAmendAddsAndCancels: an added task with no deps starts at once, one
// that waits on a plan task stays pending, a cancelled task is cancelled
// with its live job stopped, and one plan_amended event says so.
func TestAmendAddsAndCancels(t *testing.T) {
	e := newEnv(t)
	res := e.twoStep()
	build, ship := res.Tasks[0], res.Tasks[1]

	am := e.amend(res.PlanID, PlanAmendment{
		Add: []PlanTask{
			{Name: "lint", Spec: "lint it", Class: "answer", Tier: "light", Effort: "low"},
			{Name: "docs", Spec: "document it", Class: "answer", Deps: []string{"build", "lint"}},
		},
		Cancel: []string{"ship"},
	})
	if am.State != "active" || len(am.Added) != 2 || len(am.Cancelled) != 1 || am.Cancelled[0] != ship.TaskID {
		t.Fatalf("amendment = %+v, want 2 added, ship cancelled, plan active", am)
	}
	lint, docs := am.Added[0], am.Added[1]
	for col, want := range map[string]string{"tier": "light", "effective_tier": "light", "effort": "low"} {
		if got := e.str("SELECT "+col+" FROM tasks WHERE id = ?", lint.TaskID); got != want {
			t.Errorf("%s = %q, want %q", col, got, want)
		}
	}
	if lint.JobID == 0 || docs.JobID != 0 {
		t.Errorf("added = %+v, want lint started and docs waiting", am.Added)
	}
	if got := e.taskState(lint.TaskID); got != taskReady {
		t.Errorf("lint state = %q, want ready", got)
	}
	if got := e.taskState(docs.TaskID); got != taskPending {
		t.Errorf("docs state = %q, want pending", got)
	}
	if got := e.taskState(ship.TaskID); got != taskCancelled {
		t.Errorf("ship state = %q, want cancelled", got)
	}
	if got := e.int("SELECT COUNT(*) FROM task_deps WHERE task_id = ?", docs.TaskID); got != 2 {
		t.Errorf("docs deps = %d, want 2", got)
	}
	if got := e.int("SELECT COUNT(*) FROM events WHERE kind = 'plan_amended'"); got != 1 {
		t.Errorf("plan_amended events = %d, want 1", got)
	}

	// Cancelling a running task stops its job.
	g := e.leaseAny(e.runner1)
	if g == nil || g.TaskID != build.TaskID {
		t.Fatalf("lease = %+v, want build's job", g)
	}
	e.amend(res.PlanID, PlanAmendment{Cancel: []string{"build"}})
	if got := e.str("SELECT status FROM jobs WHERE id = ?", build.JobID); got != statusCancelled {
		t.Errorf("build's job = %q, want cancelled", got)
	}
	// A done task cannot be cancelled, an unknown one is refused.
	if _, err := e.eng.AmendPlan(e.ctx, e.project, res.PlanID, PlanAmendment{Cancel: []string{"nope"}}); err == nil {
		t.Error("cancelling an unknown task succeeded")
	}
	if _, err := e.eng.AmendPlan(e.ctx, e.project, res.PlanID, PlanAmendment{}); err == nil {
		t.Error("an empty amendment succeeded")
	}
	// A task cannot be added with a duplicate name or an unknown dep.
	if _, err := e.eng.AmendPlan(e.ctx, e.project, res.PlanID, PlanAmendment{Add: []PlanTask{{Name: "lint", Spec: "x"}}}); err == nil || !strings.Contains(err.Error(), "unique") {
		t.Errorf("duplicate name err = %v", err)
	}
	if _, err := e.eng.AmendPlan(e.ctx, e.project, res.PlanID, PlanAmendment{Add: []PlanTask{{Spec: "x", Deps: []string{"ghost"}}}}); err == nil {
		t.Error("an unknown dep succeeded")
	}
}

// TestAmendRepointsOffFailedTask: the dependent of a task that failed
// never starts on its own; re-pointed to a replacement it waits for that,
// re-pointed to nothing it starts in the amendment's own transaction.
func TestAmendRepointsOffFailedTask(t *testing.T) {
	e := newEnv(t)
	res := e.twoStep()
	build, ship := res.Tasks[0], res.Tasks[1]
	e.exec("UPDATE tasks SET effective_tier = 'heavy', effective_effort = 'high' WHERE plan_id = ?", res.PlanID)
	e.complete(e.leaseAny(e.runner1), Completion{Result: `{"status":"failed","summary":"no clone"}`})
	// The engine parks a failed task at needs_foreman; a task that
	// failed for good is the same row at failed.
	e.exec("UPDATE tasks SET state = 'failed' WHERE id = ?", build.TaskID)

	// Depending on the failed task is refused outright.
	if _, err := e.eng.AmendPlan(e.ctx, e.project, res.PlanID, PlanAmendment{
		Add: []PlanTask{{Name: "again", Spec: "again", Deps: []string{"build"}}},
	}); err == nil || !strings.Contains(err.Error(), "never be done") {
		t.Fatalf("dep on a failed task err = %v, want a refusal", err)
	}

	am := e.amend(res.PlanID, PlanAmendment{
		Add:      []PlanTask{{Name: "build2", Spec: "build it right", Class: "answer"}},
		Retarget: []PlanRetarget{{Task: "ship", Deps: []string{"build2"}}},
	})
	build2 := am.Added[0]
	if len(am.Retargeted) != 1 || am.Retargeted[0] != ship.TaskID {
		t.Fatalf("amendment = %+v, want ship re-pointed", am)
	}
	if got := e.taskState(ship.TaskID); got != taskPending {
		t.Errorf("ship state = %q, want pending behind build2", got)
	}
	if got := e.int("SELECT COUNT(*) FROM task_deps WHERE task_id = ? AND depends_on_task_id = ?", ship.TaskID, build.TaskID); got != 0 {
		t.Errorf("ship still depends on the failed task")
	}
	g := e.leaseAny(e.runner1)
	if g == nil || g.TaskID != build2.TaskID {
		t.Fatalf("lease = %+v, want build2", g)
	}
	e.complete(g, done())
	if got := e.taskState(ship.TaskID); got != taskReady {
		t.Errorf("ship state = %q, want ready once build2 is done", got)
	}

	// Dropping the dependency starts the waiting task at once.
	e2 := newEnv(t)
	res2 := e2.twoStep()
	e2.exec("UPDATE tasks SET state = 'failed' WHERE id = ?", res2.Tasks[0].TaskID)
	e2.amend(res2.PlanID, PlanAmendment{Retarget: []PlanRetarget{{Task: "ship", Deps: nil}}})
	if got := e2.taskState(res2.Tasks[1].TaskID); got != taskReady || e2.queuedWork(res2.Tasks[1].TaskID) != 1 {
		t.Errorf("ship = %q with %d queued jobs, want ready with its work job", got, e2.queuedWork(res2.Tasks[1].TaskID))
	}

	// Only a waiting task can be re-pointed.
	if _, err := e2.eng.AmendPlan(e2.ctx, e2.project, res2.PlanID, PlanAmendment{Retarget: []PlanRetarget{{Task: "ship", Deps: nil}}}); err == nil {
		t.Error("re-pointing a ready task succeeded")
	}
}

// TestAmendCycleRefusesWhole: a cycle through a re-pointed edge refuses
// the whole amendment — the task it adds does not land either.
func TestAmendCycleRefusesWhole(t *testing.T) {
	e := newEnv(t)
	res := e.createPlan([]PlanTask{
		{Name: "a", Spec: "a", Class: "answer"},
		{Name: "b", Spec: "b", Class: "answer", Deps: []string{"a"}},
		{Name: "c", Spec: "c", Class: "answer", Deps: []string{"b"}},
	})
	before := e.int("SELECT COUNT(*) FROM tasks WHERE plan_id = ?", res.PlanID)
	events := e.int("SELECT COUNT(*) FROM events WHERE kind = 'plan_amended'")
	_, err := e.eng.AmendPlan(e.ctx, e.project, res.PlanID, PlanAmendment{
		Add:      []PlanTask{{Name: "d", Spec: "d", Class: "answer"}},
		Retarget: []PlanRetarget{{Task: "b", Deps: []string{"c"}}},
	})
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cyclic amendment err = %v, want a cycle refusal", err)
	}
	if got := e.int("SELECT COUNT(*) FROM tasks WHERE plan_id = ?", res.PlanID); got != before {
		t.Errorf("tasks = %d, want %d: the refusal was partial", got, before)
	}
	if got := e.int("SELECT COUNT(*) FROM events WHERE kind = 'plan_amended'"); got != events {
		t.Errorf("a refused amendment left an event")
	}
	if got := e.int("SELECT COUNT(*) FROM task_deps WHERE task_id = ?", res.Tasks[1].TaskID); got != 1 {
		t.Errorf("b's deps = %d, want its original one", got)
	}
	// A cycle among added tasks is refused too.
	if _, err := e.eng.AmendPlan(e.ctx, e.project, res.PlanID, PlanAmendment{Add: []PlanTask{
		{Name: "x", Spec: "x", Deps: []string{"y"}},
		{Name: "y", Spec: "y", Deps: []string{"x"}},
	}}); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Errorf("added cycle err = %v", err)
	}
}

// TestAmendJudgesAddedTasks: an amendment of an active plan is judged by
// what it adds. Growing the plan past approval_max_tasks no longer holds
// the whole plan: it stays active and the added tasks run as usual.
func TestAmendJudgesAddedTasks(t *testing.T) {
	e := newEnv(t)
	e.setSettings(`{"approval_max_tasks": 3}`)
	res := e.createPlan([]PlanTask{
		{Name: "a", Spec: "a", Class: "answer"},
		{Name: "b", Spec: "b", Class: "answer"},
	})
	e.assertRuns(res, 2)

	am := e.amend(res.PlanID, PlanAmendment{Add: []PlanTask{
		{Name: "c", Spec: "c", Class: "answer"},
		{Name: "d", Spec: "d", Class: "answer", Deps: []string{"a"}},
	}})
	if am.State != "active" || len(am.Held) != 0 || len(am.Reasons) != 0 {
		t.Fatalf("amendment = %+v, want active with nothing held and no reasons", am)
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != "active" {
		t.Errorf("plan state = %q, want active", got)
	}
	if got := e.int("SELECT COUNT(*) FROM events WHERE kind = 'plan_awaiting_approval'"); got != 0 {
		t.Errorf("plan_awaiting_approval events = %d, want none", got)
	}
	// c waits for nothing and starts at once; d waits on a.
	if got := e.taskState(am.Added[0].TaskID); got != taskReady || am.Added[0].JobID == 0 {
		t.Errorf("c = %q job %d, want ready with a job", got, am.Added[0].JobID)
	}
	if got := e.taskState(am.Added[1].TaskID); got != taskPending || am.Added[1].JobID != 0 {
		t.Errorf("d = %q job %d, want pending with no job", got, am.Added[1].JobID)
	}
}

// TestAmendHoldsTrippingAddedTask: an added task that trips a plan rule
// is held alone — parked at needs_foreman with a needs-you line naming
// the rule — while the plan stays active and the rest runs on.
func TestAmendHoldsTrippingAddedTask(t *testing.T) {
	e := newEnv(t)
	e.setSettings(`{"approval_protected_paths": ["go.mod"]}`)
	res := e.createPlan([]PlanTask{{Name: "a", Spec: "a", Class: "answer"}})
	e.assertRuns(res, 1)

	am := e.amend(res.PlanID, PlanAmendment{Add: []PlanTask{
		{Name: "touch", Spec: "edit go.mod", Class: "answer"},
		{Name: "clean", Spec: "do something else", Class: "answer"},
	}})
	if am.State != "active" || len(am.Held) != 1 {
		t.Fatalf("amendment = %+v, want active with one held task", am)
	}
	held := am.Held[0]
	if held.Name != "touch" || !strings.Contains(held.Reason, "touches go.mod") {
		t.Errorf("held = %+v, want the touch task with the protected-path reason", held)
	}
	if got := e.taskState(held.TaskID); got != taskNeedsForeman {
		t.Errorf("held task state = %q, want needs_foreman", got)
	}
	if got := e.queuedWork(held.TaskID); got != 0 {
		t.Errorf("held task has %d queued jobs, want none", got)
	}
	// The clean task runs on.
	clean := am.Added[1]
	if got := e.taskState(clean.TaskID); got != taskReady || clean.JobID == 0 {
		t.Errorf("clean task = %q job %d, want ready with a job", got, clean.JobID)
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != "active" {
		t.Errorf("plan state = %q, want active", got)
	}
	if p, ok := e.eventPayloadOf(evTaskPlanHeld); !ok || !strings.Contains(p.Reason, "touches go.mod") {
		t.Errorf("task_plan_held event = %+v, want the rule named", p)
	}

	// The needs-you queue names the held task and the rule.
	st, err := e.eng.Status(e.ctx, e.project)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, it := range st.NeedsYou {
		if it.Kind == "task" && it.ID == held.TaskID {
			found = true
			if it.State != taskNeedsForeman || !strings.Contains(it.Reason, "touches go.mod") {
				t.Errorf("needs-you entry = %+v, want needs_foreman naming the rule", it)
			}
		}
	}
	if !found {
		t.Errorf("needs-you entries = %+v, want the held task", st.NeedsYou)
	}

	// A class outside approval_classes trips the same way.
	e2 := newEnv(t)
	e2.setSettings(`{"approval_classes": ["implement"]}`)
	res2 := e2.createPlan([]PlanTask{{Name: "a", Spec: "a", Class: "implement"}})
	am2 := e2.amend(res2.PlanID, PlanAmendment{Add: []PlanTask{{Name: "deploy", Spec: "deploy it", Class: "deploy"}}})
	if am2.State != "active" || len(am2.Held) != 1 || !strings.Contains(am2.Held[0].Reason, "class deploy outside") {
		t.Fatalf("amendment = %+v, want active with the deploy task held for its class", am2)
	}
	if got := e2.taskState(am2.Held[0].TaskID); got != taskNeedsForeman {
		t.Errorf("deploy task state = %q, want needs_foreman", got)
	}
}

// TestAmendCancellingRepointingRetieringNeverHolds: the per-task rules
// apply only to tasks the amendment adds; cancelling, re-pointing or
// retiering an existing task never holds it, whatever it trips.
func TestAmendCancellingRepointingRetieringNeverHolds(t *testing.T) {
	e := newEnv(t)
	e.setSettings(`{"approval_classes": ["implement"]}`)
	// A plan full of answer tasks would be held at creation, so build one
	// with an allowed class and then amend its tasks in place.
	res := e.createPlan([]PlanTask{
		{Name: "a", Spec: "a", Class: "implement"},
		{Name: "b", Spec: "b", Class: "implement", Deps: []string{"a"}},
	})
	am := e.amend(res.PlanID, PlanAmendment{
		Retarget: []PlanRetarget{{Task: "b", Deps: nil}},
		Retier:   []PlanRetier{{Task: "a", Tier: "heavy"}},
	})
	if am.State != "active" || len(am.Held) != 0 {
		t.Fatalf("amendment = %+v, want active with nothing held", am)
	}
	if got := e.taskState(res.Tasks[1].TaskID); got != taskReady {
		t.Errorf("b state = %q, want ready after the re-point", got)
	}
	if got := e.str("SELECT tier FROM tasks WHERE id = ?", res.Tasks[0].TaskID); got != "heavy" {
		t.Errorf("a tier = %q, want heavy", got)
	}
	am = e.amend(res.PlanID, PlanAmendment{Cancel: []string{"a"}})
	if am.State != "active" || len(am.Held) != 0 {
		t.Fatalf("cancelling amendment = %+v, want active with nothing held", am)
	}
}

// TestCancelledFailureIsNotPlannerSuccess: a task parked at needs_foreman
// and then cancelled by the foreman is an abandoned failure — the plan
// closes as a failure — while dropping a task before it started is a
// skip that leaves the plan a success.
func TestCancelledFailureIsNotPlannerSuccess(t *testing.T) {
	e := newEnv(t)
	agent := e.addAgent(e.runner1, "claude", "sonnet", "work")
	res := e.createPlan([]PlanTask{{Name: "solo", Spec: "do it", Class: "answer"}})
	e.complete(e.leaseAny(e.runner1), Completion{Result: `{"status":"failed","summary":"no clone"}`})
	am := e.amend(res.PlanID, PlanAmendment{Cancel: []string{"solo"}})
	if am.State != planDone {
		t.Fatalf("amendment = %+v, want the plan closed", am)
	}
	if a, s := e.planOutcome(agent); a != 1 || s != 0 {
		t.Errorf("plan outcome = %d/%d, want one failure for an abandoned failed task", s, a)
	}
	if p := e.str("SELECT payload FROM events WHERE kind = 'plan_done'"); !strings.Contains(p, "without the user's approval") {
		t.Errorf("plan_done payload = %s, want the abandoned task named", p)
	}

	// A done task plus one the foreman alone dropped before it started:
	// no one approved the skip, so still a failure.
	res2 := e.createPlan([]PlanTask{
		{Name: "one", Spec: "one", Class: "answer"},
		{Name: "extra", Spec: "extra", Class: "answer", Deps: []string{"one"}},
	})
	e.complete(e.leaseAny(e.runner1), done())
	e.amend(res2.PlanID, PlanAmendment{Cancel: []string{"extra"}})
	if got := e.str("SELECT state FROM plans WHERE id = ?", res2.PlanID); got != planDone {
		t.Fatalf("plan 2 = %q, want done", got)
	}
	if a, s := e.planOutcome(agent); a != 2 || s != 0 {
		t.Errorf("plan outcome = %d/%d, want 0/2: an unapproved skip is no success", s, a)
	}

	// The same cancellation with the user's approval is a skip.
	res3 := e.createPlan([]PlanTask{
		{Name: "one", Spec: "one", Class: "answer"},
		{Name: "extra", Spec: "extra", Class: "answer", Deps: []string{"one"}},
	})
	e.complete(e.leaseAny(e.runner1), done())
	e.amend(res3.PlanID, PlanAmendment{Cancel: []string{"extra"}, SkipApproved: true})
	if a, s := e.planOutcome(agent); a != 3 || s != 1 {
		t.Errorf("plan outcome = %d/%d, want 1/3: the approved skip is no failure", s, a)
	}
}

// TestRestartedThenCancelledIsNotLaundered: a failed task restarted to
// ready and then cancelled by the foreman is still an abandoned failure.
func TestRestartedThenCancelledIsNotLaundered(t *testing.T) {
	e := newEnv(t)
	agent := e.addAgent(e.runner1, "claude", "sonnet", "work")
	res := e.createPlan([]PlanTask{{Name: "solo", Spec: "do it", Class: "answer"}})
	e.exec("UPDATE tasks SET effective_tier = 'heavy', effective_effort = 'high' WHERE plan_id = ?", res.PlanID)
	e.complete(e.leaseAny(e.runner1), Completion{Result: `{"status":"failed","summary":"no clone"}`})
	if _, err := e.eng.RestartTask(e.ctx, e.project, res.TaskID, "try again"); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if got := e.taskState(res.TaskID); got != taskReady {
		t.Fatalf("task = %q, want ready after the restart", got)
	}
	e.amend(res.PlanID, PlanAmendment{Cancel: []string{"solo"}})
	if a, s := e.planOutcome(agent); a != 1 || s != 0 {
		t.Errorf("plan outcome = %d/%d, want a failure", s, a)
	}
}

// TestCancellationThenOtherTaskCompletes: the plan closes through another
// task's normal completion, and the earlier unapproved cancellation still
// counts against it.
func TestCancellationThenOtherTaskCompletes(t *testing.T) {
	e := newEnv(t)
	agent := e.addAgent(e.runner1, "claude", "sonnet", "work")
	res := e.createPlan([]PlanTask{
		{Name: "x", Spec: "x", Class: "answer"},
		{Name: "y", Spec: "y", Class: "answer"},
	})
	e.amend(res.PlanID, PlanAmendment{Cancel: []string{"x"}})
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != "active" {
		t.Fatalf("plan = %q, want active: y is live", got)
	}
	e.complete(e.leaseAny(e.runner1), done())
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != planDone {
		t.Fatalf("plan = %q, want done", got)
	}
	if a, s := e.planOutcome(agent); a != 1 || s != 0 {
		t.Errorf("plan outcome = %d/%d, want a failure", s, a)
	}
}

// TestApproveFinishesAnEmptiedPlan: a plan held for approval whose tasks
// were all cancelled finishes at approval, with the plan_done event
// naming the plan and an outcome recorded.
func TestApproveFinishesAnEmptiedPlan(t *testing.T) {
	e := newEnv(t)
	agent := e.addAgent(e.runner1, "claude", "sonnet", "work")
	e.setSettings(`{"approval_max_tasks": 0}`)
	res := e.createPlan([]PlanTask{{Name: "a", Spec: "a"}, {Name: "b", Spec: "b", Deps: []string{"a"}}})
	e.amend(res.PlanID, PlanAmendment{Cancel: []string{"a", "b"}, SkipApproved: true})
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != planAwaitingApproval {
		t.Fatalf("plan = %q, want still awaiting", got)
	}
	dec, err := e.eng.ApprovePlan(e.ctx, 0, res.PlanID)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if dec.State != planDone {
		t.Errorf("decision = %+v, want done", dec)
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != planDone {
		t.Errorf("plan = %q, want done", got)
	}
	if got := e.int("SELECT COUNT(*) FROM events WHERE kind = 'plan_done' AND json_extract(payload, '$.plan') = ?", res.PlanID); got != 1 {
		t.Errorf("plan_done events naming the plan = %d, want 1", got)
	}
	if a, s := e.planOutcome(agent); a != 1 || s != 1 {
		t.Errorf("plan outcome = %d/%d, want a success: both tasks were dropped unstarted", s, a)
	}
	// The wake the completion caused carries the plan by id and title.
	if p := e.str("SELECT prompt FROM jobs WHERE kind = 'foreman' ORDER BY id DESC LIMIT 1"); !strings.Contains(p, fmt.Sprintf("the plan is done (plan %d ", res.PlanID)) {
		t.Errorf("foreman wake = %q, want the finished plan named", p)
	}
}

// TestAmendWithNamesLikeStandIns: an unnamed task and another task
// literally named "task N" coexist in a plan, and amending it works.
func TestAmendWithNamesLikeStandIns(t *testing.T) {
	e := newEnv(t)
	res := e.createPlan([]PlanTask{{Spec: "unnamed", Class: "answer"}})
	id := res.Tasks[0].TaskID
	e.amend(res.PlanID, PlanAmendment{Add: []PlanTask{
		{Name: fmt.Sprintf("task %d", id), Spec: "lookalike", Class: "answer"},
	}})
	am := e.amend(res.PlanID, PlanAmendment{Add: []PlanTask{
		{Name: "after", Spec: "after", Class: "answer", Deps: []string{fmt.Sprint(id)}},
	}})
	if len(am.Added) != 1 {
		t.Fatalf("amendment = %+v", am)
	}
	if _, err := e.eng.AmendPlan(e.ctx, e.project, res.PlanID, PlanAmendment{
		Retarget: []PlanRetarget{{Task: "after", Deps: []string{"after"}}},
	}); err == nil {
		t.Error("a self-dependency succeeded")
	}
}

// TestFailedOrCancelledDependencyNamedInDoneReport: tasks left waiting
// behind a cancelled dependency are the plan's open end: the plan
// finishes, its done event names them, and the plan outcome is a failure
// on the foreman's agent.
func TestFailedOrCancelledDependencyNamedInDoneReport(t *testing.T) {
	e := newEnv(t)
	agent := e.addAgent(e.runner1, "claude", "sonnet", "work")
	res := e.twoStep()
	g := e.leaseAny(e.runner1)
	e.exec("UPDATE tasks SET effective_tier = 'heavy', effective_effort = 'high' WHERE plan_id = ?", res.PlanID)
	e.complete(g, Completion{Result: `{"status":"failed","summary":"no clone"}`})
	if got := e.taskState(res.Tasks[0].TaskID); got != taskNeedsForeman {
		t.Fatalf("build = %q, want parked", got)
	}
	am := e.amend(res.PlanID, PlanAmendment{Cancel: []string{"build"}})
	if am.State != planDone {
		t.Fatalf("amendment = %+v, want the plan closed: only ship, behind a cancelled task, is left", am)
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != planDone {
		t.Errorf("plan state = %q, want done", got)
	}
	payload := e.str("SELECT payload FROM events WHERE kind = 'plan_done'")
	if !strings.Contains(payload, "never ran") || !strings.Contains(payload, "ship") || !strings.Contains(payload, "cancelled") {
		t.Errorf("plan_done payload = %s, want it to name ship as never run behind a cancelled task", payload)
	}
	var inbox ForemanInbox
	if err := e.st.View(e.ctx, func(tx *sql.Tx) error {
		var err error
		inbox, err = pendingInboxTx(tx, e.project)
		return err
	}); err != nil {
		t.Fatalf("pending inbox: %v", err)
	}
	var line string
	for _, ev := range inbox.Events {
		if strings.HasPrefix(ev, "plan_done") {
			line = ev
		}
	}
	if !strings.Contains(line, "the plan is done") || !strings.Contains(line, "never ran: task") {
		t.Errorf("done report = %q, want it to name what never ran", line)
	}
	if a, s := e.planOutcome(agent); a != 1 || s != 0 {
		t.Errorf("plan outcome = %d/%d, want one failed attempt", s, a)
	}
	// A done plan is no longer amended.
	if _, err := e.eng.AmendPlan(e.ctx, e.project, res.PlanID, PlanAmendment{Cancel: []string{"ship"}}); err == nil {
		t.Error("amending a done plan succeeded")
	}
}

// TestPlanOutcomeLandsOnForemanAgent: a plan whose tasks all end done
// counts a success on the foreman's own agent — the harness and model the
// project names — and no other; a rejected plan counts a failure; a
// failed task counts a failure. The stats list the plan class rows.
func TestPlanOutcomeLandsOnForemanAgent(t *testing.T) {
	e := newEnv(t)
	foreman := e.addAgent(e.runner1, "claude", "sonnet", "work")
	other := e.addAgent(e.runner2, "kimi", "k2", "home")

	res := e.twoStep()
	for i := 0; i < 2; i++ {
		g := e.leaseAny(e.runner1)
		if g == nil {
			t.Fatalf("no job to lease at step %d", i)
		}
		e.complete(g, done())
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != planDone {
		t.Fatalf("plan state = %q, want done", got)
	}
	if a, s := e.planOutcome(foreman); a != 1 || s != 1 {
		t.Errorf("foreman plan outcome = %d/%d, want 1/1", s, a)
	}
	if a, _ := e.planOutcome(other); a != 0 {
		t.Errorf("another agent counted %d plan attempts", a)
	}

	// A task that failed out: the plan closes as a failure.
	res2 := e.twoStep()
	e.exec("UPDATE tasks SET state = 'failed' WHERE id = ?", res2.Tasks[0].TaskID)
	e.amend(res2.PlanID, PlanAmendment{Cancel: []string{"ship"}})
	if a, s := e.planOutcome(foreman); a != 2 || s != 1 {
		t.Errorf("foreman plan outcome = %d/%d, want 1/2 after a failed task", s, a)
	}

	// A rejected plan is a failure too.
	e.setSettings(`{"approval_max_tasks": 0}`)
	held := e.createPlan(nTasks(1))
	if _, err := e.eng.RejectPlan(e.ctx, 0, held.PlanID); err != nil {
		t.Fatalf("reject: %v", err)
	}
	if a, s := e.planOutcome(foreman); a != 3 || s != 1 {
		t.Errorf("foreman plan outcome = %d/%d, want 1/3 after a rejected plan", s, a)
	}

	stats, err := e.eng.Stats(e.ctx, e.project)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	var found bool
	for _, r := range stats[0].Rows {
		if r.Class == "plan" && r.AgentID == foreman {
			found = true
			if r.Attempts != 3 || r.Successes != 1 || r.Harness != "claude" {
				t.Errorf("plan row = %+v, want claude 1/3", r)
			}
		}
		if r.Class == "plan" && r.AgentID != foreman {
			t.Errorf("plan row for agent %d, want only the foreman's", r.AgentID)
		}
	}
	if !found {
		t.Errorf("stats = %+v, want a plan class row for the foreman", stats[0].Rows)
	}
}

func TestAmendValidatesTaskTierEffort(t *testing.T) {
	e := newEnv(t)
	res := e.twoStep()
	for _, tc := range []struct {
		task  PlanTask
		words string
	}{
		{PlanTask{Name: "invalid", Spec: "x", Tier: "big"}, "light, standard, heavy"},
		{PlanTask{Name: "invalid", Spec: "x", Effort: "ultra"}, "low, medium, high, max"},
	} {
		before := e.int("SELECT COUNT(*) FROM tasks")
		version := e.int("SELECT version FROM plans WHERE id = ?", res.PlanID)
		_, err := e.eng.AmendPlan(e.ctx, e.project, res.PlanID, PlanAmendment{Add: []PlanTask{{Spec: "valid"}, tc.task}, Cancel: []string{"build"}})
		if err == nil || !strings.Contains(err.Error(), "invalid") || !strings.Contains(err.Error(), tc.words) {
			t.Fatalf("refusal = %v", err)
		}
		if e.int("SELECT COUNT(*) FROM tasks") != before || e.int("SELECT version FROM plans WHERE id = ?", res.PlanID) != version || e.taskState(res.Tasks[0].TaskID) == taskCancelled {
			t.Fatal("invalid amendment partially applied")
		}
	}
}

func TestAmendRetier(t *testing.T) {
	e := newEnv(t)
	res := e.twoStep()
	build, ship := res.Tasks[0].TaskID, res.Tasks[1].TaskID
	e.exec("UPDATE tasks SET state = 'needs_foreman', tier = 'standard', effective_tier = 'heavy', effort = 'max' WHERE id = ?", build)
	am := e.amend(res.PlanID, PlanAmendment{Retier: []PlanRetier{{Task: "build", Tier: "light", Effort: "low"}}})
	if len(am.Retiered) != 1 || am.Retiered[0] != build {
		t.Fatalf("retiered = %v", am.Retiered)
	}
	for col, want := range map[string]string{"tier": "light", "effective_tier": "light", "effort": "low", "effective_effort": "low", "state": taskNeedsForeman} {
		if got := e.str("SELECT "+col+" FROM tasks WHERE id = ?", build); got != want {
			t.Errorf("%s = %q, want %q", col, got, want)
		}
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ?", build); n != 1 {
		t.Errorf("retier restarted the task: %d jobs", n)
	}
	if p, ok := e.eventPayloadOf(evPlanAmended); !ok || len(p.Retiered) != 1 || p.Retiered[0] != build {
		t.Errorf("event = %+v", p)
	}
	// By id, effort alone preserves the foreman's and effective tiers.
	e.exec("UPDATE tasks SET effective_tier = 'heavy' WHERE id = ?", build)
	e.amend(res.PlanID, PlanAmendment{Retier: []PlanRetier{{Task: fmt.Sprint(build), Effort: "medium"}, {Task: "ship", Tier: "heavy"}}})
	if got := e.str("SELECT tier || '|' || effective_tier || '|' || effort || '|' || effective_effort FROM tasks WHERE id = ?", build); got != "light|heavy|medium|medium" {
		t.Errorf("effort-only = %s", got)
	}
	if e.taskState(ship) != taskPending || e.str("SELECT tier FROM tasks WHERE id = ?", ship) != "heavy" {
		t.Error("pending task not retiered in place")
	}
	// Tier alone preserves effort and resets an escalated effective effort
	// to it. A ready task can be changed as well.
	e.exec("UPDATE tasks SET state = 'ready', effective_effort = 'high' WHERE id = ?", build)
	e.amend(res.PlanID, PlanAmendment{Retier: []PlanRetier{{Task: "build", Tier: "standard"}}})
	if got := e.str("SELECT tier || '|' || effective_tier || '|' || effort || '|' || effective_effort FROM tasks WHERE id = ?", build); got != "standard|standard|medium|medium" {
		t.Errorf("tier-only = %s", got)
	}

	for _, state := range []string{taskRunning, taskChecking, taskReviewing, taskDone, taskCancelled, taskNeedsUser, taskFailed} {
		t.Run(state, func(t *testing.T) {
			e.exec("UPDATE tasks SET state = ? WHERE id = ?", state, build)
			_, err := e.eng.AmendPlan(e.ctx, e.project, res.PlanID, PlanAmendment{
				Retier: []PlanRetier{{Task: "ship", Tier: "light"}, {Task: "build", Tier: "light"}},
				Add:    []PlanTask{{Name: "extra", Spec: "must not land"}},
			})
			if err == nil || !strings.Contains(err.Error(), "build") || !strings.Contains(err.Error(), state) {
				t.Fatalf("refusal = %v", err)
			}
			if e.str("SELECT tier FROM tasks WHERE id = ?", ship) != "heavy" || e.int("SELECT COUNT(*) FROM tasks WHERE name = 'extra'") != 0 {
				t.Error("refused amendment partially applied")
			}
		})
	}
	e.exec("UPDATE tasks SET state = 'ready' WHERE id = ?", build)
	for _, rt := range []PlanRetier{{Task: "build", Tier: "huge"}, {Task: "build", Effort: "ultra"}, {Task: "absent", Tier: "light"}} {
		if _, err := e.eng.AmendPlan(e.ctx, e.project, res.PlanID, PlanAmendment{Retier: []PlanRetier{rt}}); err == nil {
			t.Errorf("bad retier accepted: %+v", rt)
		}
	}
}
