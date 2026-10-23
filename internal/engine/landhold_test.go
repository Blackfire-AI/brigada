package engine

import (
	"database/sql"
	"strings"
	"testing"
)

// TestRecordLandedClearsHoldBesideLanding: a held row left beside an
// existing landing record is cleared by the next trailer detection, and
// its needs-you line goes with it.
func TestRecordLandedClearsHoldBesideLanding(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	e.landGateRun(author, reviewer, GateAsk)
	e.exec("INSERT INTO landings (task_id, project_id, target_branch, commit_sha, pushed, source, created_at) VALUES (?, ?, 'main', 'abc123', 'pushed', 'trailer', ?)",
		e.task, e.project, fmtTime(e.clock.Now()))

	if err := e.eng.RecordLanded(e.ctx, envRepo, "main",
		[]LandedCommit{{TaskID: e.task, Commit: "abc123"}}); err != nil {
		t.Fatal(err)
	}

	if n := e.int("SELECT COUNT(*) FROM land_holds WHERE task_id = ?", e.task); n != 0 {
		t.Errorf("land_holds rows beside a landing record = %d, want none", n)
	}
	if it, ok := e.needsYouTask(e.task); ok {
		t.Errorf("needs-you entry = %+v, want none", it)
	}
}

// TestRecordLandedClearsHeldHold: a task held at the land gate whose
// landing the trailer detection then records loses its hold, so no
// needs-you line "held by rule" remains for a task already on the branch.
func TestRecordLandedClearsHeldHold(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	e.landGateRun(author, reviewer, GateAsk)
	if it, ok := e.needsYouTask(e.task); !ok || !strings.Contains(it.Reason, "held by rule") {
		t.Fatalf("before the detection: needs-you = %+v, ok %v, want a held-by-rule line", it, ok)
	}

	if err := e.eng.RecordLanded(e.ctx, envRepo, "main",
		[]LandedCommit{{TaskID: e.task, Commit: "abc123"}}); err != nil {
		t.Fatal(err)
	}

	if n := e.int("SELECT COUNT(*) FROM land_holds WHERE task_id = ?", e.task); n != 0 {
		t.Errorf("land_holds rows after the trailer detection = %d, want none", n)
	}
	if it, ok := e.needsYouTask(e.task); ok {
		t.Errorf("needs-you entry = %+v, want none for a task landed by trailer", it)
	}
}

// TestLandJobClearsHeldHold: a land job queued for a task held at the land
// gate lands it; the landing clears the hold, so no needs-you line remains.
func TestLandJobClearsHeldHold(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	e.landGateRun(author, reviewer, GateAsk)
	if n := e.int("SELECT COUNT(*) FROM land_holds WHERE task_id = ? AND state = 'held'", e.task); n != 1 {
		t.Fatalf("land holds before the land job = %d, want 1", n)
	}

	if err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
		_, err := e.eng.queueLandJobTx(tx, e.project, e.task, e.clock.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	g := e.leaseGit(author)
	if g == nil || g.TaskID != e.task {
		t.Fatalf("the task's land job was not leased: %+v", g)
	}
	if ans := e.complete(g, Completion{Result: `{"target_branch":"main","commit":"abc123","check_passed":true,"pushed":"pushed"}`}); ans != AnsOK {
		t.Fatalf("complete the land job: %s", ans)
	}

	if n := e.int("SELECT COUNT(*) FROM landings WHERE task_id = ?", e.task); n != 1 {
		t.Fatalf("landings rows = %d, want 1", n)
	}
	if n := e.int("SELECT COUNT(*) FROM land_holds WHERE task_id = ?", e.task); n != 0 {
		t.Errorf("land_holds rows after the land job landed it = %d, want none", n)
	}
	if it, ok := e.needsYouTask(e.task); ok {
		t.Errorf("needs-you entry = %+v, want none for a landed task", it)
	}
}

// TestLandedTaskStaleHoldNotListed: a held row that outlives its task's
// landing record never shows in the needs-you queue.
func TestLandedTaskStaleHoldNotListed(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	e.landGateRun(author, reviewer, GateAsk)
	if _, ok := e.needsYouTask(e.task); !ok {
		t.Fatal("held task missing from needs-you before landing")
	}
	e.exec("INSERT INTO landings (task_id, project_id, target_branch, commit_sha, pushed, source, created_at) VALUES (?, ?, 'main', 'abc123', 'pushed', 'trailer', ?)",
		e.task, e.project, fmtTime(e.clock.Now()))
	if n := e.int("SELECT COUNT(*) FROM land_holds WHERE task_id = ? AND state = 'held'", e.task); n != 1 {
		t.Fatalf("held rows = %d, want the stale row kept", n)
	}
	if it, ok := e.needsYouTask(e.task); ok {
		t.Errorf("needs-you entry = %+v, want none for a landed task", it)
	}
}
