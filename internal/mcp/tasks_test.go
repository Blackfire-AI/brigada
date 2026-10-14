package mcp

import (
	"database/sql"
	"strconv"
	"strings"
	"testing"

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
		prompt.ComposeAnswerPrompt("the spec", "Use the REST API over the Unix socket."); got != want {
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
		prompt.ComposeWorkerPrompt("the spec", note); got != want {
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
