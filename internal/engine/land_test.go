package engine

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
)

// land helpers for the tests below.

// runToDone drives the fixture's task through work and review to done —
// the author's done under a passing check, then a substantive approve —
// which is the state the land rule reads (§7.1). The land job the rule
// queues (or the hold it parks) is left for the test to read.
func (e *env) runToDone(author, reviewer int64) {
	e.t.Helper()
	e.submit(e.work())
	g := e.leaseAny(author)
	if g == nil {
		e.t.Fatal("the work job was not leased")
	}
	if ans := e.complete(g, doneOn("task/1-1", passCheck)); ans != AnsOK {
		e.t.Fatalf("complete the work: %s", ans)
	}
	e.runReview(reviewer, `{"verdict":"approve","findings":"`+substantiveFindings+`"}`)
}

// leaseGit leases with the git label the land job requires (§7.1): the
// runner's advertisement that it can take a landing.
func (e *env) leaseGit(owner int64) *LeaseGrant {
	e.t.Helper()
	return e.lease(owner, nil, []string{"git"})
}

// workFor is the work-job submit for a specific task of the fixture plan.
func (e *env) workFor(task int64) SubmitParams {
	return SubmitParams{
		Key:       fmt.Sprintf("task:%d", task),
		ProjectID: e.project,
		TaskID:    task,
		Kind:      KindWork,
		Mode:      ModeFresh,
		Prompt:    "the composed prompt",
	}
}

// runTaskToDone drives one task of the fixture plan through work and a
// substantive approve, as runToDone drives the fixture's task.
func (e *env) runTaskToDone(task, author, reviewer int64) {
	e.t.Helper()
	e.submit(e.workFor(task))
	g := e.leaseAny(author)
	if g == nil {
		e.t.Fatalf("task %d: no work job to lease; events %v", task, e.eventKinds())
	}
	if ans := e.complete(g, doneOn("task/1-1", passCheck)); ans != AnsOK {
		e.t.Fatalf("task %d: complete the work: %s", task, ans)
	}
	e.runReview(reviewer, `{"verdict":"approve","findings":"`+substantiveFindings+`"}`)
}

// maybeLand runs the land rule for one task again, for the idempotence
// tests: the rule is what a dependency landing or a hold release calls,
// so running it twice must not queue a second job.
func (e *env) maybeLand(task int64) {
	e.t.Helper()
	if err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
		return e.eng.maybeLandTaskTx(tx, task, e.clock.Now())
	}); err != nil {
		e.t.Fatal(err)
	}
}

// landPushed leases the fixture task's land job and completes it with a
// pushed landing of commit abc123 on main.
func (e *env) landPushed(owner int64) {
	e.t.Helper()
	g := e.leaseGit(owner)
	if g == nil || g.Kind != KindLand {
		e.t.Fatalf("the land job was not leased: %+v", g)
	}
	if ans := e.complete(g, Completion{Result: `{"target_branch":"main","commit":"abc123","check_passed":true,"pushed":"pushed"}`}); ans != AnsOK {
		e.t.Fatalf("complete the land job: %s", ans)
	}
}

// TestLandJobLostParks: a land job that loses its runner MaxAttempts
// times parks the task at needs_foreman with a land failure — the
// reason, the needs-you entry and the foreman's wake — as the queue-TTL
// path does, instead of leaving it done with no landing.
func TestLandJobLostParks(t *testing.T) {
	e := newEnvCfg(t, func(c *Config) { c.MaxAttempts = 1 })
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	e.runToDone(author, reviewer)
	turns := e.foremanJobs()

	if g := e.leaseGit(author); g == nil || g.Kind != KindLand {
		t.Fatalf("the land job was not leased: %+v", g)
	}
	e.clock.Advance(e.eng.cfg.LeaseTTL + time.Second)
	if rep := e.mustSweep(); len(rep.Expired) != 1 {
		t.Fatalf("the sweep requeued nothing: %+v", rep)
	}
	if g := e.leaseGit(author); g != nil {
		t.Fatalf("the land job was offered past its attempt cap: %+v", g)
	}
	if status := e.str("SELECT status FROM jobs WHERE task_id = ? AND kind = 'land'", e.task); status != statusFailed {
		t.Errorf("land job status = %q, want failed", status)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskNeedsForeman {
		t.Errorf("task state = %q, want needs_foreman", state)
	}
	if p, ok := e.eventPayloadOf(evTaskLandFailed); !ok || !strings.Contains(p.Reason, "runner lost") {
		t.Errorf("task_land_failed event = %+v (%t), want the lost-runner reason", p, ok)
	}
	if it, ok := e.needsYouTask(e.task); !ok || it.State != taskNeedsForeman {
		t.Errorf("needs-you entry = %+v (%t), want the parked task", it, ok)
	}
	if got := e.foremanJobs(); got <= turns {
		t.Errorf("foreman turns = %d, want a wake beyond the %d before", got, turns)
	}
	if n := e.int("SELECT COUNT(*) FROM landings WHERE task_id = ?", e.task); n != 0 {
		t.Errorf("landing records = %d, want none", n)
	}
}

// TestPlanWaitsForItsLastLandJob: the plan's last task done with its
// land job queued does not close the plan; the landing does, and the
// done report says all tasks landed.
func TestPlanWaitsForItsLastLandJob(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	e.runToDone(author, reviewer)

	if n := e.landJobs(); n != 1 {
		t.Fatalf("land jobs = %d, want 1 queued at the task's done", n)
	}
	if state := e.str("SELECT state FROM plans WHERE id = ?", e.plan); state != "active" {
		t.Errorf("plan state with its land job queued = %q, want active", state)
	}
	if _, ok := e.eventPayloadOf(evPlanDone); ok {
		t.Error("plan_done written before the land job ran")
	}

	e.landPushed(author)
	if state := e.str("SELECT state FROM plans WHERE id = ?", e.plan); state != planDone {
		t.Errorf("plan state after the landing = %q, want done", state)
	}
	p, ok := e.eventPayloadOf(evPlanDone)
	if !ok || !strings.Contains(p.Reason, "all tasks landed") {
		t.Errorf("plan_done = %+v (%t), want the report to say all tasks landed", p, ok)
	}
}

// TestPlanStaysOpenWhenItsLandingFails: a landing that fails parks the
// task, so the plan does not close as a success before the foreman
// acts on it.
func TestPlanStaysOpenWhenItsLandingFails(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	e.runToDone(author, reviewer)

	g := e.leaseGit(author)
	if g == nil {
		t.Fatal("the land job was not leased")
	}
	if ans := e.complete(g, Completion{Result: `{"target_branch":"main","commit":"","check_passed":false,"conflict_files":["a.go"]}`}); ans != AnsOK {
		t.Fatalf("complete the land job: %s", ans)
	}
	if state := e.str("SELECT state FROM plans WHERE id = ?", e.plan); state != "active" {
		t.Errorf("plan state after a failed landing = %q, want active", state)
	}
	if n := e.int("SELECT COUNT(*) FROM outcomes WHERE class = 'plan'"); n != 0 {
		t.Errorf("plan outcome rows = %d, want none before the plan closes", n)
	}
}

// landJobs counts the land jobs of the fixture's task.
func (e *env) landJobs() int64 {
	return e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'land'", e.task)
}

// TestLandRuleCreatesOneLandJob: a done, approved implement task with no
// dependencies gets exactly one land job, on the task's own key, tagged
// with the git label the runner must offer.
func TestLandRuleCreatesOneLandJob(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	e.runToDone(author, reviewer)

	if n := e.landJobs(); n != 1 {
		t.Fatalf("land jobs = %d, want 1", n)
	}
	if key := e.str("SELECT key FROM jobs WHERE task_id = ? AND kind = 'land'", e.task); key != keyOf(e.task) {
		t.Errorf("land job key = %q, want the task's own %q", key, keyOf(e.task))
	}
	if labels := e.str("SELECT labels FROM jobs WHERE task_id = ? AND kind = 'land'", e.task); labels != `["git"]` {
		t.Errorf("land job labels = %q, want [\"git\"]", labels)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskDone {
		t.Errorf("task state = %q, want done while its land job waits", state)
	}
}

// TestLandGrantCarriesTheWorkersCommitMessage: the commit message the
// worker's result block wrote (§5.4) rides the land job's grant, so the
// runner's squash uses the repo's own style; without one the grant's
// field stays empty and the runner builds today's format.
func TestLandGrantCarriesTheWorkersCommitMessage(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")

	e.submit(e.work())
	g := e.leaseAny(author)
	if g == nil {
		e.t.Fatal("the work job was not leased")
	}
	res := `{"status":"done","summary":"s","branch":"task/1-1","commit_message":"feat(x): do it\n\nThe body."}`
	if ans := e.complete(g, Completion{Result: res, CheckResult: passCheck}); ans != AnsOK {
		e.t.Fatalf("complete the work: %s", ans)
	}
	e.runReview(reviewer, `{"verdict":"approve","findings":"`+substantiveFindings+`"}`)

	lg := e.leaseGit(author)
	if lg == nil {
		e.t.Fatal("the land job was not leased")
	}
	if lg.LandMessage == nil || lg.LandMessage.CommitMessage != "feat(x): do it\n\nThe body." {
		t.Errorf("the grant's commit message = %+v, want the worker's", lg.LandMessage)
	}
}

// TestLandRuleIsIdempotent: re-running the rule — what a dependency
// landing or a hold release does — does not queue a second land job.
func TestLandRuleIsIdempotent(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	e.runToDone(author, reviewer)

	// Re-run the rule directly, as a later dependency landing would.
	e.maybeLand(e.task)
	if n := e.landJobs(); n != 1 {
		t.Errorf("land jobs after a re-run = %d, want 1", n)
	}
}

// TestLandGateAskHolds: gate_land ask holds the task at the land gate
// with a needs-you entry naming the rule, and queues no land job. The
// task stays done: its work is finished, only the landing waits.
func TestLandGateAskHolds(t *testing.T) {
	e := newEnv(t)
	e.set(map[string]string{SettingGateLand: GateAsk})
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	e.runToDone(author, reviewer)

	if n := e.landJobs(); n != 0 {
		t.Fatalf("land jobs = %d, want none while held", n)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskDone {
		t.Errorf("task state = %q, want done while held", state)
	}
	if reason := e.str("SELECT reason FROM land_holds WHERE task_id = ?", e.task); !strings.Contains(reason, "held by rule gate_land: gate_land is ask") {
		t.Errorf("land hold reason = %q, want the gate_land rule", reason)
	}
	it, ok := e.needsYouTask(e.task)
	if !ok || !strings.Contains(it.Reason, "held by rule gate_land: gate_land is ask") || !strings.Contains(it.Reason, "brigada land approve --and-stop-asking") {
		t.Errorf("needs-you entry = %+v (%t), want a land-gate reason", it, ok)
	}
}

// TestLandGateProtectedPathHolds: a protected path the task touches holds
// its landing, naming the pattern.
func TestLandGateProtectedPathHolds(t *testing.T) {
	e := newEnv(t)
	e.set(map[string]string{SettingApprovalProtectedPaths: "infra/**"})
	e.exec("UPDATE tasks SET spec = 'rewrite the infra/gateway' WHERE id = ?", e.task)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	e.runToDone(author, reviewer)

	if n := e.landJobs(); n != 0 {
		t.Fatalf("land jobs = %d, want none while held", n)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskDone {
		t.Errorf("task state = %q, want done while held", state)
	}
	if reason := e.str("SELECT reason FROM land_holds WHERE task_id = ?", e.task); !strings.Contains(reason, "touches infra/**") {
		t.Errorf("land hold reason = %q, want the protected path", reason)
	}
}

// TestLandGateTierHolds: a task whose tier is among approval_tiers waits.
func TestLandGateTierHolds(t *testing.T) {
	e := newEnv(t)
	e.set(map[string]string{SettingApprovalTiers: "heavy"})
	e.exec("UPDATE tasks SET effective_tier = 'heavy' WHERE id = ?", e.task)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	e.runToDone(author, reviewer)

	if n := e.landJobs(); n != 0 {
		t.Fatalf("land jobs = %d, want none while held", n)
	}
	if reason := e.str("SELECT reason FROM land_holds WHERE task_id = ?", e.task); !strings.Contains(reason, "heavy") {
		t.Errorf("land hold reason = %q, want the tier", reason)
	}
}

// TestLandGateRestartedHolds: approval_restarted with a task that spent a
// restart holds its landing.
func TestLandGateRestartedHolds(t *testing.T) {
	e := newEnv(t)
	e.set(map[string]string{SettingApprovalRestarted: "true"})
	// A spent restart, recorded on a terminal restart job (its state does
	// not matter: the rule counts the restart, the approval reads the
	// latest done author job).
	e.exec(`INSERT INTO jobs (project_id, task_id, kind, key, mode, prompt, status, restart, created_at, updated_at)
		VALUES (?, ?, 'work', ?, 'fresh', 'p', 'cancelled', 1, ?, ?)`,
		e.project, e.task, fmt.Sprintf("task:%d", e.task), seedTS, seedTS)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	e.runToDone(author, reviewer)

	if n := e.landJobs(); n != 0 {
		t.Fatalf("land jobs = %d, want none while held", n)
	}
	if reason := e.str("SELECT reason FROM land_holds WHERE task_id = ?", e.task); !strings.Contains(reason, "restarted") {
		t.Errorf("land hold reason = %q, want the restart rule", reason)
	}
}

// TestLandSuccessMarksLanded: a land job completed with a passed check
// keeps the task done, writes the landing record and the landed event.
func TestLandSuccessMarksLanded(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	e.runToDone(author, reviewer)

	g := e.leaseGit(author)
	if g == nil || g.Kind != KindLand {
		t.Fatalf("the land job was not leased: %+v", g)
	}
	if g.LandTarget != "main" || g.Branch != branchOf(e.task) || g.BaseRef != "main" {
		t.Errorf("land grant target/branch/base = %q/%q/%q, want main/%s/main", g.LandTarget, g.Branch, g.BaseRef, branchOf(e.task))
	}
	if g.Agent != "" || g.CheckCmd != "" {
		t.Errorf("land grant agent/check = %q/%q, want none", g.Agent, g.CheckCmd)
	}
	if ans := e.complete(g, Completion{Result: `{"target_branch":"main","commit":"abc123","check_passed":true,"pushed":"pushed"}`}); ans != AnsOK {
		t.Fatalf("complete the land job: %s", ans)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskDone {
		t.Errorf("task state = %q, want done after the land", state)
	}
	if n := e.int("SELECT COUNT(*) FROM landings WHERE task_id = ? AND commit_sha = 'abc123' AND pushed = 'pushed'", e.task); n != 1 {
		t.Errorf("landing record = %d rows, want 1 with the commit and push state", n)
	}
	if p, ok := e.eventPayloadOf(evTaskLanded); !ok || p.Commit != "abc123" || p.Pushed != "pushed" {
		t.Errorf("task_landed event = %+v (%t), want the landing record", p, ok)
	}
}

// TestLandConflictParks: a conflict parks the task at needs_foreman with
// the conflicting files, nothing pushed and no landing record.
func TestLandConflictParks(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	e.runToDone(author, reviewer)

	g := e.leaseGit(author)
	if g == nil {
		t.Fatal("the land job was not leased")
	}
	if ans := e.complete(g, Completion{Result: `{"target_branch":"main","commit":"","check_passed":false,"conflict_files":["a.go","b.go"]}`}); ans != AnsOK {
		t.Fatalf("complete the land job: %s", ans)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskNeedsForeman {
		t.Errorf("task state = %q, want needs_foreman", state)
	}
	if n := e.int("SELECT COUNT(*) FROM landings WHERE task_id = ?", e.task); n != 0 {
		t.Errorf("landing records = %d, want none after a conflict", n)
	}
	if p, ok := e.eventPayloadOf(evTaskLandFailed); !ok || !strings.Contains(p.Reason, "a.go") || !strings.Contains(p.Reason, "b.go") {
		t.Errorf("task_land_failed event = %+v (%t), want the conflicting files", p, ok)
	}
	// The foreman restarts the parked task with a note, end to end: the
	// task returns to ready on a fresh work attempt.
	res, err := e.eng.RestartTask(e.ctx, e.project, e.task, "rebase onto main and resolve")
	if err != nil {
		t.Fatalf("RestartTask after the conflict: %v", err)
	}
	if res.JobID == 0 {
		t.Fatal("RestartTask queued no work job")
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskReady {
		t.Errorf("task state after the restart = %q, want ready", state)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'work' AND restart = 1", e.task); n != 1 {
		t.Errorf("restart jobs = %d, want 1", n)
	}
}

// TestLandCheckFailParks: a check that failed on the landed result parks
// the task with the check log, nothing pushed.
func TestLandCheckFailParks(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	e.runToDone(author, reviewer)

	g := e.leaseGit(author)
	if g == nil {
		t.Fatal("the land job was not leased")
	}
	if ans := e.complete(g, Completion{Result: `{"target_branch":"main","commit":"","check_passed":false,"check_log":"FAIL TestBoom"}`}); ans != AnsOK {
		t.Fatalf("complete the land job: %s", ans)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskNeedsForeman {
		t.Errorf("task state = %q, want needs_foreman", state)
	}
	if p, ok := e.eventPayloadOf(evTaskLandFailed); !ok || !strings.Contains(p.Reason, "check") || !strings.Contains(p.CheckOutput, "TestBoom") {
		t.Errorf("task_land_failed event = %+v (%t), want the check log", p, ok)
	}
}

// TestLandWaitsForDependency: a task whose dependency has not landed gets
// no land job until that dependency lands, when the rule re-evaluates it.
func TestLandWaitsForDependency(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")

	dep := e.task            // the fixture's task is the dependency
	child := e.addTask("{}") // a second task of the same plan
	e.exec("UPDATE tasks SET state = 'pending' WHERE id = ?", child)
	e.exec("INSERT INTO task_deps (plan_id, task_id, depends_on_task_id) VALUES (?, ?, ?)", e.plan, child, dep)

	// The dependency lands first: done, approved, its own land job queued.
	e.submit(e.work())
	g := e.leaseAny(author)
	if g == nil {
		t.Fatal("the dependency's work job was not leased")
	}
	if ans := e.complete(g, doneOn("task/1-1", passCheck)); ans != AnsOK {
		t.Fatalf("complete the dependency's work: %s", ans)
	}
	e.runReview(reviewer, `{"verdict":"approve","findings":"`+substantiveFindings+`"}`)
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'land'", dep); n != 1 {
		t.Fatalf("dependency land jobs = %d, want 1", n)
	}

	// The dependent runs to done; its dependency is not landed, so no land
	// job for it yet.
	g = e.leaseAny(author)
	if g == nil {
		t.Fatal("the dependent's work job was not leased")
	}
	if ans := e.complete(g, doneOn("task/1-1", passCheck)); ans != AnsOK {
		t.Fatalf("complete the dependent's work: %s", ans)
	}
	e.runReview(reviewer, `{"verdict":"approve","findings":"`+substantiveFindings+`"}`)
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'land'", child); n != 0 {
		t.Fatalf("dependent land jobs before its dependency lands = %d, want 0", n)
	}

	// The dependency lands; the rule re-evaluates the dependent and queues
	// its land job.
	lg := e.leaseGit(author)
	if lg == nil || lg.TaskID != dep {
		t.Fatalf("the dependency's land job was not leased: %+v", lg)
	}
	if ans := e.complete(lg, Completion{Result: `{"target_branch":"main","commit":"abc","check_passed":true,"pushed":"pushed"}`}); ans != AnsOK {
		t.Fatalf("complete the dependency's land: %s", ans)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'land'", child); n != 1 {
		t.Errorf("dependent land jobs after its dependency lands = %d, want 1", n)
	}
}

// TestLandHoldDoesNotBlockDependentsWork: a held task stays done, so a
// task depending on it and another task is released when that other
// finishes — the hold blocks only the dependent's landing, never its work
// (§6.3: "work on the rest of the plan goes on").
func TestLandHoldDoesNotBlockDependentsWork(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	e.set(map[string]string{SettingApprovalTiers: "heavy"})

	a := e.task // heavy: held at the land gate when it finishes
	b := e.addTask("{}")
	c := e.addTask("{}")
	e.exec("UPDATE tasks SET effective_tier = 'heavy' WHERE id = ?", a)
	e.exec("UPDATE tasks SET state = 'pending' WHERE id = ?", c)
	e.exec("INSERT INTO task_deps (plan_id, task_id, depends_on_task_id) VALUES (?, ?, ?)", e.plan, c, a)
	e.exec("INSERT INTO task_deps (plan_id, task_id, depends_on_task_id) VALUES (?, ?, ?)", e.plan, c, b)

	// A finishes first and is held; C still waits on B.
	e.runTaskToDone(a, author, reviewer)
	if state := e.str("SELECT state FROM tasks WHERE id = ?", a); state != taskDone {
		t.Fatalf("held task state = %q, want done", state)
	}
	if n := e.int("SELECT COUNT(*) FROM land_holds WHERE task_id = ?", a); n != 1 {
		t.Fatalf("land holds for A = %d, want 1", n)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", c); state != taskPending {
		t.Fatalf("C state after only A finished = %q, want pending", state)
	}

	// B finishes; both of C's dependencies are done, so C is released to
	// run its work — only C's landing (later) waits on A's hold.
	e.runTaskToDone(b, author, reviewer)
	if state := e.str("SELECT state FROM tasks WHERE id = ?", c); state != taskReady {
		t.Fatalf("C state after B finished = %q, want ready", state)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'work' AND status = 'queued'", c); n != 1 {
		t.Errorf("C work jobs = %d, want 1 queued", n)
	}
}

// TestStaleDroppedForgetsOtherWaitingLandings: a land job that deleted the
// target's stale waiting ref says so on its completion; the other
// landing that waited at the push gate is forgotten and its task's land
// job is queued again, while the completing job's own landing stays.
func TestStaleDroppedForgetsOtherWaitingLandings(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	a := e.task
	b := e.addTask("{}")

	e.runTaskToDone(a, author, reviewer)
	g := e.leaseGit(author)
	if g == nil {
		t.Fatal("A's land job was not leased")
	}
	if ans := e.complete(g, Completion{Result: `{"target_branch":"main","commit":"aaa111","check_passed":true,"pushed":"waiting"}`}); ans != AnsOK {
		t.Fatalf("complete A's land job: %s", ans)
	}
	e.runTaskToDone(b, author, reviewer)
	g = e.leaseGit(author)
	if g == nil {
		t.Fatal("B's land job was not leased")
	}
	if ans := e.complete(g, Completion{
		Result:       `{"target_branch":"main","commit":"bbb222","check_passed":true,"pushed":"waiting"}`,
		StaleDropped: "main",
	}); ans != AnsOK {
		t.Fatalf("complete B's land job: %s", ans)
	}
	if n := e.int("SELECT COUNT(*) FROM landings WHERE task_id = ?", a); n != 0 {
		t.Errorf("A's stale landing records = %d, want it forgotten", n)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'land' AND status = 'queued'", a); n != 1 {
		t.Errorf("A's queued land jobs = %d, want 1", n)
	}
	if n := e.int("SELECT COUNT(*) FROM landings WHERE task_id = ?", b); n != 1 {
		t.Errorf("B's landing records = %d, want its own kept", n)
	}
}

// staleReland lands the fixture's task A with the given push state, then
// a second task B whose land job reports the target's waiting ref
// dropped as stale, so A's landing is forgotten and its land job queued
// again. between runs after A's landing and before B's, after runs once
// A's land job is queued; each may switch the push gate. It returns the
// grant of A's re-made land job.
func (e *env) staleReland(pushed string, between, after func()) *LeaseGrant {
	e.t.Helper()
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	a := e.task
	b := e.addTask("{}")
	e.runTaskToDone(a, author, reviewer)
	g := e.leaseGit(author)
	if g == nil {
		e.t.Fatal("A's land job was not leased")
	}
	rec := `{"target_branch":"main","commit":"aaa111","check_passed":true,"pushed":"` + pushed + `"`
	if pushed == "local" {
		rec += `,"push_cmd":"git fetch origin refs/brigada/land/main && git push origin FETCH_HEAD:refs/heads/main"`
	}
	if ans := e.complete(g, Completion{Result: rec + "}"}); ans != AnsOK {
		e.t.Fatalf("complete A's land job: %s", ans)
	}
	between()
	e.runTaskToDone(b, author, reviewer)
	g = e.leaseGit(author)
	if g == nil || g.TaskID != b {
		e.t.Fatalf("B's land job was not leased: %+v", g)
	}
	if ans := e.complete(g, Completion{
		Result:       `{"target_branch":"main","commit":"bbb222","check_passed":true,"pushed":"pushed"}`,
		StaleDropped: "main",
	}); ans != AnsOK {
		e.t.Fatalf("complete B's land job: %s", ans)
	}
	if n := e.int("SELECT COUNT(*) FROM landings WHERE task_id = ?", a); n != 0 {
		e.t.Fatalf("A's stale landing records = %d, want it forgotten", n)
	}
	after()
	g = e.leaseGit(author)
	if g == nil || g.TaskID != a || g.Kind != KindLand || g.Push {
		e.t.Fatalf("A's re-made land job was not leased: %+v", g)
	}
	return g
}

// TestStaleRelandKeepsWaitingForTheYes (§6.3, §7.1): a landing that
// waited at the push gate and is re-made after its waiting ref went stale
// never pushes without the user's yes, whichever comes first — the switch
// of gate_push from ask to auto, or the drop of the stale ref. A landing
// left to the user under never is re-made too, so its push command names
// a ref that exists, and a switch to auto does not push it either.
func TestStaleRelandKeepsWaitingForTheYes(t *testing.T) {
	toAuto := func(e *env) func() {
		return func() { e.set(map[string]string{SettingGatePush: GateAuto}) }
	}
	none := func() {}
	for _, c := range []struct {
		name   string
		pushed string
		gate   string
		run    func(e *env) *LeaseGrant
		want   string
	}{
		{"switch then drop", "waiting", GateAsk, func(e *env) *LeaseGrant { return e.staleReland("waiting", toAuto(e), none) }, GateAsk},
		{"drop then switch", "waiting", GateAsk, func(e *env) *LeaseGrant { return e.staleReland("waiting", none, toAuto(e)) }, GateAsk},
		{"still ask", "waiting", GateAsk, func(e *env) *LeaseGrant { return e.staleReland("waiting", none, none) }, GateAsk},
		{"local under never", "local", GateNever, func(e *env) *LeaseGrant { return e.staleReland("local", none, none) }, GateNever},
		{"local, then auto", "local", GateNever, func(e *env) *LeaseGrant { return e.staleReland("local", none, toAuto(e)) }, GateAsk},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.set(map[string]string{SettingGatePush: c.gate})
			if g := c.run(e); g.GatePush != c.want {
				t.Errorf("re-made landing's push gate = %q, want %q", g.GatePush, c.want)
			}
		})
	}

	// A task that never waited lands under auto as auto.
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	e.runToDone(author, reviewer)
	if g := e.leaseGit(author); g == nil || g.GatePush != GateAuto {
		t.Errorf("first landing grant = %+v, want gate_push auto", g)
	}
}

// TestLandGrantCarriesTheApprovedHead (§7.1): the land job's grant names
// the head the approving review judged, from the author job's result, so
// the runner lands that head or nothing; the hand-driven land reads the
// same head.
func TestLandGrantCarriesTheApprovedHead(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	e.submit(e.work())
	g := e.leaseAny(author)
	if g == nil {
		t.Fatal("no work job to lease")
	}
	if ans := e.complete(g, Completion{
		Result:      `{"status":"done","summary":"added the thing","branch":"task/1-1","head":"head-approved"}`,
		CheckResult: passCheck,
	}); ans != AnsOK {
		t.Fatalf("complete the work: %s", ans)
	}
	e.runReview(reviewer, `{"verdict":"approve","findings":"`+substantiveFindings+`"}`)
	g = e.leaseGit(author)
	if g == nil || g.ApprovedHead != "head-approved" {
		t.Fatalf("land grant = %+v, want the approved head", g)
	}
	info, err := e.eng.TaskLand(e.ctx, e.task)
	if err != nil || info == nil || info.ApprovedHead != "head-approved" {
		t.Errorf("TaskLand = %+v, %v; want the approved head", info, err)
	}
}

// TestLandFailedNeedsYouReason: a task parked by a land conflict shows the
// conflict's own reason in the needs-you queue, not an older event's.
func TestLandFailedNeedsYouReason(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	e.runToDone(author, reviewer)

	g := e.leaseGit(author)
	if g == nil {
		t.Fatal("the land job was not leased")
	}
	if ans := e.complete(g, Completion{Result: `{"target_branch":"main","commit":"","check_passed":false,"conflict_files":["a.go"]}`}); ans != AnsOK {
		t.Fatalf("complete the land job: %s", ans)
	}
	it, ok := e.needsYouTask(e.task)
	if !ok || !strings.Contains(it.Reason, "land conflict") || !strings.Contains(it.Reason, "a.go") {
		t.Errorf("needs-you entry = %+v (%t), want the conflict's reason", it, ok)
	}
}

// TestLandFailedTaskCannotBeAnswered: the foreman cannot answer a task
// parked by a land failure — the park is the land failure's, not a
// question's, so it must restart the task instead.
func TestLandFailedTaskCannotBeAnswered(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	e.runToDone(author, reviewer)

	g := e.leaseGit(author)
	if g == nil {
		t.Fatal("the land job was not leased")
	}
	if ans := e.complete(g, Completion{Result: `{"target_branch":"main","commit":"","check_passed":false,"conflict_files":["a.go"]}`}); ans != AnsOK {
		t.Fatalf("complete the land job: %s", ans)
	}
	if _, err := e.eng.Answer(e.ctx, e.project, e.task, "here is an answer"); err == nil || !strings.Contains(err.Error(), "not by a question") {
		t.Errorf("Answer after a land failure = %v, want a refusal to answer", err)
	}
}

// TestLandMalformedRecordParks: a success record with an empty commit or
// a push state outside the vocabulary parks the task cleanly instead of
// failing the landings CHECK and leaving the runner to retry forever.
func TestLandMalformedRecordParks(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	e.runToDone(author, reviewer)

	g := e.leaseGit(author)
	if g == nil {
		t.Fatal("the land job was not leased")
	}
	// A push state outside the vocabulary: the landings CHECK would refuse
	// it, so the engine parks the task instead.
	if ans := e.complete(g, Completion{Result: `{"target_branch":"main","commit":"abc123","check_passed":true,"pushed":"bogus"}`}); ans != AnsOK {
		t.Fatalf("complete the land job: %s", ans)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskNeedsForeman {
		t.Errorf("task state = %q, want needs_foreman", state)
	}
	if n := e.int("SELECT COUNT(*) FROM landings WHERE task_id = ?", e.task); n != 0 {
		t.Errorf("landing records = %d, want none for a malformed record", n)
	}
	if p, ok := e.eventPayloadOf(evTaskLandFailed); !ok || !strings.Contains(p.Reason, "push state") {
		t.Errorf("task_land_failed event = %+v (%t), want the malformed-record reason", p, ok)
	}
}
