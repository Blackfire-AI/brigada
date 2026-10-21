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
