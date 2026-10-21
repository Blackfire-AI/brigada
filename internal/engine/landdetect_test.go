package engine

import (
	"database/sql"
	"strconv"
	"strings"
	"testing"
)

// TestRecordLandedMarksTrailerTasks: a task whose Brigada-Task trailer is
// reported on the project's default branch gets a landing record with the
// commit, source 'trailer' and pushed, and a repeat report is a no-op.
func TestRecordLandedMarksTrailerTasks(t *testing.T) {
	e := newEnv(t)

	err := e.eng.RecordLanded(e.ctx, "https://example.invalid/brigada.git", "main",
		[]LandedCommit{{TaskID: e.task, Commit: "abc123"}})
	if err != nil {
		t.Fatal(err)
	}
	if n := e.int("SELECT COUNT(*) FROM landings WHERE task_id = ?", e.task); n != 1 {
		t.Fatalf("landings rows = %d, want 1", n)
	}
	if got := e.str("SELECT commit_sha FROM landings WHERE task_id = ?", e.task); got != "abc123" {
		t.Errorf("commit = %q, want abc123", got)
	}
	if got := e.str("SELECT pushed FROM landings WHERE task_id = ?", e.task); got != "pushed" {
		t.Errorf("pushed = %q, want pushed", got)
	}
	if got := e.str("SELECT source FROM landings WHERE task_id = ?", e.task); got != "trailer" {
		t.Errorf("source = %q, want trailer", got)
	}
	if got := e.str("SELECT target_branch FROM landings WHERE task_id = ?", e.task); got != "main" {
		t.Errorf("target_branch = %q, want main", got)
	}

	// A repeat detection is idempotent: no second row, no error.
	if err := e.eng.RecordLanded(e.ctx, "https://example.invalid/brigada.git", "main",
		[]LandedCommit{{TaskID: e.task, Commit: "abc123"}}); err != nil {
		t.Fatal(err)
	}
	if n := e.int("SELECT COUNT(*) FROM landings WHERE task_id = ?", e.task); n != 1 {
		t.Errorf("landings rows after a repeat = %d, want 1", n)
	}
}

// TestRecordLandedIgnoresOtherProject: a trailer naming another project's
// task id is not recorded — the engine matches the task to the project
// the repo URL names.
func TestRecordLandedIgnoresOtherProject(t *testing.T) {
	e := newEnv(t)
	other := e.addProject("https://example.invalid/other.git")
	otherPlan := e.addPlanState(other, "the other plan", "active", e.clock.Now())
	otherTask := e.addTaskToPlan(otherPlan, "another task", "ready")

	err := e.eng.RecordLanded(e.ctx, "https://example.invalid/brigada.git", "main",
		[]LandedCommit{{TaskID: otherTask, Commit: "abc123"}})
	if err != nil {
		t.Fatal(err)
	}
	if n := e.int("SELECT COUNT(*) FROM landings WHERE task_id = ?", otherTask); n != 0 {
		t.Errorf("landings rows for the other project's task = %d, want 0", n)
	}
	// A no-such task is ignored too.
	if err := e.eng.RecordLanded(e.ctx, "https://example.invalid/brigada.git", "main",
		[]LandedCommit{{TaskID: 999999, Commit: "abc123"}}); err != nil {
		t.Fatal(err)
	}
	if n := e.int("SELECT COUNT(*) FROM landings WHERE task_id = 999999"); n != 0 {
		t.Errorf("landings rows for a missing task = %d, want 0", n)
	}
}

// TestRecordLandedShownInStatus: a task with a landing record is marked
// landed in the status's plan listing.
func TestRecordLandedShownInStatus(t *testing.T) {
	e := newEnv(t)
	if err := e.eng.RecordLanded(e.ctx, "https://example.invalid/brigada.git", "main",
		[]LandedCommit{{TaskID: e.task, Commit: "abc123"}}); err != nil {
		t.Fatal(err)
	}

	st, err := e.eng.Status(e.ctx, e.project)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Plans) != 1 || len(st.Plans[0].Tasks) != 1 {
		t.Fatalf("status plans/tasks = %d/%d, want 1/1", len(st.Plans), len(st.Plans[0].Tasks))
	}
	if !st.Plans[0].Tasks[0].Landed {
		t.Errorf("the task is not marked landed in status: %+v", st.Plans[0].Tasks[0])
	}
}

// TestPlanDoneReportLeavesLandedOutOfWaiting: a done plan whose tasks are
// landed and one is still waiting names the landed one in "landed" and
// the waiting one in "not yet landed", never the other way around.
func TestPlanDoneReportLeavesLandedOutOfWaiting(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")

	first := e.task
	second := e.addTask("{}")

	// The first task lands: its land job completes pushed.
	e.runToDone(author, reviewer)
	g := e.leaseGit(author)
	if g == nil || g.TaskID != first {
		t.Fatalf("the first task's land job was not leased: %+v", g)
	}
	if ans := e.complete(g, Completion{Result: `{"target_branch":"main","commit":"abc123","check_passed":true,"pushed":"pushed"}`}); ans != AnsOK {
		t.Fatalf("complete the first task's land: %s", ans)
	}

	// The second task reaches done and approved; its land job is queued,
	// so the plan stays open until the landing has an outcome (§7.1). To
	// see the "not yet landed" list — a done, approved task with no
	// landing record and no hold — retire the queued job without a
	// landing and close the plan directly.
	e.runTaskToDone(second, author, reviewer)
	e.exec("UPDATE jobs SET status = 'done' WHERE task_id = ? AND kind = 'land'", second)
	if err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
		_, err := e.eng.finishPlanIfClosedTx(tx, e.project, e.plan, 0, 0, e.clock.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}

	p, ok := e.eventPayloadOf(evPlanDone)
	if !ok {
		t.Fatal("no plan_done event")
	}
	if !strings.Contains(p.Reason, "landed: task "+strconv.FormatInt(first, 10)) {
		t.Errorf("plan_done report = %q, want it to name task %d as landed", p.Reason, first)
	}
	wantWait := "not yet landed: task " + strconv.FormatInt(second, 10)
	if !strings.Contains(p.Reason, wantWait) {
		t.Errorf("plan_done report = %q, want it to name task %d as not yet landed", p.Reason, second)
	}
	if strings.Contains(p.Reason, "not yet landed: task "+strconv.FormatInt(first, 10)) {
		t.Errorf("plan_done report = %q, the landed task %d must be left out of the waiting list", p.Reason, first)
	}
}

// TestRecordLandedChainNoLandJobs: a chain whose head and tail are both
// landed by hand — each trailer on the default branch, reported in one
// detection — records both landings and queues no land job for either:
// the tail's landing row is written before the head's landing releases
// it, so the rule never hands a land job to a task already on the
// branch.
func TestRecordLandedChainNoLandJobs(t *testing.T) {
	e := newEnv(t)
	e.set(map[string]string{SettingApprovalTiers: "heavy"})
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")

	head := e.task
	tail := e.addTask("{}")
	e.exec("UPDATE tasks SET effective_tier = 'heavy' WHERE id = ?", head)
	e.exec("UPDATE tasks SET state = 'pending' WHERE id = ?", tail)
	e.exec("INSERT INTO task_deps (plan_id, task_id, depends_on_task_id) VALUES (?, ?, ?)", e.plan, tail, head)

	// The head finishes and is held at the land gate — no land job. The
	// head's done releases the tail's work, which runs to done; its
	// dependency has no landing, so it queues no land job either.
	e.runToDone(author, reviewer)
	if n := e.int("SELECT COUNT(*) FROM land_holds WHERE task_id = ? AND state = 'held'", head); n != 1 {
		t.Fatalf("head land holds = %d, want 1", n)
	}
	g := e.leaseAny(author)
	if g == nil {
		t.Fatal("the tail's work job was not leased")
	}
	if ans := e.complete(g, doneOn("task/1-1", passCheck)); ans != AnsOK {
		t.Fatalf("complete the tail's work: %s", ans)
	}
	e.runReview(reviewer, `{"verdict":"approve","findings":"`+substantiveFindings+`"}`)
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'land'", tail); n != 0 {
		t.Fatalf("tail land jobs before the detection = %d, want none", n)
	}

	// Both landed by hand: the default branch carries both trailers.
	if err := e.eng.RecordLanded(e.ctx, envRepo, "main",
		[]LandedCommit{{TaskID: head, Commit: "h1"}, {TaskID: tail, Commit: "t1"}}); err != nil {
		t.Fatal(err)
	}

	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'land'", head); n != 0 {
		t.Errorf("head land jobs = %d, want none", n)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'land'", tail); n != 0 {
		t.Errorf("tail land jobs = %d, want none", n)
	}
	if n := e.int("SELECT COUNT(*) FROM landings WHERE task_id IN (?, ?)", head, tail); n != 2 {
		t.Errorf("landing rows = %d, want 2 (head and tail by trailer)", n)
	}
}

// TestRecordLandedRejectedHoldStaysRejected: a task held at the land
// gate and rejected is later found on the default branch by its trailer:
// the landing is recorded, the hold stays rejected, and no needs-you
// line is raised — the user's no already decided the landing.
func TestRecordLandedRejectedHoldStaysRejected(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	e.landGateRun(author, reviewer, GateAsk)

	res, err := e.eng.LandReject(e.ctx, e.project, e.task)
	if err != nil {
		t.Fatalf("LandReject: %v", err)
	}
	if res.Released {
		t.Fatalf("LandReject = %+v, want not released", res)
	}

	if err := e.eng.RecordLanded(e.ctx, envRepo, "main",
		[]LandedCommit{{TaskID: e.task, Commit: "abc123"}}); err != nil {
		t.Fatal(err)
	}

	if n := e.int("SELECT COUNT(*) FROM landings WHERE task_id = ? AND source = 'trailer'", e.task); n != 1 {
		t.Errorf("landing rows by trailer = %d, want 1", n)
	}
	if state := e.str("SELECT state FROM land_holds WHERE task_id = ?", e.task); state != "rejected" {
		t.Errorf("land_holds.state after the detection = %q, want rejected", state)
	}
	if it, ok := e.needsYouTask(e.task); ok {
		t.Errorf("needs-you entry = %+v, want none for a rejected landing recorded by trailer", it)
	}
}

// TestLandApproveTrailerLandedQueuesNothing: a task held at the land
// gate that the trailer detection then finds on the default branch is
// already landed; the user's approval releases the hold but queues no
// land job for a task whose landing is recorded.
func TestLandApproveTrailerLandedQueuesNothing(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	e.landGateRun(author, reviewer, GateAsk)

	if err := e.eng.RecordLanded(e.ctx, envRepo, "main",
		[]LandedCommit{{TaskID: e.task, Commit: "abc123"}}); err != nil {
		t.Fatal(err)
	}

	res, err := e.eng.LandApprove(e.ctx, e.project, e.task)
	if err != nil {
		t.Fatalf("LandApprove: %v", err)
	}
	if !res.Released {
		t.Errorf("LandApprove = %+v, want released", res)
	}
	if n := e.landJobs(); n != 0 {
		t.Errorf("land jobs after approve = %d, want none for a task already landed by trailer", n)
	}
	if n := e.int("SELECT COUNT(*) FROM land_holds WHERE task_id = ?", e.task); n != 0 {
		t.Errorf("land_holds rows after approve = %d, want none", n)
	}
}
