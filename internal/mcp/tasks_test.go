package mcp

import (
	"database/sql"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/prompt"
)

// seedParked parks a task of the token's project under the named
// parking event with the payload verbatim — the state the engine's own
// completion paths write — and returns its id.
func seedParked(t *testing.T, e *env, kind, payload string) int64 {
	t.Helper()
	var task int64
	err := e.st.Update(t.Context(), func(tx *sql.Tx) error {
		res, err := tx.Exec(`INSERT INTO plans (project_id, title, state, created_at, updated_at)
			VALUES (1, 'a plan', 'active', ?, ?)`, seedTS, seedTS)
		if err != nil {
			return err
		}
		plan, err := res.LastInsertId()
		if err != nil {
			return err
		}
		res, err = tx.Exec(`INSERT INTO tasks (plan_id, title, spec, check_cmd, class, state, base_ref, caps, created_at, updated_at)
			VALUES (?, 'a task', 'the spec', 'go test ./...', 'implement', 'needs_foreman', 'main', '{}', ?, ?)`,
			plan, seedTS, seedTS)
		if err != nil {
			return err
		}
		if task, err = res.LastInsertId(); err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO events (kind, project_id, task_id, payload, created_at)
			VALUES (?, 1, ?, ?, ?)`, kind, task, payload, seedTS)
		return err
	})
	if err != nil {
		t.Fatalf("seed the parked task: %v", err)
	}
	return task
}

// strOf reads one text column; NULL reads as the empty string.
func strOf(t *testing.T, e *env, query string, args ...any) string {
	t.Helper()
	var v sql.NullString
	if err := e.st.View(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRow(query, args...).Scan(&v)
	}); err != nil {
		t.Fatalf("query %q %v: %v", query, args, err)
	}
	return v.String
}

// countOf reads one integer column, with the query's arguments.
func countOf(t *testing.T, e *env, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := e.st.View(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRow(query, args...).Scan(&n)
	}); err != nil {
		t.Fatalf("query %q %v: %v", query, args, err)
	}
	return n
}

// TestServeCallAnswer covers the answer tool end to end against a test
// engine (§3, §12.2): the bridge — under a foreman token, as a real one
// runs — answers the parked task in the token's project, the questions
// row is filled, the fix job that resumes the author's session is
// queued with the engine's composed prompt, and the tool's text names
// the question, the job and the answer word. A repeated identical call
// replays the first answer through the bridge's minted idempotency key
// (§12.1): no second job, no second questions row.
func TestServeCallAnswer(t *testing.T) {
	e := newEnvToken(t, foremanTokenFile, "foreman-secret-9")
	task := seedParked(t, e, "task_needs_help", `{"question":"which API should the client call?"}`)
	s := start(t, e)

	a := s.call(1, "tools/call", map[string]any{
		"name":      "answer",
		"arguments": map[string]any{"task": task, "text": "Use the REST API over the Unix socket."},
	})
	text, isError := toolText(t, a)
	if isError {
		t.Fatalf("answer errored: %s", text)
	}
	for _, want := range []string{
		"task " + strconv.FormatInt(task, 10) + " answered",
		"question 1",
		"created",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("answer text = %q, want it to name %s", text, want)
		}
	}

	// The store holds what the call did: the exchange in questions (§11),
	// a session-mode fix job carrying the engine's composed prompt — the
	// answer verbatim in its own delimiters — and the task ready again.
	if question := strOf(t, e, "SELECT question FROM questions WHERE id = 1"); question != "which API should the client call?" {
		t.Errorf("questions.question = %q, want the worker's", question)
	}
	if answer := strOf(t, e, "SELECT answer FROM questions WHERE id = 1"); answer != "Use the REST API over the Unix socket." {
		t.Errorf("questions.answer = %q, want the foreman's, verbatim", answer)
	}
	if got, want := strOf(t, e, "SELECT prompt FROM jobs WHERE kind = 'fix'"),
		prompt.ComposeAnswerPrompt("the spec", "go test ./...", "Use the REST API over the Unix socket."); got != want {
		t.Errorf("answer job prompt = %q\nwant the composed\n%q", got, want)
	}
	if mode := strOf(t, e, "SELECT mode FROM jobs WHERE kind = 'fix'"); mode != "session" {
		t.Errorf("answer job mode = %q, want session", mode)
	}
	if state := strOf(t, e, "SELECT state FROM tasks WHERE id = ?", task); state != "ready" {
		t.Errorf("task state after the answer = %q, want ready", state)
	}

	// The repeat: identical arguments, the same minted key, the first
	// answer replayed — one job, one questions row.
	a = s.call(2, "tools/call", map[string]any{
		"name":      "answer",
		"arguments": map[string]any{"task": task, "text": "Use the REST API over the Unix socket."},
	})
	if again, _ := toolText(t, a); again != text {
		t.Errorf("repeated answer text = %q, want the first answer %q", again, text)
	}
	if n := countOf(t, e, "SELECT COUNT(*) FROM jobs WHERE task_id = ?", task); n != 1 {
		t.Errorf("the task's jobs after the repeat = %d, want 1: the minted key deduped the call", n)
	}
	if n := countOf(t, e, "SELECT COUNT(*) FROM questions"); n != 1 {
		t.Errorf("questions rows after the repeat = %d, want 1", n)
	}
}

// TestServeCallAnswerRefused covers the tool's error path (§12.2): a
// task parked by something other than its worker's question comes back
// as the tool's own error naming the engine's reason, and nothing is
// created.
func TestServeCallAnswerRefused(t *testing.T) {
	e := newEnvToken(t, foremanTokenFile, "foreman-secret-9")
	task := seedParked(t, e, "task_check_failed", `{"reason":"the check failed"}`)
	s := start(t, e)
	a := s.call(1, "tools/call", map[string]any{
		"name":      "answer",
		"arguments": map[string]any{"task": task, "text": "go"},
	})
	text, isError := toolText(t, a)
	if !isError {
		t.Fatalf("answer a check-failed park = %q, want the tool's error", text)
	}
	if !strings.Contains(text, "restart it instead") {
		t.Errorf("error text = %q, want the engine's reason in it", text)
	}
	if n := countOf(t, e, "SELECT COUNT(*) FROM jobs WHERE task_id = ?", task); n != 0 {
		t.Errorf("the task's jobs after the refusal = %d, want none", n)
	}
}

// TestServeCallTaskRestart covers the task_restart tool end to end
// (§3, §5.6, §12.2): a fresh work job for the parked task — restart
// flag, fresh mode, the note verbatim in the composed prompt — the task
// ready again, and the repeated call replaying the first answer through
// the minted key.
func TestServeCallTaskRestart(t *testing.T) {
	e := newEnvToken(t, foremanTokenFile, "foreman-secret-9")
	task := seedParked(t, e, "task_out_of_rounds", `{"reason":"the task's fix_rounds are spent"}`)
	s := start(t, e)

	note := "the last attempt failed because the frobnicator was off; avoid touching it"
	a := s.call(1, "tools/call", map[string]any{
		"name":      "task_restart",
		"arguments": map[string]any{"task": task, "note": note},
	})
	text, isError := toolText(t, a)
	if isError {
		t.Fatalf("task_restart errored: %s", text)
	}
	for _, want := range []string{
		"task " + strconv.FormatInt(task, 10) + " restarted",
		"created",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("task_restart text = %q, want it to name %s", text, want)
		}
	}
	if kind := strOf(t, e, "SELECT kind FROM jobs WHERE task_id = ?", task); kind != "work" {
		t.Errorf("restart job kind = %q, want work", kind)
	}
	if mode := strOf(t, e, "SELECT mode FROM jobs WHERE task_id = ?", task); mode != "fresh" {
		t.Errorf("restart job mode = %q, want fresh", mode)
	}
	if restart := strOf(t, e, "SELECT restart FROM jobs WHERE task_id = ?", task); restart != "1" {
		t.Errorf("restart job restart flag = %q, want 1", restart)
	}
	if got, want := strOf(t, e, "SELECT prompt FROM jobs WHERE task_id = ?", task),
		prompt.ComposeWorkerPrompt("the spec", "go test ./...", note); got != want {
		t.Errorf("restart job prompt = %q\nwant the composed\n%q", got, want)
	}
	if state := strOf(t, e, "SELECT state FROM tasks WHERE id = ?", task); state != "ready" {
		t.Errorf("task state after the restart = %q, want ready", state)
	}

	// The repeat replays the first answer: no second attempt.
	a = s.call(2, "tools/call", map[string]any{
		"name":      "task_restart",
		"arguments": map[string]any{"task": task, "note": note},
	})
	if again, _ := toolText(t, a); again != text {
		t.Errorf("repeated task_restart text = %q, want the first answer %q", again, text)
	}
	if n := countOf(t, e, "SELECT COUNT(*) FROM jobs WHERE task_id = ?", task); n != 1 {
		t.Errorf("the task's jobs after the repeat = %d, want 1: the minted key deduped the call", n)
	}
}

// TestServeCallTaskRetire covers the task_retire tool end to end against
// a test engine (§6.2, §12.2): the bridge — under a foreman token, as a
// real one runs — retires a done task of the token's project named by
// its plan name, the reason verbatim in the tool's text and the event,
// the caller recorded as the foreman, and the repeated call replaying
// the first answer through the minted key. A name no plan holds is the
// tool's own error.
func TestServeCallTaskRetire(t *testing.T) {
	e := newEnvToken(t, foremanTokenFile, "foreman-secret-9")
	task := seedTask(t, e, "done", "orphan")
	s := start(t, e)

	a := s.call(1, "tools/call", map[string]any{
		"name":      "task_retire",
		"arguments": map[string]any{"tasks": []any{"orphan"}, "reason": "superseded by task 274"},
	})
	text, isError := toolText(t, a)
	if isError {
		t.Fatalf("task_retire errored: %s", text)
	}
	for _, want := range []string{
		"retired task " + strconv.FormatInt(task, 10),
		"superseded by task 274",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("task_retire text = %q, want it to name %s", text, want)
		}
	}
	if state := strOf(t, e, "SELECT state FROM tasks WHERE id = ?", task); state != "retired" {
		t.Errorf("task state after the retire = %q, want retired", state)
	}
	if payload := strOf(t, e, "SELECT payload FROM events WHERE kind = 'task_retired' AND task_id = ?", task); payload == "" ||
		!strings.Contains(payload, `"caller":"foreman"`) || !strings.Contains(payload, "superseded by task 274") {
		t.Errorf("task_retired payload = %q, want the foreman caller and the reason verbatim", payload)
	}
	if !slices.Contains(ToolNames(), "task_retire") {
		t.Errorf("the foreman offer lacks task_retire: %v", ToolNames())
	}

	// The repeat replays the first answer: no second event.
	a = s.call(2, "tools/call", map[string]any{
		"name":      "task_retire",
		"arguments": map[string]any{"tasks": []any{"orphan"}, "reason": "superseded by task 274"},
	})
	if again, _ := toolText(t, a); again != text {
		t.Errorf("repeated task_retire text = %q, want the first answer %q", again, text)
	}
	if n := countOf(t, e, "SELECT COUNT(*) FROM events WHERE kind = 'task_retired' AND task_id = ?", task); n != 1 {
		t.Errorf("task_retired events after the repeat = %d, want 1: the minted key deduped the call", n)
	}

	// A name no plan holds is the tool's own error, and an id works too.
	a = s.call(3, "tools/call", map[string]any{
		"name":      "task_retire",
		"arguments": map[string]any{"tasks": []any{"no-such-task"}, "reason": "go"},
	})
	if text, isError := toolText(t, a); !isError || !strings.Contains(text, "no-such-task") {
		t.Errorf("task_retire of an unknown name = %q (error %v), want the tool's error", text, isError)
	}
	other := seedTask(t, e, "needs_foreman", "second")
	a = s.call(4, "tools/call", map[string]any{
		"name":      "task_retire",
		"arguments": map[string]any{"tasks": []any{other}, "reason": "landed by hand"},
	})
	if text, isError := toolText(t, a); isError {
		t.Errorf("task_retire by id errored: %s", text)
	}
	if state := strOf(t, e, "SELECT state FROM tasks WHERE id = ?", other); state != "retired" {
		t.Errorf("second task state = %q, want retired by id", state)
	}
}

// TestServeCallTaskRetireRefused covers the tool's error path: a task
// with a landing record is refused, the engine's reason carried as the
// tool's error, and nothing changes.
func TestServeCallTaskRetireRefused(t *testing.T) {
	e := newEnvToken(t, foremanTokenFile, "foreman-secret-9")
	task := seedTask(t, e, "done", "landed")
	if err := e.st.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO landings (task_id, project_id, target_branch, commit_sha, pushed, created_at)
			VALUES (?, 1, 'main', 'abc123', 'pushed', ?)`, task, seedTS)
		return err
	}); err != nil {
		t.Fatalf("seed the landing: %v", err)
	}
	s := start(t, e)
	a := s.call(1, "tools/call", map[string]any{
		"name":      "task_retire",
		"arguments": map[string]any{"tasks": []any{task}, "reason": "too late"},
	})
	text, isError := toolText(t, a)
	if !isError || !strings.Contains(text, "is landed") {
		t.Errorf("task_retire of a landed task = %q (error %v), want the engine's refusal", text, isError)
	}
	if state := strOf(t, e, "SELECT state FROM tasks WHERE id = ?", task); state != "done" {
		t.Errorf("task state after the refusal = %q, want done: nothing was written", state)
	}
}

// seedTask adds a task of the token's project in the named state, with
// a plan name other tools may name it by, and returns its id.
func seedTask(t *testing.T, e *env, state, name string) int64 {
	t.Helper()
	var task int64
	err := e.st.Update(t.Context(), func(tx *sql.Tx) error {
		res, err := tx.Exec(`INSERT INTO plans (project_id, title, state, created_at, updated_at)
			VALUES (1, 'a plan', 'active', ?, ?)`, seedTS, seedTS)
		if err != nil {
			return err
		}
		plan, err := res.LastInsertId()
		if err != nil {
			return err
		}
		res, err = tx.Exec(`INSERT INTO tasks (plan_id, name, title, spec, check_cmd, class, state, base_ref, caps, created_at, updated_at)
			VALUES (?, ?, 'a task', 'the spec', 'go test ./...', 'implement', ?, 'main', '{}', ?, ?)`,
			plan, name, state, seedTS, seedTS)
		if err != nil {
			return err
		}
		return tx.QueryRow("SELECT last_insert_rowid()").Scan(&task)
	})
	if err != nil {
		t.Fatalf("seed the %s task: %v", state, err)
	}
	return task
}

// TestRenderLandRetry renders each of the land retry's four outcomes as
// its own text (#133): the queued land job, the hold's reason verbatim
// with the user's release, the stale base with task_restart, and a
// refusal's reason as the tool's error.
func TestRenderLandRetry(t *testing.T) {
	const hold = "held by rule gate_land: gate_land is ask"
	const stale = "the landing is refused: the base brigada/task-3 is a superseded result of task 3, now on brigada/task-3-r1; restart the task to rebuild on the new base (task_restart)"
	for _, tc := range []struct {
		name    string
		res     api.LandRetryResult
		want    []string
		isError bool
	}{
		{"queued", api.LandRetryResult{Outcome: engine.LandRetryQueued, JobID: 42}, []string{"queued land job 42 for task 7"}, false},
		{"held", api.LandRetryResult{Outcome: engine.LandRetryHeld, Reason: hold},
			[]string{"task 7 is held at the land gate: " + hold, "brigada land approve 7"}, false},
		{"stale_base", api.LandRetryResult{Outcome: engine.LandRetryStaleBase, Reason: stale},
			[]string{"stale base", stale, "task_restart is the way forward"}, false},
		{"refused", api.LandRetryResult{Outcome: engine.LandRetryRefused, Reason: "task 7 already has a landing record (pushed)"},
			[]string{"task 7 already has a landing record (pushed)"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text, err := renderLandRetry(7, tc.res)
			if tc.isError {
				if err == nil {
					t.Fatalf("render = %q, want the refusal as the tool's error", text)
				}
				text = err.Error()
			} else if err != nil {
				t.Fatalf("render: %v", err)
			}
			for _, want := range tc.want {
				if !strings.Contains(text, want) {
					t.Errorf("render = %q, want it to carry %q", text, want)
				}
			}
		})
	}
}

// TestServeCallLandRetryRefused covers the land_retry tool end to end
// (#133): a task parked by something other than a failed landing is
// refused, the engine's reason carried as the tool's error, and the
// task stays parked with no job.
func TestServeCallLandRetryRefused(t *testing.T) {
	e := newEnvToken(t, foremanTokenFile, "foreman-secret-9")
	task := seedParked(t, e, "task_out_of_rounds", `{"reason":"the task's fix_rounds are spent"}`)
	s := start(t, e)

	a := s.call(1, "tools/call", map[string]any{
		"name":      "land_retry",
		"arguments": map[string]any{"task": task},
	})
	text, isError := toolText(t, a)
	if !isError || !strings.Contains(text, "not by a failed landing") {
		t.Errorf("land_retry = %q (error %v), want the engine's refusal as the tool's error", text, isError)
	}
	if state := strOf(t, e, "SELECT state FROM tasks WHERE id = ?", task); state != "needs_foreman" {
		t.Errorf("task state after the refusal = %q, want still parked", state)
	}
	if n := countOf(t, e, "SELECT COUNT(*) FROM jobs WHERE task_id = ?", task); n != 0 {
		t.Errorf("jobs after the refusal = %d, want none", n)
	}
	if !slices.Contains(ToolNames(), "land_retry") {
		t.Errorf("the foreman offer lacks land_retry: %v", ToolNames())
	}
}
