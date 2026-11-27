package store

import (
	"strings"
	"testing"
)

// TestClassCooldownMigrationOpensOldStore: a store built by the
// migrations before class_cooldown — its agents and outcome counts
// already recorded — opens with this binary correct: the counts stay as
// they were, the new per-agent-and-class state starts empty, and its key
// and checks hold from then on.
func TestClassCooldownMigrationOpensOldStore(t *testing.T) {
	path, db := openBeforeMigration(t, "class_cooldown")
	project := insertProject(t, db)
	agent := insertAgent(t, db, insertRunner(t, db))
	exec(t, db, `INSERT INTO outcomes (project_id, agent_id, class, skills, attempts, successes, updated_at)
		VALUES (?, ?, 'review', 0, 5, 3, ?)`, project, agent, ts)
	if err := db.Close(); err != nil {
		t.Fatalf("close raw store: %v", err)
	}

	s := openStore(t, path)
	var attempts, successes int
	if err := s.db.QueryRow("SELECT attempts, successes FROM outcomes WHERE project_id = ? AND agent_id = ? AND class = 'review'",
		project, agent).Scan(&attempts, &successes); err != nil {
		t.Fatalf("read outcome: %v", err)
	}
	if attempts != 5 || successes != 3 {
		t.Errorf("old outcome = %d/%d, want 3/5 kept", successes, attempts)
	}
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM class_cooldown").Scan(&n); err != nil || n != 0 {
		t.Fatalf("class_cooldown rows = %d, %v, want an empty start", n, err)
	}
	exec(t, s.db, `INSERT INTO class_cooldown (agent_id, class, streak, cooling_until, step, updated_at)
		VALUES (?, 'review', 2, ?, 0, ?)`, agent, ts, ts)
	if _, err := s.db.Exec(`INSERT INTO class_cooldown (agent_id, class, updated_at)
		VALUES (?, 'review', ?)`, agent, ts); err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Errorf("second row of one agent and class: err = %v, want a UNIQUE failure", err)
	}
	if _, err := s.db.Exec(`INSERT INTO class_cooldown (agent_id, class, streak, updated_at)
		VALUES (?, 'implement', -1, ?)`, agent, ts); err == nil || !strings.Contains(err.Error(), "CHECK") {
		t.Errorf("negative streak: err = %v, want a CHECK failure", err)
	}
	// The state is the agent's: removing the agent removes its rows.
	exec(t, s.db, "DELETE FROM agents WHERE id = ?", agent)
	if err := s.db.QueryRow("SELECT COUNT(*) FROM class_cooldown").Scan(&n); err != nil || n != 0 {
		t.Errorf("class_cooldown rows after the agent's removal = %d, %v, want none", n, err)
	}
}
