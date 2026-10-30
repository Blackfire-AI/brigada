package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// The land_retry endpoint's tests (#133): /v1/tasks/{id}/land-retry runs
// the engine's RetryLanding and answers each of its four outcomes —
// queued, held, stale_base, refused — with 200 and the outcome named.

// landRetryResp is the land-retry response as a caller decodes it.
type landRetryResp struct {
	TaskID  int64  `json:"task_id"`
	Outcome string `json:"outcome"`
	JobID   int64  `json:"job_id"`
	Reason  string `json:"reason"`
}

// seedLandRejected parks a task of the plan by a rejected push, the way
// the land job's completion parks it, behind a done work job and a
// substantive approve — a task the land rule would land.
func seedLandRejected(t *testing.T, e *env, plan int64) int64 {
	t.Helper()
	task := seedParked(t, e, plan, "task_land_failed", `{"reason":"push rejected: non-fast-forward"}`)
	verdict, err := json.Marshal(map[string]string{
		"verdict":  "approve",
		"findings": strings.Repeat("the change does what the spec asks and the check covers it. ", 3),
	})
	if err != nil {
		t.Fatalf("encode the verdict: %v", err)
	}
	stored, err := json.Marshal(map[string]string{"result": string(verdict)})
	if err != nil {
		t.Fatalf("encode the stored verdict: %v", err)
	}
	for _, kind := range []string{"work", "review"} {
		result := `{"result":"{}"}`
		if kind == "review" {
			result = string(stored)
		}
		e.exec(`INSERT INTO jobs (project_id, task_id, kind, key, mode, prompt, status, result, created_at, updated_at)
			VALUES ((SELECT project_id FROM plans WHERE id = ?), ?, ?, ?, 'fresh', '', 'done', ?, ?, ?)`,
			plan, task, kind, fmt.Sprintf("task:%d", task), result, seedTS, seedTS)
	}
	return task
}

// landRetry posts the land retry of task with token and decodes a 200.
func (e *env) landRetry(task int64, token, idemKey string) (landRetryResp, []byte) {
	e.t.Helper()
	code, _, data := e.do(http.MethodPost, fmt.Sprintf("/v1/tasks/%d/land-retry", task), token, idemKey, []byte(`{}`))
	if code != http.StatusOK {
		e.t.Fatalf("land retry of task %d: %d %s, want 200", task, code, data)
	}
	return decode[landRetryResp](e.t, data), data
}

// TestTaskLandRetryQueued: the foreman's retry of a task a rejected push
// parked queues its land job, named in the answer, and the idempotent
// repeat replays it without a second job; a runner token is refused.
func TestTaskLandRetryQueued(t *testing.T) {
	e := newEnv(t)
	task := seedLandRejected(t, e, e.plan)

	if code, _, _ := e.do(http.MethodPost, fmt.Sprintf("/v1/tasks/%d/land-retry", task), "r1", "", []byte(`{}`)); code != http.StatusForbidden {
		t.Fatalf("runner retries the landing: %d, want 403", code)
	}
	res, data := e.landRetry(task, "foreman1", "lr-1")
	if res.Outcome != "queued" || res.JobID == 0 || res.TaskID != task {
		t.Fatalf("land retry = %+v, want queued with its land job", res)
	}
	if kind := e.str("SELECT kind FROM jobs WHERE id = ?", res.JobID); kind != "land" {
		t.Errorf("queued job kind = %q, want land", kind)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", task); state != "done" {
		t.Errorf("task state = %q, want done", state)
	}
	code, _, repeat := e.do(http.MethodPost, fmt.Sprintf("/v1/tasks/%d/land-retry", task), "foreman1", "lr-1", []byte(`{}`))
	if code != http.StatusOK || string(repeat) != string(data) {
		t.Errorf("idempotent repeat = %d %s, want the first answer %s", code, repeat, data)
	}
	if n := e.count(t, fmt.Sprintf("SELECT COUNT(*) FROM jobs WHERE task_id = %d AND kind = 'land'", task)); n != 1 {
		t.Errorf("land jobs after the repeat = %d, want 1", n)
	}
}

// TestTaskLandRetryHeld: a land-gate rule holds the retried landing; the
// answer names the hold's reason and no job.
func TestTaskLandRetryHeld(t *testing.T) {
	e := newEnv(t)
	task := seedLandRejected(t, e, e.plan)
	e.exec(`UPDATE projects SET settings = '{"gate_land":"ask"}' WHERE id = ?`, e.project1)

	res, _ := e.landRetry(task, "foreman1", "")
	if res.Outcome != "held" || res.JobID != 0 || !strings.Contains(res.Reason, "gate_land") {
		t.Fatalf("land retry = %+v, want held by gate_land with no job", res)
	}
	if n := e.count(t, fmt.Sprintf("SELECT COUNT(*) FROM land_holds WHERE task_id = %d AND state = 'held'", task)); n != 1 {
		t.Errorf("held land_holds rows = %d, want 1", n)
	}
}

// TestTaskLandRetryStaleBase: a task whose base is a superseded round of
// its dependency is parked again by the land rule; the answer names the
// stale base and task_restart.
func TestTaskLandRetryStaleBase(t *testing.T) {
	e := newEnv(t)
	task := seedLandRejected(t, e, e.plan)
	lib := e.task
	e.exec(`UPDATE tasks SET state = 'done', branch = ? WHERE id = ?`, fmt.Sprintf("brigada/task-%d-r1", lib), lib)
	e.exec(`INSERT INTO landings (task_id, project_id, target_branch, commit_sha, pushed, created_at)
		VALUES (?, ?, 'main', 'lib-1', 'pushed', ?)`, lib, e.project1, seedTS)
	e.exec(`INSERT INTO task_deps (plan_id, task_id, depends_on_task_id) VALUES (?, ?, ?)`, e.plan, task, lib)
	e.exec(`UPDATE tasks SET base_ref = ? WHERE id = ?`, fmt.Sprintf("brigada/task-%d", lib), task)

	res, _ := e.landRetry(task, "foreman1", "")
	if res.Outcome != "stale_base" || res.JobID != 0 || !strings.Contains(res.Reason, "task_restart") ||
		!strings.Contains(res.Reason, fmt.Sprintf("brigada/task-%d-r1", lib)) {
		t.Fatalf("land retry = %+v, want stale_base naming the new base and task_restart", res)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", task); state != "needs_foreman" {
		t.Errorf("task state = %q, want needs_foreman again", state)
	}
}

// TestTaskLandRetryRefused: the engine's refusal passes through with its
// reason and nothing changes; a task of another project is the foreman's
// scope error; a missing task is a 404.
func TestTaskLandRetryRefused(t *testing.T) {
	e := newEnv(t)
	parked := seedParked(t, e, e.plan, "task_out_of_rounds", `{"reason":"the task's fix_rounds are spent"}`)
	res, _ := e.landRetry(parked, "foreman1", "")
	if res.Outcome != "refused" || res.JobID != 0 || !strings.Contains(res.Reason, "not by a failed landing") {
		t.Fatalf("land retry = %+v, want refused naming the park", res)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", parked); state != "needs_foreman" {
		t.Errorf("task state after the refusal = %q, want still parked", state)
	}
	if n := countJobs(t, e, parked); n != 0 {
		t.Errorf("jobs after the refusal = %d, want none", n)
	}

	e.exec(`INSERT INTO plans (project_id, title, state, created_at, updated_at) VALUES (?, 'other', 'active', ?, ?)`,
		e.project2, seedTS, seedTS)
	plan2 := int64(e.count(t, fmt.Sprintf("SELECT MAX(id) FROM plans WHERE project_id = %d", e.project2)))
	other := seedLandRejected(t, e, plan2)
	code, _, data := e.do(http.MethodPost, fmt.Sprintf("/v1/tasks/%d/land-retry", other), "foreman1", "", []byte(`{}`))
	if code != http.StatusForbidden {
		t.Errorf("foreman retries another project's task: %d %s, want 403", code, data)
	}
	if n := countJobs(t, e, other); n != 2 {
		t.Errorf("the other project's task jobs = %d, want its seeded two only", n)
	}
	if code, _, _ = e.do(http.MethodPost, "/v1/tasks/99999/land-retry", "foreman1", "", []byte(`{}`)); code != http.StatusNotFound {
		t.Errorf("retry a missing task: %d, want 404", code)
	}
}
