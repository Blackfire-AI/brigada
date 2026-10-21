package mcp

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// extraRecordOf reads the extra repositories the task with this spec
// recorded; nil when it recorded none.
func extraRecordOf(t *testing.T, e *env, spec string) *engine.ExtraReposSpec {
	t.Helper()
	var rec sql.NullString
	if err := e.st.View(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT extra_repos FROM tasks WHERE spec = ?", spec).Scan(&rec)
	}); err != nil {
		t.Fatalf("read the extra repositories of %q: %v", spec, err)
	}
	if !rec.Valid {
		return nil
	}
	var out engine.ExtraReposSpec
	if err := json.Unmarshal([]byte(rec.String), &out); err != nil {
		t.Fatalf("record %q: %v", rec.String, err)
	}
	return &out
}

func setProjectDefault(t *testing.T, e *env, settings string) {
	t.Helper()
	if err := e.st.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.Exec("UPDATE projects SET settings = ? WHERE id = 1", settings)
		return err
	}); err != nil {
		t.Fatalf("set the project settings: %v", err)
	}
}

const schemaDefault = `{"extra_repos":[{"name":"schema","repo":"example.invalid/org/schema","url":"https://example.invalid/org/schema.git","ref":"v1"}]}`

// TestServePlanCreateExtraRepos covers the extra_repos field of plan_create
// (§5.8) through the bridge: a repository outside the project default holds
// the plan for approval and the answer names it with its ref; an omitted
// field inherits the default; [] replaces it with nothing — the difference
// must survive the tool, the API and the engine.
func TestServePlanCreateExtraRepos(t *testing.T) {
	e := newEnvToken(t, foremanTokenFile, "foreman-secret-9")
	setProjectDefault(t, e, schemaDefault)
	s := start(t, e)
	create := func(id int, title, spec string, extra any) string {
		t.Helper()
		task := map[string]any{"spec": spec}
		if extra != nil {
			task["extra_repos"] = extra
		}
		a := s.call(id, "tools/call", map[string]any{"name": "plan_create",
			"arguments": map[string]any{"title": title, "tasks": []any{task}}})
		text, isError := toolText(t, a)
		if isError {
			t.Fatalf("plan_create %s errored: %s", title, text)
		}
		return text
	}

	text := create(1, "outside", "outside spec", []any{map[string]any{"name": "tools", "repo": "git@example.invalid:org/tools.git", "ref": "main"}})
	for _, want := range []string{"awaiting approval", "extra repositories outside the project default", "tools (example.invalid/org/tools, ref main, not a commit)"} {
		if !strings.Contains(text, want) {
			t.Errorf("answer = %q, want it to say %q", text, want)
		}
	}
	if spec := extraRecordOf(t, e, "outside spec"); spec == nil || spec.Source != engine.ExtraSourceTask || spec.Repos[0].Repo != "example.invalid/org/tools" {
		t.Errorf("record = %+v, want the task's own canonical entry", spec)
	}

	create(2, "inherits", "inherit spec", nil)
	if spec := extraRecordOf(t, e, "inherit spec"); spec == nil || spec.Source != engine.ExtraSourceDefault || spec.Repos[0].Name != "schema" {
		t.Errorf("an omitted field recorded %+v, want the project default", spec)
	}
	create(3, "cleared", "cleared spec", []any{})
	if spec := extraRecordOf(t, e, "cleared spec"); spec != nil {
		t.Errorf("[] recorded %+v, want no extra repositories", spec)
	}

	a := s.call(4, "tools/call", map[string]any{"name": "plan_create", "arguments": map[string]any{
		"title": "clash", "tasks": []any{map[string]any{"spec": "clash spec", "extra_repos": []any{
			map[string]any{"name": "a-b", "repo": "git@example.invalid:org/x.git"},
			map[string]any{"name": "a_b", "repo": "git@example.invalid:org/y.git"},
		}}}}})
	if text, isError := toolText(t, a); !isError || !strings.Contains(text, "BRIGADA_REPO_A_B") {
		t.Errorf("clashing names = %q (error %v), want the tool's error naming the variable", text, isError)
	}
}

// TestServePlanAmendExtraRepos covers the field on plan_amend (§5.8).
func TestServePlanAmendExtraRepos(t *testing.T) {
	e := newEnvToken(t, foremanTokenFile, "foreman-secret-9")
	setProjectDefault(t, e, schemaDefault)
	s := start(t, e)
	a := s.call(1, "tools/call", map[string]any{"name": "plan_create", "arguments": map[string]any{
		"title": "base", "tasks": []any{map[string]any{"name": "first", "spec": "first spec"}}}})
	if text, isError := toolText(t, a); isError {
		t.Fatalf("plan_create errored: %s", text)
	}
	var plan int64
	if err := e.st.View(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT id FROM plans ORDER BY id DESC LIMIT 1").Scan(&plan)
	}); err != nil {
		t.Fatal(err)
	}
	a = s.call(2, "tools/call", map[string]any{"name": "plan_amend", "arguments": map[string]any{
		"plan": plan,
		"add": []any{map[string]any{"spec": "added spec", "extra_repos": []any{
			map[string]any{"name": "gen", "repo": "https://example.invalid/org/gen.git", "ref": "0123456789abcdef0123456789abcdef01234567"},
		}}},
	}})
	text, isError := toolText(t, a)
	if isError {
		t.Fatalf("plan_amend errored: %s", text)
	}
	if !strings.Contains(text, "held at the plan gate") || !strings.Contains(text, "gen (example.invalid/org/gen, commit 0123456789abcdef0123456789abcdef01234567)") {
		t.Errorf("answer = %q, want the added task held for the repository, with its commit", text)
	}
	if spec := extraRecordOf(t, e, "added spec"); spec == nil || spec.Repos[0].Name != "gen" {
		t.Errorf("amend-added record = %+v", spec)
	}
}

// TestPlanToolsOfferExtraRepos: the model can only send what the schema
// names.
func TestPlanToolsOfferExtraRepos(t *testing.T) {
	for _, tool := range tools {
		if tool.Name != "plan_create" && tool.Name != "plan_amend" {
			continue
		}
		b, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), `"extra_repos"`) || !strings.Contains(string(b), "BRIGADA_REPO_") {
			t.Errorf("%s schema does not offer extra_repos: %s", tool.Name, b)
		}
	}
}
