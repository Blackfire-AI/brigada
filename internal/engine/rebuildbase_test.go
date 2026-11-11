package engine

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// The rebuild base rule tests (§6.1, §7.1): a restart, and the automatic
// retry, choose the dependent's base by how the dependency it stacks on
// ended — the land target's tip when the dependency's work is on it (b)
// or its chain was abandoned (c), its current result branch otherwise
// (a, and every other state).

// rebuildChain runs a lib → app plan until lib is done from its round-0
// branch and app has started on it, then parks app at needs_foreman with
// its worker's question, ready for the foreman's restart. lib's land job
// stays queued.
func rebuildChain(t *testing.T, e *env, author, reviewer int64) (lib, app int64) {
	t.Helper()
	res, err := e.eng.CreatePlan(e.ctx, e.project, "the chain", []PlanTask{
		{Name: "lib", Title: "the lib", Spec: "write the lib"},
		{Name: "app", Title: "the app", Spec: "use the lib", Deps: []string{"lib"}},
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	lib, app = res.Tasks[0].TaskID, res.Tasks[1].TaskID
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
	if ans := e.complete(g, Completion{Result: `{"status":"needs_help","question":"which way?"}`}); ans != AnsOK {
		t.Fatalf("park app: %s", ans)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", app); state != taskNeedsForeman {
		t.Fatalf("app state = %q, want needs_foreman", state)
	}
	return lib, app
}

// restartOnto restarts app and checks the base its restart records and
// its fresh grant carries.
func restartOnto(t *testing.T, e *env, author, app int64, want string) {
	t.Helper()
	if _, err := e.eng.RestartTask(e.ctx, e.project, app, "rebuild"); err != nil {
		t.Fatalf("restart app: %v", err)
	}
	if got := e.str("SELECT base_ref FROM tasks WHERE id = ?", app); got != want {
		t.Errorf("app base_ref after the restart = %q, want %q", got, want)
	}
	g := e.leaseAny(author)
	if g == nil || g.TaskID != app {
		t.Fatalf("lease = %+v, want app's restarted work", g)
	}
	if g.BaseRef != want {
		t.Errorf("app's restarted grant base = %q, want %q", g.BaseRef, want)
	}
}

// TestRestartOfLandedDependencysDependentBuildsOnTheLandTarget: case (b)
// — a dependency done with a landing record gives the land target's tip:
// the target branch for a pushed landing, the waiting ref for one that
// waits at the push gate or sits local under never.
func TestRestartOfLandedDependencysDependentBuildsOnTheLandTarget(t *testing.T) {
	for _, c := range []struct {
		name, gate, rec, want string
	}{
		{"pushed", GateAuto, `{"target_branch":"main","commit":"lib-1","check_passed":true,"pushed":"pushed"}`, "main"},
		{"waiting", GateAsk, `{"target_branch":"main","commit":"lib-1","check_passed":true,"pushed":"waiting"}`, "refs/brigada/land/main"},
		{"local", GateNever, `{"target_branch":"main","commit":"lib-1","check_passed":true,"pushed":"local","push_cmd":"git fetch origin refs/brigada/land/main && git push origin FETCH_HEAD:refs/heads/main"}`, "refs/brigada/land/main"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.set(map[string]string{SettingGatePush: c.gate})
			author := e.agented("author", "anthropic", "implement")
			reviewer := e.agented("reviewer", "google", "implement", "review")
			lib, app := rebuildChain(t, e, author, reviewer)
			e.completeLandOf(lib, author, c.rec)
			if pushed := e.str("SELECT pushed FROM landings WHERE task_id = ?", lib); pushed != c.name {
				t.Fatalf("lib landing = %q, want %q", pushed, c.name)
			}
			restartOnto(t, e, author, app, c.want)
		})
	}
}

// TestRestartOfAbandonedChainBuildsOnTheLandTarget: case (c) — a
// cancelled or retired dependency's result branch is never a base; the
// dependent rebuilds on the project's land target, land_target when set.
func TestRestartOfAbandonedChainBuildsOnTheLandTarget(t *testing.T) {
	for _, state := range []string{taskCancelled, taskRetired} {
		t.Run(state, func(t *testing.T) {
			e := newEnv(t)
			e.set(map[string]string{SettingLandTarget: "integration"})
			author := e.agented("author", "anthropic", "implement")
			reviewer := e.agented("reviewer", "google", "implement", "review")
			lib, app := rebuildChain(t, e, author, reviewer)
			e.exec("UPDATE tasks SET state = ? WHERE id = ?", state, lib)
			restartOnto(t, e, author, app, "integration")
		})
	}
}

// TestRestartOfRebuiltDependencysDependentBuildsOnItsNewRound: case (a)
// — a dependency restarted and done again with no landing gives its new
// -r<n> branch.
func TestRestartOfRebuiltDependencysDependentBuildsOnItsNewRound(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	lib, app := rebuildChain(t, e, author, reviewer)
	lg := e.leaseGit(author)
	if lg == nil || lg.TaskID != lib || lg.Kind != KindLand {
		t.Fatalf("lease = %+v, want lib's land job", lg)
	}
	if ans := e.complete(lg, Completion{Result: `{"target_branch":"main","commit":"","check_passed":false,"conflict_files":["a.go"]}`}); ans != AnsOK {
		t.Fatalf("fail lib's landing: %s", ans)
	}
	if _, err := e.eng.RestartTask(e.ctx, e.project, lib, "rebase onto the new head"); err != nil {
		t.Fatalf("restart lib: %v", err)
	}
	r1 := fmt.Sprintf("brigada/task-%d-r1", lib)
	g := e.leaseAny(author)
	if g == nil || g.TaskID != lib || g.Branch != r1 {
		t.Fatalf("lease = %+v, want lib's restarted work on %s", g, r1)
	}
	if ans := e.complete(g, doneOn(r1, passCheck)); ans != AnsOK {
		t.Fatalf("complete the restarted lib: %s", ans)
	}
	e.approveReview(reviewer)
	if n := e.int("SELECT COUNT(*) FROM landings WHERE task_id = ?", lib); n != 0 {
		t.Fatalf("lib landings = %d, want none: case (a)", n)
	}
	p, ok := e.taskEvent(app, evTaskBaseStale)
	if !ok || p.Branch != r1 || !strings.Contains(p.Reason, "now on "+r1) {
		t.Errorf("app's task_base_stale = %+v (%t), want the dep_rebuilt sweep's wording naming %s", p, ok, r1)
	}
	restartOnto(t, e, author, app, r1)
}

// TestRestartOfFailedDependencysDependentBuildsOnItsCurrentBranch: a
// dependency in none of the rule's ends — failed here — is no end of the
// chain, and the restart builds on its current result branch as before.
func TestRestartOfFailedDependencysDependentBuildsOnItsCurrentBranch(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	lib, app := rebuildChain(t, e, author, reviewer)
	r1 := fmt.Sprintf("brigada/task-%d-r1", lib)
	e.exec("UPDATE tasks SET state = ?, branch = ? WHERE id = ?", taskFailed, r1, lib)
	restartOnto(t, e, author, app, r1)
}

// TestAutomaticRetryOfLandedDependencysDependentBuildsOnTheLandTarget:
// the automatic retry goes through the restart's own queueing, so a
// dependent whose dependency landed retries on the land target's tip.
func TestAutomaticRetryOfLandedDependencysDependentBuildsOnTheLandTarget(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	e.agented("other", "openai", "implement")
	reviewer := e.agented("reviewer", "google", "review")
	res, err := e.eng.CreatePlan(e.ctx, e.project, "the chain", []PlanTask{
		{Name: "lib", Title: "the lib", Spec: "write the lib"},
		{Name: "app", Title: "the app", Spec: "use the lib", Deps: []string{"lib"}},
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	lib, app := res.Tasks[0].TaskID, res.Tasks[1].TaskID
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
	e.completeLandOf(lib, author, `{"target_branch":"main","commit":"lib-1","check_passed":true,"pushed":"pushed"}`)

	// app's worker fails transiently: the automatic retry queues a fresh
	// attempt on another agent, on the land target's tip.
	if ans := e.complete(g, Completion{Error: "worker CLI failed: temporarily unavailable", ErrorKind: "other"}); ans != AnsOK {
		t.Fatalf("fail app's work: %s", ans)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'work' AND restart = 1", app); n != 1 {
		t.Fatalf("app retry jobs = %d, want the automatic retry", n)
	}
	if got := e.str("SELECT base_ref FROM tasks WHERE id = ?", app); got != "main" {
		t.Errorf("app base_ref after the retry = %q, want the land target main", got)
	}
}

// TestBaseOnTheLandTargetIsNeverStale: a dependent rebuilt onto the land
// target, or onto its waiting ref, names no round of the dependency, so
// the stale-base rule never holds it, whatever branch the dependency is
// on now.
func TestBaseOnTheLandTargetIsNeverStale(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	lib, app := rebuildChain(t, e, author, reviewer)
	e.exec("UPDATE tasks SET branch = ? WHERE id = ?", fmt.Sprintf("brigada/task-%d-r1", lib), lib)
	for _, base := range []string{"main", "refs/brigada/land/main"} {
		e.exec("UPDATE tasks SET base_ref = ? WHERE id = ?", base, app)
		var stale bool
		if err := e.st.View(e.ctx, func(tx *sql.Tx) error {
			var err error
			_, _, _, stale, err = staleBaseTx(tx, app)
			return err
		}); err != nil {
			t.Fatalf("staleBaseTx: %v", err)
		}
		if stale {
			t.Errorf("base %q read as stale, want never", base)
		}
	}
}
