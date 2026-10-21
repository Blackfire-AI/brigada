package engine

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
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
	if reason := e.str("SELECT reason FROM land_holds WHERE task_id = ?", e.task); !strings.Contains(reason, "gate_land is ask") {
		t.Errorf("land hold reason = %q, want the gate_land rule", reason)
	}
	it, ok := e.needsYouTask(e.task)
	if !ok || !strings.Contains(it.Reason, "waits to land") || !strings.Contains(it.Reason, "gate_land is ask") {
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
	e.exec("UPDATE tasks SET tier = 'heavy', effective_tier = 'heavy' WHERE id = ?", e.task)
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
	e.exec("UPDATE tasks SET tier = 'heavy', effective_tier = 'heavy' WHERE id = ?", a)
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
