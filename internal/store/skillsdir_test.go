package store

import "testing"

// TestTaskSkillsDirMigrationBackfills: opening a pre-0070 store writes
// each existing task's skills_dir once — its project's setting, or the
// default when the project sets none — and a task made afterwards starts
// with none recorded.
func TestTaskSkillsDirMigrationBackfills(t *testing.T) {
	path, db := openBeforeMigration(t, "task_skills_dir")
	set := insertProject(t, db)
	exec(t, db, "UPDATE projects SET settings = ? WHERE id = ?", `{"skills_dir":"tools/skills"}`, set)
	unset := insertID(t, db, `INSERT INTO projects (repo_url, default_branch, settings, foreman_harness, foreman_model, created_at, updated_at)
		VALUES ('https://example.invalid/other.git', 'main', '{}', 'claude', 'sonnet', ?, ?)`, ts, ts)
	setTask := insertTask(t, db, insertPlan(t, db, set))
	unsetTask := insertTask(t, db, insertPlan(t, db, unset))
	if err := db.Close(); err != nil {
		t.Fatalf("close raw store: %v", err)
	}

	s := openStore(t, path)
	for _, tc := range []struct {
		id   int64
		want string
	}{{setTask, "tools/skills"}, {unsetTask, ".brigada/skills"}} {
		var got string
		if err := s.db.QueryRow("SELECT skills_dir FROM tasks WHERE id = ?", tc.id).Scan(&got); err != nil || got != tc.want {
			t.Errorf("task %d: skills_dir = %q, %v, want %q", tc.id, got, err, tc.want)
		}
	}
	fresh := insertTask(t, s.db, insertPlan(t, s.db, set))
	var got string
	if err := s.db.QueryRow("SELECT skills_dir FROM tasks WHERE id = ?", fresh).Scan(&got); err != nil || got != "" {
		t.Errorf("new task skills_dir = %q, %v, want '' until the engine records it", got, err)
	}
}
