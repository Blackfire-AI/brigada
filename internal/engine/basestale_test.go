package engine

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// The stale-base tests (issue #124): a dependency reopened after its
// dependents started must never leave them landed on its old round.

// taskEvent reads the newest event of kind for one task, when there is
// one.
func (e *env) taskEvent(task int64, kind string) (eventPayload, bool) {
	e.t.Helper()
	var payload string
	err := e.st.View(e.ctx, func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT payload FROM events
			WHERE task_id = ? AND kind = ? ORDER BY id DESC LIMIT 1`, task, kind).Scan(&payload)
	})
	if err != nil {
		return eventPayload{}, false
	}
	var p eventPayload
	if json.Unmarshal([]byte(payload), &p) != nil {
		return eventPayload{}, false
	}
	return p, true
}

// approveReview runs the queued review of whatever task the review is
// for, with a substantive approve.
func (e *env) approveReview(reviewer int64) {
	e.t.Helper()
	e.runReview(reviewer, `{"verdict":"approve","findings":"`+substantiveFindings+`"}`)
}

// TestReopenedDependencyHoldsStartedDependents reproduces plan 81
// (issue #124): the dependent started on the dependency's round-0
// branch; the dependency's landing failed, it was restarted and landed
// from -r1; the dependent must be held on its stale base — parked, one
// wake naming the dependency and it — and never land from round 0, and
// a restart of it rebuilds on -r1.
func TestReopenedDependencyHoldsStartedDependents(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	res, err := e.eng.CreatePlan(e.ctx, e.project, "the stack", []PlanTask{
		{Name: "lib", Title: "the lib", Spec: "write the lib"},
		{Name: "mid", Title: "the mid", Spec: "use the lib", Deps: []string{"lib"}},
		{Name: "top", Title: "the top", Spec: "use the mid", Deps: []string{"mid"}},
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	lib, mid, top := res.Tasks[0].TaskID, res.Tasks[1].TaskID, res.Tasks[2].TaskID

	// The dependency done from round 0: mid is released stacked on its
	// round-0 branch and runs to done on it; top, stacked on mid, only
	// starts (its work job stays leased, running).
	g := e.leaseAny(author)
	if g == nil || g.TaskID != lib {
		t.Fatalf("lease = %+v, want lib's work", g)
	}
	if ans := e.complete(g, doneOn(branchOf(lib), passCheck)); ans != AnsOK {
		t.Fatalf("complete lib: %s", ans)
	}
	e.approveReview(reviewer)
	if got := e.str("SELECT base_ref FROM tasks WHERE id = ?", mid); got != branchOf(lib) {
		t.Fatalf("mid base after lib's done = %q, want %q", got, branchOf(lib))
	}
	g = e.leaseAny(author)
	if g == nil || g.TaskID != mid {
		t.Fatalf("lease = %+v, want mid's work", g)
	}
	if ans := e.complete(g, doneOn(branchOf(mid), passCheck)); ans != AnsOK {
		t.Fatalf("complete mid: %s", ans)
	}
	e.approveReview(reviewer)
	g = e.leaseAny(author)
	if g == nil || g.TaskID != top {
		t.Fatalf("lease = %+v, want top's work", g)
	}

	// The dependency is reopened: its landing conflicts, it parks, and
	// the foreman restarts it onto -r1.
	lg := e.leaseGit(author)
	if lg == nil || lg.TaskID != lib || lg.Kind != KindLand {
		t.Fatalf("lease = %+v, want lib's land job", lg)
	}
	if ans := e.complete(lg, Completion{Result: `{"target_branch":"main","commit":"","check_passed":false,"conflict_files":["a.go"]}`}); ans != AnsOK {
		t.Fatalf("fail lib's landing: %s", ans)
	}
	if _, err := e.eng.RestartTask(e.ctx, e.project, lib, "rebase onto the new head"); err != nil {
		t.Fatalf("restart lib: %v", err)
	}
	r1 := fmt.Sprintf("brigada/task-%d-r1", lib)
	g = e.leaseAny(author)
	if g == nil || g.TaskID != lib || g.Branch != r1 {
		t.Fatalf("lease = %+v, want lib's restarted work on %s", g, r1)
	}
	if ans := e.complete(g, doneOn(r1, passCheck)); ans != AnsOK {
		t.Fatalf("complete the restarted lib: %s", ans)
	}
	e.approveReview(reviewer)

	// mid is held on the stale round-0 base: parked, both refs named, no
	// landing; top, stacked on mid, is not mid's dependency's to hold.
	if state := e.str("SELECT state FROM tasks WHERE id = ?", mid); state != taskNeedsForeman {
		t.Fatalf("mid state after lib's rebuild = %q, want needs_foreman", state)
	}
	p, ok := e.taskEvent(mid, evTaskBaseStale)
	if !ok || !strings.Contains(p.Reason, branchOf(lib)) || !strings.Contains(p.Reason, r1) {
		t.Errorf("mid's task_base_stale = %+v (%t), want both refs %q and %q", p, ok, branchOf(lib), r1)
	}
	if p.Branch != r1 {
		t.Errorf("mid's task_base_stale branch = %q, want the new %q", p.Branch, r1)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'land'", mid); n != 0 {
		t.Errorf("mid land jobs = %d, want none from the stale base", n)
	}
	if n := e.int("SELECT COUNT(*) FROM landings WHERE task_id = ?", mid); n != 0 {
		t.Errorf("mid landings = %d, want none", n)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", top); state != taskRunning {
		t.Errorf("top state = %q, want running: its base is mid's, not lib's", state)
	}
	if _, ok := e.taskEvent(top, evTaskBaseStale); ok {
		t.Error("top was held, stacked on mid whose branch never moved")
	}
	// One wake names the dependency and every stale task: the event sits
	// in the foreman's inbox for its next turn (the turn the dependency's
	// land failure already woke stays live, so no second turn is queued).
	p, ok = e.taskEvent(lib, evDepRebuilt)
	if !ok || !strings.Contains(p.Reason, fmt.Sprintf("task %d", mid)) {
		t.Errorf("dep_rebuilt = %+v (%t), want the wake naming mid", p, ok)
	}
	if n := e.int(`SELECT COUNT(*) FROM foreman_inbox i JOIN events ev ON ev.id = i.event_id
		WHERE ev.kind = ? AND ev.task_id = ?`, evDepRebuilt, lib); n != 1 {
		t.Errorf("dep_rebuilt inbox rows = %d, want the one wake", n)
	}
	if it, ok := e.needsYouTask(mid); !ok || !strings.Contains(it.Reason, "superseded") {
		t.Errorf("needs-you entry = %+v (%t), want the stale base named", it, ok)
	}

	// The foreman's rebuild spends a restart and starts fresh on the
	// dependency's new result: the restart's grant carries -r1 as its
	// base.
	if _, err := e.eng.RestartTask(e.ctx, e.project, mid, "rebuild on the landed rework"); err != nil {
		t.Fatalf("restart mid: %v", err)
	}
	g = e.leaseAny(author)
	if g == nil || g.TaskID != mid {
		t.Fatalf("lease = %+v, want mid's restarted work", g)
	}
	if g.BaseRef != r1 {
		t.Errorf("mid's restarted base = %q, want the new %q", g.BaseRef, r1)
	}
	if got := e.str("SELECT base_ref FROM tasks WHERE id = ?", mid); got != r1 {
		t.Errorf("mid base_ref after the restart = %q, want %q", got, r1)
	}
}

// TestRestartBeforeDependentsStartedChangesNothing: a dependency
// restarted before any dependent started behaves as before (§6.1) — no
// wake, no hold, and the released dependent's base is the restart's
// branch.
func TestRestartBeforeDependentsStartedChangesNothing(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	res, err := e.eng.CreatePlan(e.ctx, e.project, "fresh dependents", []PlanTask{
		{Name: "lib", Title: "the lib", Spec: "write the lib"},
		{Name: "app", Title: "the app", Spec: "use the lib", Deps: []string{"lib"}},
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	lib, app := res.Tasks[0].TaskID, res.Tasks[1].TaskID

	g := e.leaseAny(author)
	if g == nil || g.TaskID != lib {
		t.Fatalf("lease = %+v, want lib's work", g)
	}
	if ans := e.complete(g, Completion{Result: `{"status":"needs_help","question":"which way?"}`}); ans != AnsOK {
		t.Fatalf("park lib: %s", ans)
	}
	if _, err := e.eng.RestartTask(e.ctx, e.project, lib, "start over"); err != nil {
		t.Fatalf("restart lib: %v", err)
	}
	r1 := fmt.Sprintf("brigada/task-%d-r1", lib)
	g = e.leaseAny(author)
	if g == nil || g.Branch != r1 {
		t.Fatalf("lease = %+v, want lib's restarted work on %s", g, r1)
	}
	if ans := e.complete(g, doneOn(r1, passCheck)); ans != AnsOK {
		t.Fatalf("complete the restarted lib: %s", ans)
	}
	e.approveReview(reviewer)

	if state := e.str("SELECT state FROM tasks WHERE id = ?", app); state != taskReady {
		t.Fatalf("app state = %q, want ready: a pending dependent releases as always", state)
	}
	if got := e.str("SELECT base_ref FROM tasks WHERE id = ?", app); got != r1 {
		t.Errorf("app base = %q, want the restart's %q", got, r1)
	}
	g = e.leaseAny(author)
	if g == nil || g.TaskID != app || g.BaseRef != r1 {
		t.Fatalf("app grant = %+v, want base %q", g, r1)
	}
	if _, ok := e.taskEvent(lib, evDepRebuilt); ok {
		t.Error("a restart with no started dependent woke the foreman")
	}
	for _, id := range []int64{lib, app} {
		if _, ok := e.taskEvent(id, evTaskBaseStale); ok {
			t.Errorf("task %d was held on a stale base it never had", id)
		}
	}
}

// TestLandedDependentUntouchedByReopen: a dependent that already landed
// before the dependency was reopened keeps its landing and its state —
// the stale-base rule holds only what has not landed.
func TestLandedDependentUntouchedByReopen(t *testing.T) {
	e := newEnv(t)
	e.set(map[string]string{SettingGatePush: GateAsk})
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	res, err := e.eng.CreatePlan(e.ctx, e.project, "landed chain", []PlanTask{
		{Name: "lib", Title: "the lib", Spec: "write the lib"},
		{Name: "mid", Title: "the mid", Spec: "use the lib", Deps: []string{"lib"}},
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	lib, mid := res.Tasks[0].TaskID, res.Tasks[1].TaskID

	// The dependency lands waiting at the push gate; the dependent runs
	// and lands waiting on top of it.
	g := e.leaseAny(author)
	if g == nil || g.TaskID != lib {
		t.Fatalf("lease = %+v, want lib's work", g)
	}
	if ans := e.complete(g, doneOn(branchOf(lib), passCheck)); ans != AnsOK {
		t.Fatalf("complete lib: %s", ans)
	}
	e.approveReview(reviewer)
	g = e.leaseAny(author) // mid's work, queued at lib's done
	if g == nil || g.TaskID != mid {
		t.Fatalf("lease = %+v, want mid's work", g)
	}
	lg := e.leaseGit(author)
	if lg == nil || lg.TaskID != lib || lg.Kind != KindLand {
		t.Fatalf("lease = %+v, want lib's land job", lg)
	}
	if ans := e.complete(lg, Completion{Result: `{"target_branch":"main","commit":"lib-1","check_passed":true,"pushed":"waiting"}`}); ans != AnsOK {
		t.Fatalf("land lib waiting: %s", ans)
	}
	if ans := e.complete(g, doneOn(branchOf(mid), passCheck)); ans != AnsOK {
		t.Fatalf("complete mid: %s", ans)
	}
	e.approveReview(reviewer)
	lg = e.leaseGit(author)
	if lg == nil || lg.TaskID != mid || lg.Kind != KindLand {
		t.Fatalf("lease = %+v, want mid's land job", lg)
	}
	if ans := e.complete(lg, Completion{Result: `{"target_branch":"main","commit":"mid-1","check_passed":true,"pushed":"waiting"}`}); ans != AnsOK {
		t.Fatalf("land mid waiting: %s", ans)
	}

	// The dependency is reopened: its push is rejected, it parks, and
	// the foreman restarts it onto -r1.
	if _, err := e.eng.PushApprove(e.ctx, "https://example.invalid/brigada.git"); err != nil {
		t.Fatalf("approve the push: %v", err)
	}
	lg = e.leaseGit(author)
	if lg == nil || lg.TaskID != lib || lg.Kind != KindLand {
		t.Fatalf("lease = %+v, want lib's push job", lg)
	}
	if ans := e.complete(lg, Completion{Error: "push rejected: non-fast-forward"}); ans != AnsOK {
		t.Fatalf("fail lib's push: %s", ans)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", lib); state != taskNeedsForeman {
		t.Fatalf("lib state after the rejected push = %q, want needs_foreman", state)
	}
	if _, err := e.eng.RestartTask(e.ctx, e.project, lib, "rebuild on the target"); err != nil {
		t.Fatalf("restart lib: %v", err)
	}
	r1 := fmt.Sprintf("brigada/task-%d-r1", lib)
	g = e.leaseAny(author)
	if g == nil || g.TaskID != lib || g.Branch != r1 {
		t.Fatalf("lease = %+v, want lib's restarted work on %s", g, r1)
	}
	if ans := e.complete(g, doneOn(r1, passCheck)); ans != AnsOK {
		t.Fatalf("complete the restarted lib: %s", ans)
	}
	e.approveReview(reviewer)

	// The landed dependent is untouched: still done, its waiting landing
	// kept, nothing held and nobody woken for stale bases.
	if state := e.str("SELECT state FROM tasks WHERE id = ?", mid); state != taskDone {
		t.Errorf("mid state = %q, want done: a landed dependent is not held", state)
	}
	if pushed := e.str("SELECT pushed FROM landings WHERE task_id = ?", mid); pushed != "waiting" {
		t.Errorf("mid landing = %q, want its waiting record kept", pushed)
	}
	if _, ok := e.taskEvent(mid, evTaskBaseStale); ok {
		t.Error("a landed dependent was held on a stale base")
	}
	if _, ok := e.taskEvent(lib, evDepRebuilt); ok {
		t.Error("a rebuild with nothing stale to hold woke the foreman")
	}
}

// TestLandRefusedOnStaleBase: the land path never queues a job for a
// task whose recorded base is a superseded round of its dependency —
// the task parks with an event naming both refs, whatever released the
// landing (issue #124's land refusal).
func TestLandRefusedOnStaleBase(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	res, err := e.eng.CreatePlan(e.ctx, e.project, "refused landing", []PlanTask{
		{Name: "lib", Title: "the lib", Spec: "write the lib"},
		{Name: "mid", Title: "the mid", Spec: "use the lib", Deps: []string{"lib"}},
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	lib, mid := res.Tasks[0].TaskID, res.Tasks[1].TaskID

	// The dependency done from round 0 releases mid, whose work stays
	// leased — started, so the reopen reaches it, but unreachable for a
	// park while it runs.
	g := e.leaseAny(author)
	if g == nil || g.TaskID != lib {
		t.Fatalf("lease = %+v, want lib's work", g)
	}
	if ans := e.complete(g, doneOn(branchOf(lib), passCheck)); ans != AnsOK {
		t.Fatalf("complete lib: %s", ans)
	}
	e.approveReview(reviewer)
	midWork := e.leaseAny(author)
	if midWork == nil || midWork.TaskID != mid {
		t.Fatalf("lease = %+v, want mid's work", midWork)
	}

	// The dependency is reopened and lands from -r1 while mid runs: mid
	// is named in the one wake and held from landing, not parked under
	// its live job.
	lg := e.leaseGit(author)
	if lg == nil || lg.TaskID != lib || lg.Kind != KindLand {
		t.Fatalf("lease = %+v, want lib's land job", lg)
	}
	if ans := e.complete(lg, Completion{Result: `{"target_branch":"main","commit":"","check_passed":false,"conflict_files":["a.go"]}`}); ans != AnsOK {
		t.Fatalf("fail lib's landing: %s", ans)
	}
	if _, err := e.eng.RestartTask(e.ctx, e.project, lib, "rebase onto the new head"); err != nil {
		t.Fatalf("restart lib: %v", err)
	}
	r1 := fmt.Sprintf("brigada/task-%d-r1", lib)
	g = e.leaseAny(author)
	if g == nil || g.TaskID != lib || g.Branch != r1 {
		t.Fatalf("lease = %+v, want lib's restarted work on %s", g, r1)
	}
	if ans := e.complete(g, doneOn(r1, passCheck)); ans != AnsOK {
		t.Fatalf("complete the restarted lib: %s", ans)
	}
	e.approveReview(reviewer)
	if state := e.str("SELECT state FROM tasks WHERE id = ?", mid); state != taskRunning {
		t.Fatalf("mid state = %q, want running under its live job", state)
	}
	if p, ok := e.taskEvent(lib, evDepRebuilt); !ok || !strings.Contains(p.Reason, "held from landing") {
		t.Errorf("dep_rebuilt = %+v (%t), want mid named as held from landing", p, ok)
	}

	// mid finishes on the stale base, its dependency lands from -r1, and
	// the landing the release would queue is refused: parked, both refs
	// named, no land job, no record.
	if ans := e.complete(midWork, doneOn(branchOf(mid), passCheck)); ans != AnsOK {
		t.Fatalf("complete mid: %s", ans)
	}
	e.approveReview(reviewer)
	lg = e.leaseGit(author)
	if lg == nil || lg.TaskID != lib || lg.Kind != KindLand {
		t.Fatalf("lease = %+v, want lib's second land job", lg)
	}
	if ans := e.complete(lg, Completion{Result: `{"target_branch":"main","commit":"lib-2","check_passed":true,"pushed":"pushed"}`}); ans != AnsOK {
		t.Fatalf("land lib: %s", ans)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", mid); state != taskNeedsForeman {
		t.Fatalf("mid state after the refused landing = %q, want needs_foreman", state)
	}
	p, ok := e.taskEvent(mid, evTaskBaseStale)
	if !ok || !strings.Contains(p.Reason, "the landing is refused") ||
		!strings.Contains(p.Reason, branchOf(lib)) || !strings.Contains(p.Reason, r1) {
		t.Errorf("mid's task_base_stale = %+v (%t), want the refusal naming both refs", p, ok)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'land'", mid); n != 0 {
		t.Errorf("mid land jobs = %d, want none: a stale base never lands", n)
	}
	if n := e.int("SELECT COUNT(*) FROM landings WHERE task_id = ?", mid); n != 0 {
		t.Errorf("mid landings = %d, want none", n)
	}
}
