package engine

import (
	"reflect"
	"testing"
	"time"
)

// The start-up reopen pass (issue #137): plans the engine closed `done`
// before #133's live-work rule — the old rule — still hold tasks parked
// at needs_foreman, and every tool that reads a plan's state refuses
// them. The tests simulate the old rule the only faithful way: the
// plan's state is set to done with SQL, exactly what the old engine
// wrote, and the pass must bring it back to active so the existing
// paths — a retried landing, an amendment's cancel, the plan's own
// re-close — run again.

// reopenStrandedPlans runs the pass for the fixture's project.
func (e *env) reopenStrandedPlans() []int64 {
	e.t.Helper()
	reopened, err := e.eng.ReopenStrandedPlans(e.ctx)
	if err != nil {
		e.t.Fatalf("ReopenStrandedPlans: %v", err)
	}
	return reopened
}

// parkByRejectedPush drives the plan's only ready task to done and fails
// its landing with the rejected push RetryLanding retries (#133).
func (e *env) parkByRejectedPush(author int64) {
	e.t.Helper()
	reviewer := e.agented("reviewer", "google", "implement", "review")
	e.runToDone(author, reviewer)
	g := e.leaseGit(author)
	if g == nil || g.Kind != KindLand {
		e.t.Fatalf("the land job was not leased: %+v", g)
	}
	if ans := e.complete(g, Completion{Error: landRejectedErr}); ans != AnsOK {
		e.t.Fatalf("fail the landing: %s", ans)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskNeedsForeman {
		e.t.Fatalf("task state after the rejected push = %q, want needs_foreman", state)
	}
}

// TestReopenPassLetsLandRetryRunAndTheDependentLand: the production
// strand of issue #137. A task parked by a rejected push sits in a plan
// the old rule closed done, its dependent done behind it with the
// landing still to come. The pass reopens the plan, RetryLanding answers
// queued instead of refused, and once the parent lands the dependent's
// own landing is queued — everything through the existing paths, the
// guards untouched.
func TestReopenPassLetsLandRetryRunAndTheDependentLand(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	res := e.createPlan([]PlanTask{
		{Name: "base", Spec: "the base work"},
		{Name: "top", Spec: "the stacked work", Deps: []string{"base"}},
	})
	base, top := res.Tasks[0].TaskID, res.Tasks[1].TaskID
	// Base runs to done and its landing queues; top finishes behind it,
	// done and approved, its own landing waiting on the base's.
	e.runTaskToDone(base, author, reviewer)
	e.runTaskToDone(top, author, reviewer)
	g := e.leaseGit(author)
	if g == nil || g.TaskID != base {
		t.Fatalf("lease = %+v, want base's land job", g)
	}
	if ans := e.complete(g, Completion{Error: landRejectedErr}); ans != AnsOK {
		t.Fatalf("fail base's push: %s", ans)
	}
	if state := e.taskState(base); state != taskNeedsForeman {
		t.Fatalf("base state = %q, want needs_foreman", state)
	}
	// The old rule's close, written straight: the pre-#133 engine
	// closed this plan done over the parked task and its dependent.
	e.exec("UPDATE plans SET state = ? WHERE id = ?", planDone, res.PlanID)

	// The pass reopens the plan over both: base parked at needs_foreman,
	// top done with a landing still to come. No foreman is woken.
	turns := e.foremanJobs()
	reopened := e.reopenStrandedPlans()
	if len(reopened) != 1 || reopened[0] != res.PlanID {
		t.Fatalf("reopened plans = %v, want [%d]", reopened, res.PlanID)
	}
	if state := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); state != "active" {
		t.Fatalf("plan state after the pass = %q, want active", state)
	}
	p, ok := e.eventPayloadOf(evPlanReopened)
	if !ok {
		t.Fatal("no plan_reopened event")
	}
	if p.Plan != res.PlanID || !reflect.DeepEqual(p.Tasks, []int64{base, top}) {
		t.Errorf("plan_reopened = %+v, want plan %d naming tasks %d and %d", p, res.PlanID, base, top)
	}
	if got := e.foremanJobs(); got != turns {
		t.Errorf("foreman turns = %d, want the %d before: the pass wakes no foreman", got, turns)
	}

	// The guard stands, the retry runs: RetryLanding answers queued.
	r := e.retryLanding(e.project, base)
	if r.Outcome != LandRetryQueued || r.JobID == 0 {
		t.Fatalf("RetryLanding = %+v, want queued with its land job", r)
	}
	// The parent lands, and the dependent's own landing is queued by the
	// landing's release — the existing path a reopened plan runs on.
	g2 := e.leaseGit(author)
	if g2 == nil || g2.JobID != r.JobID {
		t.Fatalf("lease = %+v, want the retried land job %d", g2, r.JobID)
	}
	if ans := e.complete(g2, Completion{Result: `{"target_branch":"main","commit":"aaa111","check_passed":true,"pushed":"pushed"}`}); ans != AnsOK {
		t.Fatalf("land base: %s", ans)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'land' AND status IN ('queued', 'leased', 'running')", top); n != 1 {
		t.Fatalf("top's live land jobs = %d, want 1: base's landing releases it", n)
	}
	if state := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); state != "active" {
		t.Fatalf("plan state = %q, want active while the dependent's landing is live", state)
	}
}

// TestReopenPassLetsAmendCancelParkedTasks: two tasks parked in a done
// plan — one by a land conflict, one by a failed land check — are
// cancelled by plan_amend once the pass reopened the plan, and with
// nothing live left the plan closes again with exactly one new
// plan_done event.
func TestReopenPassLetsAmendCancelParkedTasks(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	res := e.createPlan([]PlanTask{
		{Name: "conflicted", Spec: "the conflicting work"},
	})
	conflicted := res.Tasks[0].TaskID
	e.runTaskToDone(conflicted, author, reviewer)
	// The second task is added once the first is done and its landing
	// queued, so each drives its own work and landing in order.
	e.amend(res.PlanID, PlanAmendment{Add: []PlanTask{
		{Name: "unchecked", Spec: "the unchecked work"},
	}})
	unchecked := e.int("SELECT id FROM tasks WHERE plan_id = ? AND name = 'unchecked'", res.PlanID)
	e.runTaskToDone(unchecked, author, reviewer)
	// The landings fail as the two content failures park: a conflict,
	// and a failed check on the landed result.
	byTask := map[int64]Completion{
		conflicted: {Result: `{"target_branch":"main","commit":"","check_passed":false,"conflict_files":["docs/design.md"]}`},
		unchecked:  {Result: `{"target_branch":"main","commit":"x","check_passed":false,"check_log":"TestPush: rejected"}`},
	}
	for i := 0; i < 2; i++ {
		g := e.leaseGit(author)
		if g == nil || g.Kind != KindLand {
			t.Fatalf("land job %d was not leased: %+v", i, g)
		}
		c, ok := byTask[g.TaskID]
		if !ok {
			t.Fatalf("lease = %+v, want one of the plan's land jobs", g)
		}
		if ans := e.complete(g, c); ans != AnsOK {
			t.Fatalf("fail the landing of task %d: %s", g.TaskID, ans)
		}
	}
	for _, id := range []int64{conflicted, unchecked} {
		if state := e.taskState(id); state != taskNeedsForeman {
			t.Fatalf("task %d state = %q, want needs_foreman", id, state)
		}
	}
	// The old rule's close, then the pass.
	e.exec("UPDATE plans SET state = ? WHERE id = ?", planDone, res.PlanID)
	if reopened := e.reopenStrandedPlans(); len(reopened) != 1 || reopened[0] != res.PlanID {
		t.Fatalf("reopened plans = %v, want [%d]", reopened, res.PlanID)
	}
	if state := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); state != "active" {
		t.Fatalf("plan state after the pass = %q, want active", state)
	}

	// The cancels the closed plan refused now run, one amendment each.
	am := e.amend(res.PlanID, PlanAmendment{Cancel: []string{"conflicted"}})
	if len(am.Cancelled) != 1 || am.Cancelled[0] != conflicted {
		t.Fatalf("amendment = %+v, want conflicted cancelled", am)
	}
	if state := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); state != "active" {
		t.Fatalf("plan state after the first cancel = %q, want active under the parked task", state)
	}
	if _, ok := e.eventPayloadOf(evPlanDone); ok {
		t.Error("the plan closed done with a task still parked")
	}
	am = e.amend(res.PlanID, PlanAmendment{Cancel: []string{"unchecked"}})
	if len(am.Cancelled) != 1 || am.Cancelled[0] != unchecked {
		t.Fatalf("amendment = %+v, want unchecked cancelled", am)
	}
	// Nothing live is left: the plan closes again, once, through the
	// closing rule itself.
	if state := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); state != planDone {
		t.Fatalf("plan state after the second cancel = %q, want done", state)
	}
	if n := e.int("SELECT COUNT(*) FROM events WHERE kind = ?", evPlanDone); n != 1 {
		t.Errorf("plan_done events = %d, want exactly one new close", n)
	}
}

// TestReopenPassLeavesSettledPlansDone: the pass touches only a done
// plan that still holds live work — a settled one stays done, one whose
// open tasks are pending behind a failed task stays done, a cancelled
// plan is not its business, and a second run over a reopened plan
// changes nothing.
func TestReopenPassLeavesSettledPlansDone(t *testing.T) {
	t.Run("settled done plan", func(t *testing.T) {
		e := newEnv(t)
		author := e.agented("author", "anthropic", "implement")
		reviewer := e.agented("reviewer", "google", "implement", "review")
		e.runToDone(author, reviewer)
		e.landPushed(author)
		if state := e.str("SELECT state FROM plans WHERE id = ?", e.plan); state != planDone {
			t.Fatalf("plan state = %q, want done before the pass", state)
		}
		if reopened := e.reopenStrandedPlans(); len(reopened) != 0 {
			t.Errorf("reopened plans = %v, want none: no live work remains", reopened)
		}
		if state := e.str("SELECT state FROM plans WHERE id = ?", e.plan); state != planDone {
			t.Errorf("plan state after the pass = %q, want done", state)
		}
		if _, ok := e.eventPayloadOf(evPlanReopened); ok {
			t.Error("the pass wrote plan_reopened over a settled plan")
		}
	})

	t.Run("pending behind a failed task", func(t *testing.T) {
		e := newEnv(t)
		author := e.agented("author", "anthropic", "implement")
		res := e.createPlan([]PlanTask{
			{Name: "broken", Spec: "the failing work"},
			{Name: "waiting", Spec: "the follow-up work", Deps: []string{"broken"}},
		})
		g := e.leaseAny(author)
		if g == nil || g.TaskID != res.Tasks[0].TaskID {
			t.Fatalf("lease = %+v, want broken's work", g)
		}
		if ans := e.complete(g, Completion{Result: `{"status":"failed","summary":"it did not work"}`}); ans != AnsOK {
			t.Fatalf("fail the work: %s", ans)
		}
		// The failure's own judgment parks at needs_foreman; the state
		// the close names is failed, as the old engine left it.
		e.exec("UPDATE tasks SET state = ? WHERE id = ?", taskFailed, res.Tasks[0].TaskID)
		e.finishPlan(res.PlanID)
		if state := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); state != planDone {
			t.Fatalf("plan state = %q, want done: the pending task never kept it open", state)
		}
		if reopened := e.reopenStrandedPlans(); len(reopened) != 0 {
			t.Errorf("reopened plans = %v, want none: a pending task is not live work", reopened)
		}
		if state := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); state != planDone {
			t.Errorf("plan state after the pass = %q, want done", state)
		}
		if _, ok := e.eventPayloadOf(evPlanReopened); ok {
			t.Error("the pass wrote plan_reopened over a plan with nothing live")
		}
	})

	t.Run("cancelled plan holding a parked task", func(t *testing.T) {
		e := newEnv(t)
		author := e.agented("author", "anthropic", "implement")
		e.parkByRejectedPush(author)
		e.exec("UPDATE plans SET state = ? WHERE id = ?", planCancelled, e.plan)
		if reopened := e.reopenStrandedPlans(); len(reopened) != 0 {
			t.Errorf("reopened plans = %v, want none: a cancelled plan is not the pass's", reopened)
		}
		if state := e.str("SELECT state FROM plans WHERE id = ?", e.plan); state != planCancelled {
			t.Errorf("plan state after the pass = %q, want cancelled", state)
		}
		if state := e.taskState(e.task); state != taskNeedsForeman {
			t.Errorf("task state = %q, want needs_foreman untouched", state)
		}
		if _, ok := e.eventPayloadOf(evPlanReopened); ok {
			t.Error("the pass wrote plan_reopened over a cancelled plan")
		}
	})

	t.Run("second run changes nothing", func(t *testing.T) {
		e := newEnv(t)
		author := e.agented("author", "anthropic", "implement")
		e.parkByRejectedPush(author)
		e.exec("UPDATE plans SET state = ? WHERE id = ?", planDone, e.plan)
		if reopened := e.reopenStrandedPlans(); len(reopened) != 1 || reopened[0] != e.plan {
			t.Fatalf("reopened plans = %v, want [%d]", reopened, e.plan)
		}
		updated := e.str("SELECT updated_at FROM plans WHERE id = ?", e.plan)
		e.clock.Advance(time.Minute)
		if reopened := e.reopenStrandedPlans(); len(reopened) != 0 {
			t.Errorf("reopened plans = %v, want none: the plan is active now", reopened)
		}
		if got := e.str("SELECT updated_at FROM plans WHERE id = ?", e.plan); got != updated {
			t.Errorf("updated_at = %q, want the first pass's %q: the second run changes nothing", got, updated)
		}
		if n := e.int("SELECT COUNT(*) FROM events WHERE kind = ?", evPlanReopened); n != 1 {
			t.Errorf("plan_reopened events = %d, want 1", n)
		}
	})
}
