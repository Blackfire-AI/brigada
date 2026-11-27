package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// TestClientBridgePlanScope: the client bridge's status and history take
// the plan argument with the CLI's meaning, and status takes json.
func TestClientBridgePlanScope(t *testing.T) {
	e := newEnv(t)
	err := e.st.Update(context.Background(), func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO plans (project_id, title, state, created_at, updated_at)
			VALUES (1, 'fix the frobnicator', 'active', ?, ?)`, seedTS, seedTS); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO tasks (plan_id, title, spec, check_cmd, class, state, base_ref, caps, created_at, updated_at)
			VALUES (1, 'the frobnicator', 'the spec', 'go test ./...', 'implement', 'ready', 'main', '{}', ?, ?)`, seedTS, seedTS); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO events (kind, project_id, task_id, job_id, payload, created_at)
			VALUES ('plan_created', 1, NULL, NULL, '{"plan":1}', ?)`, seedTS)
		return err
	})
	if err != nil {
		t.Fatalf("seed the plan: %v", err)
	}
	s := startClient(t, e)
	plan := 1

	a := s.call(1, "tools/call", map[string]any{"name": "status", "arguments": map[string]any{"plan": plan}})
	text, isErr := toolText(t, a)
	if isErr || !strings.Contains(text, fmt.Sprintf("plan #%d active", plan)) || strings.Contains(text, "engine pid") || strings.Contains(text, "jobs:") {
		t.Errorf("status plan = %q (error %v), want the compact plan summary", text, isErr)
	}

	a = s.call(2, "tools/call", map[string]any{"name": "status", "arguments": map[string]any{"plan": plan, "json": true}})
	text, isErr = toolText(t, a)
	var got struct {
		Plans     []struct{ ID int64 } `json:"plans"`
		PlanScope *struct{}            `json:"plan_scope"`
	}
	if err := json.Unmarshal([]byte(text), &got); isErr || err != nil || len(got.Plans) != 1 || got.PlanScope == nil {
		t.Errorf("status plan json = %q (error %v, %v)", text, isErr, err)
	}

	a = s.call(3, "tools/call", map[string]any{"name": "status", "arguments": map[string]any{"json": true}})
	if text, isErr = toolText(t, a); isErr || json.Unmarshal([]byte(text), &got) != nil {
		t.Errorf("status json = %q (error %v)", text, isErr)
	}

	a = s.call(4, "tools/call", map[string]any{"name": "status", "arguments": map[string]any{"plan": 9999}})
	if text, isErr = toolText(t, a); !isErr {
		t.Errorf("status of an unknown plan = %q, want an error", text)
	}

	a = s.call(5, "tools/call", map[string]any{"name": "history", "arguments": map[string]any{"plan": plan}})
	if text, isErr = toolText(t, a); isErr {
		t.Errorf("history plan = %q, want the plan's timeline", text)
	}
	a = s.call(6, "tools/call", map[string]any{"name": "history", "arguments": map[string]any{"plan": "x"}})
	if text, isErr = toolText(t, a); !isErr {
		t.Errorf("history with a malformed plan = %q, want an error", text)
	}
}
