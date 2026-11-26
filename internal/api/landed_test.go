package api

import (
	"fmt"
	"net/http"
	"testing"
)

// TestLandedEndpoint: the runner and client roles report landings
// detected from the default branch's trailers (§7.1); the engine records
// the ones that belong to the repo's project and whose trailer branch
// and head decode, and the foreman role is refused (it runs no git).
func TestLandedEndpoint(t *testing.T) {
	e := newEnv(t)
	// The candidate validates only against the task's own recorded
	// branch and approved head (§7.1).
	e.exec("UPDATE tasks SET branch = 'task/1-1' WHERE id = ?", e.task)
	e.exec(`INSERT INTO jobs (project_id, task_id, kind, key, mode, prompt, status, result, created_at, updated_at)
		VALUES (?, ?, 'work', ?, 'fresh', 'the prompt', 'done', '{"result":"{\"head\":\"aa11\"}"}', ?, ?)`,
		e.project1, e.task, fmt.Sprintf("task:%d", e.task), seedTS, seedTS)
	body := fmt.Sprintf(`{"repo_url":"https://example.invalid/one.git","branch":"main","landed":[{"task_id":%d,"commit":"abc123","branch":"task/1-1","head":"aa11"}]}`, e.task)

	code, _, data := e.do(http.MethodPost, "/v1/landed", "r1", "", []byte(body))
	if code != http.StatusOK {
		t.Fatalf("record landed as runner: %d %s", code, data)
	}
	if got := e.str("SELECT commit_sha FROM landings WHERE task_id = ?", e.task); got != "abc123" {
		t.Errorf("commit = %q, want abc123", got)
	}
	if got := e.str("SELECT COALESCE(trailer_branch, '') FROM landings WHERE task_id = ?", e.task); got != "task/1-1" {
		t.Errorf("trailer_branch = %q, want task/1-1", got)
	}
	if got := e.str("SELECT COALESCE(trailer_head, '') FROM landings WHERE task_id = ?", e.task); got != "aa11" {
		t.Errorf("trailer_head = %q, want aa11", got)
	}

	// The client role — the attach path — reports the same way.
	code, _, data = e.do(http.MethodPost, "/v1/landed", "alice", "", []byte(body))
	if code != http.StatusOK {
		t.Fatalf("record landed as client: %d %s", code, data)
	}

	// A foreman token cannot: the foreman has no shell to read the branch
	// with.
	code, _, data = e.do(http.MethodPost, "/v1/landed", "foreman1", "", []byte(body))
	if code != http.StatusForbidden {
		t.Fatalf("record landed as foreman: %d %s, want 403", code, data)
	}
}
