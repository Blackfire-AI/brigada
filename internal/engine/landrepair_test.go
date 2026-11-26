package engine

import (
	"testing"
)

// seedTrailerLanding inserts a landings row of source 'trailer' with the
// given trailer columns; an empty branch stores NULLs, the shape of a
// record written before the trailer was stored.
func (e *env) seedTrailerLanding(task int64, commit, branch, head string) {
	e.t.Helper()
	if branch == "" {
		e.exec(`INSERT INTO landings (task_id, project_id, target_branch, commit_sha, pushed, source, created_at)
			VALUES (?, ?, 'main', ?, 'pushed', 'trailer', ?)`,
			task, e.project, commit, fmtTime(e.clock.Now()))
		return
	}
	e.exec(`INSERT INTO landings (task_id, project_id, target_branch, commit_sha, pushed, source, trailer_branch, trailer_head, created_at)
		VALUES (?, ?, 'main', ?, 'pushed', 'trailer', ?, ?, ?)`,
		task, e.project, commit, branch, head, fmtTime(e.clock.Now()))
}

// TestRepairDropsFalseTrailerLanding: the wedge's shape — a done,
// approved task recorded landed at an unrelated old commit whose trailer
// names another store's branch and head. The repair pass drops the
// record, leaves the task done, re-runs the land rule so the real land
// job queues, and writes one event naming the dropped commit and the
// reason. A second run repairs nothing.
func TestRepairDropsFalseTrailerLanding(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	e.runToDone(author, reviewer)
	e.trailerOK(e.task)
	// The land job the rule queued has served the test; retire it so the
	// repair's re-run of the rule queues a fresh one.
	e.exec("UPDATE jobs SET status = 'done' WHERE task_id = ? AND kind = 'land'", e.task)
	e.seedTrailerLanding(e.task, "a7b61ba4", "brigada/task-4", "0e31d17aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")

	repaired, err := e.eng.RepairFalseTrailerLandings(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(repaired) != 1 || repaired[0] != e.task {
		t.Fatalf("repaired = %v, want [%d]", repaired, e.task)
	}
	if n := e.int("SELECT COUNT(*) FROM landings WHERE task_id = ?", e.task); n != 0 {
		t.Errorf("landings rows = %d, want the false record dropped", n)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskDone {
		t.Errorf("task state = %q, want done: the repair never moves the task", state)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'land' AND status = 'queued'", e.task); n != 1 {
		t.Errorf("queued land jobs = %d, want the task's real landing re-queued", n)
	}
	p, ok := e.eventPayloadOf(evTaskLandingRepaired)
	if !ok {
		t.Fatal("no task_landing_repaired event")
	}
	if p.Commit != "a7b61ba4" {
		t.Errorf("event commit = %q, want the dropped a7b61ba4", p.Commit)
	}
	if p.Reason == "" {
		t.Error("the event names no reason")
	}

	// Idempotent: a second run repairs nothing.
	repaired, err = e.eng.RepairFalseTrailerLandings(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(repaired) != 0 {
		t.Errorf("second run repaired = %v, want none", repaired)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'land' AND status = 'queued'", e.task); n != 1 {
		t.Errorf("queued land jobs after the second run = %d, want the same one", n)
	}
}

// TestRepairDropsUnvalidatableTrailerLanding: a record from before the
// trailer columns existed — its trailer_branch and trailer_head NULL —
// cannot be validated, so the repair pass drops it and the task's land
// rule re-runs.
func TestRepairDropsUnvalidatableTrailerLanding(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	e.runToDone(author, reviewer)
	e.trailerOK(e.task)
	e.exec("UPDATE jobs SET status = 'done' WHERE task_id = ? AND kind = 'land'", e.task)
	e.seedTrailerLanding(e.task, "0ldrec0rd", "", "")

	repaired, err := e.eng.RepairFalseTrailerLandings(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(repaired) != 1 || repaired[0] != e.task {
		t.Fatalf("repaired = %v, want [%d]", repaired, e.task)
	}
	if n := e.int("SELECT COUNT(*) FROM landings WHERE task_id = ?", e.task); n != 0 {
		t.Errorf("landings rows = %d, want the NULL-trailer record dropped", n)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'land' AND status = 'queued'", e.task); n != 1 {
		t.Errorf("queued land jobs = %d, want the task's real landing re-queued", n)
	}
	p, ok := e.eventPayloadOf(evTaskLandingRepaired)
	if !ok {
		t.Fatal("no task_landing_repaired event")
	}
	if p.Commit != "0ldrec0rd" {
		t.Errorf("event commit = %q, want the dropped 0ldrec0rd", p.Commit)
	}
}

// TestRepairKeepsGenuineTrailerLanding: a landing the validated scan
// recorded — its stored trailer naming the task's own branch and
// approved head — survives the repair pass untouched, with no event.
func TestRepairKeepsGenuineTrailerLanding(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	e.runToDone(author, reviewer)
	e.trailerOK(e.task)
	// A hand landing recorded through the scan path itself.
	if err := e.eng.RecordLanded(e.ctx, envRepo, "main",
		[]LandedCommit{e.trailerFor(e.task, "8e37d19e")}); err != nil {
		t.Fatal(err)
	}

	repaired, err := e.eng.RepairFalseTrailerLandings(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(repaired) != 0 {
		t.Errorf("repaired = %v, want none", repaired)
	}
	if got := e.str("SELECT commit_sha FROM landings WHERE task_id = ?", e.task); got != "8e37d19e" {
		t.Errorf("commit = %q, want the genuine record kept", got)
	}
	if _, ok := e.eventPayloadOf(evTaskLandingRepaired); ok {
		t.Error("a task_landing_repaired event was written for a genuine record")
	}
}

// TestRepairLeavesJobLandingsAlone: a landing the land job recorded —
// source 'job', no trailer stored — is not the scan's business: the
// repair pass reads only trailer rows and keeps it.
func TestRepairLeavesJobLandingsAlone(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	e.runToDone(author, reviewer)
	g := e.leaseGit(author)
	if g == nil {
		t.Fatal("the land job was not leased")
	}
	if ans := e.complete(g, Completion{Result: `{"target_branch":"main","commit":"abc123","check_passed":true,"pushed":"pushed"}`}); ans != AnsOK {
		t.Fatalf("complete the land job: %s", ans)
	}

	repaired, err := e.eng.RepairFalseTrailerLandings(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(repaired) != 0 {
		t.Errorf("repaired = %v, want none", repaired)
	}
	if n := e.int("SELECT COUNT(*) FROM landings WHERE task_id = ? AND source = 'job'", e.task); n != 1 {
		t.Errorf("job landing rows = %d, want the record kept", n)
	}
}

// TestRepairReopensPlanClosedOverFalseLanding: a plan that closed done
// over a false trailer landing reopens when the repair pass runs before
// the reopen pass, as it does at engine start-up (§7.1, §13): dropping
// the record leaves the done task as live work again, and
// ReopenStrandedPlans returns the plan to active.
func TestRepairReopensPlanClosedOverFalseLanding(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	e.runToDone(author, reviewer)
	e.trailerOK(e.task)
	e.exec("UPDATE jobs SET status = 'done' WHERE task_id = ? AND kind = 'land'", e.task)
	e.seedTrailerLanding(e.task, "a7b61ba4", "brigada/task-4", "0e31d17aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	// The plan closed done over the false record.
	e.exec("UPDATE plans SET state = 'done' WHERE id = ?", e.plan)

	if _, err := e.eng.RepairFalseTrailerLandings(e.ctx); err != nil {
		t.Fatal(err)
	}
	reopened, err := e.eng.ReopenStrandedPlans(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(reopened) != 1 || reopened[0] != e.plan {
		t.Fatalf("reopened = %v, want [%d]", reopened, e.plan)
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", e.plan); got != "active" {
		t.Errorf("plan state = %q, want active", got)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'land' AND status = 'queued'", e.task); n != 1 {
		t.Errorf("queued land jobs = %d, want the task's real landing re-queued", n)
	}
}
