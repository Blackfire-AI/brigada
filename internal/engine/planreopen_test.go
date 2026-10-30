package engine

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// The stranded-plan reopen tests (issue #137): plans a pre-#133 engine
// closed `done` over live work — simulated the honest way, by setting the
// plan's state to done with SQL once the old rule's shape is standing —
// return to active at start-up, and the tools that refuse a plan that is
// not live reach their tasks again through the existing paths.

// reopenStranded runs the pass and fails the test on its error.
func (e *env) reopenStranded() []int64 {
	e.t.Helper()
	reopened, err := e.eng.ReopenStrandedPlans(e.ctx)
	if err != nil {
		e.t.Fatalf("ReopenStrandedPlans: %v", err)
	}
	return reopened
}

// parkTaskByRejectedPush drives one task of a created plan to done and
// parks it at needs_foreman with a push the target moved under (the
// task_land_failed reason landRetryablePush accepts, #133).
func (e *env) parkTaskByRejectedPush(task, author int64) {
	e.t.Helper()
	g := e.leaseGit(author)
	if g == nil || g.TaskID != task || g.Kind != KindLand {
		e.t.Fatalf("lease = %+v, want %d's land job", g, task)
	}
	if ans := e.complete(g, Completion{Error: landRejectedErr}); ans != AnsOK {
		e.t.Fatalf("fail %d's push: %s", task, ans)
	}
	if got := e.taskState(task); got != taskNeedsForeman {
		e.t.Fatalf("task %d state = %q, want needs_foreman after the rejected push", task, got)
	}
}

// TestReopenStrandedPlansLetsTheRetryAndDependentsRun is the production
// strand of plans 81, 83 and 88 (#137): a task a rejected push parked
// sits in a plan the old rule closed done, a dependent added behind it
// pending. The pass reopens the plan — active again, one plan_reopened
// event naming the parked task, the file mirror owing a push, no foreman
// woken — so RetryLanding answers queued instead of refused, the parent
// lands, and the dependent runs. A second pass changes nothing.
func TestReopenStrandedPlansLetsTheRetryAndDependentsRun(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	res := e.createPlan([]PlanTask{{Name: "parent", Spec: "the parent work"}})
	parent := res.Tasks[0].TaskID
	plan := res.PlanID
	e.runTaskToDone(parent, author, reviewer)
	e.parkTaskByRejectedPush(parent, author)
	// The dependent, added while the parent sat parked: pending behind a
	// task that is not done, it never started.
	am := e.amend(plan, PlanAmendment{Add: []PlanTask{
		{Name: "child", Spec: "the child work", Deps: []string{"parent"}},
	}})
	if len(am.Added) != 1 {
		t.Fatalf("amendment = %+v, want the child added", am)
	}
	child := am.Added[0].TaskID
	if got := e.taskState(child); got != taskPending {
		t.Fatalf("child state = %q, want pending behind the parked parent", got)
	}
	wakes := e.int("SELECT COUNT(*) FROM jobs WHERE kind = ?", KindForeman)

	// The old rule's close: the plan went done over the parked task.
	e.exec("UPDATE plans SET state = ? WHERE id = ?", planDone, plan)
	e.clock.Advance(time.Second)
	before := e.str("SELECT updated_at FROM plans WHERE id = ?", plan)
	reopened := e.reopenStranded()
	if len(reopened) != 1 || reopened[0] != plan {
		t.Fatalf("reopened = %v, want [%d]", reopened, plan)
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", plan); got != "active" {
		t.Fatalf("plan state after the pass = %q, want active", got)
	}
	if got := e.str("SELECT updated_at FROM plans WHERE id = ?", plan); got == before {
		t.Error("the pass left updated_at unchanged")
	}
	if n := e.int("SELECT COUNT(*) FROM plan_mirror_dirty WHERE plan_id = ?", plan); n != 1 {
		t.Error("the reopened plan's file is not owed a mirror push")
	}
	p, ok := e.eventPayloadOf(evPlanReopened)
	if !ok {
		t.Fatal("no plan_reopened event")
	}
	if p.Plan != plan || p.Status != "active" {
		t.Errorf("plan_reopened payload = %+v, want the plan and active", p)
	}
	if !strings.Contains(p.Reason, fmt.Sprintf("task %d parent (needs_foreman)", parent)) {
		t.Errorf("plan_reopened reason = %q, want the parked task named", p.Reason)
	}
	if got := e.int("SELECT COUNT(*) FROM jobs WHERE kind = ?", KindForeman); got != wakes {
		t.Errorf("foreman jobs after the pass = %d, want the %d there were: the pass wakes no one", got, wakes)
	}

	// The retry the refusal used to block: queued, the parent done again
	// and its landing submitted — and its pending dependent freed, its
	// work job now the oldest one the author's runner sees.
	retry := e.retryLanding(e.project, parent)
	if retry.Outcome != LandRetryQueued || retry.JobID == 0 {
		t.Fatalf("RetryLanding = %+v, want queued with its land job", retry)
	}
	cw := e.leaseAny(author)
	if cw == nil || cw.TaskID != child || cw.Kind != KindWork {
		t.Fatalf("lease = %+v, want the child's freed work job", cw)
	}
	g := e.leaseGit(author)
	if g == nil || g.JobID != retry.JobID {
		t.Fatalf("lease = %+v, want the retried land job %d", g, retry.JobID)
	}
	if ans := e.complete(g, Completion{Result: `{"target_branch":"main","commit":"p44e","check_passed":true,"pushed":"pushed"}`}); ans != AnsOK {
		t.Fatalf("land the parent: %s", ans)
	}
	if got := e.str("SELECT state FROM tasks WHERE id = ?", parent); got != taskDone {
		t.Errorf("parent state after landing = %q, want done", got)
	}
	if got := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'work' AND status IN ('queued', 'leased', 'running')", child); got != 1 {
		t.Fatalf("child's live work jobs = %d, want 1: the parent's return to done frees it", got)
	}
	if got := e.taskState(child); got != taskRunning {
		t.Errorf("child state = %q, want running on its freed work job", got)
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", plan); got != "active" {
		t.Errorf("plan state = %q, want active while the child runs", got)
	}

	// A second pass is a no-op: the plan is active, nothing to reopen.
	if reopened := e.reopenStranded(); len(reopened) != 0 {
		t.Errorf("second pass reopened %v, want nothing", reopened)
	}
	if n := e.countEvents(evPlanReopened); n != 1 {
		t.Errorf("plan_reopened events = %d, want exactly one", n)
	}
}

// TestReopenStrandedPlansLetsCancelsCloseItAgain: two tasks parked by a
// land conflict and a failed land check — the failures land_retry refuses
// in favour of task_restart — in a plan the old rule closed done. The
// pass reopens it, a plan_amend cancel of each succeeds, and once
// nothing live is left the plan closes again with exactly one new
// plan_done event.
func TestReopenStrandedPlansLetsCancelsCloseItAgain(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	res := e.createPlan([]PlanTask{
		{Name: "one", Spec: "the first work"},
		{Name: "two", Spec: "the second work"},
	})
	one, two := res.Tasks[0].TaskID, res.Tasks[1].TaskID
	plan := res.PlanID

	// Both tasks run to done and approved, their land jobs leased in
	// order: one's landing conflicts, two's fails its landed check.
	g := e.leaseAny(author)
	if g == nil || g.TaskID != one {
		t.Fatalf("lease = %+v, want one's work", g)
	}
	if ans := e.complete(g, doneOn(branchOf(one), passCheck)); ans != AnsOK {
		t.Fatalf("complete one's work: %s", ans)
	}
	g = e.leaseAny(author)
	if g == nil || g.TaskID != two {
		t.Fatalf("lease = %+v, want two's work", g)
	}
	if ans := e.complete(g, doneOn(branchOf(two), passCheck)); ans != AnsOK {
		t.Fatalf("complete two's work: %s", ans)
	}
	e.approveReview(reviewer)
	e.approveReview(reviewer)
	for _, task := range []int64{one, two} {
		if got := e.taskState(task); got != taskDone {
			t.Fatalf("task %d state = %q, want done", task, got)
		}
	}
	g = e.leaseGit(author)
	if g == nil || g.TaskID != one {
		t.Fatalf("lease = %+v, want one's land job", g)
	}
	if ans := e.complete(g, Completion{Result: `{"target_branch":"main","commit":"","check_passed":false,"conflict_files":["rejected.go"]}`}); ans != AnsOK {
		t.Fatalf("conflict one's landing: %s", ans)
	}
	g = e.leaseGit(author)
	if g == nil || g.TaskID != two {
		t.Fatalf("lease = %+v, want two's land job", g)
	}
	if ans := e.complete(g, Completion{Result: `{"target_branch":"main","commit":"x","check_passed":false,"check_log":"TestPush: rejected"}`}); ans != AnsOK {
		t.Fatalf("fail two's landed check: %s", ans)
	}
	for _, task := range []int64{one, two} {
		if got := e.taskState(task); got != taskNeedsForeman {
			t.Fatalf("task %d state = %q, want needs_foreman", task, got)
		}
	}
	e.exec("UPDATE plans SET state = ? WHERE id = ?", planDone, plan)

	reopened := e.reopenStranded()
	if len(reopened) != 1 || reopened[0] != plan {
		t.Fatalf("reopened = %v, want [%d]", reopened, plan)
	}
	p, ok := e.eventPayloadOf(evPlanReopened)
	if !ok || !strings.Contains(p.Reason, fmt.Sprintf("task %d one (needs_foreman)", one)) ||
		!strings.Contains(p.Reason, fmt.Sprintf("task %d two (needs_foreman)", two)) {
		t.Fatalf("plan_reopened = %+v (%t), want both parked tasks named", p, ok)
	}

	// The foreman cancels each parked task in its own amendment: the
	// refusals a done plan raised are gone.
	for _, name := range []string{"one", "two"} {
		am := e.amend(plan, PlanAmendment{Cancel: []string{name}})
		if len(am.Cancelled) != 1 {
			t.Fatalf("cancel %s = %+v, want it to succeed", name, am)
		}
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", plan); got != planDone {
		t.Fatalf("plan state after the cancels = %q, want done: nothing live is left", got)
	}
	if n := e.int("SELECT COUNT(*) FROM events WHERE kind = ? AND project_id = ?", evPlanDone, e.project); n != 1 {
		t.Errorf("plan_done events = %d, want exactly the one new close", n)
	}
}

// TestReopenStrandedPlansTakesBlockedShapesToo: the live-work test
// reads the task alone — the landable test does not exclude a dead
// dependency (§7.1) — so a done plan the earlier behavior closed over a
// stranded chain (a failed dependency under done, approved tasks whose
// landings never happened) holds live work and reopens like any other.
// Back active, the blockage rule holds it open again, announced at the
// plan's next closing evaluation.
func TestReopenStrandedPlansTakesBlockedShapesToo(t *testing.T) {
	e, _, plan, x, a, b := seedChain(t)
	e.exec("UPDATE plans SET state = ? WHERE id = ?", planDone, plan)

	reopened := e.reopenStranded()
	if len(reopened) != 1 || reopened[0] != plan {
		t.Fatalf("reopened = %v, want [%d]: the stranded chain is live work by the close rule's test", reopened, plan)
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", plan); got != "active" {
		t.Fatalf("plan state after the pass = %q, want active", got)
	}
	p, ok := e.eventPayloadOf(evPlanReopened)
	if !ok || !strings.Contains(p.Reason, fmt.Sprintf("task %d a (done)", a)) ||
		!strings.Contains(p.Reason, fmt.Sprintf("task %d b (done)", b)) {
		t.Fatalf("plan_reopened = %+v (%t), want the stranded done tasks named", p, ok)
	}
	if strings.Contains(p.Reason, fmt.Sprintf("task %d x (", x)) {
		t.Errorf("plan_reopened reason = %q, want the failed task not named live", p.Reason)
	}

	// The reopened plan is a blocked shape back active: the closing rule
	// keeps it open and announces the blockage anew.
	e.finishPlan(plan)
	if got := e.str("SELECT state FROM plans WHERE id = ?", plan); got != "active" {
		t.Errorf("plan state after the re-evaluation = %q, want active as blocked", got)
	}
	if _, ok := e.eventPayloadOf(evPlanBlocked); !ok {
		t.Error("no plan_blocked event after the reopened plan's closing evaluation")
	}
	if _, ok := e.eventPayloadOf(evPlanDone); ok {
		t.Error("the reopened blocked plan closed done over its stranded work")
	}
}

// TestReopenStrandedPlansLeavesSettledPlansAlone: a plan done with no
// live work — its only task landed — stays done, with no event.
func TestReopenStrandedPlansLeavesSettledPlansAlone(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	res := e.createPlan([]PlanTask{{Name: "solo", Spec: "the only work"}})
	solo := res.Tasks[0].TaskID
	e.runTaskToDone(solo, author, reviewer)
	g := e.leaseGit(author)
	if g == nil || g.TaskID != solo || g.Kind != KindLand {
		t.Fatalf("lease = %+v, want solo's land job", g)
	}
	if ans := e.complete(g, Completion{Result: `{"target_branch":"main","commit":"s0l0","check_passed":true,"pushed":"pushed"}`}); ans != AnsOK {
		t.Fatalf("land solo: %s", ans)
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != planDone {
		t.Fatalf("plan state = %q, want done before the pass", got)
	}

	if reopened := e.reopenStranded(); len(reopened) != 0 {
		t.Errorf("reopened = %v, want nothing: no live work remains", reopened)
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != planDone {
		t.Errorf("plan state after the pass = %q, want done", got)
	}
	if n := e.countEvents(evPlanReopened); n != 0 {
		t.Errorf("plan_reopened events = %d, want none", n)
	}
}

// TestReopenStrandedPlansLeavesPendingBehindFailedAlone: a done plan
// whose only open tasks are pending behind a failed task — the plan's
// open end, named in its done report — stays done.
func TestReopenStrandedPlansLeavesPendingBehindFailedAlone(t *testing.T) {
	e := newEnv(t)
	res := e.createPlan([]PlanTask{
		{Name: "failer", Spec: "the work that failed"},
		{Name: "waiter", Spec: "the work behind it", Deps: []string{"failer"}},
	})
	failer := res.Tasks[0].TaskID
	e.exec("UPDATE tasks SET state = ? WHERE id = ?", taskFailed, failer)
	e.exec("UPDATE plans SET state = ? WHERE id = ?", planDone, res.PlanID)

	if reopened := e.reopenStranded(); len(reopened) != 0 {
		t.Errorf("reopened = %v, want nothing: pending behind failed is not live work", reopened)
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != planDone {
		t.Errorf("plan state after the pass = %q, want done", got)
	}
	if n := e.countEvents(evPlanReopened); n != 0 {
		t.Errorf("plan_reopened events = %d, want none", n)
	}
}

// TestReopenStrandedPlansLeavesCancelledPlansAlone: a cancelled plan
// holding a parked task is untouched — only done plans reopen.
func TestReopenStrandedPlansLeavesCancelledPlansAlone(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	res := e.createPlan([]PlanTask{{Name: "solo", Spec: "the only work"}})
	solo := res.Tasks[0].TaskID
	e.runTaskToDone(solo, author, reviewer)
	e.parkTaskByRejectedPush(solo, author)
	e.exec("UPDATE plans SET state = ? WHERE id = ?", planCancelled, res.PlanID)

	if reopened := e.reopenStranded(); len(reopened) != 0 {
		t.Errorf("reopened = %v, want nothing: a cancelled plan stays as it is", reopened)
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != planCancelled {
		t.Errorf("plan state after the pass = %q, want cancelled", got)
	}
	if n := e.countEvents(evPlanReopened); n != 0 {
		t.Errorf("plan_reopened events = %d, want none", n)
	}
}
