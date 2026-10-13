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

// TestCreatePlanRefusesOtherSizes covers the one-task rule (§6.1): zero
// or more than one task is a clear error naming the rule, and nothing —
// no plan, task, job or event — is left behind.
func TestCreatePlanRefusesOtherSizes(t *testing.T) {
	e := newEnv(t)
	for name, tasks := range map[string][]PlanTask{
		"no tasks":  nil,
		"two tasks": {{Spec: "one"}, {Spec: "two"}},
	} {
		_, err := e.eng.CreatePlan(e.ctx, e.project, "a plan", tasks)
		if err == nil || !strings.Contains(err.Error(), oneTaskRule) {
			t.Errorf("%s: err = %v, want the one-task rule named", name, err)
		}
	}
	if got := e.int("SELECT COUNT(*) FROM plans"); got != 1 {
		t.Errorf("plans after the refusals = %d, want the seeded 1", got)
	}
	if got := e.int("SELECT COUNT(*) FROM tasks"); got != 1 {
		t.Errorf("tasks after the refusals = %d, want the seeded 1", got)
	}
	if got := e.int("SELECT COUNT(*) FROM jobs"); got != 0 {
		t.Errorf("jobs after the refusals = %d, want 0", got)
	}
	if got := e.int("SELECT COUNT(*) FROM events WHERE kind = 'plan_created'"); got != 0 {
		t.Errorf("plan_created events after the refusals = %d, want 0", got)
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
