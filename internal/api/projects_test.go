package api

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// The round trip: the client role chooses the foreman's harness and
// model for a repo — creating the project on the way — and the client,
// admin and foreman roles read the choice back. The foreman token is
// scoped to the created project: the seeding hands ids 1 and 2 to its
// two projects, so the create lands on id 3.
func TestProjectForemanRoundTrip(t *testing.T) {
	e := newEnvTokens(t, tokenFileBody+"foreman3 foreman:3 foreman-secret-3\n")
	repo := "https://example.invalid/foreman.git"

	body := []byte(`{"repo_url":"` + repo + `","harness":"claude","model":"opus"}`)
	code, _, data := e.do(http.MethodPut, "/v1/projects/foreman", "alice", "choose-foreman-1", body)
	if code != http.StatusOK {
		t.Fatalf("choose the foreman: %d %s, want 200", code, data)
	}
	set := decode[struct {
		ProjectID int64  `json:"project_id"`
		RepoURL   string `json:"repo_url"`
		Harness   string `json:"harness"`
		Model     string `json:"model"`
	}](t, data)
	if set.ProjectID == 0 || set.RepoURL != repo || set.Harness != "claude" || set.Model != "opus" {
		t.Fatalf("the choice answered %+v; want the repo's project on claude and opus", set)
	}
	if set.ProjectID != 3 {
		t.Fatalf("the created project = %d, want 3: foreman3's scope assumes it", set.ProjectID)
	}

	// The repeat under the same idempotency key replays the first answer
	// (§12.1), and a new key with a new value updates the choice.
	code, _, replay := e.do(http.MethodPut, "/v1/projects/foreman", "alice", "choose-foreman-1", body)
	if code != http.StatusOK || string(replay) != string(data) {
		t.Fatalf("the idempotent repeat = %d %s, want the first answer replayed", code, replay)
	}
	update := []byte(`{"repo_url":"` + repo + `","harness":"claude","model":"sonnet"}`)
	code, _, data = e.do(http.MethodPut, "/v1/projects/foreman", "alice", "choose-foreman-2", update)
	if code != http.StatusOK {
		t.Fatalf("choose again: %d %s, want 200", code, data)
	}

	// Every reading role sees the newest choice. foreman3 rides the
	// literal header: the shared secrets map knows only the stock tokens.
	for _, tc := range []struct{ name, authz string }{
		{"alice", e.bearer("alice")},
		{"root", e.bearer("root")},
		{"foreman3", "Bearer foreman-secret-3"},
	} {
		code, _, data, err := e.call(http.MethodGet, "/v1/projects/foreman?repo="+url.QueryEscape(repo), tc.authz, "", nil)
		if err != nil {
			t.Fatalf("read the choice as %s: %v", tc.name, err)
		}
		if code != http.StatusOK {
			t.Fatalf("read the choice as %s: %d %s, want 200", tc.name, code, data)
		}
		got := decode[struct {
			Harness string `json:"harness"`
			Model   string `json:"model"`
		}](t, data)
		if got.Harness != "claude" || got.Model != "sonnet" {
			t.Errorf("the choice as %s = %+v, want claude on sonnet", tc.name, got)
		}
	}
}

// The endpoint's rules: a runner token may neither choose nor read; an
// unresumable harness, an empty field and a missing repo are the
// caller's 400s; a repo with no project reads back as 404 for the
// unscoped roles, and as the same 403 a foreman hears for another
// project's repo.
func TestProjectForemanRules(t *testing.T) {
	e := newEnv(t)
	repo := "https://example.invalid/one.git" // the seeded project

	// A runner token is out both ways.
	code, _, data := e.do(http.MethodPut, "/v1/projects/foreman", "r1", "",
		[]byte(`{"repo_url":"`+repo+`","harness":"claude","model":"opus"}`))
	if code != http.StatusForbidden {
		t.Errorf("a runner chooses: %d %s, want 403", code, data)
	}
	code, _, data = e.do(http.MethodGet, "/v1/projects/foreman?repo="+url.QueryEscape(repo), "r1", "", nil)
	if code != http.StatusForbidden {
		t.Errorf("a runner reads: %d %s, want 403", code, data)
	}

	// A foreman token may read but not choose.
	code, _, data = e.do(http.MethodPut, "/v1/projects/foreman", "foreman1", "",
		[]byte(`{"repo_url":"`+repo+`","harness":"claude","model":"opus"}`))
	if code != http.StatusForbidden {
		t.Errorf("a foreman chooses: %d %s, want 403", code, data)
	}

	// A foreman token is walled into its own project (§12.1): another
	// project's repo and a repo with no project both answer the same
	// 403, so the code names nothing the foreman may not know.
	for name, repo := range map[string]string{
		"another project's repo": "https://example.invalid/two.git",
		"an unknown repo":        "https://example.invalid/nope.git",
	} {
		code, _, data = e.do(http.MethodGet, "/v1/projects/foreman?repo="+url.QueryEscape(repo), "foreman1", "", nil)
		if code != http.StatusForbidden {
			t.Fatalf("a foreman reads %s: %d %s, want 403", name, code, data)
		}
		if err := decode[errBody](t, data); err.Code != codeForbidden {
			t.Errorf("a foreman reads %s: code = %q, want %q", name, err.Code, codeForbidden)
		}
	}

	// An unresumable harness is a 400 that names the refusal.
	code, _, data = e.do(http.MethodPut, "/v1/projects/foreman", "alice", "",
		[]byte(`{"repo_url":"`+repo+`","harness":"kimi","model":"opus"}`))
	if code != http.StatusBadRequest {
		t.Fatalf("an unresumable harness: %d %s, want 400", code, data)
	}
	if err := decode[errBody](t, data); !strings.Contains(err.Error, "cannot resume headless") || err.Code != codeBadRequest {
		t.Errorf("the refusal = %+v, want the bad_request code and the resume rule named", err)
	}

	// Codex resumed by thread id is foreman-capable now (§4.4's switch).
	code, _, data = e.do(http.MethodPut, "/v1/projects/foreman", "alice", "",
		[]byte(`{"repo_url":"`+repo+`","harness":"codex","model":"gpt-5.3-codex"}`))
	if code != http.StatusOK {
		t.Fatalf("a codex foreman: %d %s, want 200", code, data)
	}

	// Empty fields are the caller's mistakes too.
	for name, body := range map[string]string{
		"empty harness": `{"repo_url":"` + repo + `","harness":"","model":"opus"}`,
		"empty model":   `{"repo_url":"` + repo + `","harness":"claude","model":""}`,
	} {
		code, _, data = e.do(http.MethodPut, "/v1/projects/foreman", "alice", "", []byte(body))
		if code != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", name, code, data)
		}
	}

	// The read wants a repo, and a repo with no project is not-found.
	code, _, data = e.do(http.MethodGet, "/v1/projects/foreman", "alice", "", nil)
	if code != http.StatusBadRequest {
		t.Errorf("a read without repo: %d %s, want 400", code, data)
	}
	code, _, data = e.do(http.MethodGet, "/v1/projects/foreman?repo=https%3A%2F%2Fexample.invalid%2Fnope.git", "alice", "", nil)
	if code != http.StatusNotFound {
		t.Errorf("a read of an unknown repo: %d %s, want 404", code, data)
	}
}

// A store failure is the engine's own, not the caller's mistake: the
// choose answers 500 with the internal code, never the caller's 400.
func TestProjectForemanStoreFailure(t *testing.T) {
	e := newEnv(t)
	if err := e.st.Close(); err != nil {
		t.Fatalf("close the store: %v", err)
	}
	code, _, data := e.do(http.MethodPut, "/v1/projects/foreman", "alice", "",
		[]byte(`{"repo_url":"https://example.invalid/one.git","harness":"claude","model":"opus"}`))
	if code != http.StatusInternalServerError {
		t.Fatalf("choose the foreman on a failed store: %d %s, want 500", code, data)
	}
	if err := decode[errBody](t, data); err.Code != codeInternal {
		t.Errorf("the failure's code = %q, want %q", err.Code, codeInternal)
	}
}
