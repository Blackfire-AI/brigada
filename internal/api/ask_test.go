package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// TestAskRoute: only the client and admin roles may send the foreman a
// user message (§4.2 item 3, §12.1) — a foreman or runner token is
// refused, so no turn's tool call and no worker's output can speak as
// the user (§1.4). The client's message wakes a turn and the answer
// names the outcome; an empty message is a bad request, an unknown repo
// a not-found, and with two projects one must be named.
func TestAskRoute(t *testing.T) {
	e := newEnv(t)
	body := func(repo, text string) []byte {
		return attachBody(map[string]any{"repo_url": repo, "text": text})
	}
	for _, who := range []string{"foreman1", "r1"} {
		if code, _, data := e.do(http.MethodPost, "/v1/foreman/messages", who, "", body(repo1, "speak for the user")); code != http.StatusForbidden {
			t.Errorf("ask as %s = %d %s, want 403", who, code, data)
		}
	}
	code, _, data := e.do(http.MethodPost, "/v1/foreman/messages", "alice", "", body(repo1, "add the discoveries field"))
	if code != http.StatusOK {
		t.Fatalf("ask as the client = %d %s, want 200", code, data)
	}
	if res := decode[engine.AskResult](t, data); res.ProjectID != e.project1 || res.Outcome != engine.WakeWoken || res.EventID == 0 {
		t.Errorf("ask = %+v, want woken on project %d", res, e.project1)
	}
	cases := []struct {
		name, repo, text string
		want             int
		says             string
	}{
		{"empty message", repo1, " ", http.StatusBadRequest, "empty"},
		{"unknown repo", "https://example.invalid/none.git", "hi", http.StatusNotFound, "no project"},
		{"two projects, none named", "", "hi", http.StatusNotFound, "name one"},
	}
	for _, tc := range cases {
		code, _, data := e.do(http.MethodPost, "/v1/foreman/messages", "alice", "", body(tc.repo, tc.text))
		if code != tc.want || !strings.Contains(string(data), tc.says) {
			t.Errorf("%s = %d %s, want %d saying %q", tc.name, code, data, tc.want, tc.says)
		}
	}
}

// TestReviewRoute: only the client and admin roles may queue a
// standalone review (§7, §12.1); a blank branch is a bad request and an
// unknown repo a not-found.
func TestReviewRoute(t *testing.T) {
	e := newEnv(t)
	body := func(repo, branch string) []byte {
		return attachBody(map[string]any{"repo_url": repo, "branch": branch})
	}
	for _, who := range []string{"foreman1", "r1"} {
		if code, _, data := e.do(http.MethodPost, "/v1/reviews", who, "", body(repo1, "feature/x")); code != http.StatusForbidden {
			t.Errorf("review as %s = %d %s, want 403", who, code, data)
		}
	}
	if code, _, data := e.do(http.MethodPost, "/v1/reviews", "alice", "", body(repo1, " ")); code != http.StatusBadRequest || !strings.Contains(string(data), "branch") {
		t.Errorf("blank branch = %d %s, want 400 naming the branch", code, data)
	}
	if code, _, data := e.do(http.MethodPost, "/v1/reviews", "alice", "", body("https://example.invalid/none.git", "feature/x")); code != http.StatusNotFound {
		t.Errorf("unknown repo = %d %s, want 404", code, data)
	}
}
