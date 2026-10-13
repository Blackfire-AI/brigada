package mcp

import (
	"database/sql"
	"strconv"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/prompt"
)

// planCreateArgs is one whole plan_create call the tests make: the plan
// and its single task, every field set.
var planCreateCall = map[string]any{
	"title": "fix the frobnicator",
	"tasks": []any{map[string]any{
		"title": "the frobnicator",
		"spec":  "Fix the frobnicator.\nKeep its tests whole.",
		"check": "go test ./...",
	}},
}

// planRows reads the one plan the call created: its ids and the job's
// prompt, straight from the store the engine sits on.
func planRows(t *testing.T, e *env) (plan, task, job int64, jobPrompt string) {
	t.Helper()
	err := e.st.View(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT p.id, t.id, j.id, j.prompt
			FROM plans p JOIN tasks t ON t.plan_id = p.id JOIN jobs j ON j.task_id = t.id`).Scan(&plan, &task, &job, &jobPrompt)
	})
	if err != nil {
		t.Fatalf("read the plan's rows: %v", err)
	}
	return plan, task, job, jobPrompt
}

// assertOneActivePlan reads the whole creation from the store: exactly
// one plan, active, in the token's project, one ready task on it, one
// queued work job on the task's key (§6.2).
func assertOneActivePlan(t *testing.T, e *env, plan, task, job int64) {
	t.Helper()
	var state string
	var project int64
	err := e.st.View(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT state, project_id FROM plans WHERE id = ?", plan).Scan(&state, &project)
	})
	if err != nil {
		t.Fatalf("read the plan: %v", err)
	}
	if state != "active" || project != 1 {
		t.Errorf("plan %d = %s in project %d, want active in the token's project 1", plan, state, project)
	}
	var taskState, baseRef string
	var check sql.NullString
	err = e.st.View(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT state, base_ref, check_cmd FROM tasks WHERE id = ?", task).Scan(&taskState, &baseRef, &check)
	})
	if err != nil {
		t.Fatalf("read the task: %v", err)
	}
	if taskState != "ready" || baseRef != "main" || !check.Valid || check.String != "go test ./..." {
		t.Errorf("task = %s/%s/%v, want ready on main with its check", taskState, baseRef, check)
	}
	var key, status, kind string
	err = e.st.View(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT key, status, kind FROM jobs WHERE id = ?", job).Scan(&key, &status, &kind)
	})
	if err != nil {
		t.Fatalf("read the job: %v", err)
	}
	if key != "task:"+strconv.FormatInt(task, 10) || status != "queued" || kind != "work" {
		t.Errorf("job = %s/%s/%s, want a queued work job on the task's key", key, status, kind)
	}
}

// TestServeCallPlanCreate covers the plan_create tool end to end against
// a test engine (§6.2, §12.2): the bridge — bearing a foreman token, as
// a real one does — creates the plan in the token's project, the task
// lands ready with its first work job queued carrying the engine's
// composed prompt, and the tool's text names the plan, the task and the
// job. A repeated identical call replays the first answer through the
// bridge's minted idempotency key (§12.1): no second plan.
func TestServeCallPlanCreate(t *testing.T) {
	e := newEnvToken(t, foremanTokenFile, "foreman-secret-9")
	s := start(t, e)
	a := s.call(1, "tools/call", map[string]any{"name": "plan_create", "arguments": planCreateCall})
	text, isError := toolText(t, a)
	if isError {
		t.Fatalf("plan_create errored: %s", text)
	}
	plan, task, job, jobPrompt := planRows(t, e)
	for _, want := range []string{
		"plan " + strconv.FormatInt(plan, 10),
		"task " + strconv.FormatInt(task, 10),
		"job " + strconv.FormatInt(job, 10),
		`"fix the frobnicator"`,
		"created",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("plan_create text = %q, want it to name %s", text, want)
		}
	}
	if jobPrompt != prompt.ComposeWorkerPrompt("Fix the frobnicator.\nKeep its tests whole.", "") {
		t.Errorf("job prompt = %q, want the engine's composed template plus the spec verbatim", jobPrompt)
	}
	assertOneActivePlan(t, e, plan, task, job)

	// The repeat: identical arguments, the same minted key, the first
	// answer replayed — one plan, not two.
	a = s.call(2, "tools/call", map[string]any{"name": "plan_create", "arguments": planCreateCall})
	again, isError := toolText(t, a)
	if isError {
		t.Fatalf("repeated plan_create errored: %s", again)
	}
	if again != text {
		t.Errorf("repeated plan_create text = %q, want the first answer %q", again, text)
	}
	var n int64
	if err := e.st.View(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT COUNT(*) FROM plans").Scan(&n)
	}); err != nil {
		t.Fatalf("count plans: %v", err)
	}
	if n != 1 {
		t.Errorf("plans after the repeat = %d, want 1: the minted key deduped the call", n)
	}
}

// TestServeCallPlanCreateRefusesTwoTasks covers the one-task rule as
// the foreman reads it (§6.1): the engine's refusal comes back as the
// tool's own error naming the rule, and nothing is created.
func TestServeCallPlanCreateRefusesTwoTasks(t *testing.T) {
	e := newEnvToken(t, foremanTokenFile, "foreman-secret-9")
	s := start(t, e)
	a := s.call(1, "tools/call", map[string]any{
		"name": "plan_create",
		"arguments": map[string]any{
			"title": "two tasks",
			"tasks": []any{
				map[string]any{"spec": "one"},
				map[string]any{"spec": "two"},
			},
		},
	})
	text, isError := toolText(t, a)
	if !isError {
		t.Fatalf("plan_create with two tasks = %q, want the tool's error", text)
	}
	if !strings.Contains(text, "one task per plan until plans with dependencies") {
		t.Errorf("error text = %q, want the one-task rule named", text)
	}
	var n int64
	if err := e.st.View(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT COUNT(*) FROM plans").Scan(&n)
	}); err != nil {
		t.Fatalf("count plans: %v", err)
	}
	if n != 0 {
		t.Errorf("plans after the refusal = %d, want 0", n)
	}
}

// TestIdemKey covers the key's mint (§12.1): one logical call — the
// same arguments — mints one key; another call mints another.
func TestIdemKey(t *testing.T) {
	a := planCreateArgs{Title: "t", Tasks: []planTaskArg{{Spec: "s"}}}
	b := planCreateArgs{Title: "t", Tasks: []planTaskArg{{Spec: "s"}}}
	if idemKey("plan_create", a.canonical()) != idemKey("plan_create", b.canonical()) {
		t.Error("two identical calls minted different keys")
	}
	b.Tasks[0].Class = "implement" // the default, made explicit
	if idemKey("plan_create", a.canonical()) != idemKey("plan_create", b.canonical()) {
		t.Error("an explicit default class minted another key for one logical call")
	}
	b.Tasks[0].Spec = "other"
	if idemKey("plan_create", a.canonical()) == idemKey("plan_create", b.canonical()) {
		t.Error("a different call minted the same key")
	}
	if idemKey("plan_create", a.canonical()) == idemKey("plan_amend", a.canonical()) {
		t.Error("the key does not name its tool")
	}
}
