package api

import (
	"fmt"
	"net/http"
	"testing"
)

// TestLandedEndpoint: the runner and client roles report landings
// detected from the default branch's trailers (§7.1); the engine records
// the ones that belong to the repo's project, and the foreman role is
// refused (it runs no git).
func TestLandedEndpoint(t *testing.T) {
	e := newEnv(t)
	body := fmt.Sprintf(`{"repo_url":"https://example.invalid/one.git","branch":"main","landed":[{"task_id":%d,"commit":"abc123"}]}`, e.task)

	code, _, data := e.do(http.MethodPost, "/v1/landed", "r1", "", []byte(body))
	if code != http.StatusOK {
		t.Fatalf("record landed as runner: %d %s", code, data)
	}
	if got := e.str("SELECT commit_sha FROM landings WHERE task_id = ?", e.task); got != "abc123" {
		t.Errorf("commit = %q, want abc123", got)
	}

	// The client role — the attach path — reports the same way.
	code, _, data = e.do(http.MethodPost, "/v1/landed", "alice", "", []byte(body))
	if code != http.StatusOK {
		t.Fatalf("record landed as client: %d %s", code, data)
	}

	// A foreman token cannot: the foreman runs no git (product §5 rule 1).
	code, _, data = e.do(http.MethodPost, "/v1/landed", "foreman1", "", []byte(body))
	if code != http.StatusForbidden {
		t.Fatalf("record landed as foreman: %d %s, want 403", code, data)
	}
}
