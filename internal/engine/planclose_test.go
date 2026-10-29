package engine

import (
	"fmt"
	"strings"
	"testing"
)

// The plan-close rule of a landing still to come (#133): a done task
// the land rule would land — with no landing record yet, most often
// because a dependency of its own has not landed, or because it is
// held at the land gate, its landing waiting on the user's release —
// is live work, so the plan does not close done over it. Plan 84
// closed exactly there: its tasks sat done behind a dependency held
// at the land gate, their land jobs never queued, and a landing that
// later failed parked under a plan already finished. Here the plan
// stays open until the landings settle — by the land job, by the
// approval that releases a hold, or by the trailer on the branch —
// and the foreman can still act on a task a failed landing parks. A
// hold on the push of a landing already recorded is not that hold:
// the push phase's is a waiting push (§7.1), and the plan may close
// over it, named under "waits to push".

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

// TestTrailerHoldsDependentAndKeepsPlanOpen: the trailer path of the
// settle. base is held at the land gate and its dependent, heavy too,
// sits done with no landing record and no hold — the plan open on
// #133's rule. base's trailer then shows on the default branch: the
// record settles base and releases the dependent's landing, which the
// heavy tier holds at the land gate — a landing still to come, so the
// close that runs after the release keeps the plan open (#133, part
// two) until the user releases the dependent and it lands.
func TestTrailerHoldsDependentAndKeepsPlanOpen(t *testing.T) {
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
	// hold and releases top's landing, which the tier holds at the
	// gate — a hold the release just made, which keeps the plan open.
	if err := e.eng.RecordLanded(e.ctx, envRepo, "main",
		[]LandedCommit{{TaskID: base, Commit: "h1d1"}}); err != nil {
		t.Fatal(err)
	}
	if n := e.int("SELECT COUNT(*) FROM land_holds WHERE task_id = ? AND state = 'held'", top); n != 1 {
		t.Fatalf("top land holds = %d, want 1: the release held its landing at the gate", n)
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != "active" {
		t.Fatalf("plan state = %q, want active: the dependent's hold is a landing still to come", got)
	}
	if _, ok := e.eventPayloadOf(evPlanDone); ok {
		t.Fatal("the plan closed done over the dependent held at the land gate")
	}

	// The user releases the dependent and its landing settles: the
	// plan closes with both landed.
	if _, err := e.eng.LandApprove(e.ctx, e.project, top, false); err != nil {
		t.Fatalf("release top's hold: %v", err)
	}
	g := e.leaseGit(author)
	if g == nil || g.TaskID != top {
		t.Fatalf("lease = %+v, want top's land job", g)
	}
	if ans := e.complete(g, Completion{Result: `{"target_branch":"main","commit":"t0p0","check_passed":true,"pushed":"pushed"}`}); ans != AnsOK {
		t.Fatalf("land top: %s", ans)
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != planDone {
		t.Fatalf("plan state = %q, want done once the held dependent landed", got)
	}
	p, ok := e.eventPayloadOf(evPlanDone)
	if !ok {
		t.Fatal("no plan_done event")
	}
	if p.Reason != "all tasks landed" {
		t.Errorf("plan_done report = %q, want all tasks landed", p.Reason)
	}
}

// TestLandGateHeldTaskKeepsPlanOpenUntilItLands: a plan whose only
// unsettled task is held at the land gate stays open (#133, part two)
// — the held landing is still to come, waiting on the user's release —
// and closes once the release's landing settles, the task under
// landed. The hold alone used to close the plan under "waits at a
// gate"; it no longer does.
func TestLandGateHeldTaskKeepsPlanOpenUntilItLands(t *testing.T) {
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
	// heavy tier holds it at the land gate, and the plan stays open —
	// the hold is a landing still to come, not a plan to finish.
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
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != "active" {
		t.Fatalf("plan state = %q, want active: a land-gate hold keeps the plan open", got)
	}
	if _, ok := e.eventPayloadOf(evPlanDone); ok {
		t.Fatal("the plan closed done over a task held at the land gate")
	}

	// The user releases the hold and the landing settles: the plan
	// closes done, both tasks under landed.
	if _, err := e.eng.LandApprove(e.ctx, e.project, held, false); err != nil {
		t.Fatalf("release held's hold: %v", err)
	}
	g2 := e.leaseGit(author)
	if g2 == nil || g2.TaskID != held {
		t.Fatalf("lease = %+v, want held's land job", g2)
	}
	if ans := e.complete(g2, Completion{Result: `{"target_branch":"main","commit":"bbb222","check_passed":true,"pushed":"pushed"}`}); ans != AnsOK {
		t.Fatalf("land held: %s", ans)
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != planDone {
		t.Fatalf("plan state = %q, want done once the held task landed", got)
	}
	p, ok := e.eventPayloadOf(evPlanDone)
	if !ok {
		t.Fatal("no plan_done event")
	}
	if p.Reason != "all tasks landed" {
		t.Errorf("plan_done report = %q, want all tasks landed", p.Reason)
	}
}

// TestLandGateHeldTaskClosesOnTheTrailerLanding: the settle of a hold
// by the branch itself. A plan whose only task is held at the land
// gate stays open; its trailer then shows on the default branch — the
// record clears the hold and settles the landing — and the close that
// follows the settle finishes the plan, the task under landed (#133,
// part two: the settle path the plan waited for).
func TestLandGateHeldTaskClosesOnTheTrailerLanding(t *testing.T) {
	e := newEnv(t)
	e.set(map[string]string{SettingApprovalTiers: "heavy"})
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	res := e.createPlan([]PlanTask{
		{Name: "solo", Spec: "the only work", Tier: "heavy"},
	})
	solo := res.Tasks[0].TaskID

	e.runTaskToDone(solo, author, reviewer)
	if n := e.int("SELECT COUNT(*) FROM land_holds WHERE task_id = ? AND state = 'held'", solo); n != 1 {
		t.Fatalf("solo land holds = %d, want 1: the heavy tier holds its landing", n)
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != "active" {
		t.Fatalf("plan state = %q, want active: the held landing is still to come", got)
	}
	if _, ok := e.eventPayloadOf(evPlanDone); ok {
		t.Fatal("the plan closed done over a task held at the land gate")
	}

	// The trailer on the branch is the landing's outcome: the hold
	// clears, the record settles, and the plan closes.
	if err := e.eng.RecordLanded(e.ctx, envRepo, "main",
		[]LandedCommit{{TaskID: solo, Commit: "s0l0"}}); err != nil {
		t.Fatal(err)
	}
	if n := e.int("SELECT COUNT(*) FROM land_holds WHERE task_id = ?", solo); n != 0 {
		t.Errorf("land_holds rows after the trailer landing = %d, want none", n)
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != planDone {
		t.Fatalf("plan state = %q, want done once the held task's trailer landed", got)
	}
	p, ok := e.eventPayloadOf(evPlanDone)
	if !ok {
		t.Fatal("no plan_done event")
	}
	if p.Reason != "all tasks landed" {
		t.Errorf("plan_done report = %q, want all tasks landed", p.Reason)
	}
}

// TestPushGateNeverStillClosesThePlan: a plan whose only open task
// waits at the push gate under gate_push never closes (#133): the
// landing is recorded local with the exact push command — the push is
// the user's by design (§6.3) — and a recorded landing, waiting on
// nothing of the plan's, lets the close, the task under landed.
func TestPushGateNeverStillClosesThePlan(t *testing.T) {
	e := newEnv(t)
	e.set(map[string]string{SettingGatePush: GateNever})
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	res := e.createPlan([]PlanTask{
		{Name: "solo", Spec: "the only work"},
	})
	solo := res.Tasks[0].TaskID
	e.runTaskToDone(solo, author, reviewer)

	cmd := "git fetch origin refs/brigada/land/main && git push origin FETCH_HEAD:refs/heads/main"
	g := e.leaseGit(author)
	if g == nil || g.TaskID != solo {
		t.Fatalf("lease = %+v, want solo's land job", g)
	}
	if ans := e.complete(g, Completion{Result: `{"target_branch":"main","commit":"abc123","check_passed":true,"pushed":"local","push_cmd":"` + cmd + `"}`}); ans != AnsOK {
		t.Fatalf("land solo: %s", ans)
	}
	if got := e.str("SELECT pushed FROM landings WHERE task_id = ?", solo); got != "local" {
		t.Fatalf("landing pushed = %q, want local", got)
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != planDone {
		t.Fatalf("plan state = %q, want done: a gate_push never landing is recorded, the push the user's", got)
	}
	p, ok := e.eventPayloadOf(evPlanDone)
	if !ok {
		t.Fatal("no plan_done event")
	}
	if p.Reason != "all tasks landed" {
		t.Errorf("plan_done report = %q, want all tasks landed", p.Reason)
	}
}

// TestPushPhaseHoldLetsThePlanClose: the two holds apart (#133, part
// two). base's landing records waiting at the push gate and its
// dependent holds at the land gate, so the plan stays open on the
// dependent's hold; the push gate's land_ready hold then sits on
// base's waiting record — the push phase's hold, beside a landing
// already recorded — and when the dependent's release lands it, the
// close succeeds over that hold, base named under "waits to push".
func TestPushPhaseHoldLetsThePlanClose(t *testing.T) {
	e := newEnv(t)
	e.set(map[string]string{SettingApprovalTiers: "heavy", SettingGatePush: GateAsk})
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	res := e.createPlan([]PlanTask{
		{Name: "base", Spec: "the base work", Tier: "heavy"},
		{Name: "top", Spec: "the stacked work", Tier: "heavy", Deps: []string{"base"}},
	})
	base, top := res.Tasks[0].TaskID, res.Tasks[1].TaskID
	plan := res.PlanID
	e.runTaskToDone(base, author, reviewer)
	e.runTaskToDone(top, author, reviewer)

	// The user releases base: its landing records waiting at the push
	// gate, and the release holds top at the land gate — the plan
	// stays open on top's hold, a landing still to come.
	if _, err := e.eng.LandApprove(e.ctx, e.project, base, false); err != nil {
		t.Fatalf("release base's hold: %v", err)
	}
	g := e.leaseGit(author)
	if g == nil || g.TaskID != base {
		t.Fatalf("lease = %+v, want base's land job", g)
	}
	if ans := e.complete(g, Completion{Result: `{"target_branch":"main","commit":"aaa111","check_passed":true,"pushed":"waiting"}`}); ans != AnsOK {
		t.Fatalf("land base: %s", ans)
	}
	if got := e.str("SELECT pushed FROM landings WHERE task_id = ?", base); got != "waiting" {
		t.Fatalf("base landing pushed = %q, want waiting", got)
	}
	if n := e.int("SELECT COUNT(*) FROM land_holds WHERE task_id = ? AND state = 'held'", top); n != 1 {
		t.Fatalf("top land holds = %d, want 1: base's landing held its dependent at the gate", n)
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", plan); got != "active" {
		t.Fatalf("plan state = %q, want active: the dependent's land-gate hold keeps it open", got)
	}

	// The push is released and the land_ready gate holds it: the hold
	// sits beside base's waiting record — the push phase's hold, not a
	// land-gate one — and the plan stays open on top's hold alone.
	if _, err := e.eng.PushApprove(e.ctx, envRepo); err != nil {
		t.Fatalf("approve the push: %v", err)
	}
	pg := e.leaseGit(author)
	if pg == nil || pg.TaskID != base || !pg.Push {
		t.Fatalf("lease = %+v, want base's push job", pg)
	}
	if ans := e.complete(pg, landHeld("the release window is closed")); ans != AnsOK {
		t.Fatalf("hold base's push: %s", ans)
	}
	if reason := e.str("SELECT reason FROM land_holds WHERE task_id = ? AND state = 'held'", base); reason != "held by rule land_ready: the release window is closed" {
		t.Fatalf("base land hold reason = %q, want the land_ready rule beside the waiting record", reason)
	}
	if got := e.str("SELECT pushed FROM landings WHERE task_id = ?", base); got != "waiting" {
		t.Fatalf("base landing pushed = %q, want waiting under the push-phase hold", got)
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", plan); got != "active" {
		t.Fatalf("plan state = %q, want active: only the dependent's land-gate hold keeps it open", got)
	}

	// The dependent's release lands it: the close runs over base's
	// push-phase hold — a waiting push — and the plan finishes, base
	// named under "waits to push", top under landed.
	if _, err := e.eng.LandApprove(e.ctx, e.project, top, false); err != nil {
		t.Fatalf("release top's hold: %v", err)
	}
	g2 := e.leaseGit(author)
	if g2 == nil || g2.TaskID != top {
		t.Fatalf("lease = %+v, want top's land job", g2)
	}
	if ans := e.complete(g2, Completion{Result: `{"target_branch":"main","commit":"t0p0","check_passed":true,"pushed":"pushed"}`}); ans != AnsOK {
		t.Fatalf("land top: %s", ans)
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", plan); got != planDone {
		t.Fatalf("plan state = %q, want done: a push-phase hold is a waiting push and lets the plan close", got)
	}
	p, ok := e.eventPayloadOf(evPlanDone)
	if !ok {
		t.Fatal("no plan_done event")
	}
	if !strings.Contains(p.Reason, fmt.Sprintf("landed: task %d top (main@t0p0)", top)) ||
		!strings.Contains(p.Reason, fmt.Sprintf("waits at a gate: task %d base (waits to push)", base)) {
		t.Errorf("plan_done report = %q, want top landed and base named as waiting to push", p.Reason)
	}
}
