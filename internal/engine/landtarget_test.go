package engine

import (
	"fmt"
	"strings"
	"testing"
)

// landTargetEnv is an env whose project lands on a staging branch
// (§6.3): the land_target the tests below build on.
func landTargetEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	e.set(map[string]string{SettingLandTarget: "brigada/landed"})
	return e
}

// seededDep adds a task to the seeded plan — another plan than any the
// tests create — in state, with a landing on target pushed as push when
// push is not empty: the shape of a landed task of another plan, whole
// or in part.
func seededDep(e *env, state, target, push string) int64 {
	e.t.Helper()
	e.exec(`INSERT INTO tasks (plan_id, title, spec, class, state, base_ref, caps, created_at, updated_at)
		VALUES (?, 'landed elsewhere', 'the earlier work', 'implement', ?, 'main', '{}', ?, ?)`,
		e.plan, state, seedTS, seedTS)
	id := e.int("SELECT MAX(id) FROM tasks")
	if push != "" {
		e.exec(`INSERT INTO landings (task_id, project_id, target_branch, commit_sha, pushed, created_at)
			VALUES (?, ?, ?, 'abc123', ?, ?)`, id, e.project, target, push, seedTS)
	}
	return id
}

// TestLandTargetBasesNewTasks: with land_target set, a plan's tasks are
// created on it — the row and the grant both carry it — and a task
// stacked on a dependency still moves to that dependency's branch when
// its work is queued (§6.1).
func TestLandTargetBasesNewTasks(t *testing.T) {
	e := landTargetEnv(t)
	res, err := e.eng.CreatePlan(e.ctx, e.project, "on the target", []PlanTask{
		{Name: "root", Spec: "one", Class: "answer"},
		{Name: "later", Spec: "two", Class: "answer", Deps: []string{"root"}},
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	root, later := res.Tasks[0].TaskID, res.Tasks[1].TaskID
	for _, id := range []int64{root, later} {
		if got := e.str("SELECT base_ref FROM tasks WHERE id = ?", id); got != "brigada/landed" {
			t.Errorf("task %d base ref = %q, want the land target brigada/landed", id, got)
		}
	}
	g := e.leaseAny(e.runner1)
	if g == nil || g.TaskID != root || g.BaseRef != "brigada/landed" {
		t.Fatalf("root grant = %+v, want base brigada/landed", g)
	}
	if ans := e.complete(g, done()); ans != AnsOK {
		t.Fatalf("complete root: %s", ans)
	}
	g = e.leaseAny(e.runner1)
	if g == nil || g.TaskID != later || g.BaseRef != branchOf(root) {
		t.Errorf("later grant = %+v, want base %q", g, branchOf(root))
	}
}

// TestLandTargetBasesAddedTasks: a task an amendment adds is created on
// the land target as a created task is, row and grant both.
func TestLandTargetBasesAddedTasks(t *testing.T) {
	e := landTargetEnv(t)
	res, err := e.eng.CreatePlan(e.ctx, e.project, "amend on the target", []PlanTask{
		{Name: "solo", Spec: "one", Class: "answer"},
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	am := e.amend(res.PlanID, PlanAmendment{Add: []PlanTask{{Name: "extra", Spec: "two", Class: "answer"}}})
	extra := am.Added[0].TaskID
	if got := e.str("SELECT base_ref FROM tasks WHERE id = ?", extra); got != "brigada/landed" {
		t.Errorf("added task base ref = %q, want the land target brigada/landed", got)
	}
	leased := false
	for {
		g := e.leaseAny(e.runner1)
		if g == nil {
			break
		}
		if g.TaskID == extra {
			leased = true
			if g.BaseRef != "brigada/landed" {
				t.Errorf("added task grant base = %q, want brigada/landed", g.BaseRef)
			}
		}
		e.complete(g, done())
	}
	if !leased {
		t.Error("the added task's work job was never leased")
	}
}

// TestLandGrantDiffsAgainstLandTarget: a task that started from the
// land target lands against it — the grant's BaseRef is the target the
// work began on, so the runner's diff and squash are taken against it
// (§7.1).
func TestLandGrantDiffsAgainstLandTarget(t *testing.T) {
	e := landTargetEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	res, err := e.eng.CreatePlan(e.ctx, e.project, "to land", []PlanTask{{Spec: "the work"}})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	g := e.leaseAny(author)
	if g == nil || g.TaskID != res.TaskID {
		t.Fatalf("work grant = %+v", g)
	}
	if ans := e.complete(g, doneOn(branchOf(res.TaskID), passCheck)); ans != AnsOK {
		t.Fatalf("complete the work: %s", ans)
	}
	e.runReview(reviewer, `{"verdict":"approve","findings":"`+substantiveFindings+`"}`)

	lg := e.leaseGit(author)
	if lg == nil || lg.Kind != KindLand {
		t.Fatalf("the land job was not leased: %+v", lg)
	}
	if lg.BaseRef != "brigada/landed" || lg.LandTarget != "brigada/landed" || lg.DefaultBranch != "main" {
		t.Errorf("land grant base/target/default = %q/%q/%q, want brigada/landed/brigada/landed/main",
			lg.BaseRef, lg.LandTarget, lg.DefaultBranch)
	}
	if ans := e.complete(lg, Completion{Result: `{"target_branch":"brigada/landed","commit":"abc123","check_passed":true,"pushed":"pushed"}`}); ans != AnsOK {
		t.Fatalf("complete the land job: %s", ans)
	}
}

// TestCrossPlanDepLandedOnTarget: a dep may name, by id, a landed task
// of another plan whose work is on the land target (§6.1): it counts as
// done from the start — no edge row is written for it — so a task whose
// only dep is such a one starts at once, and a task that also waits on
// a task of its own plan waits on that one alone.
func TestCrossPlanDepLandedOnTarget(t *testing.T) {
	e := landTargetEnv(t)
	dep := seededDep(e, taskDone, "brigada/landed", "pushed")

	res, err := e.eng.CreatePlan(e.ctx, e.project, "builds on landed work", []PlanTask{
		{Name: "base", Spec: "one", Class: "answer"},
		{Name: "next", Spec: "two", Class: "answer", Deps: []string{"base", fmt.Sprint(dep)}},
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	base, next := res.Tasks[0].TaskID, res.Tasks[1].TaskID
	if got := e.int("SELECT COUNT(*) FROM task_deps WHERE task_id = ?", next); got != 1 {
		t.Errorf("next edges = %d, want 1 alone: a landed dep is satisfied, no edge is written for it", got)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", next); state != taskPending {
		t.Errorf("next state = %q, want pending on base alone", state)
	}
	if got := e.str("SELECT base_ref FROM tasks WHERE id = ?", next); got != "brigada/landed" {
		t.Errorf("next base ref while waiting = %q, want the land target", got)
	}
	if ans := e.complete(e.leaseAny(e.runner1), done()); ans != AnsOK {
		t.Fatalf("complete base: %s", ans)
	}
	if got := e.str("SELECT base_ref FROM tasks WHERE id = ?", next); got != branchOf(base) {
		t.Errorf("next base ref = %q, want base's branch %q", got, branchOf(base))
	}

	solo, err := e.eng.CreatePlan(e.ctx, e.project, "on landed work alone", []PlanTask{
		{Spec: "straight on", Class: "answer", Deps: []string{fmt.Sprint(dep)}},
	})
	if err != nil {
		t.Fatalf("create the solo plan: %v", err)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", solo.TaskID); state != taskReady {
		t.Errorf("solo state = %q, want ready: a landed dep is already satisfied", state)
	}
	if got := e.int("SELECT COUNT(*) FROM task_deps WHERE task_id = ?", solo.TaskID); got != 0 {
		t.Errorf("solo edges = %d, want none", got)
	}
	for {
		g := e.leaseAny(e.runner1)
		if g == nil {
			break
		}
		if g.TaskID == solo.TaskID && g.BaseRef != "brigada/landed" {
			t.Errorf("solo grant base = %q, want the land target", g.BaseRef)
		}
		e.complete(g, done())
	}
}

// TestCrossPlanDepRefused: a dep naming a task of another plan that is
// not the project's own, not done, not landed, not pushed to the target,
// or landed on another branch refuses the whole plan with the reason
// (§6.1).
func TestCrossPlanDepRefused(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(e *env) int64
		want  string
	}{
		{"landed on another branch", func(e *env) int64 {
			return seededDep(e, taskDone, "main", "pushed")
		}, "not the project's land target"},
		{"not landed", func(e *env) int64 {
			return seededDep(e, taskDone, "brigada/landed", "")
		}, "has not landed"},
		{"push waiting", func(e *env) int64 {
			return seededDep(e, taskDone, "brigada/landed", "waiting")
		}, "has not pushed to the target"},
		{"not done", func(e *env) int64 {
			return seededDep(e, taskRunning, "brigada/landed", "pushed")
		}, "not landed"},
		{"another project", func(e *env) int64 {
			e.exec(`INSERT INTO projects (repo_url, default_branch, settings, foreman_harness, foreman_model, created_at, updated_at)
				VALUES ('https://example.invalid/other.git', 'main', '{}', '', '', ?, ?)`, seedTS, seedTS)
			p2 := e.int("SELECT MAX(id) FROM projects")
			e.exec(`INSERT INTO plans (project_id, title, state, created_at, updated_at)
				VALUES (?, 'another project plan', 'active', ?, ?)`, p2, seedTS, seedTS)
			pl2 := e.int("SELECT MAX(id) FROM plans")
			e.exec(`INSERT INTO tasks (plan_id, title, spec, class, state, base_ref, caps, created_at, updated_at)
				VALUES (?, 'elsewhere', 's', 'implement', 'done', 'main', '{}', ?, ?)`, pl2, seedTS, seedTS)
			id := e.int("SELECT MAX(id) FROM tasks")
			e.exec(`INSERT INTO landings (task_id, project_id, target_branch, commit_sha, pushed, created_at)
				VALUES (?, ?, 'brigada/landed', 'abc123', 'pushed', ?)`, id, p2, seedTS)
			return id
		}, "another project"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := landTargetEnv(t)
			dep := c.setup(e)
			_, err := e.eng.CreatePlan(e.ctx, e.project, "refused", []PlanTask{
				{Spec: "s", Class: "answer", Deps: []string{fmt.Sprint(dep)}},
			})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want it to contain %q", err, c.want)
			}
			if got := e.int("SELECT COUNT(*) FROM plans WHERE title = 'refused'"); got != 0 {
				t.Errorf("a refused plan left %d plans behind", got)
			}
		})
	}
}

// TestCrossPlanDepInAmend: an amendment takes a landed dep of another
// plan as a created plan does — an added task starts at once, a
// retarget onto it drops the wait — and refuses one whose work is not
// on the land target.
func TestCrossPlanDepInAmend(t *testing.T) {
	e := landTargetEnv(t)
	dep := seededDep(e, taskDone, "brigada/landed", "pushed")
	res, err := e.eng.CreatePlan(e.ctx, e.project, "to amend", []PlanTask{
		{Name: "solo", Spec: "one", Class: "answer"},
		{Name: "waiter", Spec: "two", Class: "answer", Deps: []string{"solo"}},
		{Name: "park", Spec: "four", Class: "answer", Deps: []string{"solo"}},
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	waiter := res.Tasks[1].TaskID
	am := e.amend(res.PlanID, PlanAmendment{Add: []PlanTask{
		{Name: "later", Spec: "three", Class: "answer", Deps: []string{fmt.Sprint(dep)}},
	}})
	later := am.Added[0].TaskID
	if state := e.str("SELECT state FROM tasks WHERE id = ?", later); state != taskReady {
		t.Errorf("added task state = %q, want ready: the landed dep is satisfied", state)
	}
	if got := e.int("SELECT COUNT(*) FROM task_deps WHERE task_id = ?", later); got != 0 {
		t.Errorf("added task edges = %d, want none", got)
	}

	am = e.amend(res.PlanID, PlanAmendment{Retarget: []PlanRetarget{
		{Task: "waiter", Deps: []string{fmt.Sprint(dep)}},
	}})
	if len(am.Retargeted) != 1 || am.Retargeted[0] != waiter {
		t.Fatalf("retargeted = %v, want the waiter %d", am.Retargeted, waiter)
	}
	if got := e.int("SELECT COUNT(*) FROM task_deps WHERE task_id = ?", waiter); got != 0 {
		t.Errorf("retargeted waiter edges = %d, want none", got)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", waiter); state != taskReady {
		t.Errorf("retargeted waiter state = %q, want ready: nothing waits on a landed dep", state)
	}

	bad := seededDep(e, taskDone, "main", "pushed")
	for name, call := range map[string]func() error{
		"add": func() error {
			_, err := e.eng.AmendPlan(e.ctx, e.project, res.PlanID, PlanAmendment{Add: []PlanTask{
				{Name: "no", Spec: "x", Class: "answer", Deps: []string{fmt.Sprint(bad)}},
			}})
			return err
		},
		"retarget": func() error {
			_, err := e.eng.AmendPlan(e.ctx, e.project, res.PlanID, PlanAmendment{Retarget: []PlanRetarget{
				{Task: "park", Deps: []string{fmt.Sprint(bad)}},
			}})
			return err
		},
	} {
		if err := call(); err == nil || !strings.Contains(err.Error(), "not the project's land target") {
			t.Errorf("%s with a dep off the target: err = %v, want the refusal", name, err)
		}
	}
}
