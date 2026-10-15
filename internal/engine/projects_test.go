package engine

import (
	"errors"
	"strings"
	"testing"
)

// Setting the foreman of a repo the engine has no project for creates
// the project — with the conventional default branch, setup having no
// base ref to derive one from — stores the choice, and logs it.
func TestSetProjectForemanCreatesProject(t *testing.T) {
	e := newEnv(t)
	repo := "https://example.invalid/new.git"

	set, err := e.eng.SetProjectForeman(e.ctx, repo, "claude", "opus")
	if err != nil {
		t.Fatalf("set the foreman of %s: %v", repo, err)
	}
	if set.ProjectID == 0 || set.RepoURL != repo || set.Harness != "claude" || set.Model != "opus" {
		t.Fatalf("the setting = %+v; want the repo, claude and opus on a real project id", set)
	}
	if got := e.int("SELECT COUNT(*) FROM projects WHERE repo_url = ?", repo); got != 1 {
		t.Fatalf("projects for %s = %d, want 1", repo, got)
	}
	if got := e.str("SELECT default_branch FROM projects WHERE id = ?", set.ProjectID); got != setupDefaultBranch {
		t.Errorf("a setup-born project's default branch = %q, want %s", got, setupDefaultBranch)
	}

	// The choice reads back the same.
	got, err := e.eng.ProjectForeman(e.ctx, repo)
	if err != nil {
		t.Fatalf("read the foreman back: %v", err)
	}
	if got == nil || *got != set {
		t.Fatalf("the setting read back = %+v, want %+v", got, set)
	}

	// The event names the choice in its payload (§9).
	if n := e.int("SELECT COUNT(*) FROM events WHERE kind = 'project_foreman_set' AND project_id = ? AND task_id IS NULL AND job_id IS NULL", set.ProjectID); n != 1 {
		t.Fatalf("project_foreman_set events = %d, want 1", n)
	}
	if payload := e.str("SELECT payload FROM events WHERE kind = 'project_foreman_set' AND project_id = ?", set.ProjectID); payload != `{"harness":"claude","model":"opus"}` {
		t.Errorf("the event's payload = %s, want the harness and model", payload)
	}
}

// Setting the foreman of a repo with a project updates the row in place:
// no second project, the new values in place of the old, the event
// logged again.
func TestSetProjectForemanUpdatesProject(t *testing.T) {
	e := newEnv(t)
	repo := "https://example.invalid/brigada.git" // the fixture's project

	set, err := e.eng.SetProjectForeman(e.ctx, repo, "claude", "opus")
	if err != nil {
		t.Fatalf("set the foreman: %v", err)
	}
	if set.ProjectID != e.project {
		t.Fatalf("the setting's project = %d, want the existing %d", set.ProjectID, e.project)
	}
	if got := e.str("SELECT foreman_model FROM projects WHERE id = ?", e.project); got != "opus" {
		t.Errorf("foreman_model = %q, want opus: the choice replaces the old one", got)
	}
	if got := e.int("SELECT COUNT(*) FROM projects"); got != 1 {
		t.Errorf("projects = %d, want 1: an update creates nothing", got)
	}
	if got := e.int("SELECT COUNT(*) FROM events WHERE kind = 'project_foreman_set' AND project_id = ?", e.project); got != 1 {
		t.Errorf("project_foreman_set events = %d, want 1", got)
	}

	got, err := e.eng.ProjectForeman(e.ctx, repo)
	if err != nil {
		t.Fatalf("read the foreman back: %v", err)
	}
	if got == nil || got.Harness != "claude" || got.Model != "opus" {
		t.Fatalf("the setting read back = %+v, want claude on opus", got)
	}
}

// Setup refuses what a foreman cannot run on: no repo, no harness, no
// model, or a harness that cannot resume headless. Every refusal leaves
// both the projects table and the log untouched.
func TestSetProjectForemanRefusals(t *testing.T) {
	e := newEnv(t)
	repo := "https://example.invalid/refused.git"
	for _, tc := range []struct {
		name, url, harness, model, want string
	}{
		{"no repo", "", "claude", "opus", "repo URL is required"},
		{"no harness", repo, "", "opus", "foreman harness is required"},
		{"no model", repo, "claude", "", "foreman model is required"},
		{"unresumable harness", repo, "kimi", "opus", `harness "kimi" cannot resume headless`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := e.eng.SetProjectForeman(e.ctx, tc.url, tc.harness, tc.model)
			if err == nil {
				t.Fatalf("set the foreman (%q, %q, %q) succeeded; want refusal", tc.url, tc.harness, tc.model)
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("the refusal = %v, want it to contain %q", err, tc.want)
			}
			if !errors.Is(err, ErrInvalidForeman) {
				t.Errorf("the refusal = %v, want it to carry ErrInvalidForeman: the API's 400 turns on that sentinel", err)
			}
			if got := e.int("SELECT COUNT(*) FROM projects WHERE repo_url = ?", repo); got != 0 {
				t.Errorf("projects for %s = %d, want 0: a refusal creates nothing", repo, got)
			}
			if got := e.int("SELECT COUNT(*) FROM events WHERE kind = 'project_foreman_set'"); got != 0 {
				t.Errorf("project_foreman_set events = %d, want 0: a refusal logs nothing", got)
			}
		})
	}

	// The refusal names the harnesses that do resume, so the caller knows
	// what to pick instead.
	if _, err := e.eng.SetProjectForeman(e.ctx, repo, "kimi", "opus"); err == nil ||
		!strings.Contains(err.Error(), "claude") {
		t.Errorf("the refusal = %v, want it to name a resume-capable harness", err)
	}
}

// A repo the engine has no project for reads back as no setting, not as
// an error.
func TestProjectForemanUnknownRepo(t *testing.T) {
	e := newEnv(t)
	got, err := e.eng.ProjectForeman(e.ctx, "https://example.invalid/none.git")
	if err != nil {
		t.Fatalf("read the foreman of an unknown repo: %v", err)
	}
	if got != nil {
		t.Fatalf("the setting = %+v, want nil for a repo with no project", got)
	}
}
