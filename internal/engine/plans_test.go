package engine

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/prompt"
)

func TestCreatePlanOneTask(t *testing.T) {
	e := newEnv(t)

	res, err := e.eng.CreatePlan(e.ctx, e.project, "fix the frobnicator", []PlanTask{{
		Title: "the frobnicator",
		Spec:  "Fix the frobnicator.\nKeep its tests whole.",
		Check: "go test ./...",
	}})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	if res.Answer != AnsCreated || res.Run != 1 {
		t.Fatalf("create plan = %+v, want created run 1", res)
	}

	// The plan (§6.2): active at once — the approval limit arrives with
	// the plan phase — in the named project.
	if state := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); state != "active" {
		t.Errorf("plan state = %q, want active", state)
	}
	if got := e.str("SELECT title FROM plans WHERE id = ?", res.PlanID); got != "fix the frobnicator" {
		t.Errorf("plan title = %q", got)
	}
	if got := e.int("SELECT project_id FROM plans WHERE id = ?", res.PlanID); got != e.project {
		t.Errorf("plan project = %d, want %d", got, e.project)
	}
	// The plan_created event names it (§9).
	if got := e.int("SELECT COUNT(*) FROM events WHERE kind = 'plan_created' AND project_id = ?", e.project); got != 1 {
		t.Errorf("plan_created events = %d, want 1", got)
	}

	// The task (§6.1): ready at once, on the plan, its class defaulted,
	// its check and base ref — the project's default branch — in place.
	if got := e.int("SELECT plan_id FROM tasks WHERE id = ?", res.TaskID); got != res.PlanID {
		t.Errorf("task plan = %d, want %d", got, res.PlanID)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", res.TaskID); state != "ready" {
		t.Errorf("task state = %q, want ready", state)
	}
	if class := e.str("SELECT class FROM tasks WHERE id = ?", res.TaskID); class != "implement" {
		t.Errorf("task class = %q, want the default implement", class)
	}
	if check := e.str("SELECT COALESCE(check_cmd, '') FROM tasks WHERE id = ?", res.TaskID); check != "go test ./..." {
		t.Errorf("task check = %q", check)
	}
	if base := e.str("SELECT base_ref FROM tasks WHERE id = ?", res.TaskID); base != "main" {
		t.Errorf("task base ref = %q, want the project's default branch", base)
	}

	// The job: the first work round, queued on the task's key, its
	// prompt the composed template plus the spec verbatim (§1.4, §5.3).
	if key := e.str("SELECT key FROM jobs WHERE id = ?", res.JobID); key != fmt.Sprintf("task:%d", res.TaskID) {
		t.Errorf("job key = %q, want task:%d", key, res.TaskID)
	}
	if kind := e.str("SELECT kind FROM jobs WHERE id = ?", res.JobID); kind != string(KindWork) {
		t.Errorf("job kind = %q, want work", kind)
	}
	if mode := e.str("SELECT mode FROM jobs WHERE id = ?", res.JobID); mode != string(ModeFresh) {
		t.Errorf("job mode = %q, want fresh", mode)
	}
	if status := e.str("SELECT status FROM jobs WHERE id = ?", res.JobID); status != statusQueued {
		t.Errorf("job status = %q, want queued", status)
	}
	spec := "Fix the frobnicator.\nKeep its tests whole."
	if p := e.str("SELECT prompt FROM jobs WHERE id = ?", res.JobID); p != prompt.ComposeWorkerPrompt(spec, "") {
		t.Errorf("job prompt = %q, want the composed template plus the spec verbatim", p)
	} else if !strings.Contains(p, "<task-spec>\n"+spec+"\n</task-spec>") {
		t.Errorf("job prompt = %q, want the spec verbatim between delimiters", p)
	}
	// The submit's task_spec message rode along (§9).
	if got := e.int("SELECT COUNT(*) FROM messages WHERE kind = 'task_spec' AND event_id = (SELECT id FROM events WHERE job_id = ?)", res.JobID); got != 1 {
		t.Errorf("task_spec messages for the job = %d, want 1", got)
	}
}

// TestCreatePlanTaskDefaults covers the fields the caller may leave
// out: the class defaults to implement, the task's title to the spec's
// first line.
func TestCreatePlanTaskDefaults(t *testing.T) {
	e := newEnv(t)
	res, err := e.eng.CreatePlan(e.ctx, e.project, "a plan", []PlanTask{{
		Spec:  "\n\nfix the frobnicator\nsecond line",
		Class: "review",
	}})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	if got := e.str("SELECT title FROM tasks WHERE id = ?", res.TaskID); got != "fix the frobnicator" {
		t.Errorf("task title = %q, want the spec's first non-empty line", got)
	}
	if class := e.str("SELECT class FROM tasks WHERE id = ?", res.TaskID); class != "review" {
		t.Errorf("task class = %q, want review", class)
	}
}

// TestCreatePlanRefusesBadGraphs covers the graph's validation (§6.1):
// no tasks, a dependency naming no task of the plan, a self-dependency,
// a duplicate name and a cycle each refuse the plan whole, the cycle's
// task names in the error, and nothing — no plan, task, edge, job or
// event — is left behind.
func TestCreatePlanRefusesBadGraphs(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct {
		name  string
		tasks []PlanTask
		want  string
	}{
		{"no tasks", nil, "at least one task"},
		{"outside dep", []PlanTask{{Name: "a", Spec: "one", Deps: []string{"zzz"}}}, `"zzz"`},
		{"self dep", []PlanTask{{Name: "a", Spec: "one"}, {Name: "b", Spec: "two", Deps: []string{"b"}}}, "b depends on itself"},
		{"duplicate name", []PlanTask{{Name: "a", Spec: "one"}, {Name: "a", Spec: "two"}}, `"a"`},
		{"cycle", []PlanTask{
			{Name: "root", Spec: "r"},
			{Name: "a", Spec: "one", Deps: []string{"root", "c"}},
			{Name: "b", Spec: "two", Deps: []string{"a"}},
			{Name: "c", Spec: "three", Deps: []string{"b"}},
		}, "a -> c -> b -> a"},
	} {
		_, err := e.eng.CreatePlan(e.ctx, e.project, "a plan", c.tasks)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to contain %q", c.name, err, c.want)
		}
	}
	if got := e.int("SELECT COUNT(*) FROM plans"); got != 1 {
		t.Errorf("plans after the refusals = %d, want the seeded 1", got)
	}
	if got := e.int("SELECT COUNT(*) FROM tasks"); got != 1 {
		t.Errorf("tasks after the refusals = %d, want the seeded 1", got)
	}
	if got := e.int("SELECT COUNT(*) FROM task_deps"); got != 0 {
		t.Errorf("edges after the refusals = %d, want 0", got)
	}
	if got := e.int("SELECT COUNT(*) FROM jobs"); got != 0 {
		t.Errorf("jobs after the refusals = %d, want 0", got)
	}
	if got := e.int("SELECT COUNT(*) FROM events WHERE kind = 'plan_created'"); got != 0 {
		t.Errorf("plan_created events after the refusals = %d, want 0", got)
	}
}

// twoStep creates build -> ship: ship waits on build, both of the
// non-implement class so a done finishes the task outright.
func (e *env) twoStep() CreatePlanResult {
	e.t.Helper()
	res, err := e.eng.CreatePlan(e.ctx, e.project, "two steps", []PlanTask{
		{Name: "build", Spec: "build it", Class: "answer"},
		{Name: "ship", Spec: "ship it", Class: "answer", Deps: []string{"build", "build"}},
	})
	if err != nil {
		e.t.Fatalf("create plan: %v", err)
	}
	return res
}

// TestCreatePlanWithDeps: the edges land in the table, the dependent
// waits pending with no job, the root is ready with its work job.
func TestCreatePlanWithDeps(t *testing.T) {
	e := newEnv(t)
	res := e.twoStep()
	if len(res.Tasks) != 2 {
		t.Fatalf("tasks = %+v, want 2", res.Tasks)
	}
	build, ship := res.Tasks[0], res.Tasks[1]
	if res.TaskID != build.TaskID || res.JobID != build.JobID || build.JobID == 0 || ship.JobID != 0 {
		t.Errorf("result = %+v, want the root's task and job answering and no job for ship", res)
	}
	if got := e.str("SELECT state FROM tasks WHERE id = ?", build.TaskID); got != taskReady {
		t.Errorf("build state = %q, want ready", got)
	}
	if got := e.str("SELECT state FROM tasks WHERE id = ?", ship.TaskID); got != taskPending {
		t.Errorf("ship state = %q, want pending", got)
	}
	if got := e.int("SELECT COUNT(*) FROM task_deps WHERE plan_id = ? AND task_id = ? AND depends_on_task_id = ?",
		res.PlanID, ship.TaskID, build.TaskID); got != 1 {
		t.Errorf("ship->build edges = %d, want 1 (a repeated dep folds)", got)
	}
	if got := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ?", ship.TaskID); got != 0 {
		t.Errorf("ship jobs = %d, want 0 while it waits", got)
	}
}

// TestCompletionReleasesDependent: the last dependency's completion
// moves the dependent to ready and queues its work job in the same
// completion, and the plan stays active until the dependent is done.
func TestCompletionReleasesDependent(t *testing.T) {
	e := newEnv(t)
	res := e.twoStep()
	build, ship := res.Tasks[0], res.Tasks[1]

	g := e.leaseAny(e.runner1)
	if g == nil || g.JobID != build.JobID {
		t.Fatalf("lease = %+v, want build's job %d", g, build.JobID)
	}
	if ans := e.complete(g, done()); ans != AnsOK {
		t.Fatalf("complete build: %s", ans)
	}
	if got := e.str("SELECT state FROM tasks WHERE id = ?", ship.TaskID); got != taskReady {
		t.Errorf("ship state = %q, want ready", got)
	}
	if got := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'work' AND status = 'queued'", ship.TaskID); got != 1 {
		t.Errorf("ship queued work jobs = %d, want 1", got)
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != "active" {
		t.Errorf("plan state = %q, want active", got)
	}
	g = e.leaseAny(e.runner1)
	if g == nil || g.TaskID != ship.TaskID {
		t.Fatalf("lease = %+v, want ship's job", g)
	}
	if ans := e.complete(g, done()); ans != AnsOK {
		t.Fatalf("complete ship: %s", ans)
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != planDone {
		t.Errorf("plan state = %q, want done", got)
	}
}

// TestFailedDependencyKeepsDependentWaiting: a dependency that failed
// parks at needs_foreman and its dependent stays pending, no job.
func TestFailedDependencyKeepsDependentWaiting(t *testing.T) {
	e := newEnv(t)
	res := e.twoStep()
	build, ship := res.Tasks[0], res.Tasks[1]

	g := e.leaseAny(e.runner1)
	e.complete(g, Completion{Result: `{"status":"failed","summary":"the repo would not clone"}`})
	if got := e.str("SELECT state FROM tasks WHERE id = ?", build.TaskID); got != taskNeedsForeman {
		t.Fatalf("build state = %q, want needs_foreman", got)
	}
	if got := e.str("SELECT state FROM tasks WHERE id = ?", ship.TaskID); got != taskPending {
		t.Errorf("ship state = %q, want pending", got)
	}
	if got := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ?", ship.TaskID); got != 0 {
		t.Errorf("ship jobs = %d, want 0", got)
	}
}

// TestStatusNamesTheWait: a waiting task shows its open dependencies by
// name, and the name goes once the dependency is done.
func TestStatusNamesTheWait(t *testing.T) {
	e := newEnv(t)
	res := e.twoStep()
	find := func() StatusTask {
		t.Helper()
		st, err := e.eng.Status(e.ctx, e.project)
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		for _, p := range st.Plans {
			if p.ID != res.PlanID {
				continue
			}
			for _, tk := range p.Tasks {
				if tk.ID == res.Tasks[1].TaskID {
					return tk
				}
			}
		}
		t.Fatal("ship not in status")
		return StatusTask{}
	}
	if tk := find(); tk.State != taskPending || len(tk.WaitingOn) != 1 || tk.WaitingOn[0] != "build" {
		t.Errorf("ship = %+v, want pending waiting on [build]", tk)
	}
	e.complete(e.leaseAny(e.runner1), done())
	if tk := find(); tk.State != taskReady || len(tk.WaitingOn) != 0 {
		t.Errorf("ship after build = %+v, want ready, waiting on nothing", tk)
	}
}

// TestCreatePlanValidates covers the caller's other mistakes: no title,
// no spec, a project the store does not know.
func TestCreatePlanValidates(t *testing.T) {
	e := newEnv(t)
	for name, call := range map[string]func() error{
		"no title": func() error {
			_, err := e.eng.CreatePlan(e.ctx, e.project, "", []PlanTask{{Spec: "s"}})
			return err
		},
		"no spec": func() error {
			_, err := e.eng.CreatePlan(e.ctx, e.project, "a plan", []PlanTask{{}})
			return err
		},
		"no such project": func() error {
			_, err := e.eng.CreatePlan(e.ctx, e.project+99, "a plan", []PlanTask{{Spec: "s"}})
			return err
		},
	} {
		if err := call(); err == nil {
			t.Errorf("%s: accepted, want refusal", name)
		}
	}
	if got := e.int("SELECT COUNT(*) FROM plans"); got != 1 {
		t.Errorf("plans after the refusals = %d, want the seeded 1", got)
	}
}
