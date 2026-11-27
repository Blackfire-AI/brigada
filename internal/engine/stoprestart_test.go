package engine

import (
	"strings"
	"testing"
)

// The stop-running restart tests (§5.6): task_restart with stop_running
// on a task whose job is live.

// stoppedOn is the completion the runner sends for a run the engine's
// stop_running answer stopped: the stalled outcome, its reason, and the
// branch the stopped run pushed ("" when it pushed none).
func stoppedOn(branch string) Completion {
	block := `{"status":"stalled","reason":"stop_running"`
	if branch != "" {
		block += `,"branch":"` + branch + `"`
	}
	return Completion{Result: block + "}", Outcome: OutcomeStalled}
}

// runningWork queues the fixture task's work and leases it to author,
// the task running under a live lease.
func (e *env) runningWork(author int64) *LeaseGrant {
	e.t.Helper()
	e.submit(e.work())
	g := e.authorLease(author)
	if g == nil {
		e.t.Fatalf("no work job to lease; have %v", e.eventKinds())
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskRunning {
		e.t.Fatalf("task state under the lease = %q, want running", state)
	}
	return g
}

func (e *env) stopRunning(task int64, note string) RestartResult {
	e.t.Helper()
	res, err := e.eng.RestartTaskStopRunning(e.ctx, e.project, task, note)
	if err != nil {
		e.t.Fatalf("restart with stop_running: %v", err)
	}
	return res
}

func (e *env) restartJobs(task int64) int64 {
	return e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND restart = 1 AND retry = 0", task)
}

// TestStopRunningRestartStopsTheRunAndSeedsTheFreshAttempt covers the
// ordering end to end: the request leaves the running job live and the
// task running; the heartbeat answers stop_running; the runner's stalled
// completion settles the job and queues the fresh attempt seeded from
// the branch it pushed, the note in its prompt — no park, no foreman
// wake — and the restart is counted once.
func TestStopRunningRestartStopsTheRunAndSeedsTheFreshAttempt(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	g := e.runningWork(author)
	inbox := e.int("SELECT COUNT(*) FROM foreman_inbox WHERE project_id = ?", e.project)
	note := "keep the parser, drop the cache layer"

	res := e.stopRunning(e.task, note)
	if res.Answer != AnsPending || res.JobID != g.JobID {
		t.Fatalf("stop-running restart = %+v, want pending on job %d", res, g.JobID)
	}
	if status := e.str("SELECT status FROM jobs WHERE id = ?", g.JobID); status != statusLeased && status != statusRunning {
		t.Errorf("the running job's status after the request = %q, want it live", status)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskRunning {
		t.Errorf("task state after the request = %q, want running", state)
	}
	if got := e.str("SELECT stop_pending FROM tasks WHERE id = ?", e.task); got != note {
		t.Errorf("tasks.stop_pending = %q, want the note", got)
	}
	if n := e.restartJobs(e.task); n != 0 {
		t.Errorf("restart jobs at the request = %d, want none until the run stops", n)
	}
	if p, ok := e.eventPayloadOf(evTaskStopRequested); !ok || p.Job != g.JobID || p.Reason != note {
		t.Errorf("task_stop_requested payload = %+v (%t), want job %d and the note", p, ok, g.JobID)
	}

	// Every beat answers stop_running until the job ends.
	for i := 0; i < 2; i++ {
		if ans := e.heartbeat(g); ans != AnsStopRunning {
			t.Fatalf("heartbeat %d = %s, want stop_running", i, ans)
		}
	}

	if ans := e.complete(g, stoppedOn(g.Branch)); ans != AnsOK {
		t.Fatalf("complete the stopped run: %s", ans)
	}
	if status := e.str("SELECT status FROM jobs WHERE id = ?", g.JobID); status != statusStalled {
		t.Errorf("the stopped job's status = %q, want stalled", status)
	}
	fresh := e.int("SELECT id FROM jobs WHERE task_id = ? AND restart = 1", e.task)
	if fresh == 0 {
		t.Fatalf("no fresh attempt queued; have %v", e.eventKinds())
	}
	for _, check := range []struct{ query, want, what string }{
		{"SELECT kind FROM jobs WHERE id = ?", string(KindWork), "kind"},
		{"SELECT mode FROM jobs WHERE id = ?", string(ModeFresh), "mode"},
		{"SELECT status FROM jobs WHERE id = ?", statusQueued, "status"},
		{"SELECT COALESCE(seed_branch, '') FROM jobs WHERE id = ?", g.Branch, "seed branch"},
	} {
		if got := e.str(check.query, fresh); got != check.want {
			t.Errorf("fresh attempt %s = %q, want %q", check.what, got, check.want)
		}
	}
	if p := e.str("SELECT prompt FROM jobs WHERE id = ?", fresh); !strings.Contains(p, note) {
		t.Errorf("fresh attempt prompt = %q, want the note in it", p)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskReady {
		t.Errorf("task state after the stop = %q, want ready", state)
	}
	if pending := e.int("SELECT stop_pending IS NOT NULL FROM tasks WHERE id = ?", e.task); pending != 0 {
		t.Error("the pending restart survived its consumption")
	}
	if p, ok := e.eventPayloadOf(evTaskRestarted); !ok || p.Branch != g.Branch || p.Reason != note {
		t.Errorf("task_restarted payload = %+v (%t), want the seed %s and the note", p, ok, g.Branch)
	}
	if _, ok := e.eventPayloadOf(evTaskFailed); ok {
		t.Error("the stopped run parked the task as failed; want the restart")
	}
	if got := e.int("SELECT COUNT(*) FROM foreman_inbox WHERE project_id = ?", e.project); got != inbox {
		t.Errorf("foreman inbox grew from %d to %d; the consumed stop wakes nobody", inbox, got)
	}
	if n := e.foremanJobs(); n != 0 {
		t.Errorf("foreman turns = %d, want none", n)
	}
	if n := e.restartJobs(e.task); n != 1 {
		t.Errorf("restarts counted = %d, want exactly one", n)
	}

	// The fresh attempt's grant names the seed beside the task's own base.
	ng := e.leaseAny(author)
	if ng == nil || ng.JobID != fresh {
		t.Fatalf("lease = %+v, want the fresh attempt %d", ng, fresh)
	}
	if ng.SeedBranch != g.Branch || ng.BaseRef != "main" || ng.Branch == g.Branch {
		t.Errorf("fresh grant seed %q base %q branch %q, want seed %s on base main and a new branch", ng.SeedBranch, ng.BaseRef, ng.Branch, g.Branch)
	}
	if ans := e.heartbeat(ng); ans != AnsOK {
		t.Errorf("the fresh attempt's heartbeat = %s, want ok", ans)
	}
}

// TestStopRunningRestartRebuildsTheBaseAtConsumption: the rebuild base
// rule runs when the stopped job's completion consumes the restart, not
// at the request — a dependency that moved in between is the new base —
// and the dependents of the stopping task are left alone throughout.
func TestStopRunningRestartRebuildsTheBaseAtConsumption(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "openai", "review")
	res, err := e.eng.CreatePlan(e.ctx, e.project, "the chain", []PlanTask{
		{Name: "lib", Title: "the lib", Spec: "write the lib"},
		{Name: "app", Title: "the app", Spec: "use the lib", Deps: []string{"lib"}},
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	lib, app := res.Tasks[0].TaskID, res.Tasks[1].TaskID
	e.driveSync(author)
	g := e.leaseAny(author)
	if g == nil || g.TaskID != lib {
		t.Fatalf("lease = %+v, want lib's work", g)
	}
	if ans := e.complete(g, doneOn(branchOf(lib), passCheck)); ans != AnsOK {
		t.Fatalf("complete lib: %s", ans)
	}
	e.approveReview(reviewer)
	g = e.leaseAny(author)
	if g == nil || g.TaskID != app || g.BaseRef != branchOf(lib) {
		t.Fatalf("lease = %+v, want app's work on %s", g, branchOf(lib))
	}

	moved := branchOf(lib) + "-r1"
	e.exec("UPDATE tasks SET branch = ? WHERE id = ?", moved, lib)
	if r := e.stopRunning(app, "rebuild"); r.Answer != AnsPending {
		t.Fatalf("stop-running restart = %+v, want pending", r)
	}
	if got := e.str("SELECT base_ref FROM tasks WHERE id = ?", app); got != branchOf(lib) {
		t.Errorf("app base_ref at the request = %q, want %q untouched", got, branchOf(lib))
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", lib); state != taskDone {
		t.Errorf("lib state at the request = %q, want done untouched", state)
	}

	if ans := e.complete(g, stoppedOn(g.Branch)); ans != AnsOK {
		t.Fatalf("complete the stopped run: %s", ans)
	}
	if got := e.str("SELECT base_ref FROM tasks WHERE id = ?", app); got != moved {
		t.Errorf("app base_ref after the consumption = %q, want the rebuilt %q", got, moved)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", app); state != taskReady {
		t.Errorf("app state = %q, want ready", state)
	}
}

// TestStopRunningRestartOfQueuedJobRestartsNow: a live job no runner has
// leased is cancelled and the fresh attempt queued at once, as the plain
// restart replaces a live job; nothing is left pending.
func TestStopRunningRestartOfQueuedJobRestartsNow(t *testing.T) {
	e := newEnv(t)
	queued := e.submit(e.work()).JobID

	res := e.stopRunning(e.task, "start over")
	if res.Answer != AnsCreated || res.JobID == 0 || res.JobID == queued {
		t.Fatalf("stop-running restart of a queued job = %+v, want a fresh job created", res)
	}
	if status := e.str("SELECT status FROM jobs WHERE id = ?", queued); status != statusCancelled {
		t.Errorf("the queued job's status = %q, want cancelled", status)
	}
	if restart := e.int("SELECT restart FROM jobs WHERE id = ?", res.JobID); restart != 1 {
		t.Errorf("the fresh job's restart flag = %d, want 1", restart)
	}
	if seed := e.str("SELECT COALESCE(seed_branch, '') FROM jobs WHERE id = ?", res.JobID); seed != "" {
		t.Errorf("the fresh job's seed = %q, want none: nothing ran", seed)
	}
	if pending := e.int("SELECT stop_pending IS NOT NULL FROM tasks WHERE id = ?", e.task); pending != 0 {
		t.Error("a queued job's restart left a pending stop")
	}
	if _, ok := e.eventPayloadOf(evTaskStopRequested); ok {
		t.Error("a queued job's restart logged a stop request")
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskReady {
		t.Errorf("task state = %q, want ready", state)
	}
}

// TestStopRunningRestartWithoutLiveJobIsThePlainRestart: nothing to
// stop — a parked task restarts as task_restart restarts it, and a task
// with no live job that is not parked is refused the same way.
func TestStopRunningRestartWithoutLiveJobIsThePlainRestart(t *testing.T) {
	e := newEnv(t)
	if _, err := e.eng.RestartTaskStopRunning(e.ctx, e.project, e.task, "x"); err == nil ||
		!strings.Contains(err.Error(), "not parked") {
		t.Errorf("stop-running restart of an idle ready task = %v, want the not-parked refusal", err)
	}
	author := e.agented("author", "anthropic", "implement")
	e.parkAtNeedsHelp(author, "which way?")
	res := e.stopRunning(e.task, "this way")
	if res.Answer != AnsCreated || res.JobID == 0 {
		t.Fatalf("stop-running restart of a parked task = %+v, want created", res)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskReady {
		t.Errorf("task state = %q, want ready", state)
	}
}

// TestStopRunningRestartRefusesASecondRestart: while a stop-running
// restart is pending, another — stop_running or plain — is refused.
func TestStopRunningRestartRefusesASecondRestart(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	e.runningWork(author)
	e.stopRunning(e.task, "first")
	if _, err := e.eng.RestartTaskStopRunning(e.ctx, e.project, e.task, "second"); err == nil ||
		!strings.Contains(err.Error(), "already has a stop-running restart pending") {
		t.Errorf("second stop-running restart = %v, want the pending refusal", err)
	}
	if _, err := e.eng.RestartTask(e.ctx, e.project, e.task, "plain"); err == nil ||
		!strings.Contains(err.Error(), "already has a stop-running restart pending") {
		t.Errorf("plain restart while pending = %v, want the pending refusal", err)
	}
	if got := e.str("SELECT stop_pending FROM tasks WHERE id = ?", e.task); got != "first" {
		t.Errorf("stop_pending = %q, want the first request's note", got)
	}
}

// TestStopRunningRestartBudgetSpent: a spent restart budget refuses at
// the request, the running job left running and nothing pending.
func TestStopRunningRestartBudgetSpent(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	e.exec(`UPDATE tasks SET caps = '{"restart":0}' WHERE id = ?`, e.task)
	g := e.runningWork(author)
	if _, err := e.eng.RestartTaskStopRunning(e.ctx, e.project, e.task, "x"); err == nil ||
		!strings.Contains(err.Error(), "restart budget") {
		t.Errorf("stop-running restart past the budget = %v, want the budget named", err)
	}
	if pending := e.int("SELECT stop_pending IS NOT NULL FROM tasks WHERE id = ?", e.task); pending != 0 {
		t.Error("a refused request left a pending stop")
	}
	if ans := e.heartbeat(g); ans != AnsOK {
		t.Errorf("heartbeat after the refusal = %s, want ok", ans)
	}
}

// TestStopRunningRestartClearedByCancel: a cancel of the stopping job —
// or of the task, or its retirement — clears the pending restart, and
// nothing is queued behind it.
func TestStopRunningRestartClearedByCancel(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	g := e.runningWork(author)
	e.stopRunning(e.task, "x")
	if ans := e.cancel(g.JobID); ans != AnsOK {
		t.Fatalf("cancel the stopping job: %s", ans)
	}
	if pending := e.int("SELECT stop_pending IS NOT NULL FROM tasks WHERE id = ?", e.task); pending != 0 {
		t.Error("the job's cancel left the pending restart")
	}
	if n := e.restartJobs(e.task); n != 0 {
		t.Errorf("restart jobs after the cancel = %d, want none", n)
	}

	other := e.addTask("{}")
	e.submit(e.workOn(other))
	if og := e.authorLease(author); og == nil || og.TaskID != other {
		t.Fatalf("lease = %+v, want the second task's work", og)
	}
	e.stopRunning(other, "y")
	if _, err := e.eng.Retire(e.ctx, e.project, []int64{other}, "landed by hand", "foreman"); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if pending := e.int("SELECT stop_pending IS NOT NULL FROM tasks WHERE id = ?", other); pending != 0 {
		t.Error("the retirement left the pending restart")
	}
	if n := e.restartJobs(other); n != 0 {
		t.Errorf("restart jobs after the retirement = %d, want none", n)
	}
}

// TestStopRunningRestartSurvivesALeaseRequeue: a lapsed lease is not the
// job's end — the next lease's heartbeat answers stop_running again.
func TestStopRunningRestartSurvivesALeaseRequeue(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	g := e.runningWork(author)
	e.stopRunning(e.task, "x")
	e.expireLease()
	if n := e.restartJobs(e.task); n != 0 {
		t.Fatalf("restart jobs after the lapse = %d, want none: the requeue is not terminal", n)
	}
	g2 := e.authorLease(author)
	if g2 == nil || g2.JobID != g.JobID {
		t.Fatalf("re-lease = %+v, want job %d again", g2, g.JobID)
	}
	if ans := e.heartbeat(g2); ans != AnsStopRunning {
		t.Errorf("the re-lease's heartbeat = %s, want stop_running", ans)
	}
}

// TestStopRunningRestartRunnerGoneRestartsUnseeded: a job that ends with
// no stop completion — its runner lost past the attempts — consumes the
// pending restart from the rebuild base, unseeded, and parks nothing.
func TestStopRunningRestartRunnerGoneRestartsUnseeded(t *testing.T) {
	e := newEnvCfg(t, func(c *Config) { c.MaxAttempts = 1 })
	author := e.agented("author", "anthropic", "implement")
	g := e.runningWork(author)
	e.stopRunning(e.task, "the runner is gone")
	e.expireLease()
	// The next poll fails the job past its attempts; the consumption
	// queues the fresh attempt, which the same poll may lease.
	e.authorLease(author)
	if status := e.str("SELECT status FROM jobs WHERE id = ?", g.JobID); status != statusFailed {
		t.Fatalf("the lost job's status = %q, want failed", status)
	}
	fresh := e.int("SELECT id FROM jobs WHERE task_id = ? AND restart = 1", e.task)
	if fresh == 0 {
		t.Fatalf("no fresh attempt queued; have %v", e.eventKinds())
	}
	if seed := e.str("SELECT COALESCE(seed_branch, '') FROM jobs WHERE id = ?", fresh); seed != "" {
		t.Errorf("fresh attempt seed = %q, want none: the lost run pushed nothing", seed)
	}
	if p := e.str("SELECT prompt FROM jobs WHERE id = ?", fresh); !strings.Contains(p, "the runner is gone") {
		t.Errorf("fresh attempt prompt = %q, want the note", p)
	}
	if _, ok := e.eventPayloadOf(evTaskFailed); ok {
		t.Error("the lost job parked the task as failed; want the restart")
	}
	if pending := e.int("SELECT stop_pending IS NOT NULL FROM tasks WHERE id = ?", e.task); pending != 0 {
		t.Error("the pending restart survived its consumption")
	}
}
