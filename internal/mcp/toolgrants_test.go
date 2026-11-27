package mcp

import (
	"database/sql"
	"strings"
	"testing"
)

// TestServeCallPlanToolGrants: the plan tools carry a task's tool_grants
// to the store — plan_create's task, plan_amend's add and respec — and a
// pattern outside the allowlist refuses the call, named (§6.1).
func TestServeCallPlanToolGrants(t *testing.T) {
	e := newEnvToken(t, foremanTokenFile, "foreman-secret-9")
	setProjectDefault(t, e, `{"default_check": "go test ./..."}`)
	s := start(t, e)
	grantsOf := func(name string) string {
		t.Helper()
		var grants string
		err := e.st.View(t.Context(), func(tx *sql.Tx) error {
			return tx.QueryRow("SELECT tool_grants FROM tasks WHERE name = ?", name).Scan(&grants)
		})
		if err != nil {
			t.Fatalf("read %s's grants: %v", name, err)
		}
		return grants
	}

	call := map[string]any{
		"title": "grants",
		"tasks": []any{
			map[string]any{"name": "build", "spec": "build it", "check": "go test ./..."},
			map[string]any{"name": "ship", "spec": "ship it", "check": "go test ./...", "deps": []any{"build"},
				"tool_grants": []any{"Bash(git merge:*)"}},
		},
	}
	a := s.call(1, "tools/call", map[string]any{"name": "plan_create", "arguments": call})
	if text, isError := toolText(t, a); isError {
		t.Fatalf("plan_create with grants errored: %s", text)
	}
	if got := grantsOf("ship"); got != `["Bash(git merge:*)"]` {
		t.Errorf("created tool_grants = %q, want the merge grant", got)
	}
	if got := grantsOf("build"); got != "" {
		t.Errorf("grantless tool_grants = %q, want ''", got)
	}

	// amend-add and amend-respec carry theirs.
	amend := map[string]any{
		"plan": 1,
		"add": []any{map[string]any{"name": "extra", "spec": "extra work", "check": "go test ./...", "deps": []any{"build"},
			"tool_grants": []any{"Bash(git restore:*)"}}},
		"respec": []any{map[string]any{"task": "ship", "spec": "ship it again", "tool_grants": []any{"Bash(git cherry-pick:*)"}}},
	}
	a = s.call(2, "tools/call", map[string]any{"name": "plan_amend", "arguments": amend})
	if text, isError := toolText(t, a); isError {
		t.Fatalf("plan_amend with grants errored: %s", text)
	}
	if got := grantsOf("extra"); got != `["Bash(git restore:*)"]` {
		t.Errorf("added tool_grants = %q, want the restore grant", got)
	}
	if got := grantsOf("ship"); got != `["Bash(git cherry-pick:*)"]` {
		t.Errorf("respecced tool_grants = %q, want the cherry-pick grant", got)
	}

	// A pattern outside the allowlist refuses the whole call, named.
	bad := map[string]any{
		"title": "bad grants",
		"tasks": []any{map[string]any{"spec": "s", "check": "go test ./...", "tool_grants": []any{"Bash(git push:*)"}}},
	}
	a = s.call(3, "tools/call", map[string]any{"name": "plan_create", "arguments": bad})
	text, isError := toolText(t, a)
	if !isError || !strings.Contains(text, "Bash(git push:*)") {
		t.Errorf("off-allowlist pattern: isError=%v text=%q, want the refusal naming it", isError, text)
	}
}
