package engine

import (
	"fmt"
	"strings"
	"testing"
)

// TestTaskLandReadsTheRealStore runs TaskLand over the real schema:
// dependencies, the approving review after the last author job, and the
// author's summary and family.
func TestTaskLandReadsTheRealStore(t *testing.T) {
	e := newEnv(t)
	dep := e.addTask("{}")
	e.exec(`INSERT INTO task_deps (plan_id, task_id, depends_on_task_id) VALUES (?, ?, ?)`, e.plan, e.task, dep)
	e.seedAgent(e.runner1, "claude", "claude", "sonnet", "acct")
	agent := e.int(`SELECT id FROM agents WHERE model = 'sonnet'`)
	e.exec(`UPDATE tasks SET state = 'done' WHERE id = ?`, e.task)

	job := func(kind, key, result string, agentID int64) {
		e.exec(`INSERT INTO jobs (project_id, task_id, kind, key, mode, prompt, status, agent_id, result, created_at, updated_at)
			VALUES (?, ?, ?, ?, 'fresh', 'p', 'done', NULLIF(?, 0), ?, ?, ?)`,
			e.project, e.task, kind, key, agentID, result, seedTS, seedTS)
	}
	worker := `{"result":"{\"status\":\"done\",\"summary\":\"did the work\"}"}`
	job("work", "w1", worker, agent)
	findings := strings.Repeat("substantive ", 40)
	approve := fmt.Sprintf(`{"result":"{\"verdict\":\"approve\",\"findings\":\"%s\"}"}`, findings)
	job("review", "r1", approve, 0)

	got, err := e.eng.TaskLand(e.ctx, e.task)
	if err != nil || got == nil {
		t.Fatalf("TaskLand: %v, %v", got, err)
	}
	if len(got.Deps) != 1 || got.Deps[0] != dep {
		t.Errorf("deps = %v, want [%d]", got.Deps, dep)
	}
	if !got.Approved {
		t.Errorf("a substantive approve after the last author job must read as approved")
	}
	if got.Summary != "did the work" || got.Family != "claude" || got.Model != "sonnet" {
		t.Errorf("summary/family/model = %q/%q/%q", got.Summary, got.Family, got.Model)
	}

	// A later author job starts a new cycle: the old approve no longer counts.
	job("fix", "f1", worker, agent)
	got, err = e.eng.TaskLand(e.ctx, e.task)
	if err != nil || got.Approved {
		t.Errorf("an approve before the last author job must not count: %v approved=%v", err, got != nil && got.Approved)
	}

	if none, err := e.eng.TaskLand(e.ctx, 99999); err != nil || none != nil {
		t.Errorf("a missing task reads as nil, got %v, %v", none, err)
	}
}
