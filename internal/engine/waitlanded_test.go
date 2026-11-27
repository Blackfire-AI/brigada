package engine

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

// waitLandedPlan builds a plan with one implement dep and one
// wait_landed dependent, returning their ids. The dep is ready (first
// wave) and the dependent pending behind it.
func waitLandedPlan(e *env) (dep, verify int64) {
	e.t.Helper()
	res := e.createPlan([]PlanTask{
		{Name: "dep", Spec: "the dep"},
		{Name: "verify", Spec: "the verify", Deps: []string{"dep"}, WaitLanded: true},
	})
	return res.Tasks[0].TaskID, res.Tasks[1].TaskID
}

// landTask leases task's queued land job and completes it with result,
// the land completion that settles (or does not settle) the barrier.
func (e *env) landTask(task, owner int64, result string) {
	e.t.Helper()
	g := e.leaseGit(owner)
	if g == nil || g.Kind != KindLand || g.TaskID != task {
		e.t.Fatalf("task %d: no land job to lease: %+v", task, g)
	}
	if ans := e.complete(g, Completion{Result: result}); ans != AnsOK {
		e.t.Fatalf("task %d: complete the land job: %s", task, ans)
	}
}

// TestWaitLandedDoneUnlandedHolds: a wait_landed dependent stays pending
// while its implement dep is done but unlanded, where a plain dependent
// releases on the dep's mere completion.
func TestWaitLandedDoneUnlandedHolds(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	res := e.createPlan([]PlanTask{
		{Name: "dep", Spec: "the dep"},
		{Name: "verify", Spec: "verify", Deps: []string{"dep"}, WaitLanded: true},
		{Name: "plain", Spec: "plain", Deps: []string{"dep"}},
	})
	dep, verify, plain := res.Tasks[0].TaskID, res.Tasks[1].TaskID, res.Tasks[2].TaskID

	e.runTaskToDone(dep, author, reviewer)

	if got := e.str("SELECT state FROM tasks WHERE id = ?", plain); got != taskReady {
		t.Fatalf("plain state = %q, want ready on the dep's done", got)
	}
	if got := e.str("SELECT state FROM tasks WHERE id = ?", verify); got != taskPending {
		t.Fatalf("verify state = %q, want pending until the dep lands", got)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'work'", verify); n != 0 {
		t.Fatalf("verify work jobs = %d, want none", n)
	}
}

// TestWaitLandedWaitingOrLocalLandingHolds: a landing that waits at the
// push gate or sits local is not on the target, so it does not release
// the barrier.
func TestWaitLandedWaitingOrLocalLandingHolds(t *testing.T) {
	for _, pushed := range []string{"waiting", "local"} {
		t.Run(pushed, func(t *testing.T) {
			e := newEnv(t)
			author := e.agented("author", "anthropic", "implement")
			reviewer := e.agented("reviewer", "google", "implement", "review")
			dep, verify := waitLandedPlan(e)
			e.runTaskToDone(dep, author, reviewer)

			result := `{"target_branch":"main","commit":"abc123","check_passed":true,"pushed":"` + pushed + `"}`
			if pushed == "local" {
				result = `{"target_branch":"main","commit":"abc123","check_passed":true,"pushed":"local","push_cmd":"git push origin HEAD:main"}`
			}
			e.landTask(dep, author, result)

			if got := e.str("SELECT state FROM tasks WHERE id = ?", verify); got != taskPending {
				t.Fatalf("verify state = %q, want pending while the landing is %s", got, pushed)
			}
			if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'work'", verify); n != 0 {
				t.Fatalf("verify work jobs = %d, want none", n)
			}
		})
	}
}

// TestWaitLandedInFlightPushHolds: a push-approved land job still in
// flight does not release the barrier — the landing record is still
// waiting, and only work actually on the target counts.
func TestWaitLandedInFlightPushHolds(t *testing.T) {
	e := newEnv(t)
	e.set(map[string]string{SettingGatePush: GateAsk})
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	dep, verify := waitLandedPlan(e)
	e.runTaskToDone(dep, author, reviewer)
	e.landTask(dep, author, `{"target_branch":"main","commit":"abc123","check_passed":true,"pushed":"waiting"}`)

	if _, err := e.eng.PushApprove(e.ctx, envRepo); err != nil {
		t.Fatal(err)
	}
	if got := e.str("SELECT pushed FROM landings WHERE task_id = ?", dep); got != "waiting" {
		t.Fatalf("dep pushed = %q, want waiting while the push is in flight", got)
	}
	if got := e.str("SELECT state FROM tasks WHERE id = ?", verify); got != taskPending {
		t.Fatalf("verify state = %q, want pending: an in-flight push does not count as landed", got)
	}
}

// TestWaitLandedPushedLandingReleases: a landing pushed to the target
// releases the barrier with no foreman action.
func TestWaitLandedPushedLandingReleases(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	dep, verify := waitLandedPlan(e)
	e.runTaskToDone(dep, author, reviewer)

	e.landTask(dep, author, `{"target_branch":"main","commit":"abc123","check_passed":true,"pushed":"pushed"}`)

	if got := e.str("SELECT state FROM tasks WHERE id = ?", verify); got != taskReady {
		t.Fatalf("verify state = %q, want ready once the dep's landing is pushed", got)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'work'", verify); n != 1 {
		t.Fatalf("verify work jobs = %d, want 1", n)
	}
}

// TestWaitLandedTrailerRecordedReleases: a landing recorded by the
// trailer scan is on the target, so it releases the barrier.
func TestWaitLandedTrailerRecordedReleases(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	dep, verify := waitLandedPlan(e)
	e.runTaskToDone(dep, author, reviewer)
	e.trailerOK(dep)

	if err := e.eng.RecordLanded(e.ctx, envRepo, "main",
		[]LandedCommit{e.trailerFor(dep, "abc123")}); err != nil {
		t.Fatal(err)
	}

	if got := e.str("SELECT state FROM tasks WHERE id = ?", verify); got != taskReady {
		t.Fatalf("verify state = %q, want ready once the trailer records the landing", got)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'work'", verify); n != 1 {
		t.Fatalf("verify work jobs = %d, want 1", n)
	}
}

// TestWaitLandedPushGateReleases: a dep that lands under gate_push ask is
// recorded waiting; the push phase's completion moves it to pushed and
// releases the dependent with no foreman action.
func TestWaitLandedPushGateReleases(t *testing.T) {
	e := newEnv(t)
	e.set(map[string]string{SettingGatePush: GateAsk})
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	dep, verify := waitLandedPlan(e)
	e.runTaskToDone(dep, author, reviewer)
	e.landTask(dep, author, `{"target_branch":"main","commit":"abc123","check_passed":true,"pushed":"waiting"}`)

	if got := e.str("SELECT state FROM tasks WHERE id = ?", verify); got != taskPending {
		t.Fatalf("verify state = %q, want pending while the landing waits", got)
	}
	if _, err := e.eng.PushApprove(e.ctx, envRepo); err != nil {
		t.Fatal(err)
	}
	if got := e.str("SELECT state FROM tasks WHERE id = ?", verify); got != taskPending {
		t.Fatalf("verify state = %q, want pending while the push is in flight", got)
	}

	pg := e.leaseGit(author)
	if pg == nil || !pg.Push || pg.TaskID != dep {
		t.Fatalf("push grant = %+v, want dep's push phase", pg)
	}
	if ans := e.complete(pg, Completion{Result: `{"target_branch":"main","commit":"abc123","pushed":"pushed"}`}); ans != AnsOK {
		t.Fatalf("complete the dep's push: %s", ans)
	}

	if got := e.str("SELECT state FROM tasks WHERE id = ?", verify); got != taskReady {
		t.Fatalf("verify state = %q, want ready once the push lands the dep", got)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'work'", verify); n != 1 {
		t.Fatalf("verify work jobs = %d, want 1", n)
	}
}

// TestWaitLandedNonImplementDepNeverBlocks: a dep of a class that never
// lands releases the barrier on its done, as any dep does.
func TestWaitLandedNonImplementDepNeverBlocks(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	res := e.createPlan([]PlanTask{
		{Name: "dep", Spec: "report", Class: "answer"},
		{Name: "verify", Spec: "verify", Deps: []string{"dep"}, WaitLanded: true},
	})
	dep, verify := res.Tasks[0].TaskID, res.Tasks[1].TaskID

	e.submit(e.workFor(dep))
	g := e.leaseAny(author)
	if g == nil || g.TaskID != dep {
		t.Fatalf("lease = %+v, want dep's work", g)
	}
	if ans := e.complete(g, done()); ans != AnsOK {
		t.Fatalf("complete the dep: %s", ans)
	}

	if got := e.str("SELECT state FROM tasks WHERE id = ?", verify); got != taskReady {
		t.Fatalf("verify state = %q, want ready: a non-implement dep never blocks", got)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'work'", verify); n != 1 {
		t.Fatalf("verify work jobs = %d, want 1", n)
	}
}

// TestWaitLandedNoDepsBehavesAsToday: a wait_landed task with no
// dependencies is ready at once, nothing to wait on.
func TestWaitLandedNoDepsBehavesAsToday(t *testing.T) {
	e := newEnv(t)
	res := e.createPlan([]PlanTask{{Name: "verify", Spec: "verify", WaitLanded: true}})
	verify := res.Tasks[0].TaskID
	if got := e.str("SELECT state FROM tasks WHERE id = ?", verify); got != taskReady {
		t.Fatalf("verify state = %q, want ready: no deps means nothing waits", got)
	}
}

// TestWaitLandedCrossPlanDepNeverWaits: a cross-plan dep writes no edge
// row and counts as done from the start, so a wait_landed task with such
// a dep never waits (§6.1).
func TestWaitLandedCrossPlanDepNeverWaits(t *testing.T) {
	e := landTargetEnv(t)
	dep := seededDep(e, taskDone, "brigada/landed", "pushed")
	res := e.createPlan([]PlanTask{
		{Name: "verify", Spec: "verify", Deps: []string{fmt.Sprint(dep)}, WaitLanded: true},
	})
	verify := res.Tasks[0].TaskID
	if got := e.str("SELECT state FROM tasks WHERE id = ?", verify); got != taskReady {
		t.Fatalf("verify state = %q, want ready: a landed cross-plan dep is already satisfied", got)
	}
	if got := e.int("SELECT COUNT(*) FROM task_deps WHERE task_id = ?", verify); got != 0 {
		t.Fatalf("verify edges = %d, want none: no edge is written for a cross-plan dep", got)
	}
}

// TestWaitLandedDeclinedLandingStaysBlocked: a dep whose landing is
// declined at the land gate never lands, so its wait_landed dependent
// stays pending (§7.1).
func TestWaitLandedDeclinedLandingStaysBlocked(t *testing.T) {
	e := newEnv(t)
	e.set(map[string]string{SettingGateLand: GateAsk})
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	dep, verify := waitLandedPlan(e)
	e.runTaskToDone(dep, author, reviewer)

	res, err := e.eng.LandReject(e.ctx, e.project, dep)
	if err != nil {
		t.Fatalf("LandReject: %v", err)
	}
	if res.Released {
		t.Fatalf("LandReject = %+v, want not released", res)
	}
	if got := e.str("SELECT state FROM tasks WHERE id = ?", verify); got != taskPending {
		t.Fatalf("verify state = %q, want pending: a declined landing never lands the dep", got)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'work'", verify); n != 0 {
		t.Fatalf("verify work jobs = %d, want none", n)
	}
}

// TestWaitLandedLandingCompletionVsRelease: the landing completion and
// the dependent's release race on a file-backed store; the barrier
// releases exactly once, with exactly one landing row and one work job.
func TestWaitLandedLandingCompletionVsRelease(t *testing.T) {
	e := newEnvAt(t, filepath.Join(t.TempDir(), "barrier.db"), nil)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	dep, verify := waitLandedPlan(e)
	e.runTaskToDone(dep, author, reviewer)
	e.trailerOK(dep)
	trailer := e.trailerFor(dep, "abc123")

	var mu sync.Mutex
	var errs []error
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := e.eng.RecordLanded(e.ctx, envRepo, "main", []LandedCommit{trailer}); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	if len(errs) > 0 {
		t.Fatalf("concurrent RecordLanded: %v", errs[0])
	}

	if got := e.int("SELECT COUNT(*) FROM landings WHERE task_id = ?", dep); got != 1 {
		t.Errorf("dep landings = %d, want exactly 1", got)
	}
	if got := e.str("SELECT state FROM tasks WHERE id = ?", verify); got != taskReady {
		t.Errorf("verify state = %q, want ready", got)
	}
	if got := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'work'", verify); got != 1 {
		t.Errorf("verify work jobs = %d, want exactly 1", got)
	}
}
