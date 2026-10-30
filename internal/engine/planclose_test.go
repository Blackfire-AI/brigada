package engine

import (
	"fmt"
	"strings"
	"testing"
)

// The plan-close rule of a landing still to come (#133): a done task
// the land rule would land — with no landing record yet and no hold at
// the land gate, most often because a dependency of its own has not
// landed — is live work, so the plan does not close done over it. Plan
// 84 closed exactly there: its tasks sat done behind a dependency held
// at the land gate, their land jobs never queued, and a landing that
// later failed parked under a plan already finished. Here the plan
// stays open until the landings settle, and the foreman can still act
// on a task a failed landing parks.

// seedHeldChain builds the plan-84 shape (#133): base, whose heavy tier
// holds its landing at the land gate, and top, stacked on it — both
// done and approved. base's hold leaves it done with no landing record,
// so top's own landing never queues: done, landable, with neither a
// record nor a hold, the state that used to close the plan.
func seedHeldChain(t *testing.T) (e *env, author, plan, base, top int64) {
	t.Helper()
	e = newEnv(t)
	e.set(map[string]string{SettingApprovalTiers: "heavy"})
	author = e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	res := e.createPlan([]PlanTask{
		{Name: "base", Spec: "the base work", Tier: "heavy"},
		{Name: "top", Spec: "the stacked work", Deps: []string{"base"}},
	})
	base, top = res.Tasks[0].TaskID, res.Tasks[1].TaskID
	plan = res.PlanID
	e.runTaskToDone(base, author, reviewer)
	if n := e.int("SELECT COUNT(*) FROM land_holds WHERE task_id = ? AND state = 'held'", base); n != 1 {
		t.Fatalf("base land holds = %d, want 1: the heavy tier holds its landing", n)
	}
	e.runTaskToDone(top, author, reviewer)
	return e, author, plan, base, top
}

// TestUnlandedDependencyKeepsPlanOpen: the plan-84 sequence. A
// dependency held at the land gate, its dependent done with no landing
// record and no hold, does not let the plan close: no plan_done is
// written while the dependent's landing is still to come. Once the
// hold is released and both landings settle, the plan closes with the
// usual done report.
func TestUnlandedDependencyKeepsPlanOpen(t *testing.T) {
	e, author, plan, base, top := seedHeldChain(t)

	// The close is refused: top is done, landable, with neither a
	// landing record nor a hold — its landing is live work still.
	if got := e.str("SELECT state FROM plans WHERE id = ?", plan); got != "active" {
		t.Fatalf("plan state = %q, want active: a done, unlanded dependent keeps it open", got)
	}
	if _, ok := e.eventPayloadOf(evPlanDone); ok {
		t.Error("the plan closed done over a landing still to come")
	}

	// The user releases the hold: base's landing queues and lands, which
	// releases top's landing — the plan stays open while it is live.
	if _, err := e.eng.LandApprove(e.ctx, e.project, base, false); err != nil {
		t.Fatalf("release base's hold: %v", err)
	}
	g := e.leaseGit(author)
	if g == nil || g.TaskID != base {
		t.Fatalf("lease = %+v, want base's land job", g)
	}
	if ans := e.complete(g, Completion{Result: `{"target_branch":"main","commit":"aaa111","check_passed":true,"pushed":"pushed"}`}); ans != AnsOK {
		t.Fatalf("land base: %s", ans)
	}
	if got := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'land' AND status IN ('queued', 'leased', 'running')", top); got != 1 {
		t.Fatalf("top's live land jobs = %d, want 1: base's landing releases it", got)
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", plan); got != "active" {
		t.Fatalf("plan state after base landed = %q, want active while top's landing is live", got)
	}

	// Top's landing settles: the plan closes and its report names both.
	g2 := e.leaseGit(author)
	if g2 == nil || g2.TaskID != top {
		t.Fatalf("lease = %+v, want top's land job", g2)
	}
	if ans := e.complete(g2, Completion{Result: `{"target_branch":"main","commit":"bbb222","check_passed":true,"pushed":"pushed"}`}); ans != AnsOK {
		t.Fatalf("land top: %s", ans)
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", plan); got != planDone {
		t.Fatalf("plan state = %q, want done once both landings settled", got)
	}
	p, ok := e.eventPayloadOf(evPlanDone)
	if !ok {
		t.Fatal("no plan_done event after both landings settled")
	}
	if !strings.Contains(p.Reason, "all tasks landed") {
		t.Errorf("plan_done report = %q, want all tasks landed", p.Reason)
	}
}

// TestLandFailureParksUnderAnOpenPlan: with the plan held open by the
// dependent's unsettled landing, a landing that fails parks the task at
// needs_foreman under a plan that is still active — not under one
// already closed done, as plan 84 did — so the foreman's own tools
// still work: task_cancel closes the plan, its report naming the
// cancellation and what landed.
func TestLandFailureParksUnderAnOpenPlan(t *testing.T) {
	e, author, plan, base, top := seedHeldChain(t)
	if _, err := e.eng.LandApprove(e.ctx, e.project, base, false); err != nil {
		t.Fatalf("release base's hold: %v", err)
	}
	g := e.leaseGit(author)
	if g == nil || g.TaskID != base {
		t.Fatalf("lease = %+v, want base's land job", g)
	}
	if ans := e.complete(g, Completion{Result: `{"target_branch":"main","commit":"aaa111","check_passed":true,"pushed":"pushed"}`}); ans != AnsOK {
		t.Fatalf("land base: %s", ans)
	}
	g2 := e.leaseGit(author)
	if g2 == nil || g2.TaskID != top {
		t.Fatalf("lease = %+v, want top's land job", g2)
	}
	// The landing conflicts: nothing is pushed, top parks at
	// needs_foreman, and the plan stays open under it.
	if ans := e.complete(g2, Completion{Result: `{"target_branch":"main","commit":"x","check_passed":false,"pushed":"","conflict_files":["docs/design.md"]}`}); ans != AnsOK {
		t.Fatalf("fail top's landing: %s", ans)
	}
	if got := e.taskState(top); got != taskNeedsForeman {
		t.Fatalf("top state = %q, want needs_foreman after the land conflict", got)
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", plan); got != "active" {
		t.Fatalf("plan state = %q, want active under a parked task", got)
	}

	// The foreman cancels the parked task in its own plan, without the
	// user: the plan closes, its report naming what landed and the
	// cancellation.
	am := e.amend(plan, PlanAmendment{Cancel: []string{"top"}})
	if len(am.Cancelled) != 1 || am.Cancelled[0] != top {
		t.Fatalf("amendment = %+v, want top cancelled", am)
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", plan); got != planDone {
		t.Fatalf("plan state = %q, want done after the cancel", got)
	}
	p, ok := e.eventPayloadOf(evPlanDone)
	if !ok {
		t.Fatal("no plan_done event after the cancel")
	}
	if !strings.Contains(p.Reason, fmt.Sprintf("landed: task %d base (main@aaa111)", base)) ||
		!strings.Contains(p.Reason, fmt.Sprintf("task %d top (cancelled without the user's approval)", top)) {
		t.Errorf("plan_done report = %q, want base's landing and top's cancellation named", p.Reason)
	}
}

// TestTrailerSettlesBaseAndHoldsDependentAndCloses: the trailer path of
// the settle. base is held at the land gate and its dependent, heavy
// too, sits done with no landing record and no hold — the plan open on
// #133's rule. base's trailer then shows on the default branch: the
// record settles base and releases the dependent's landing, which the
// heavy tier holds at the land gate — a hold with no job — and the
// close runs after the release, the plan finishing with base landed
// and the dependent named under "waits at a gate".
func TestTrailerSettlesBaseAndHoldsDependentAndCloses(t *testing.T) {
	e := newEnv(t)
	e.set(map[string]string{SettingApprovalTiers: "heavy"})
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	res := e.createPlan([]PlanTask{
		{Name: "base", Spec: "the base work", Tier: "heavy"},
		{Name: "top", Spec: "the stacked work", Tier: "heavy", Deps: []string{"base"}},
	})
	base, top := res.Tasks[0].TaskID, res.Tasks[1].TaskID
	e.runTaskToDone(base, author, reviewer)
	e.runTaskToDone(top, author, reviewer)
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != "active" {
		t.Fatalf("plan state = %q, want active: top's landing is still to come", got)
	}
	if _, ok := e.eventPayloadOf(evPlanDone); ok {
		t.Fatal("the plan closed done over a landing still to come")
	}

	// The detection reports base's trailer: the record clears base's
	// hold and releases top's landing, which the tier holds at the gate.
	if err := e.eng.RecordLanded(e.ctx, envRepo, "main",
		[]LandedCommit{{TaskID: base, Commit: "h1d1"}}); err != nil {
		t.Fatal(err)
	}
	if n := e.int("SELECT COUNT(*) FROM land_holds WHERE task_id = ? AND state = 'held'", top); n != 1 {
		t.Fatalf("top land holds = %d, want 1: the release held its landing at the gate", n)
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != planDone {
		t.Fatalf("plan state = %q, want done: only a held task remains, and a hold lets the plan close", got)
	}
	p, ok := e.eventPayloadOf(evPlanDone)
	if !ok {
		t.Fatal("no plan_done event")
	}
	if !strings.Contains(p.Reason, fmt.Sprintf("landed: task %d base (main@h1d1)", base)) ||
		!strings.Contains(p.Reason, fmt.Sprintf("waits at a gate: task %d top (", top)) {
		t.Errorf("plan_done report = %q, want base landed and top named at the gate", p.Reason)
	}
}

// TestLandGateHeldTaskAloneStillCloses: a plan whose only unsettled
// task is held at the land gate closes as before, the task named under
// "waits at a gate" — the hold is the user's to release, with no job
// and nothing of the plan left to wait for.
func TestLandGateHeldTaskAloneStillCloses(t *testing.T) {
	e := newEnv(t)
	e.set(map[string]string{SettingApprovalTiers: "heavy"})
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	res := e.createPlan([]PlanTask{
		{Name: "free", Spec: "lands on its own"},
		{Name: "held", Spec: "waits at the land gate", Tier: "heavy", Deps: []string{"free"}},
	})
	free, held := res.Tasks[0].TaskID, res.Tasks[1].TaskID

	// The free task finishes; its dependent runs right after it — done
	// and approved while its dependency has not landed: no land job, no
	// hold, and the plan stays open over both (#133), free's live land
	// job and held's landing still to come.
	e.runTaskToDone(free, author, reviewer)
	e.runTaskToDone(held, author, reviewer)
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != "active" {
		t.Fatalf("plan state = %q, want active while the landings are unsettled", got)
	}

	// The free task lands pushed, which releases held's landing: the
	// heavy tier holds it at the land gate, and the plan closes naming
	// the landing and the hold.
	g := e.leaseGit(author)
	if g == nil || g.TaskID != free {
		t.Fatalf("lease = %+v, want free's land job", g)
	}
	if ans := e.complete(g, Completion{Result: `{"target_branch":"main","commit":"aaa111","check_passed":true,"pushed":"pushed"}`}); ans != AnsOK {
		t.Fatalf("land free: %s", ans)
	}
	if n := e.int("SELECT COUNT(*) FROM land_holds WHERE task_id = ? AND state = 'held'", held); n != 1 {
		t.Fatalf("held land holds = %d, want 1", n)
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != planDone {
		t.Fatalf("plan state = %q, want done: a land-gate hold alone closes the plan", got)
	}
	p, ok := e.eventPayloadOf(evPlanDone)
	if !ok {
		t.Fatal("no plan_done event")
	}
	if !strings.Contains(p.Reason, fmt.Sprintf("landed: task %d free (main@aaa111)", free)) ||
		!strings.Contains(p.Reason, fmt.Sprintf("waits at a gate: task %d held (", held)) {
		t.Errorf("plan_done report = %q, want free landed and held named at the gate", p.Reason)
	}
}
