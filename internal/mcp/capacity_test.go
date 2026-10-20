package mcp

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// TestServeCallCapacity covers the capacity tool end to end (§12.2)
// against a test engine the bridge reaches with a foreman token: one
// line per agent — the effective cap and its layer, the running jobs,
// the quota state and the cost weight — then one line per layer with
// cap, use and queue depth, ready and cooling and broken agents each
// rendering their quota state in words.
func TestServeCallCapacity(t *testing.T) {
	e := newEnvToken(t, foremanTokenFile, "foreman-secret-9")
	coolUntil := time.Now().UTC().Add(time.Hour).Format(engine.TimeLayout)
	err := e.st.Update(t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO plans (project_id, title, state, created_at, updated_at)
			VALUES (1, 'a plan', 'active', ?, ?)`, seedTS, seedTS); err != nil {
			return err
		}
		res, err := tx.Exec(`INSERT INTO tasks (plan_id, title, spec, class, state, base_ref, caps, created_at, updated_at)
			VALUES (1, 'a task', 'the spec', 'implement', 'ready', 'main', '{}', ?, ?)`, seedTS, seedTS)
		if err != nil {
			return err
		}
		task, err := res.LastInsertId()
		if err != nil {
			return err
		}
		res, err = tx.Exec(`INSERT INTO runners (name, created_at, last_seen_at) VALUES ('runner-1', ?, ?)`, seedTS, seedTS)
		if err != nil {
			return err
		}
		runner, err := res.LastInsertId()
		if err != nil {
			return err
		}
		agent := func(health, cooling string) (int64, error) {
			res, err := tx.Exec(`INSERT INTO agents (runner_id, adapter, harness, model, family, account, classes, health, cooling_until, created_at, updated_at)
				VALUES (?, 'claude', 'claude', 'sonnet', 'claude', 'work', '[]', ?, ?, ?, ?)`,
				runner, health, cooling, seedTS, seedTS)
			if err != nil {
				return 0, err
			}
			return res.LastInsertId()
		}
		ready, err := agent("ready", "")
		if err != nil {
			return err
		}
		if _, err := agent("ready", coolUntil); err != nil { // cooling until the deadline
			return err
		}
		broken, err := agent("broken", "")
		if err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE agents SET effort = 'high', tools = 'Bash(npm test:*)+Bash(npx jest:*)' WHERE id = ?", broken); err != nil {
			return err
		}
		// The live job, recorded on the ready agent and held by the
		// runner, so both it and the layers that count it show use 1.
		_, err = tx.Exec(`INSERT INTO jobs (project_id, task_id, kind, key, agent_id, lease_owner, mode, prompt, status, created_at, updated_at)
			VALUES (1, ?, 'work', 'task:1', ?, ?, 'fresh', 'prompt', 'running', ?, ?)`,
			task, ready, runner, seedTS, seedTS)
		return err
	})
	if err != nil {
		t.Fatalf("seed the engine: %v", err)
	}

	s := start(t, e)
	a := s.call(1, "tools/call", map[string]any{"name": "capacity", "arguments": map[string]any{}})
	text, isError := toolText(t, a)
	if isError {
		t.Fatalf("capacity errored: %s", text)
	}
	for _, want := range []string{
		"agent 1 claude/sonnet tier=standard effort=default account=work cap=none running=1 quota=ready cost=1",
		"agent 2 claude/sonnet tier=standard effort=default account=work cap=none running=0 quota=cooling until " + coolUntil + " cost=1",
		"agent 3 claude/sonnet tier=standard effort=high account=work cap=none running=0 quota=broken cost=1 tools=Bash(npm test:*)+Bash(npx jest:*)",
		"runner 1 cap=none use=1 queue=0",
		"account work cap=none use=1 queue=0",
		"plan 1 cap=none use=1 queue=0",
		"global cap=none use=1 queue=0",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("capacity text = %q, want the line %q", text, want)
		}
	}
	// One line per agent, then one line per layer: agents first.
	lines := strings.Split(text, "\n")
	if len(lines) != 7 {
		t.Errorf("capacity text = %d lines, want 3 agents + 4 layers = 7", len(lines))
	}
	if !strings.HasPrefix(lines[0], "agent 1 ") || !strings.HasPrefix(lines[3], "runner ") {
		t.Errorf("capacity lines start %q, %q, want agents first then layers", lines[0], lines[3])
	}
}
