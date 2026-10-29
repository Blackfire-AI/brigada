package mcp

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// issueFetchStub is the bridge tests' issue fetch: no test reaches the
// network (§12.2), so the env's engine reads this instead of gh. A test
// that needs its own answer or refusal swaps the var before newEnv and
// restores it with t.Cleanup.
var issueFetchStub = func(_ context.Context, repoURL string, number int, comments bool) (*engine.Issue, error) {
	return &engine.Issue{Number: number, Title: "the seeded issue", Body: "the seeded issue's body", Author: "octocat"}, nil
}

// TestServeCallIssueRead covers the issue_read tool (§12.2) against a
// test engine: the tool is in the offer, it takes a number and no
// repository argument, and its answer carries the fetched issue inside
// <issue> data delimiters — the repository the engine read being the
// foreman token's own project, never one the call named.
func TestServeCallIssueRead(t *testing.T) {
	var gotRepo string
	var gotNumber int
	var gotComments bool
	old := issueFetchStub
	issueFetchStub = func(_ context.Context, repoURL string, number int, comments bool) (*engine.Issue, error) {
		gotRepo, gotNumber, gotComments = repoURL, number, comments
		return &engine.Issue{Number: number, Title: "Fix the frobnicator", Body: "it frobs", Author: "octocat",
			PullRequest: true, Comments: []engine.IssueComment{{Author: "reviewer", Body: "seconded"}}}, nil
	}
	t.Cleanup(func() { issueFetchStub = old })
	e := newEnvToken(t, foremanTokenFile, "foreman-secret-9")
	s := start(t, e)

	a := s.call(5, "tools/call", map[string]any{"name": "issue_read", "arguments": map[string]any{"number": 12, "comments": true}})
	text, isError := toolText(t, a)
	if isError {
		t.Fatalf("issue_read errored: %s", text)
	}
	for _, want := range []string{
		"\n<issue>\n",
		"#12 Fix the frobnicator (pull request)",
		"by octocat",
		"it frobs",
		"comment by reviewer:",
		"seconded",
		"\n</issue>\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("issue_read text = %q, want it to contain %q", text, want)
		}
	}
	if want := "https://example.invalid/x.git"; gotRepo != want {
		t.Errorf("the engine read %q, want the token's own project %q: the repository is never an argument", gotRepo, want)
	}
	if gotNumber != 12 || !gotComments {
		t.Errorf("fetch got number=%d comments=%v, want 12 and true", gotNumber, gotComments)
	}

	// The offer carries the tool, and its schema takes a number alone:
	// no repository argument exists to take.
	tool, ok := toolByName(tools, "issue_read")
	if !ok {
		t.Fatalf("the foreman offer lost issue_read: %v", ToolNames())
	}
	schema, _ := tool.InputSchema.(map[string]any)
	props, _ := schema["properties"].(map[string]any)
	if len(props) == 0 || strings.Contains(strings.Join(propNames(props), " "), "repo") {
		t.Errorf("issue_read properties = %v, want the number (and comments) alone", propNames(props))
	}
}

// propNames lists a schema's property names, for the failure message.
func propNames(props map[string]any) []string {
	var out []string
	for name := range props {
		out = append(out, name)
	}
	return out
}

// TestServeCallIssueReadFailure covers the fetch failure (§12.2): the
// engine's reason comes back as the tool's own error — isError with the
// words the foreman reports — and never a fallback answer.
func TestServeCallIssueReadFailure(t *testing.T) {
	old := issueFetchStub
	issueFetchStub = func(_ context.Context, repoURL string, number int, comments bool) (*engine.Issue, error) {
		return nil, fmt.Errorf("%w: gh is not logged in on the engine's host", engine.ErrIssueRead)
	}
	t.Cleanup(func() { issueFetchStub = old })
	e := newEnvToken(t, foremanTokenFile, "foreman-secret-9")
	s := start(t, e)

	a := s.call(3, "tools/call", map[string]any{"name": "issue_read", "arguments": map[string]any{"number": 7}})
	text, isError := toolText(t, a)
	if !isError {
		t.Fatalf("issue_read over a failed fetch = %q, want isError", text)
	}
	if !strings.Contains(text, "gh is not logged in") {
		t.Errorf("error text = %q, want the engine's reason in it", text)
	}
}
