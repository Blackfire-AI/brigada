package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// issueFetchStub is the API tests' issue fetch: no test reaches the
// network (§12.2), so the env's engine reads this instead of gh. A test
// that needs its own answer or refusal swaps the var before newEnv and
// restores it with t.Cleanup.
var issueFetchStub = func(_ context.Context, repoURL string, number int, comments bool) (*engine.Issue, error) {
	return &engine.Issue{Number: number, Title: "the seeded issue", Body: "the seeded issue's body", Author: "octocat"}, nil
}

// TestIssueRead covers the issues endpoint (§12.1, §12.2): the foreman
// role alone may call it, the repository is the token's own project's —
// never an argument — and the number and the comments flag reach the
// engine as given.
func TestIssueRead(t *testing.T) {
	var gotRepo string
	var gotNumber int
	var gotComments bool
	old := issueFetchStub
	issueFetchStub = func(_ context.Context, repoURL string, number int, comments bool) (*engine.Issue, error) {
		gotRepo, gotNumber, gotComments = repoURL, number, comments
		return &engine.Issue{Number: number, Title: "Fix the frobnicator", Body: "it frobs", Author: "octocat"}, nil
	}
	t.Cleanup(func() { issueFetchStub = old })
	e := newEnv(t)

	code, _, data := e.do(http.MethodGet, "/v1/issues/12?comments=1", "foreman1", "", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /v1/issues/12: %d %s", code, data)
	}
	res := decode[engine.Issue](e.t, data)
	if res.Title != "Fix the frobnicator" || res.Author != "octocat" {
		t.Errorf("issue = %+v, want the fetched one", res)
	}
	if want := "https://example.invalid/one.git"; gotRepo != want {
		t.Errorf("fetch read %q, want the foreman token's own project %q", gotRepo, want)
	}
	if gotNumber != 12 || !gotComments {
		t.Errorf("fetch got number=%d comments=%v, want 12 and true", gotNumber, gotComments)
	}

	// The client role may not call it: the issues group is the
	// foreman's alone (§12.1).
	if code, _, _ := e.do(http.MethodGet, "/v1/issues/12", "alice", "", nil); code != http.StatusForbidden {
		t.Errorf("client token got %d, want 403", code)
	}
	// The endpoint reads alone: any other method is refused.
	if code, _, _ := e.do(http.MethodPost, "/v1/issues/12", "foreman1", "", []byte("{}")); code != http.StatusMethodNotAllowed {
		t.Errorf("POST got %d, want 405", code)
	}
	// The number must name something.
	if code, _, data := e.do(http.MethodGet, "/v1/issues/0", "foreman1", "", nil); code != http.StatusBadRequest {
		t.Errorf("number 0 got %d %s, want 400", code, data)
	}
}

// TestIssueReadRefusal covers the failure the foreman can report
// (§12.2): a number that does not exist answers not-found with the
// reason, and an unreadable repository answers the caller's refusal
// with it — never a fallback search.
func TestIssueReadRefusal(t *testing.T) {
	old := issueFetchStub
	issueFetchStub = func(_ context.Context, repoURL string, number int, comments bool) (*engine.Issue, error) {
		if number == 99 {
			return nil, fmt.Errorf("%w (gh: Not Found)", engine.ErrIssueNotFound)
		}
		return nil, fmt.Errorf("%w: gh is not logged in on the engine's host", engine.ErrIssueRead)
	}
	t.Cleanup(func() { issueFetchStub = old })
	e := newEnv(t)

	code, _, data := e.do(http.MethodGet, "/v1/issues/99", "foreman1", "", nil)
	if code != http.StatusNotFound {
		t.Fatalf("GET /v1/issues/99: %d %s, want 404", code, data)
	}
	res := decode[errBody](e.t, data)
	if !strings.Contains(res.Error, "no such issue or pull request") {
		t.Errorf("refusal = %q, want the reason in it", res.Error)
	}

	code, _, data = e.do(http.MethodGet, "/v1/issues/7", "foreman1", "", nil)
	if code != http.StatusBadRequest {
		t.Fatalf("GET /v1/issues/7: %d %s, want 400", code, data)
	}
	res = decode[errBody](e.t, data)
	if !strings.Contains(res.Error, "gh is not logged in") {
		t.Errorf("refusal = %q, want the reason in it", res.Error)
	}
}
