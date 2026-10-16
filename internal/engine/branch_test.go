package engine

import (
	"fmt"
	"strings"
	"testing"
)

// branchOf is the task's first-attempt branch name (§5.1), for the
// tests below.
func branchOf(task int64) string { return fmt.Sprintf("brigada/task-%d", task) }

// TestLeaseNamesTheWorkBranch covers the engine naming each job's
// branch (§5.1, §5.6): the task's first work job takes the task's own
// name, a restart takes a name of its own after the task's restart
// count — r1, r2 — so its push from a fresh base is a fast-forward of a
// branch no earlier attempt touched, and a re-lease of the same job
// keeps the branch its first lease named.
func TestLeaseNamesTheWorkBranch(t *testing.T) {
	e := newEnv(t)
	e.submit(e.work())

	g := e.leaseAny(e.runner1)
	if g == nil {
		t.Fatal("the work job was not leased")
	}
	if g.Branch != branchOf(e.task) {
		t.Errorf("first work grant branch = %q, want %q", g.Branch, branchOf(e.task))
	}

	// The lease lapses and another runner takes the job over: the branch
	// the first lease named is the job's own, kept by every later lease.
	// The sentinel written into the column proves the later grant
	// carries the stored name rather than a recomputation — recomputed
	// here, the same restart count would name brigada/task-<id> again
	// and the test could not tell the two apart.
	kept := branchOf(e.task) + "-kept"
	e.exec("UPDATE jobs SET branch = ? WHERE id = ?", kept, g.JobID)
	e.expireLease()
	g = e.leaseAny(e.runner2)
	if g == nil {
		t.Fatal("the lapsed work job was not leased again")
	}
	if g.Branch != kept {
		t.Errorf("re-leased work grant branch = %q, want the job's stored %q", g.Branch, kept)
	}
	if b := e.str("SELECT COALESCE(branch, '') FROM jobs WHERE id = ?", g.JobID); b != kept {
		t.Errorf("job row branch = %q, want %q", b, kept)
	}

	parkNeedsHelp(t, e, g)
	if _, err := e.eng.RestartTask(e.ctx, e.project, e.task, "start over"); err != nil {
		t.Fatalf("restart the task: %v", err)
	}
	g = e.leaseAny(e.runner1)
	if g == nil {
		t.Fatal("the restarted work job was not leased")
	}
	if want := fmt.Sprintf("brigada/task-%d-r1", e.task); g.Branch != want {
		t.Errorf("restarted work grant branch = %q, want %q", g.Branch, want)
	}

	parkNeedsHelp(t, e, g)
	if _, err := e.eng.RestartTask(e.ctx, e.project, e.task, "once more"); err != nil {
		t.Fatalf("restart the task again: %v", err)
	}
	g = e.leaseAny(e.runner2)
	if g == nil {
		t.Fatal("the twice-restarted work job was not leased")
	}
	if want := fmt.Sprintf("brigada/task-%d-r2", e.task); g.Branch != want {
		t.Errorf("twice-restarted work grant branch = %q, want %q", g.Branch, want)
	}
}

// TestFixJobContinuesTheTaskBranch covers the fix round's branch (§3,
// §5.1): the round continues the task's current branch — recorded from
// the job the engine named it for when the work completed — never a
// name of its own, so the runner's worktree starts at the work already
// pushed and the round's push is a fast-forward. A review reads the
// task's branch and is granted none of its own (§7).
func TestFixJobContinuesTheTaskBranch(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")

	// The work ends with its check failed: the task's branch is recorded
	// from the leased job, and the fix round the failure queues
	// continues it.
	e.submit(e.work())
	g := e.leaseAny(author)
	if g == nil || g.Branch != branchOf(e.task) {
		t.Fatalf("work grant = %+v, want branch %q", g, branchOf(e.task))
	}
	if ans := e.complete(g, doneOn(branchOf(e.task), failCheck)); ans != AnsOK {
		t.Fatalf("complete the failed-check work: %s", ans)
	}
	if b := e.str("SELECT COALESCE(branch, '') FROM tasks WHERE id = ?", e.task); b != branchOf(e.task) {
		t.Errorf("task branch after the failed check = %q, want %q", b, branchOf(e.task))
	}

	fg := e.authorLease(author)
	if fg == nil || fg.Kind != KindFix {
		t.Fatalf("the fix round was not leased: %+v", fg)
	}
	if fg.Branch != branchOf(e.task) {
		t.Errorf("fix grant branch = %q, want the task's %q", fg.Branch, branchOf(e.task))
	}

	// The round's own done keeps the branch, and the review that follows
	// reads it.
	reviewer := e.agented("reviewer", "google", "implement", "review")
	if ans := e.complete(fg, doneOn(branchOf(e.task), passCheck)); ans != AnsOK {
		t.Fatalf("complete the fixed work: %s", ans)
	}
	rg := e.leaseAny(reviewer)
	if rg == nil || rg.Kind != KindReview {
		t.Fatalf("the review was not leased: %+v", rg)
	}
	if rg.ReviewBranch != branchOf(e.task) || rg.Branch != "" {
		t.Errorf("review grant branches = %q/%q, want the task's %q and none of its own",
			rg.ReviewBranch, rg.Branch, branchOf(e.task))
	}
}

// TestRestartedWorkRecordsTheNewBranch covers the restarted attempt's
// completion (§5.6): the task's branch names the restart's branch, so
// the review that follows reads the new attempt's work, not the first
// attempt's.
func TestRestartedWorkRecordsTheNewBranch(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")

	e.submit(e.work())
	parkNeedsHelp(t, e, e.leaseAny(author))
	if _, err := e.eng.RestartTask(e.ctx, e.project, e.task, "start over"); err != nil {
		t.Fatalf("restart the task: %v", err)
	}
	restart := fmt.Sprintf("brigada/task-%d-r1", e.task)
	g := e.leaseAny(author)
	if g == nil || g.Branch != restart {
		t.Fatalf("restarted work grant = %+v, want branch %q", g, restart)
	}
	if ans := e.complete(g, doneOn(restart, passCheck)); ans != AnsOK {
		t.Fatalf("complete the restarted work: %s", ans)
	}
	if b := e.str("SELECT COALESCE(branch, '') FROM tasks WHERE id = ?", e.task); b != restart {
		t.Errorf("task branch after the restart's done = %q, want the restart's %q", b, restart)
	}
	rg := e.leaseAny(reviewer)
	if rg == nil || rg.ReviewBranch != restart {
		t.Fatalf("review grant = %+v, want the restart's branch %q", rg, restart)
	}
}

// TestStackedBaseFollowsTheRestartBranch covers a dependent's base after
// its dependency was restarted (§5.6, §6.1): the base is the branch the
// engine recorded when the dependency finally completed — the restart's
// -r<n> name, not the first attempt's — and it is written when the
// dependent is released, with the grant carrying it to the runner.
func TestStackedBaseFollowsTheRestartBranch(t *testing.T) {
	e := newEnv(t)
	res, err := e.eng.CreatePlan(e.ctx, e.project, "restarted base", []PlanTask{
		{Name: "lib", Spec: "write the lib", Class: "answer"},
		{Name: "app", Spec: "use the lib", Class: "answer", Deps: []string{"lib"}},
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	lib, app := res.Tasks[0].TaskID, res.Tasks[1].TaskID

	g := e.leaseAny(e.runner1)
	if g == nil || g.TaskID != lib {
		t.Fatalf("lease = %+v, want lib's job", g)
	}
	if ans := e.complete(g, Completion{Result: `{"status":"needs_help","question":"which way?"}`}); ans != AnsOK {
		t.Fatalf("park lib: %s", ans)
	}
	if _, err := e.eng.RestartTask(e.ctx, e.project, lib, "start over"); err != nil {
		t.Fatalf("restart lib: %v", err)
	}
	if got := e.str("SELECT base_ref FROM tasks WHERE id = ?", app); got != "main" {
		t.Errorf("app base ref before lib is done = %q, want main", got)
	}
	g = e.leaseAny(e.runner1)
	restart := fmt.Sprintf("brigada/task-%d-r1", lib)
	if g == nil || g.Branch != restart {
		t.Fatalf("restarted lib grant = %+v, want branch %q", g, restart)
	}
	if ans := e.complete(g, done()); ans != AnsOK {
		t.Fatalf("complete the restarted lib: %s", ans)
	}
	if got := e.str("SELECT base_ref FROM tasks WHERE id = ?", app); got != restart {
		t.Errorf("app base ref = %q, want the restart's branch %q", got, restart)
	}
	g = e.leaseAny(e.runner1)
	if g == nil || g.TaskID != app || g.BaseRef != restart {
		t.Fatalf("app grant = %+v, want base %q", g, restart)
	}
}

// parkNeedsHelp completes the leased work job with a worker question,
// parking the task for the foreman (§3) so a restart may act on it.
func parkNeedsHelp(t *testing.T, e *env, g *LeaseGrant) {
	t.Helper()
	if g == nil {
		t.Fatal("no work job to park")
	}
	if ans := e.complete(g, Completion{Result: `{"status":"needs_help","question":"which way?"}`}); ans != AnsOK {
		t.Fatalf("complete the work with a question: %s", ans)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskNeedsForeman {
		t.Fatalf("task state = %q, want parked at needs_foreman", state)
	}
}

// TestWorkerNamedBranchDoesNotOverrideTheEngines covers the review's
// branch when the worker's result block names another (§5.1, §7): the
// branch is the engine's — the leased job's own, the one the runner
// pushed — and the block's name cannot redirect the review's column,
// event, prompt or grant. The block's name stands in only for a job no
// engine named a branch for.
func TestWorkerNamedBranchDoesNotOverrideTheEngines(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")

	e.submit(e.work())
	g := e.leaseAny(author)
	if g == nil {
		t.Fatal("the work job was not leased")
	}
	if ans := e.complete(g, doneOn("worker/made-this-up", passCheck)); ans != AnsOK {
		t.Fatalf("complete the work: %s", ans)
	}
	if b := e.str("SELECT COALESCE(branch, '') FROM tasks WHERE id = ?", e.task); b != branchOf(e.task) {
		t.Errorf("task branch = %q, want the engine's %q, not the block's name", b, branchOf(e.task))
	}
	if p, ok := e.eventPayloadOf(evTaskReviewing); !ok || p.Branch != branchOf(e.task) {
		t.Errorf("reviewing event = %+v (%t), want the engine's branch %q", p, ok, branchOf(e.task))
	}
	rg := e.leaseAny(reviewer)
	if rg == nil || rg.ReviewBranch != branchOf(e.task) {
		t.Fatalf("review grant = %+v, want the engine's branch %q", rg, branchOf(e.task))
	}
	if strings.Contains(rg.Prompt, "worker/made-this-up") {
		t.Errorf("the review prompt carries the worker's branch name:\n%s", rg.Prompt)
	}
}

// TestUnpushedRunsKeepTheTaskBranch covers what a completion that
// pushed nothing does to the task's branch (§6.4): the branch follows
// the work that shipped — a done run's, a check-failed run's too, the
// push precedes the check (§5.5) — and a questioning or failed run
// leaves it where the last shipped work sits. A restart that ships
// nothing must not repoint the task at its own -r<n> branch, which may
// hold no work at all.
func TestUnpushedRunsKeepTheTaskBranch(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	taskBranch := "SELECT COALESCE(branch, '') FROM tasks WHERE id = ?"

	// A first attempt that asks instead of shipping records nothing: the
	// runner pushes nothing for a needs_help run.
	e.submit(e.work())
	parkNeedsHelp(t, e, e.leaseAny(author))
	if b := e.str(taskBranch, e.task); b != "" {
		t.Fatalf("task branch after an unshipped first attempt = %q, want unset", b)
	}

	// The restart ships — under a failed check, but the push precedes
	// the check — and the task's branch becomes the restart's.
	if _, err := e.eng.RestartTask(e.ctx, e.project, e.task, "again"); err != nil {
		t.Fatalf("restart the task: %v", err)
	}
	restart := fmt.Sprintf("brigada/task-%d-r1", e.task)
	g := e.leaseAny(author)
	if g == nil || g.Branch != restart {
		t.Fatalf("restarted work grant = %+v, want branch %q", g, restart)
	}
	if ans := e.complete(g, doneOn(restart, failCheck)); ans != AnsOK {
		t.Fatalf("complete the restarted work: %s", ans)
	}
	if b := e.str(taskBranch, e.task); b != restart {
		t.Fatalf("task branch after the restart shipped = %q, want %q", b, restart)
	}

	// The fix round the failure queues asks instead of fixing, and the
	// second restart behind it fails outright: neither shipped anything,
	// and the task's branch stays with the attempt that did.
	parkNeedsHelp(t, e, e.authorLease(author))
	if _, err := e.eng.RestartTask(e.ctx, e.project, e.task, "once more"); err != nil {
		t.Fatalf("restart the task again: %v", err)
	}
	g = e.leaseAny(author)
	if g == nil || g.Branch != fmt.Sprintf("brigada/task-%d-r2", e.task) {
		t.Fatalf("twice-restarted work grant = %+v, want its own -r2 branch", g)
	}
	if ans := e.complete(g, Completion{Result: `{"status":"failed","summary":"no luck"}`}); ans != AnsOK {
		t.Fatalf("complete the failed restart: %s", ans)
	}
	if b := e.str(taskBranch, e.task); b != restart {
		t.Errorf("task branch after the failed restart = %q, want the shipped %q", b, restart)
	}
}
