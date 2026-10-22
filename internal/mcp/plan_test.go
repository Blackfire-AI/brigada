package mcp

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/prompt"
	"github.com/Blackfire-AI/brigada/internal/store"
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
	setProjectDefault(t, e, `{"default_check": "go test ./..."}`) // the call's check: the plan gate passes it
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
	if jobPrompt != prompt.ComposeWorkerPrompt("Fix the frobnicator.\nKeep its tests whole.", "go test ./...", "") {
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

// TestServeCallPlanCreateRefusesCycle covers the graph rule as the
// foreman reads it (§6.1): a dependency cycle comes back as the tool's
// own error naming the cycle's tasks, and nothing is created.
func TestServeCallPlanCreateRefusesCycle(t *testing.T) {
	e := newEnvToken(t, foremanTokenFile, "foreman-secret-9")
	s := start(t, e)
	a := s.call(1, "tools/call", map[string]any{
		"name": "plan_create",
		"arguments": map[string]any{
			"title": "two tasks",
			"tasks": []any{
				map[string]any{"name": "a", "spec": "one", "deps": []any{"b"}},
				map[string]any{"name": "b", "spec": "two", "deps": []any{"a"}},
			},
		},
	})
	text, isError := toolText(t, a)
	if !isError {
		t.Fatalf("plan_create with a cycle = %q, want the tool's error", text)
	}
	if !strings.Contains(text, "dependency cycle: a -> b -> a") {
		t.Errorf("error text = %q, want the cycle named", text)
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

// TestServeCallPlanCreateAwaitsApproval covers the approval limit as the
// foreman reads it (§6.3): a plan over it comes back naming the state
// and the reasons, with no job queued, and the foreman has no tool to
// approve it.
func TestServeCallPlanCreateAwaitsApproval(t *testing.T) {
	e := newEnvToken(t, foremanTokenFile, "foreman-secret-9")
	if err := e.st.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE projects SET settings = ?`, `{"approval_max_tasks": 5}`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	s := start(t, e)
	var tasks []any
	for _, spec := range []string{"a", "b", "c", "d", "e", "f"} {
		tasks = append(tasks, map[string]any{"spec": spec})
	}
	a := s.call(1, "tools/call", map[string]any{
		"name":      "plan_create",
		"arguments": map[string]any{"title": "a big plan", "tasks": tasks},
	})
	text, isError := toolText(t, a)
	if isError {
		t.Fatalf("plan_create errored: %s", text)
	}
	for _, want := range []string{"awaiting approval", "pending_approval", "6 tasks, over the limit of 5", "brigada plan approve"} {
		if !strings.Contains(text, want) {
			t.Errorf("plan_create text = %q, want it to say %q", text, want)
		}
	}
	var jobs int64
	if err := e.st.View(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT COUNT(*) FROM jobs WHERE kind = 'work'").Scan(&jobs)
	}); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	if jobs != 0 {
		t.Errorf("jobs = %d, want none queued while the plan awaits", jobs)
	}
	for _, tool := range tools {
		if strings.Contains(tool.Name, "approve") {
			t.Errorf("the foreman has a tool %q: confirmation is the user's", tool.Name)
		}
	}
}

// TestServeCallPlanAmend covers plan_amend end to end (§12.2): a plan of
// two tasks, a amended — a cancelled, b re-pointed off it and started,
// c added, one dependency named by its bare id — and the text names what
// changed and started; a cycle comes back as the tool's own error and
// changes nothing.
func TestServeCallPlanAmend(t *testing.T) {
	e := newEnvToken(t, foremanTokenFile, "foreman-secret-9")
	s := start(t, e)
	a := s.call(1, "tools/call", map[string]any{"name": "plan_create", "arguments": map[string]any{
		"title": "two steps",
		"tasks": []any{
			map[string]any{"name": "a", "spec": "one"},
			map[string]any{"name": "b", "spec": "two", "deps": []any{"a"}},
		},
	}})
	if text, isError := toolText(t, a); isError {
		t.Fatalf("plan_create errored: %s", text)
	}
	state := func(name string) string {
		var st string
		if err := e.st.View(t.Context(), func(tx *sql.Tx) error {
			return tx.QueryRow("SELECT state FROM tasks WHERE name = ?", name).Scan(&st)
		}); err != nil {
			t.Fatalf("read task %s: %v", name, err)
		}
		return st
	}

	a = s.call(2, "tools/call", map[string]any{"name": "plan_amend", "arguments": map[string]any{
		"plan":     1,
		"cancel":   []any{"a"},
		"retarget": []any{map[string]any{"task": "b", "deps": []any{}}},
		"add":      []any{map[string]any{"name": "c", "spec": "three", "deps": []any{1}}},
	}})
	text, isError := toolText(t, a)
	if !isError || !strings.Contains(text, "cancelled and will never be done") && !strings.Contains(text, "which is cancelled") {
		t.Fatalf("a dep on the cancelled task a (by id) = %q, error %v, want a refusal", text, isError)
	}
	if state("a") != "ready" || state("b") != "pending" {
		t.Fatalf("a refused amendment changed the plan: a %s, b %s", state("a"), state("b"))
	}

	a = s.call(3, "tools/call", map[string]any{"name": "plan_amend", "arguments": map[string]any{
		"plan":     1,
		"cancel":   []any{"a"},
		"retarget": []any{map[string]any{"task": "b", "deps": []any{}}},
		"add":      []any{map[string]any{"name": "c", "spec": "three", "deps": []any{"b"}}},
	}})
	text, isError = toolText(t, a)
	if isError {
		t.Fatalf("plan_amend errored: %s", text)
	}
	for _, want := range []string{"plan 1 amended", "state active", "added task", "cancelled task 1", "re-pointed task 2", "work job"} {
		if !strings.Contains(text, want) {
			t.Errorf("plan_amend text = %q, want it to say %q", text, want)
		}
	}
	if state("a") != "cancelled" || state("b") != "ready" || state("c") != "pending" {
		t.Errorf("states a %s, b %s, c %s; want cancelled, ready, pending", state("a"), state("b"), state("c"))
	}

	a = s.call(4, "tools/call", map[string]any{"name": "plan_amend", "arguments": map[string]any{
		"plan":     1,
		"retarget": []any{map[string]any{"task": "c", "deps": []any{"c"}}},
	}})
	if text, isError = toolText(t, a); !isError || !strings.Contains(text, "depends on itself") {
		t.Errorf("a self-dependency = %q, error %v, want the tool's error", text, isError)
	}
	a = s.call(5, "tools/call", map[string]any{"name": "plan_amend", "arguments": map[string]any{}})
	if text, isError = toolText(t, a); !isError || !strings.Contains(text, "plan is required") {
		t.Errorf("no plan = %q, error %v, want the tool's error", text, isError)
	}
}

// TestPlanBaseSchemaAndCalls covers the optional base of a task (§6.1):
// plan_create's task items, plan_amend's added items and its retarget
// items all declare it; a base that is a dep is stored on its edge, one
// that is not refuses the whole call with its reason, and the same holds
// through plan_amend.
func TestPlanBaseSchemaAndCalls(t *testing.T) {
	e := newEnvToken(t, foremanTokenFile, "foreman-secret-9")
	s := start(t, e)

	a := s.call(1, "tools/list", nil)
	var list struct {
		Tools []toolInfo `json:"tools"`
	}
	if err := json.Unmarshal(a.Result, &list); err != nil {
		t.Fatalf("decode tools/list: %v", err)
	}
	// item digs out properties.<field>.items.properties of a tool.
	item := func(tool, field string) map[string]any {
		t.Helper()
		for _, tl := range list.Tools {
			if tl.Name != tool {
				continue
			}
			props := tl.InputSchema.(map[string]any)["properties"].(map[string]any)
			return props[field].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
		}
		t.Fatalf("no tool %s", tool)
		return nil
	}
	for name, props := range map[string]map[string]any{
		"plan_create tasks":   item("plan_create", "tasks"),
		"plan_amend add":      item("plan_amend", "add"),
		"plan_amend retarget": item("plan_amend", "retarget"),
	} {
		if _, ok := props["base"]; !ok {
			t.Errorf("%s declares no base: %v", name, props)
		}
	}

	a = s.call(2, "tools/call", map[string]any{"name": "plan_create", "arguments": map[string]any{
		"title": "stack",
		"tasks": []any{
			map[string]any{"name": "a", "spec": "one"},
			map[string]any{"name": "b", "spec": "two"},
			map[string]any{"name": "c", "spec": "three", "deps": []any{"a", "b"}, "base": "b"},
		},
	}})
	if text, isError := toolText(t, a); isError {
		t.Fatalf("plan_create with a base errored: %s", text)
	}
	var baseOf string
	if err := e.st.View(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT x.name FROM task_deps d JOIN tasks x ON x.id = d.depends_on_task_id
			WHERE d.is_base = 1`).Scan(&baseOf)
	}); err != nil || baseOf != "b" {
		t.Fatalf("stored base = %q (%v), want b", baseOf, err)
	}

	a = s.call(3, "tools/call", map[string]any{"name": "plan_create", "arguments": map[string]any{
		"title": "bad",
		"tasks": []any{
			map[string]any{"name": "a", "spec": "one"},
			map[string]any{"name": "b", "spec": "two", "deps": []any{"a"}, "base": "c"},
		},
	}})
	if text, isError := toolText(t, a); !isError || !strings.Contains(text, "not one of its deps") {
		t.Errorf("a base outside deps = %q, error %v, want the refusal with its reason", text, isError)
	}

	a = s.call(4, "tools/call", map[string]any{"name": "plan_amend", "arguments": map[string]any{
		"plan":     1,
		"retarget": []any{map[string]any{"task": "c", "deps": []any{"a"}, "base": "b"}},
	}})
	if text, isError := toolText(t, a); !isError || !strings.Contains(text, "not one of its deps") {
		t.Errorf("an amend base outside deps = %q, error %v, want the refusal", text, isError)
	}
	a = s.call(5, "tools/call", map[string]any{"name": "plan_amend", "arguments": map[string]any{
		"plan":     1,
		"retarget": []any{map[string]any{"task": "c", "deps": []any{"a", "b"}, "base": "a"}},
	}})
	if text, isError := toolText(t, a); isError {
		t.Fatalf("plan_amend with a base errored: %s", text)
	}
	if err := e.st.View(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT x.name FROM task_deps d JOIN tasks x ON x.id = d.depends_on_task_id
			WHERE d.is_base = 1`).Scan(&baseOf)
	}); err != nil || baseOf != "a" {
		t.Errorf("base after the retarget = %q (%v), want a", baseOf, err)
	}
}

// TestPlanContinueSchemaAndCalls covers the optional continue of a task
// (§6.1): plan_create's task items, plan_amend's added items and its
// retarget items all declare it; a continued dep that is a dep is stored
// on its edge, one that is not refuses the whole call with its reason,
// and a retarget replaces it.
func TestPlanContinueSchemaAndCalls(t *testing.T) {
	e := newEnvToken(t, foremanTokenFile, "foreman-secret-9")
	s := start(t, e)

	a := s.call(1, "tools/list", nil)
	var list struct {
		Tools []toolInfo `json:"tools"`
	}
	if err := json.Unmarshal(a.Result, &list); err != nil {
		t.Fatalf("decode tools/list: %v", err)
	}
	item := func(tool, field string) map[string]any {
		t.Helper()
		for _, tl := range list.Tools {
			if tl.Name != tool {
				continue
			}
			props := tl.InputSchema.(map[string]any)["properties"].(map[string]any)
			return props[field].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
		}
		t.Fatalf("no tool %s", tool)
		return nil
	}
	for name, props := range map[string]map[string]any{
		"plan_create tasks":   item("plan_create", "tasks"),
		"plan_amend add":      item("plan_amend", "add"),
		"plan_amend retarget": item("plan_amend", "retarget"),
	} {
		if _, ok := props["continue"]; !ok {
			t.Errorf("%s declares no continue: %v", name, props)
		}
	}

	a = s.call(2, "tools/call", map[string]any{"name": "plan_create", "arguments": map[string]any{
		"title": "continued",
		"tasks": []any{
			map[string]any{"name": "a", "spec": "one"},
			map[string]any{"name": "b", "spec": "two", "deps": []any{"a"}, "continue": "a"},
		},
	}})
	if text, isError := toolText(t, a); isError {
		t.Fatalf("plan_create with a continue errored: %s", text)
	}
	var continueOf string
	if err := e.st.View(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT x.name FROM task_deps d JOIN tasks x ON x.id = d.depends_on_task_id
			WHERE d.is_continue = 1`).Scan(&continueOf)
	}); err != nil || continueOf != "a" {
		t.Fatalf("stored continued dep = %q (%v), want a", continueOf, err)
	}

	a = s.call(3, "tools/call", map[string]any{"name": "plan_create", "arguments": map[string]any{
		"title": "bad",
		"tasks": []any{
			map[string]any{"name": "a", "spec": "one"},
			map[string]any{"name": "b", "spec": "two", "deps": []any{"a"}, "continue": "zzz"},
		},
	}})
	if text, isError := toolText(t, a); !isError || !strings.Contains(text, "not one of its deps") {
		t.Errorf("a continued dep outside deps = %q, error %v, want the refusal with its reason", text, isError)
	}

	a = s.call(4, "tools/call", map[string]any{"name": "plan_amend", "arguments": map[string]any{
		"plan":     1,
		"add":      []any{map[string]any{"name": "c", "spec": "three", "deps": []any{"a"}}},
		"retarget": []any{map[string]any{"task": "b", "deps": []any{"a"}, "continue": "zzz"}},
	}})
	if text, isError := toolText(t, a); !isError || !strings.Contains(text, "not one of its deps") {
		t.Errorf("an amend continued dep outside deps = %q, error %v, want the refusal", text, isError)
	}
	a = s.call(5, "tools/call", map[string]any{"name": "plan_amend", "arguments": map[string]any{
		"plan":     1,
		"add":      []any{map[string]any{"name": "c", "spec": "three", "deps": []any{"a", "b"}, "continue": "b"}},
		"retarget": []any{map[string]any{"task": "b", "deps": []any{"a"}}},
	}})
	if text, isError := toolText(t, a); isError {
		t.Fatalf("plan_amend with a continue errored: %s", text)
	}
	if err := e.st.View(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT x.name FROM task_deps d JOIN tasks x ON x.id = d.depends_on_task_id
			WHERE d.is_continue = 1`).Scan(&continueOf)
	}); err != nil || continueOf != "b" {
		t.Errorf("continued dep after the amend = %q (%v), want b (the retarget dropped b's)", continueOf, err)
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
	if idemKey("plan_create", a.canonical()) == idemKey("plan_other", a.canonical()) {
		t.Error("the key does not name its tool")
	}
}

func TestPlanTaskTierEffortSchema(t *testing.T) {
	for _, tool := range tools {
		field := ""
		switch tool.Name {
		case "plan_create":
			field = "tasks"
		case "plan_amend":
			field = "add"
		default:
			continue
		}
		props := tool.InputSchema.(map[string]any)["properties"].(map[string]any)[field].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
		for key, want := range map[string][]string{"tier": {"light", "standard", "heavy"}, "effort": {"low", "medium", "high", "max"}} {
			got := props[key].(map[string]any)["enum"]
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s %s enum = %v, want %v", tool.Name, key, got, want)
			}
		}
	}
	for _, tc := range []struct{ tier, effort, want string }{
		{"", "", ""}, {"standard", "", ""}, {"heavy", "high", " (heavy, effort high)"}, {"light", "", " (light)"}, {"standard", "max", " (standard, effort max)"},
	} {
		if got := taskTierEffort(tc.tier, tc.effort); got != tc.want {
			t.Errorf("taskTierEffort(%q,%q) = %q, want %q", tc.tier, tc.effort, got, tc.want)
		}
	}
	a := planCreateArgs{Tasks: []planTaskArg{{Spec: "x"}}}
	b := planCreateArgs{Tasks: []planTaskArg{{Spec: "x", Tier: "standard"}}}
	if idemKey("plan_create", a.canonical()) != idemKey("plan_create", b.canonical()) {
		t.Error("omitted and standard tiers hash differently")
	}
}

type planTransport func(*http.Request) (*http.Response, error)

func (f planTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestServePlanTaskTierEffort(t *testing.T) {
	ctx := t.Context()
	st, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	eng := engine.New(st, engine.Config{PlanReview: &planReviewOff})
	tokenPath := filepath.Join(t.TempDir(), "tokens")
	if err := os.WriteFile(tokenPath, []byte(foremanTokenFile), 0600); err != nil {
		t.Fatal(err)
	}
	srv, err := api.New(ctx, st, eng, nil, tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Update(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO projects (repo_url, default_branch, settings, foreman_harness, foreman_model, created_at, updated_at)
			VALUES ('https://example.invalid/tier.git', 'main', '{"approval_max_tasks": 5}', 'claude', 'sonnet', ?, ?)`, seedTS, seedTS)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	client := api.NewClientWithHTTPClient(&http.Client{Transport: planTransport(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)
		return w.Result(), nil
	})}, "foreman-secret-9")
	e := &env{t: t, st: st, client: client}
	s := start(t, e)
	for i, tasks := range [][]any{
		{map[string]any{"name": "build", "spec": "build", "tier": "heavy", "effort": "high"}},
		{map[string]any{"name": "build", "spec": "build", "tier": "heavy", "effort": "high"}, map[string]any{"spec": "plain"}},
	} {
		a := s.call(i+1, "tools/call", map[string]any{"name": "plan_create", "arguments": map[string]any{"title": "sized", "tasks": tasks}})
		text, isError := toolText(t, a)
		if isError || !strings.Contains(text, "build (heavy, effort high)") {
			t.Fatalf("create answer = %q, error %v", text, isError)
		}
	}
	var plan int64
	if err := e.st.View(t.Context(), func(tx *sql.Tx) error { return tx.QueryRow("SELECT MIN(id) FROM plans").Scan(&plan) }); err != nil {
		t.Fatal(err)
	}
	a := s.call(3, "tools/call", map[string]any{"name": "plan_amend", "arguments": map[string]any{"plan": plan, "add": []any{map[string]any{"name": "extra", "spec": "extra", "tier": "heavy", "effort": "high"}}}})
	text, isError := toolText(t, a)
	if isError || !strings.Contains(text, "extra (heavy, effort high)") {
		t.Fatalf("amend answer = %q, error %v", text, isError)
	}
	var tier, effective, effort string
	if err := e.st.View(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT tier,effective_tier,effort FROM tasks WHERE name = 'extra'").Scan(&tier, &effective, &effort)
	}); err != nil {
		t.Fatal(err)
	}
	if tier != "heavy" || effective != "heavy" || effort != "high" {
		t.Errorf("stored amendment settings = %s/%s/%s", tier, effective, effort)
	}
	// Retier travels through the MCP arguments, client and API, and its
	// answer lists the task without restarting it.
	a = s.call(30, "tools/call", map[string]any{"name": "plan_amend", "arguments": map[string]any{
		"plan": plan, "retier": []any{map[string]any{"task": "extra", "tier": "light", "effort": "low"}},
	}})
	text, isError = toolText(t, a)
	if isError || !strings.Contains(text, "retiered task") {
		t.Fatalf("retier answer = %q, error %v", text, isError)
	}
	if err := e.st.View(ctx, func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT tier,effective_tier,effort FROM tasks WHERE name = 'extra'").Scan(&tier, &effective, &effort)
	}); err != nil {
		t.Fatal(err)
	}
	if tier != "light" || effective != "light" || effort != "low" {
		t.Errorf("retier settings = %s/%s/%s", tier, effective, effort)
	}
	a = s.call(31, "tools/call", map[string]any{"name": "plan_amend", "arguments": map[string]any{
		"plan": plan, "retier": []any{map[string]any{"task": "extra", "tier": "invalid"}},
	}})
	if text, isError := toolText(t, a); !isError || !strings.Contains(text, "invalid") {
		t.Errorf("invalid retier = %q (%v)", text, isError)
	}
	_, err = client.CreatePlan(ctx, api.PlanCreate{Title: "bad", Tasks: []api.PlanTask{{Name: "invalid", Spec: "invalid", Tier: "big"}}}, "bad-tier")
	var statusErr *api.StatusError
	if !errors.As(err, &statusErr) || statusErr.Status != http.StatusBadRequest || !strings.Contains(err.Error(), "light, standard, heavy") {
		t.Fatalf("bad tier response = %v", err)
	}
	tasks := make([]any, 6)
	for i := range tasks {
		tasks[i] = map[string]any{"spec": "plain"}
	}
	tasks[0] = map[string]any{"name": "large", "spec": "large", "tier": "heavy", "effort": "high"}
	a = s.call(4, "tools/call", map[string]any{"name": "plan_create", "arguments": map[string]any{"title": "approval", "tasks": tasks}})
	text, isError = toolText(t, a)
	if isError || !strings.Contains(text, "awaiting approval") || !strings.Contains(text, "large (heavy, effort high)") {
		t.Fatalf("held plan answer = %q, error %v", text, isError)
	}
}

// TestServeCallPlanAmendRespec: plan_amend's respec edits a pending task's
// spec end to end and the text says so; a started task is refused.
func TestServeCallPlanAmendRespec(t *testing.T) {
	e := newEnvToken(t, foremanTokenFile, "foreman-secret-9")
	s := start(t, e)
	a := s.call(1, "tools/call", map[string]any{"name": "plan_create", "arguments": map[string]any{
		"title": "two steps",
		"tasks": []any{
			map[string]any{"name": "a", "spec": "one"},
			map[string]any{"name": "b", "spec": "two", "deps": []any{"a"}},
		},
	}})
	if text, isError := toolText(t, a); isError {
		t.Fatalf("plan_create errored: %s", text)
	}
	a = s.call(2, "tools/call", map[string]any{"name": "plan_amend", "arguments": map[string]any{
		"plan":   1,
		"respec": []any{map[string]any{"task": "b", "spec": "three", "mode": "append"}},
	}})
	text, isError := toolText(t, a)
	if isError || !strings.Contains(text, "respecified task 2") {
		t.Fatalf("respec = %q, error %v", text, isError)
	}
	var spec string
	if err := e.st.View(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT spec FROM tasks WHERE name = 'b'").Scan(&spec)
	}); err != nil {
		t.Fatal(err)
	}
	if spec != "two\n\nthree" {
		t.Errorf("spec = %q", spec)
	}
	a = s.call(3, "tools/call", map[string]any{"name": "plan_amend", "arguments": map[string]any{
		"plan":   1,
		"respec": []any{map[string]any{"task": "a", "spec": "x"}},
	}})
	if text, isError = toolText(t, a); !isError || !strings.Contains(text, "follow-up task") {
		t.Errorf("started task = %q, error %v, want the follow-up advice", text, isError)
	}
}

func TestPlanAmendRespecSchema(t *testing.T) {
	for _, tool := range tools {
		if tool.Name != "plan_amend" {
			continue
		}
		props := tool.InputSchema.(map[string]any)["properties"].(map[string]any)
		rs, ok := props["respec"].(map[string]any)
		if !ok {
			t.Fatal("respec missing from schema")
		}
		fields := rs["items"].(map[string]any)["properties"].(map[string]any)
		for _, field := range []string{"task", "spec", "mode"} {
			if _, ok := fields[field]; !ok {
				t.Errorf("respec missing %s", field)
			}
		}
		return
	}
	t.Fatal("no plan_amend tool")
}

func TestPlanAmendRetierSchema(t *testing.T) {
	for _, tool := range tools {
		if tool.Name != "plan_amend" {
			continue
		}
		props := tool.InputSchema.(map[string]any)["properties"].(map[string]any)
		rt, ok := props["retier"].(map[string]any)
		if !ok {
			t.Fatal("retier missing from schema")
		}
		fields := rt["items"].(map[string]any)["properties"].(map[string]any)
		for _, field := range []string{"task", "tier", "effort"} {
			if _, ok := fields[field]; !ok {
				t.Errorf("retier missing %s", field)
			}
		}
		return
	}
	t.Fatal("no plan_amend tool")
}
