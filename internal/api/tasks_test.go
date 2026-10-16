package api

import (
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// str reads one text column of the api test env's store; NULL reads as
// the empty string.
func (e *env) str(query string, args ...any) string {
	e.t.Helper()
	var v sql.NullString
	if err := e.st.View(e.ctx, func(tx *sql.Tx) error {
		return tx.QueryRow(query, args...).Scan(&v)
	}); err != nil {
		e.t.Fatalf("query %q %v: %v", query, args, err)
	}
	return v.String
}

// seedParked inserts a task under the plan, parked at needs_foreman
// under the named parking event with the payload verbatim — the state
// the engine's own completion paths write — and returns its id. The
// event's project is the plan's.
func seedParked(t *testing.T, e *env, plan int64, kind, payload string) int64 {
	t.Helper()
	var task int64
	err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`INSERT INTO tasks (plan_id, title, spec, check_cmd, class, state, base_ref, caps, created_at, updated_at)
			VALUES (?, 'a task', 'the spec', 'go test ./...', 'implement', 'needs_foreman', 'main', '{}', ?, ?)`,
			plan, seedTS, seedTS)
		if err != nil {
			return err
		}
		if task, err = res.LastInsertId(); err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO events (kind, project_id, task_id, payload, created_at)
			VALUES (?, (SELECT project_id FROM plans WHERE id = ?), ?, ?, ?)`, kind, plan, task, payload, seedTS)
		return err
	})
	if err != nil {
		t.Fatalf("seed the parked task: %v", err)
	}
	return task
}

// countJobs counts the task's jobs.
func countJobs(t *testing.T, e *env, task int64) int64 {
	t.Helper()
	var n int64
	if err := e.st.View(e.ctx, func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT COUNT(*) FROM jobs WHERE task_id = ?", task).Scan(&n)
	}); err != nil {
		t.Fatalf("count the task's jobs: %v", err)
	}
	return n
}

// TestTaskGetEndpoint covers GET /v1/tasks/{id}: a client reads the
// task's repo, base ref and result branch; a missing task is a 404; a
// runner token may not read.
func TestTaskGetEndpoint(t *testing.T) {
	e := newEnv(t)
	task := seedParked(t, e, e.plan, "task_needs_help", `{"question":"q"}`)
	if err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec("UPDATE tasks SET branch = 'brigada/task-1-r2' WHERE id = ?", task)
		return err
	}); err != nil {
		t.Fatalf("set the branch: %v", err)
	}
	path := fmt.Sprintf("/v1/tasks/%d", task)
	code, _, data := e.do(http.MethodGet, path, "foreman1", "", nil)
	if code != http.StatusOK {
		t.Fatalf("read a task: %d %s, want 200", code, data)
	}
	for _, want := range []string{`"base_ref":"main"`, `"branch":"brigada/task-1-r2"`, `"repo_url":`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("task body %s lacks %s", data, want)
		}
	}
	if code, _, _ := e.do(http.MethodGet, "/v1/tasks/99999", "foreman1", "", nil); code != http.StatusNotFound {
		t.Errorf("read a missing task: %d, want 404", code)
	}
	if code, _, _ := e.do(http.MethodGet, path, "r1", "", nil); code != http.StatusForbidden {
		t.Errorf("runner reads a task: %d, want 403", code)
	}
}

// TestTaskAnswerEndpoint covers /v1/tasks/{id}/answer (§3, §12.1): the
// foreman answers its own project's parked task, a foreman token
// outside its project is a 403, a runner may not, the engine's
// refusals ride as 400s, and an idempotent repeat returns the first
// result.
func TestTaskAnswerEndpoint(t *testing.T) {
	e := newEnv(t)
	park := seedParked(t, e, e.plan, "task_needs_help", `{"question":"which API should the client call?"}`)

	// The role check: a runner token may not answer.
	if code, _, _ := e.do(http.MethodPost, fmt.Sprintf("/v1/tasks/%d/answer", park), "r1", "", []byte(`{"text":"go"}`)); code != http.StatusForbidden {
		t.Fatalf("runner answers: %d, want 403", code)
	}
	// A task that is not there is a 404, uniform JSON.
	if code, _, data := e.do(http.MethodPost, "/v1/tasks/99999/answer", "foreman1", "", []byte(`{"text":"go"}`)); code != http.StatusNotFound {
		t.Fatalf("answer a missing task: %d %s, want 404", code, data)
	}

	// The foreman answers its own project's task (§12.1: the task's id
	// names the project; the token's scope is the wall).
	body := `{"text":"Use the REST API over the Unix socket."}`
	code, _, data := e.do(http.MethodPost, fmt.Sprintf("/v1/tasks/%d/answer", park), "foreman1", "ans-1", []byte(body))
	if code != http.StatusOK {
		t.Fatalf("foreman answers: %d %s, want 200", code, data)
	}
	res := decode[struct {
		Answer     string `json:"answer"`
		QuestionID int64  `json:"question_id"`
		JobID      int64  `json:"job_id"`
		Run        int    `json:"run"`
	}](t, data)
	if res.Answer != "created" || res.QuestionID == 0 || res.JobID == 0 {
		t.Fatalf("answer = %+v, want created with a question row and a job", res)
	}
	if question := e.str("SELECT question FROM questions WHERE id = ?", res.QuestionID); question != "which API should the client call?" {
		t.Errorf("questions.question = %q, want the worker's", question)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", park); state != "ready" {
		t.Errorf("task state after the answer = %q, want ready", state)
	}
	if kind, mode := e.str("SELECT kind FROM jobs WHERE id = ?", res.JobID),
		e.str("SELECT mode FROM jobs WHERE id = ?", res.JobID); kind != "fix" || mode != "session" {
		t.Errorf("answer job = %s/%s, want a session-mode fix job", kind, mode)
	}

	// An idempotent repeat returns the first result and creates nothing.
	code, _, repeat := e.do(http.MethodPost, fmt.Sprintf("/v1/tasks/%d/answer", park), "foreman1", "ans-1", []byte(body))
	if code != http.StatusOK {
		t.Fatalf("idempotent repeat: %d %s, want 200", code, repeat)
	}
	if string(repeat) != string(data) {
		t.Errorf("repeat = %s, want the first answer %s", repeat, data)
	}
	if n := countJobs(t, e, park); n != 1 {
		t.Errorf("jobs after the repeat = %d, want 1: the repeat created nothing", n)
	}
	// The same key with another body is a 409 (§12.1).
	if code, _, _ := e.do(http.MethodPost, fmt.Sprintf("/v1/tasks/%d/answer", park), "foreman1", "ans-1", []byte(`{"text":"another"}`)); code != http.StatusConflict {
		t.Errorf("same key, another body: %d, want 409", code)
	}

	// A client may answer too — the debug path (§12.1).
	other := seedParked(t, e, e.plan, "task_needs_help", `{"question":"sources?"}`)
	if code, _, _ = e.do(http.MethodPost, fmt.Sprintf("/v1/tasks/%d/answer", other), "alice", "", []byte(`{"text":"go"}`)); code != http.StatusOK {
		t.Fatalf("client answers: %d, want 200", code)
	}

	// The engine's refusals ride as 400s naming the reason (§12.1): an
	// empty text, and a task parked by something other than a question.
	empty := seedParked(t, e, e.plan, "task_needs_help", `{"question":"words?"}`)
	if code, _, data = e.do(http.MethodPost, fmt.Sprintf("/v1/tasks/%d/answer", empty), "foreman1", "", []byte(`{"text":""}`)); code != http.StatusBadRequest {
		t.Fatalf("answer with no text: %d %s, want 400", code, data)
	}
	checkFailed := seedParked(t, e, e.plan, "task_check_failed", `{"reason":"the check failed"}`)
	if code, _, data = e.do(http.MethodPost, fmt.Sprintf("/v1/tasks/%d/answer", checkFailed), "foreman1", "", []byte(`{"text":"go"}`)); code != http.StatusBadRequest {
		t.Fatalf("answer a check-failed park: %d %s, want 400", code, data)
	}
	if !strings.Contains(decode[errBody](t, data).Error, "restart it instead") {
		t.Errorf("refusal = %s, want it to name the restart", data)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", checkFailed); state != "needs_foreman" {
		t.Errorf("check-failed task after the refusal = %q, want still parked", state)
	}

	// A foreman token is walled into its own project (§12.1).
	var foreignPlan int64
	if err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`INSERT INTO plans (project_id, title, state, created_at, updated_at)
			VALUES (?, 'another plan', 'active', ?, ?)`, e.project2, seedTS, seedTS)
		if err != nil {
			return err
		}
		foreignPlan, err = res.LastInsertId()
		return err
	}); err != nil {
		t.Fatalf("seed the other project's plan: %v", err)
	}
	foreign := seedParked(t, e, foreignPlan, "task_needs_help", `{"question":"mine?"}`)
	if code, _, data = e.do(http.MethodPost, fmt.Sprintf("/v1/tasks/%d/answer", foreign), "foreman1", "", []byte(`{"text":"no"}`)); code != http.StatusForbidden {
		t.Fatalf("foreman answers another project's task: %d %s, want 403", code, data)
	}
	if err := decode[errBody](t, data); err.Code != codeForbidden {
		t.Errorf("scope error code = %q, want %q", err.Code, codeForbidden)
	}
}

// TestTaskRestartEndpoint covers /v1/tasks/{id}/restart (§3, §5.6,
// §12.1): a fresh work attempt for a parked task of any parker, the
// note verbatim, the spent restart budget refused with the task left
// parked, and the idempotent repeat.
func TestTaskRestartEndpoint(t *testing.T) {
	e := newEnv(t)
	parked := seedParked(t, e, e.plan, "task_out_of_rounds", `{"reason":"the task's fix_rounds are spent"}`)

	if code, _, _ := e.do(http.MethodPost, fmt.Sprintf("/v1/tasks/%d/restart", parked), "r1", "", []byte(`{}`)); code != http.StatusForbidden {
		t.Fatalf("runner restarts: %d, want 403", code)
	}

	note := "the last attempt failed because the frobnicator was off; avoid touching it"
	body := fmt.Sprintf(`{"note":%q}`, note)
	code, _, data := e.do(http.MethodPost, fmt.Sprintf("/v1/tasks/%d/restart", parked), "foreman1", "rst-1", []byte(body))
	if code != http.StatusOK {
		t.Fatalf("foreman restarts: %d %s, want 200", code, data)
	}
	res := decode[struct {
		Answer string `json:"answer"`
		JobID  int64  `json:"job_id"`
		Run    int    `json:"run"`
	}](t, data)
	if res.Answer != "created" || res.JobID == 0 {
		t.Fatalf("restart = %+v, want created with a job", res)
	}
	for _, check := range []struct {
		query string
		want  string
		what  string
	}{
		{`SELECT kind FROM jobs WHERE id = ?`, "work", "kind"},
		{`SELECT mode FROM jobs WHERE id = ?`, "fresh", "mode"},
		{`SELECT restart FROM jobs WHERE id = ?`, "1", "restart flag"},
	} {
		if got := e.str(check.query, res.JobID); got != check.want {
			t.Errorf("restart job %s = %q, want %q", check.what, got, check.want)
		}
	}
	if prompt := e.str("SELECT prompt FROM jobs WHERE id = ?", res.JobID); !strings.Contains(prompt, note) {
		t.Errorf("restart prompt = %q, want the note verbatim in it", prompt)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", parked); state != "ready" {
		t.Errorf("task state after the restart = %q, want ready", state)
	}

	code, _, repeat := e.do(http.MethodPost, fmt.Sprintf("/v1/tasks/%d/restart", parked), "foreman1", "rst-1", []byte(body))
	if code != http.StatusOK || string(repeat) != string(data) {
		t.Errorf("idempotent repeat = %d %s, want the first answer %s", code, repeat, data)
	}
	if n := countJobs(t, e, parked); n != 1 {
		t.Errorf("jobs after the repeat = %d, want 1", n)
	}

	// The spent restart budget refuses plainly and creates nothing: the
	// task's caps name a budget of zero.
	spent := seedParked(t, e, e.plan, "task_needs_help", `{"question":"again?"}`)
	e.exec(`UPDATE tasks SET caps = '{"restart":0}' WHERE id = ?`, spent)
	if code, _, data = e.do(http.MethodPost, fmt.Sprintf("/v1/tasks/%d/restart", spent), "foreman1", "", []byte(`{}`)); code != http.StatusBadRequest {
		t.Fatalf("restart past the budget: %d %s, want 400", code, data)
	}
	if !strings.Contains(decode[errBody](t, data).Error, "restart budget") {
		t.Errorf("refusal = %s, want the budget named", data)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", spent); state != "needs_foreman" {
		t.Errorf("task state after the spent budget = %q, want still parked", state)
	}
	if n := countJobs(t, e, spent); n != 0 {
		t.Errorf("jobs after the spent budget = %d, want none", n)
	}

	// A task that is not parked refuses; a missing task is a 404.
	ready := seedParked(t, e, e.plan, "task_needs_help", `{"question":"x"}`)
	e.exec(`UPDATE tasks SET state = 'ready' WHERE id = ?`, ready)
	if code, _, _ = e.do(http.MethodPost, fmt.Sprintf("/v1/tasks/%d/restart", ready), "foreman1", "", []byte(`{}`)); code != http.StatusBadRequest {
		t.Errorf("restart a ready task: %d, want 400", code)
	}
	if code, _, _ = e.do(http.MethodPost, "/v1/tasks/99999/restart", "foreman1", "", []byte(`{}`)); code != http.StatusNotFound {
		t.Errorf("restart a missing task: %d, want 404", code)
	}
}
