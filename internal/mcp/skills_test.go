package mcp

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// seedSkills seeds the fixture project's imported skills straight
// through the store, as the client's import does: each entry a name,
// an audience and a text.
func seedSkills(t *testing.T, e *env, skills ...[3]string) {
	t.Helper()
	digest := func(text string) string {
		sum := sha256.Sum256([]byte(text))
		return hex.EncodeToString(sum[:])
	}
	if err := e.st.Update(t.Context(), func(tx *sql.Tx) error {
		for _, s := range skills {
			if _, err := tx.Exec(`INSERT INTO skills (project_id, name, description, for_role, text, digest, tree_digest)
				VALUES (1, ?, '', ?, ?, ?, '')`, s[0], s[1], s[2], digest(s[2])); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed the skills: %v", err)
	}
}

// skillsOf reads the skill names a task recorded, in order.
func skillsOf(t *testing.T, e *env, spec string) []string {
	t.Helper()
	var raw string
	if err := e.st.View(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT skills FROM tasks WHERE spec = ?", spec).Scan(&raw)
	}); err != nil {
		t.Fatalf("read the skills of %q: %v", spec, err)
	}
	var s []struct {
		Name string `json:"name"`
	}
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &s); err != nil {
			t.Fatalf("parse the skills %q: %v", raw, err)
		}
	}
	out := make([]string, len(s))
	for i, x := range s {
		out[i] = x.Name
	}
	return out
}

// TestServePlanSkills: the skills field rides plan_create and
// plan_amend's respec and retier through the bridge — the names
// recorded in the order given, an absent field keeping the recorded
// ones, [] clearing them — and an unknown name is the tool's error
// naming the known skills.
func TestServePlanSkills(t *testing.T) {
	e := newEnvToken(t, foremanTokenFile, "foreman-secret-9")
	seedSkills(t, e,
		[3]string{"cli-flag", "both", "register the flag"},
		[3]string{"surface-parity", "reviewer", "find the siblings"},
	)
	s := start(t, e)

	a := s.call(1, "tools/call", map[string]any{"name": "plan_create", "arguments": map[string]any{
		"title": "skilled", "tasks": []any{
			map[string]any{"name": "build", "spec": "build spec"},
			map[string]any{"name": "ship", "spec": "ship spec", "deps": []any{"build"}, "skills": []any{"surface-parity", "cli-flag"}},
		}}})
	if text, isError := toolText(t, a); isError {
		t.Fatalf("plan_create errored: %s", text)
	}
	if got := skillsOf(t, e, "ship spec"); !reflect.DeepEqual(got, []string{"surface-parity", "cli-flag"}) {
		t.Errorf("skills = %v, want the order given", got)
	}
	var instructions, meta string
	if err := e.st.View(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT instructions, instructions_meta FROM tasks WHERE spec = 'build spec'").Scan(&instructions, &meta)
	}); err != nil {
		t.Fatal(err)
	}
	if instructions != "" || !strings.Contains(meta, `"source":"default"`) {
		t.Errorf("the created task recorded worker text %q with meta %q, want none with the not-configured source", instructions, meta)
	}

	var plan int64
	if err := e.st.View(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT id FROM plans ORDER BY id DESC LIMIT 1").Scan(&plan)
	}); err != nil {
		t.Fatal(err)
	}
	amend := func(id int, respec, retier any) string {
		t.Helper()
		args := map[string]any{"plan": plan}
		if respec != nil {
			args["respec"] = respec
		}
		if retier != nil {
			args["retier"] = retier
		}
		a := s.call(id, "tools/call", map[string]any{"name": "plan_amend", "arguments": args})
		text, isError := toolText(t, a)
		if isError {
			t.Fatalf("plan_amend errored: %s", text)
		}
		return text
	}
	amend(2, []any{map[string]any{"task": "ship", "spec": "ship v2", "skills": []any{"cli-flag"}}}, nil)
	if got := skillsOf(t, e, "ship v2"); !reflect.DeepEqual(got, []string{"cli-flag"}) {
		t.Errorf("after a respec with skills = %v, want cli-flag alone", got)
	}
	amend(3, []any{map[string]any{"task": "ship", "spec": "ship v3"}}, nil)
	if got := skillsOf(t, e, "ship v3"); !reflect.DeepEqual(got, []string{"cli-flag"}) {
		t.Errorf("after a respec without skills = %v, want the recorded skill kept", got)
	}
	amend(4, nil, []any{map[string]any{"task": "ship", "tier": "light", "skills": []any{}}})
	if got := skillsOf(t, e, "ship v3"); len(got) != 0 {
		t.Errorf("after a retier with [] = %v, want none", got)
	}

	a = s.call(5, "tools/call", map[string]any{"name": "plan_amend", "arguments": map[string]any{
		"plan": plan, "respec": []any{map[string]any{"task": "ship", "spec": "x", "skills": []any{"nope"}}}}})
	if text, isError := toolText(t, a); !isError || !strings.Contains(text, "nope") || !strings.Contains(text, "cli-flag, surface-parity") {
		t.Errorf("an unknown skill = %q (error %v), want the tool's error naming it and the known skills", text, isError)
	}
	a = s.call(6, "tools/call", map[string]any{"name": "plan_create", "arguments": map[string]any{
		"title": "unknown", "tasks": []any{map[string]any{"spec": "unknown spec", "skills": []any{"nope"}}}}})
	if text, isError := toolText(t, a); !isError || !strings.Contains(text, "nope") {
		t.Errorf("plan_create with an unknown skill = %q (error %v), want the tool's error naming it", text, isError)
	}
}

// TestPlanToolsOfferSkills: the model can only send what the schema
// names.
func TestPlanToolsOfferSkills(t *testing.T) {
	for _, tool := range tools {
		if tool.Name != "plan_create" && tool.Name != "plan_amend" {
			continue
		}
		b, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), `"skills"`) {
			t.Errorf("%s schema does not offer skills: %s", tool.Name, b)
		}
	}
}
