package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestReadIssueFetchesOwnRepository covers the engine's issue read
// (§12.1 issues, §12.2): the repository comes from the project's own
// repo URL, never from the call, and the number and the comments flag
// reach the fetch as given. The fetch is injected, so the test reads no
// forge.
func TestReadIssueFetchesOwnRepository(t *testing.T) {
	var gotRepo string
	var gotNumber int
	var gotComments bool
	e := newEnvCfg(t, func(c *Config) {
		c.IssueFetch = func(_ context.Context, repoURL string, number int, comments bool) (*Issue, error) {
			gotRepo, gotNumber, gotComments = repoURL, number, comments
			return &Issue{Number: number, Title: "Fix the frobnicator", Body: "it frobs", Author: "octocat",
				PullRequest: true, Comments: []IssueComment{{Author: "reviewer", Body: "seconded", CreatedAt: "2026-10-12T12:00:00Z"}}, URL: "https://example.invalid/x/issues/7"}, nil
		}
	})
	iss, err := e.eng.ReadIssue(e.ctx, e.project, 7, true)
	if err != nil {
		t.Fatalf("ReadIssue: %v", err)
	}
	if want := "https://example.invalid/brigada.git"; gotRepo != want {
		t.Errorf("fetch read %q, want the project's own repo URL %q", gotRepo, want)
	}
	if gotNumber != 7 || !gotComments {
		t.Errorf("fetch got number=%d comments=%v, want 7 and true", gotNumber, gotComments)
	}
	if iss.Title != "Fix the frobnicator" || !iss.PullRequest || len(iss.Comments) != 1 || iss.Comments[0].Author != "reviewer" {
		t.Errorf("issue = %+v, want the fetched one whole", iss)
	}
}

// TestReadIssueRefusals covers the reasons the engine answers instead
// of an issue (§12.2): a repository not on GitHub — the default fetch
// refuses it before any gh runs — a project that does not exist, and a
// number that is not positive.
func TestReadIssueRefusals(t *testing.T) {
	e := newEnv(t)
	_, err := e.eng.ReadIssue(e.ctx, e.project, 5, false)
	if !errors.Is(err, ErrIssueRead) || !strings.Contains(err.Error(), "not on GitHub") {
		t.Errorf("ReadIssue of a project off GitHub = %v, want ErrIssueRead saying the repository is not on GitHub", err)
	}
	_, err = e.eng.ReadIssue(e.ctx, e.project+100, 5, false)
	if !errors.Is(err, ErrIssueRead) || !strings.Contains(err.Error(), "no such project") {
		t.Errorf("ReadIssue of a missing project = %v, want ErrIssueRead saying no such project", err)
	}
	if _, err = e.eng.ReadIssue(e.ctx, e.project, 0, false); err == nil || strings.Contains(err.Error(), "issue unreadable") {
		t.Errorf("ReadIssue of number 0 = %v, want the caller's own mistake, not a fetch refusal", err)
	}
}
