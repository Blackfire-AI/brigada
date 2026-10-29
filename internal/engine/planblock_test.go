package engine

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// seedChain builds the plan the blocked-plan tests share (§6.2): x, a
// (deps x) and b (deps a), all implement, each driven to done through
// work and an approving review — then x, whose landing never happened,
// fails as a restarted attempt would. a is stranded by the dead
// dependency, b stranded through a; a's recorded base is x's result
// branch, the stack the default single-dependency rule builds, unless
// the test moves it to the land target.
func seedChain(t *testing.T) (e *env, author, plan, x, a, b int64) {
	t.Helper()
	e = newEnv(t)
	author = e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	res := e.createPlan([]PlanTask{
		{Name: "x", Spec: "the base work"},
		{Name: "a", Spec: "a", Deps: []string{"x"}},
		{Name: "b", Spec: "b", Deps: []string{"a"}},
	})
	x, a, b = res.Tasks[0].TaskID, res.Tasks[1].TaskID, res.Tasks[2].TaskID
	plan = res.PlanID
	e.runTaskToDone(x, author, reviewer)
	e.runTaskToDone(a, author, reviewer)
	e.runTaskToDone(b, author, reviewer)
	// The dead dependency: failed, and its queued landing retired.
	e.exec("UPDATE tasks SET state = ? WHERE id = ?", taskFailed, x)
	e.exec("UPDATE jobs SET status = 'done' WHERE task_id = ? AND kind = 'land'", x)
	return e, author, plan, x, a, b
}

// finishPlan runs the plan-closing rule once, the way a completion or
// an amendment does.
func (e *env) finishPlan(plan int64) {
	e.t.Helper()
	if err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
		_, err := e.eng.finishPlanIfClosedTx(tx, e.project, plan, 0, 0, e.clock.Now())
		return err
	}); err != nil {
		e.t.Fatalf("finish plan %d: %v", plan, err)
	}
}

// TestBlockedPlanStaysActiveAndWakesOnce: a plan whose done work can
// never land — a failed dependency under it — does not close. It stays
// active as blocked, one plan_blocked wake names every stranded task
// with its root cause, the needs-you queue carries one line per
// stranded task, and re-evaluating the blockage neither wakes again nor
// writes plan_done.
func TestBlockedPlanStaysActiveAndWakesOnce(t *testing.T) {
	e, _, plan, x, a, b := seedChain(t)
	e.finishPlan(plan)

	if got := e.str("SELECT state FROM plans WHERE id = ?", plan); got != "active" {
		t.Fatalf("plan state = %q, want active as blocked", got)
	}
	p, ok := e.eventPayloadOf(evPlanBlocked)
	if !ok {
		t.Fatal("no plan_blocked event")
	}
	if p.Count != 2 ||
		!strings.Contains(p.Reason, fmt.Sprintf("task %d a cannot land", a)) ||
		!strings.Contains(p.Reason, fmt.Sprintf("task %d b cannot land", b)) ||
		!strings.Contains(p.Reason, fmt.Sprintf("task %d x is failed", x)) {
		t.Errorf("plan_blocked = %+v, want it to name a and b stranded by the failed x", p)
	}
	if got := e.foremanJobs(); got != 1 {
		t.Errorf("foreman turns = %d, want the one wake of the blockage", got)
	}

	// One needs-you line per stranded task, naming the dead dependency.
	st, err := e.eng.Status(e.ctx, e.project)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{a, b} {
		var found bool
		for _, it := range st.NeedsYou {
			if it.Kind == "task" && it.ID == id {
				found = true
				if it.State != taskDone || !strings.Contains(it.Reason, fmt.Sprintf("task %d x is failed", x)) {
					t.Errorf("needs-you entry = %+v, want done naming the failed dependency", it)
				}
			}
		}
		if !found {
			t.Errorf("needs-you entries = %+v, want one for task %d", st.NeedsYou, id)
		}
	}

	// Re-evaluating the blockage wakes and announces nothing again, and
	// the plan never reaches plan_done while it is blocked.
	e.finishPlan(plan)
	if got := e.countEvents(evPlanBlocked); got != 1 {
		t.Errorf("plan_blocked events = %d, want the one of the episode", got)
	}
	if got := e.foremanJobs(); got != 1 {
		t.Errorf("foreman turns after the re-evaluation = %d, want still one", got)
	}
	if _, ok := e.eventPayloadOf(evPlanDone); ok {
		t.Error("a blocked plan wrote plan_done")
	}
}

// TestRetargetOfStrandedDoneUnblocksAndCloses: re-pointing a stranded
// done task off its dead dependency makes it landable without running
// again. The retarget queues its landing at once — on the land target,
// its own patch alone — the landing releases the task above it, and the
// plan closes with plan_done only after the whole chain landed.
func TestRetargetOfStrandedDoneUnblocksAndCloses(t *testing.T) {
	e, author, plan, x, a, b := seedChain(t)
	// The blockage is announced — the wake and the needs-you lines of
	// §6.2 — before the foreman answers it.
	e.finishPlan(plan)
	if _, ok := e.eventPayloadOf(evPlanBlocked); !ok {
		t.Fatal("no plan_blocked event before the retarget")
	}
	// a's work was built on the land target, not on x's branch.
	e.exec("UPDATE tasks SET base_ref = 'main' WHERE id = ?", a)

	am := e.amend(plan, PlanAmendment{Retarget: []PlanRetarget{{Task: "a", Deps: nil}}})
	if len(am.Retargeted) != 1 || am.Retargeted[0] != a {
		t.Fatalf("amendment = %+v, want a re-pointed", am)
	}
	if got := e.int("SELECT COUNT(*) FROM task_deps WHERE task_id = ? AND depends_on_task_id = ?", a, x); got != 0 {
		t.Errorf("a still depends on the failed x")
	}
	if got := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'land'", a); got != 1 {
		t.Fatalf("a's land jobs = %d, want the retarget to queue the landing", got)
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", plan); got != "active" {
		t.Fatalf("plan state = %q, want active while a's landing is live", got)
	}
	if got := e.countEvents(evPlanUnblocked); got != 1 {
		t.Errorf("plan_unblocked events = %d, want the episode's end recorded", got)
	}
	// The stranded needs-you lines left with the blockage.
	st, err := e.eng.Status(e.ctx, e.project)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range st.NeedsYou {
		if it.Kind == "task" && (it.ID == a || it.ID == b) && strings.Contains(it.Reason, "cannot land") {
			t.Errorf("needs-you entry = %+v, want the blockage gone", it)
		}
	}

	// a lands its own patch alone: the grant's base is the land target.
	lg := e.leaseGit(author)
	if lg == nil || lg.TaskID != a {
		t.Fatalf("lease = %+v, want a's land job", lg)
	}
	if lg.BaseRef != "main" || lg.LandTarget != "main" {
		t.Errorf("a's land grant base/target = %q/%q, want the land target: the patch is a's alone, none of the cancelled work",
			lg.BaseRef, lg.LandTarget)
	}
	if ans := e.complete(lg, Completion{Result: `{"target_branch":"main","commit":"aaa111","check_passed":true,"pushed":"pushed"}`}); ans != AnsOK {
		t.Fatalf("land a: %s", ans)
	}

	// b waited on a; with a landed its own landing is released, and the
	// plan closes only behind it.
	if got := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'land'", b); got != 1 {
		t.Fatalf("b's land jobs = %d, want its landing released by a's", got)
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", plan); got != "active" {
		t.Fatalf("plan state after a landed = %q, want active while b's landing is live", got)
	}
	lg2 := e.leaseGit(author)
	if lg2 == nil || lg2.TaskID != b {
		t.Fatalf("lease = %+v, want b's land job", lg2)
	}
	if ans := e.complete(lg2, Completion{Result: `{"target_branch":"main","commit":"bbb222","check_passed":true,"pushed":"pushed"}`}); ans != AnsOK {
		t.Fatalf("land b: %s", ans)
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", plan); got != planDone {
		t.Fatalf("plan state = %q, want done once the chain landed", got)
	}
	p, ok := e.eventPayloadOf(evPlanDone)
	if !ok || !strings.Contains(p.Reason, fmt.Sprintf("landed: task %d a (main@aaa111)", a)) ||
		!strings.Contains(p.Reason, fmt.Sprintf("task %d b (main@bbb222)", b)) {
		t.Errorf("plan_done = %+v, want the report to name both landings", p)
	}
}

// TestRetargetOfStrandedDoneStackedOnDeadDepRefused: a done task whose
// recorded base is the dead dependency's result branch carries the
// cancelled work in its diff; re-pointing it is refused with the reason,
// and the refusal applies nothing.
func TestRetargetOfStrandedDoneStackedOnDeadDepRefused(t *testing.T) {
	e, _, plan, x, a, _ := seedChain(t)
	xBranch := e.str("SELECT branch FROM tasks WHERE id = ?", x)
	if base := e.str("SELECT base_ref FROM tasks WHERE id = ?", a); base == "" || base != xBranch {
		t.Fatalf("a's base = %q, want x's branch %q: the default stack", base, xBranch)
	}
	version := e.int("SELECT version FROM plans WHERE id = ?", plan)
	events := e.int("SELECT COUNT(*) FROM events")

	_, err := e.eng.AmendPlan(e.ctx, e.project, plan, PlanAmendment{
		Retarget: []PlanRetarget{{Task: "a", Deps: nil}},
	})
	if err == nil || !strings.Contains(err.Error(), "stacked on") || !strings.Contains(err.Error(), "restarted on a new base") {
		t.Fatalf("retarget err = %v, want the stacked refusal", err)
	}
	if got := e.int("SELECT COUNT(*) FROM task_deps WHERE task_id = ? AND depends_on_task_id = ?", a, x); got != 1 {
		t.Errorf("a's dep on x = %d, want it kept: the refusal was partial", got)
	}
	if got := e.int("SELECT version FROM plans WHERE id = ?", plan); got != version {
		t.Errorf("plan version moved under a refused amendment")
	}
	if got := e.int("SELECT COUNT(*) FROM events"); got != events {
		t.Errorf("the refused amendment left an event")
	}
}

// TestCancelWithDependents: cancelling a task is refused, whole
// amendment applied nothing, while any dependent with work under way or
// an approved result still points at it — each refusal naming the
// dependent and its state. A dependent cancelled in the same amendment,
// a pending one re-pointed off the cancelled task, and a dependent that
// already ended cancelled or failed, never block.
func TestCancelWithDependents(t *testing.T) {
	for _, state := range []string{
		taskPending, taskReady, taskRunning, taskChecking, taskReviewing,
		taskNeedsForeman, taskNeedsUser, taskDone,
	} {
		t.Run(state, func(t *testing.T) {
			e := newEnv(t)
			res := e.createPlan([]PlanTask{
				{Name: "x", Spec: "x", Class: "answer"},
				{Name: "d", Spec: "d", Class: "answer", Deps: []string{"x"}},
			})
			x, d := res.Tasks[0].TaskID, res.Tasks[1].TaskID
			e.exec("UPDATE tasks SET state = 'running' WHERE id = ?", x)
			e.exec("UPDATE tasks SET state = ? WHERE id = ?", state, d)
			version := e.int("SELECT version FROM plans WHERE id = ?", res.PlanID)
			events := e.int("SELECT COUNT(*) FROM events")

			_, err := e.eng.AmendPlan(e.ctx, e.project, res.PlanID, PlanAmendment{Cancel: []string{"x"}})
			if err == nil || !strings.Contains(err.Error(), "dependents built on its branch") ||
				!strings.Contains(err.Error(), fmt.Sprintf("d (%s)", state)) {
				t.Fatalf("cancel with a %s dependent: err = %v, want a refusal naming the dependent and its state", state, err)
			}
			if got := e.taskState(x); got != "running" || e.taskState(d) != state {
				t.Errorf("states after the refusal = %q/%q, want them unchanged", e.taskState(x), e.taskState(d))
			}
			if got := e.int("SELECT version FROM plans WHERE id = ?", res.PlanID); got != version {
				t.Errorf("the refused amendment bumped the plan version")
			}
			if got := e.int("SELECT COUNT(*) FROM events"); got != events {
				t.Errorf("the refused amendment left an event")
			}
		})
	}

	// A pending dependent is covered by cancelling it in the same call.
	t.Run("covered by cancel", func(t *testing.T) {
		e := newEnv(t)
		res := e.createPlan([]PlanTask{
			{Name: "x", Spec: "x", Class: "answer"},
			{Name: "d", Spec: "d", Class: "answer", Deps: []string{"x"}},
		})
		e.amend(res.PlanID, PlanAmendment{Cancel: []string{"x", "d"}})
		if got := e.taskState(res.Tasks[0].TaskID); got != taskCancelled {
			t.Errorf("x = %q, want cancelled", got)
		}
		if got := e.taskState(res.Tasks[1].TaskID); got != taskCancelled {
			t.Errorf("d = %q, want cancelled with its dependency", got)
		}
	})

	// A pending dependent is covered by re-pointing it off x in the
	// same call, and then starts.
	t.Run("covered by retarget", func(t *testing.T) {
		e := newEnv(t)
		res := e.createPlan([]PlanTask{
			{Name: "x", Spec: "x", Class: "answer"},
			{Name: "d", Spec: "d", Class: "answer", Deps: []string{"x"}},
		})
		e.amend(res.PlanID, PlanAmendment{
			Cancel:   []string{"x"},
			Retarget: []PlanRetarget{{Task: "d", Deps: nil}},
		})
		if got := e.taskState(res.Tasks[0].TaskID); got != taskCancelled {
			t.Errorf("x = %q, want cancelled", got)
		}
		d := res.Tasks[1].TaskID
		if got := e.taskState(d); got != taskReady || e.queuedWork(d) != 1 {
			t.Errorf("d = %q with %d queued jobs, want ready and started off the dead dependency", got, e.queuedWork(d))
		}
	})

	// Terminal dependents cannot continue work; they never block.
	for _, tc := range []struct {
		name  string
		state string
	}{{"failed dependent", taskFailed}, {"cancelled dependent", taskCancelled}} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			res := e.createPlan([]PlanTask{
				{Name: "x", Spec: "x", Class: "answer"},
				{Name: "d", Spec: "d", Class: "answer", Deps: []string{"x"}},
			})
			e.exec("UPDATE tasks SET state = 'running' WHERE id = ?", res.Tasks[0].TaskID)
			e.exec("UPDATE tasks SET state = ? WHERE id = ?", tc.state, res.Tasks[1].TaskID)
			e.amend(res.PlanID, PlanAmendment{Cancel: []string{"x"}})
			if got := e.taskState(res.Tasks[0].TaskID); got != taskCancelled {
				t.Errorf("x = %q, want cancelled beside a %s dependent", got, tc.state)
			}
			if got := e.taskState(res.Tasks[1].TaskID); got != tc.state {
				t.Errorf("d = %q, want its terminal state kept", got)
			}
		})
	}
}

// TestRetargetOfStartedTaskStillRefused: a done task that is not
// stranded, and any other started task, keeps the pending-only rule of
// retarget.
func TestRetargetOfStartedTaskStillRefused(t *testing.T) {
	e, _, plan, x, a, _ := seedChain(t)
	// x whole again: a is an ordinary done task, unstranded, and its
	// re-point is refused as any started task's is.
	e.exec("UPDATE tasks SET state = ? WHERE id = ?", taskDone, x)
	_, err := e.eng.AmendPlan(e.ctx, e.project, plan, PlanAmendment{
		Retarget: []PlanRetarget{{Task: "a", Deps: nil}},
	})
	if err == nil || !strings.Contains(err.Error(), "only a waiting (pending) task") {
		t.Fatalf("retarget of a done, unstranded task err = %v, want the pending-only refusal", err)
	}
	e.exec("UPDATE tasks SET state = ? WHERE id = ?", taskReviewing, a)
	_, err = e.eng.AmendPlan(e.ctx, e.project, plan, PlanAmendment{
		Retarget: []PlanRetarget{{Task: "a", Deps: nil}},
	})
	if err == nil || !strings.Contains(err.Error(), "only a waiting (pending) task") {
		t.Fatalf("retarget of a reviewing task err = %v, want the pending-only refusal", err)
	}
}

// TestNonImplementDependencyNeverStrands: the blockage follows the
// landing predicate (§7.1), which skips dependencies of a class that
// never lands — a failed or cancelled non-implement dependency strands
// nothing, and the plan closes normally with plan_done.
func TestNonImplementDependencyNeverStrands(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	res := e.createPlan([]PlanTask{
		{Name: "x", Spec: "read the docs", Class: "answer"},
		{Name: "a", Spec: "a", Deps: []string{"x"}},
	})
	x, a := res.Tasks[0].TaskID, res.Tasks[1].TaskID
	// The answer task finishes outright; a runs to done and approved.
	e.submit(e.workFor(x))
	g := e.leaseAny(author)
	if g == nil || g.TaskID != x {
		t.Fatalf("lease = %+v, want x's work", g)
	}
	e.complete(g, done())
	e.runTaskToDone(a, author, reviewer)

	// x dies after a finished; its class never lands, so a is not
	// stranded: the landing completes and the plan closes.
	e.exec("UPDATE tasks SET state = ? WHERE id = ?", taskFailed, x)
	lg := e.leaseGit(author)
	if lg == nil || lg.TaskID != a {
		t.Fatalf("lease = %+v, want a's land job", lg)
	}
	if ans := e.complete(lg, Completion{Result: `{"target_branch":"main","commit":"abc123","check_passed":true,"pushed":"pushed"}`}); ans != AnsOK {
		t.Fatalf("land a: %s", ans)
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != planDone {
		t.Fatalf("plan state = %q, want done: a non-implement dependency never blocks", got)
	}
	if _, ok := e.eventPayloadOf(evPlanBlocked); ok {
		t.Error("a non-implement dependency raised plan_blocked")
	}
	p, ok := e.eventPayloadOf(evPlanDone)
	if !ok || !strings.Contains(p.Reason, fmt.Sprintf("landed: task %d a (main@abc123)", a)) {
		t.Errorf("plan_done = %+v, want a's landing reported", p)
	}
}

// TestNonImplementTaskNeverStranded: stranded-task detection applies
// only to done tasks eligible for landing (§7.1). A done task of a
// class that never lands is never stranded, whatever its dependencies
// did, and never holds the plan open.
func TestNonImplementTaskNeverStranded(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	res := e.createPlan([]PlanTask{
		{Name: "x", Spec: "the base work"},
		{Name: "a", Spec: "report on it", Class: "answer", Deps: []string{"x"}},
	})
	x, a := res.Tasks[0].TaskID, res.Tasks[1].TaskID
	e.runTaskToDone(x, author, reviewer)
	e.exec("UPDATE jobs SET status = 'done' WHERE task_id = ? AND kind = 'land'", x)
	// The dependency dies before the dependent finishes: the done answer
	// task's completion is where the plan would judge closing.
	e.exec("UPDATE tasks SET state = ? WHERE id = ?", taskFailed, x)
	e.submit(e.workFor(a))
	g := e.leaseAny(author)
	if g == nil || g.TaskID != a {
		t.Fatalf("lease = %+v, want a's work", g)
	}
	e.complete(g, done())

	e.finishPlan(res.PlanID)
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != planDone {
		t.Fatalf("plan state = %q, want done: a done answer task never holds it open", got)
	}
	if _, ok := e.eventPayloadOf(evPlanBlocked); ok {
		t.Error("a done non-implement task raised plan_blocked")
	}
	p, ok := e.eventPayloadOf(evPlanDone)
	if !ok || strings.Contains(p.Reason, "not yet landed") {
		t.Errorf("plan_done = %+v, want no not-yet-landed word for a class that never lands", p)
	}
}

// TestPlanDoneReportsApprovedCancellation: the done report says each
// task as it counts (§6.2): an unapproved cancellation among the
// failures, an approved one as cancelled.
func TestPlanDoneReportsApprovedCancellation(t *testing.T) {
	e := newEnv(t)
	agent := e.addAgent(e.runner1, "claude", "sonnet", "work")
	res := e.createPlan([]PlanTask{
		{Name: "one", Spec: "one", Class: "answer"},
		{Name: "extra", Spec: "extra", Class: "answer", Deps: []string{"one"}},
	})
	e.complete(e.leaseAny(e.runner1), done())
	e.amend(res.PlanID, PlanAmendment{Cancel: []string{"extra"}, SkipApproved: true})
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != planDone {
		t.Fatalf("plan state = %q, want done", got)
	}
	p, ok := e.eventPayloadOf(evPlanDone)
	if !ok || !strings.Contains(p.Reason, "cancelled: task") || !strings.Contains(p.Reason, "extra") ||
		!strings.Contains(p.Reason, "(with the user's approval)") {
		t.Errorf("plan_done = %+v, want the approved cancellation reported as cancelled", p)
	}
	if a, s := e.planOutcome(agent); a != 1 || s != 1 {
		t.Errorf("plan outcome = %d/%d, want a success: the skip was approved", s, a)
	}
}

// TestLandedLabelsInDigestAndStatus: each task line the foreman reads —
// the digest's plan lines and the per-task lines the status renders —
// carries `landed@<sha>` or `not landed` for a done implement task, so
// a task done and not landed reads differently from a landed one; a
// class that never lands says neither.
func TestLandedLabelsInDigestAndStatus(t *testing.T) {
	e := newEnv(t)
	landed := e.task
	notLanded := e.addTask("{}")
	answer := e.addTask("{}")
	e.exec("UPDATE tasks SET state = 'done', class = 'implement' WHERE id IN (?, ?)", landed, notLanded)
	e.exec("UPDATE tasks SET state = 'done', class = 'answer' WHERE id = ?", answer)
	e.exec(`INSERT INTO landings (task_id, project_id, target_branch, commit_sha, pushed, created_at)
		VALUES (?, ?, 'main', 'abc123', 'pushed', ?)`, landed, e.project, seedTS)

	var tasks []StatusTask
	var digest string
	if err := e.st.View(e.ctx, func(tx *sql.Tx) error {
		var err error
		tasks, err = statusPlanTasksTx(tx, e.plan, e.clock.Now())
		if err != nil {
			return err
		}
		digest, err = foremanDigestTx(tx, e.project, e.clock.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	labels := map[int64]string{landed: "", notLanded: "", answer: ""}
	for _, tk := range tasks {
		if _, ok := labels[tk.ID]; ok {
			labels[tk.ID] = tk.LandedLabel()
		}
	}
	if labels[landed] != "landed@abc123" {
		t.Errorf("landed task's label = %q, want landed@abc123", labels[landed])
	}
	if labels[notLanded] != "not landed" {
		t.Errorf("done-not-landed task's label = %q, want not landed", labels[notLanded])
	}
	if labels[answer] != "" {
		t.Errorf("answer task's label = %q, want none: the class never lands", labels[answer])
	}
	lineOf := func(id int64) string {
		for _, l := range strings.Split(digest, "\n") {
			if strings.Contains(l, fmt.Sprintf("task %d \"", id)) {
				return l
			}
		}
		return ""
	}
	if l := lineOf(landed); !strings.Contains(l, "landed@abc123") {
		t.Errorf("digest line of the landed task = %q, want the landed@ marker", l)
	}
	if l := lineOf(notLanded); !strings.Contains(l, "not landed") {
		t.Errorf("digest line of the done-not-landed task = %q, want the not-landed marker", l)
	}
	if l := lineOf(answer); strings.Contains(l, "landed") {
		t.Errorf("digest line of the answer task = %q, want no landing marker: the class never lands", l)
	}
}
