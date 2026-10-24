package engine

import (
	"fmt"
	"strings"
	"testing"
)

// landGateRun drives the fixture's task to done and returns its land
// state: held at the land gate (the hold's reason), or queued (land job
// count). It is runToDone plus the rule's outcome, for the decision
// tests below.
func (e *env) landGateRun(author, reviewer int64, gateLand string) {
	e.t.Helper()
	e.set(map[string]string{SettingGateLand: gateLand})
	e.runToDone(author, reviewer)
}

// TestLandApproveReleasesTheHold: the user's approval deletes the land
// hold and re-evaluates the landing, which queues the land job.
func TestLandApproveReleasesTheHold(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	e.landGateRun(author, reviewer, GateAsk)

	if n := e.landJobs(); n != 0 {
		t.Fatalf("land jobs = %d, want none while held", n)
	}
	res, err := e.eng.LandApprove(e.ctx, e.project, e.task, false)
	if err != nil {
		t.Fatalf("LandApprove: %v", err)
	}
	if !res.Released || !res.Queued || res.JobID == 0 {
		t.Errorf("LandApprove = %+v, want released with its land job queued", res)
	}
	if n := e.int("SELECT COUNT(*) FROM land_holds WHERE task_id = ? AND state = 'held'", e.task); n != 0 {
		t.Errorf("held land_holds rows after approve = %d, want none", n)
	}
	if n := e.landJobs(); n != 1 {
		t.Errorf("land jobs after approve = %d, want 1", n)
	}
	if p, ok := e.eventPayloadOf(evTaskLandReleased); !ok || p.Status != taskDone {
		t.Errorf("task_land_released event = %+v (%t), want a done release", p, ok)
	}
}

// TestLandApproveBehindALiveJob: an approval while a live job holds the
// task's key queues nothing and says so, naming that job instead of
// reporting a land job it did not queue (§6.3).
func TestLandApproveBehindALiveJob(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	e.landGateRun(author, reviewer, GateAsk)
	live := e.submit(SubmitParams{Key: fmt.Sprintf("task:%d", e.task), ProjectID: e.project, TaskID: e.task,
		Kind: KindLand, Mode: ModeFresh, Labels: []string{"git"}})
	if live.Answer != AnsCreated {
		t.Fatalf("seed the live job: %+v", live)
	}
	res, err := e.eng.LandApprove(e.ctx, e.project, e.task, false)
	if err != nil {
		t.Fatalf("LandApprove: %v", err)
	}
	if !res.Released || res.Queued || res.JobID != live.JobID {
		t.Errorf("LandApprove = %+v, want released, nothing queued, job %d named", res, live.JobID)
	}
	if n := e.landJobs(); n != 1 {
		t.Errorf("land jobs = %d, want the live one alone", n)
	}
}

// TestLandRejectCancelsTheLanding: the user's no keeps the hold row,
// marked rejected, queues nothing, leaves the branch, and the land rule
// never re-holds the task when it is re-evaluated.
func TestLandRejectCancelsTheLanding(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	e.landGateRun(author, reviewer, GateAsk)

	res, err := e.eng.LandReject(e.ctx, e.project, e.task)
	if err != nil {
		t.Fatalf("LandReject: %v", err)
	}
	if res.Released {
		t.Errorf("LandReject = %+v, want not released", res)
	}
	if state := e.str("SELECT state FROM land_holds WHERE task_id = ?", e.task); state != "rejected" {
		t.Errorf("land_holds.state after reject = %q, want rejected", state)
	}
	if n := e.landJobs(); n != 0 {
		t.Errorf("land jobs after reject = %d, want none", n)
	}
	if p, ok := e.eventPayloadOf(evTaskLandRejected); !ok || p.Status != taskDone {
		t.Errorf("task_land_rejected event = %+v (%t), want a done rejection", p, ok)
	}
	// Re-evaluating the rule — as a dependency landing would — does not
	// re-hold a landing the user refused.
	e.maybeLand(e.task)
	if n := e.int("SELECT COUNT(*) FROM land_holds WHERE task_id = ? AND state = 'held'", e.task); n != 0 {
		t.Errorf("held land_holds rows after re-evaluation = %d, want none", n)
	}
	if n := e.landJobs(); n != 0 {
		t.Errorf("land jobs after re-evaluation = %d, want none", n)
	}
}

// TestLandApproveRefusesANonHold: a decision on a task that is not held
// at the land gate refuses.
func TestLandApproveRefusesANonHold(t *testing.T) {
	e := newEnv(t)
	if _, err := e.eng.LandApprove(e.ctx, e.project, e.task, false); err == nil || !strings.Contains(err.Error(), "not held") {
		t.Errorf("LandApprove on a task with no hold = %v, want a not-held refusal", err)
	}
}

// TestPushGateAskWaitsThenApproves: gate_push ask records the landing as
// waiting, with a needs-you entry, until `push approve` queues a push job
// — the land job reused, its grant the push phase — whose completion
// moves the record to pushed.
func TestPushGateAskWaitsThenApproves(t *testing.T) {
	e := newEnv(t)
	e.set(map[string]string{SettingGatePush: GateAsk})
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	e.runToDone(author, reviewer)

	g := e.leaseGit(author)
	if g == nil || g.Kind != KindLand {
		t.Fatalf("the land job was not leased: %+v", g)
	}
	if g.GatePush != GateAsk {
		t.Errorf("land grant gate_push = %q, want ask", g.GatePush)
	}
	if ans := e.complete(g, Completion{Result: `{"target_branch":"main","commit":"abc123","check_passed":true,"pushed":"waiting"}`}); ans != AnsOK {
		t.Fatalf("complete the land job: %s", ans)
	}
	if pushed := e.str("SELECT pushed FROM landings WHERE task_id = ?", e.task); pushed != "waiting" {
		t.Fatalf("landing pushed = %q, want waiting", pushed)
	}
	it, ok := e.needsYouTask(e.task)
	if !ok || !strings.Contains(it.Reason, "waits to push") {
		t.Fatalf("needs-you entry = %+v (%t), want a push-gate reason", it, ok)
	}

	// The user approves the push: a push job queues, the land job reused.
	res, err := e.eng.PushApprove(e.ctx, envRepo)
	if err != nil {
		t.Fatalf("PushApprove: %v", err)
	}
	if res.Released != 1 {
		t.Fatalf("PushApprove released = %d, want 1", res.Released)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'land' AND status = 'queued'", e.task); n != 1 {
		t.Fatalf("queued push jobs = %d, want 1", n)
	}
	// The waiting entry goes while the push is on its way.
	if _, ok := e.needsYouTask(e.task); ok {
		t.Errorf("needs-you entry still lists the task while its push is queued")
	}

	pg := e.leaseGit(author)
	if pg == nil || !pg.Push || pg.LandCommit != "abc123" || pg.LandTarget != "main" {
		t.Fatalf("push grant = %+v, want a push phase for main@abc123", pg)
	}
	if ans := e.complete(pg, Completion{Result: `{"target_branch":"main","commit":"abc123","pushed":"pushed"}`}); ans != AnsOK {
		t.Fatalf("complete the push job: %s", ans)
	}
	if pushed := e.str("SELECT pushed FROM landings WHERE task_id = ?", e.task); pushed != "pushed" {
		t.Errorf("landing pushed after the push = %q, want pushed", pushed)
	}
	if p, ok := e.eventPayloadOf(evTaskPushed); !ok || p.Commit != "abc123" || p.Pushed != "pushed" {
		t.Errorf("task_pushed event = %+v (%t), want the push", p, ok)
	}
}

// TestPushGateNeverRecordsThePushCmd: gate_push never records the landing
// as local with the exact push command, queues no push job, and lists
// the command in the needs-you queue: the push is the user's.
func TestPushGateNeverRecordsThePushCmd(t *testing.T) {
	e := newEnv(t)
	e.set(map[string]string{SettingGatePush: GateNever})
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	e.runToDone(author, reviewer)

	g := e.leaseGit(author)
	if g == nil {
		t.Fatal("the land job was not leased")
	}
	if g.GatePush != GateNever {
		t.Errorf("land grant gate_push = %q, want never", g.GatePush)
	}
	cmd := "git fetch origin refs/brigada/land/main && git push origin FETCH_HEAD:refs/heads/main"
	if ans := e.complete(g, Completion{Result: `{"target_branch":"main","commit":"abc123","check_passed":true,"pushed":"local","push_cmd":"` + cmd + `"}`}); ans != AnsOK {
		t.Fatalf("complete the land job: %s", ans)
	}
	if pushed := e.str("SELECT pushed FROM landings WHERE task_id = ?", e.task); pushed != "local" {
		t.Errorf("landing pushed = %q, want local", pushed)
	}
	if got := e.str("SELECT push_cmd FROM landings WHERE task_id = ?", e.task); got != cmd {
		t.Errorf("landing push_cmd = %q, want the exact push command", got)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'land'", e.task); n != 1 {
		t.Errorf("land jobs = %d, want only the squash job (no push)", n)
	}
	if p, ok := e.eventPayloadOf(evTaskLanded); !ok || p.Pushed != "local" || p.PushCmd != cmd {
		t.Errorf("task_landed event = %+v (%t), want local with the push command", p, ok)
	}
	it, ok := e.needsYouTask(e.task)
	if !ok || !strings.Contains(it.Reason, "push by hand: "+cmd) {
		t.Fatalf("needs-you entry = %+v (%t), want the exact push command", it, ok)
	}
}

// TestAfterLandFailureReportedNeverUndone: an after_land failure rides the
// landing record and the history, and changes nothing about the task's
// state or the landing.
func TestAfterLandFailureReportedNeverUndone(t *testing.T) {
	e := newEnv(t)
	e.set(map[string]string{SettingAfterLand: "go run ./cmd/post-land", SettingLandReady: ".brigada/land-ready.sh"})
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	e.runToDone(author, reviewer)

	g := e.leaseGit(author)
	if g == nil {
		t.Fatal("the land job was not leased")
	}
	if g.AfterLand != "go run ./cmd/post-land" {
		t.Errorf("land grant after_land = %q, want the command", g.AfterLand)
	}
	if g.LandReady != ".brigada/land-ready.sh" {
		t.Errorf("land grant land_ready = %q, want the command", g.LandReady)
	}
	if ans := e.complete(g, Completion{Result: `{"target_branch":"main","commit":"abc123","check_passed":true,"pushed":"pushed","after_land_error":"exit 1: boom"}`}); ans != AnsOK {
		t.Fatalf("complete the land job: %s", ans)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskDone {
		t.Errorf("task state after the after_land failure = %q, want done", state)
	}
	if err := e.str("SELECT after_land_error FROM landings WHERE task_id = ?", e.task); err != "exit 1: boom" {
		t.Errorf("landing after_land_error = %q, want the failure", err)
	}
	if p, ok := e.eventPayloadOf(evTaskLanded); !ok || p.AfterLandError != "exit 1: boom" || p.Pushed != "pushed" {
		t.Errorf("task_landed event = %+v (%t), want pushed with the after_land failure", p, ok)
	}
}

// TestPlanDoneReportNamesLandings: a plan whose tasks all finish closes
// with a done report naming what landed and what waits at a gate — a
// dependency that landed, and a dependent held at the land gate.
func TestPlanDoneReportNamesLandings(t *testing.T) {
	e := newEnv(t)
	e.set(map[string]string{SettingApprovalTiers: "heavy"})
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")

	dep := e.task // lands pushed
	child := e.addTask("{}")
	e.exec("UPDATE tasks SET tier = 'heavy', effective_tier = 'heavy' WHERE id = ?", child)
	e.exec("INSERT INTO task_deps (plan_id, task_id, depends_on_task_id) VALUES (?, ?, ?)", e.plan, child, dep)

	// The dependency lands pushed.
	e.runToDone(author, reviewer)
	g := e.leaseGit(author)
	if g == nil || g.TaskID != dep {
		t.Fatalf("the dependency's land job was not leased: %+v", g)
	}
	if ans := e.complete(g, Completion{Result: `{"target_branch":"main","commit":"abc123","check_passed":true,"pushed":"pushed"}`}); ans != AnsOK {
		t.Fatalf("complete the dependency's land: %s", ans)
	}

	// The dependent runs to done and is held at the land gate (heavy).
	e.runTaskToDone(child, author, reviewer)
	if n := e.int("SELECT COUNT(*) FROM land_holds WHERE task_id = ? AND state = 'held'", child); n != 1 {
		t.Fatalf("land holds for the child = %d, want 1", n)
	}

	p, ok := e.eventPayloadOf(evPlanDone)
	if !ok {
		t.Fatal("no plan_done event")
	}
	if !strings.Contains(p.Reason, "landed:") || !strings.Contains(p.Reason, "abc123") {
		t.Errorf("plan_done report = %q, want it to name what landed", p.Reason)
	}
	if !strings.Contains(p.Reason, "waits at a gate:") || !strings.Contains(p.Reason, "heavy") {
		t.Errorf("plan_done report = %q, want it to name what waits at a gate", p.Reason)
	}
}
