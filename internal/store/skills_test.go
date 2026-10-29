package store

import (
	"strings"
	"testing"
)

// TestTaskSkillsMigrationKeepsLegacySnapshot: opening a pre-0065 store
// keeps every task's instructions and their provenance exactly as they
// were — the worker snapshot the task's jobs read — and gives the task
// an explicitly empty reviewer snapshot and no skills.
func TestTaskSkillsMigrationKeepsLegacySnapshot(t *testing.T) {
	path, db := openBeforeMigration(t, "task_skills")
	project := insertProject(t, db)
	plan := insertPlan(t, db, project)
	legacy := insertTask(t, db, plan)
	const oldText = "Old rules: keep the tabs."
	const oldMeta = `{"source":"imported","hash":"cafe01","digest":"d0","bytes":25}`
	exec(t, db, "UPDATE tasks SET instructions = ?, instructions_meta = ? WHERE id = ?", oldText, oldMeta, legacy)
	bare := insertTask(t, db, plan)
	if err := db.Close(); err != nil {
		t.Fatalf("close raw store: %v", err)
	}

	s := openStore(t, path)
	for _, tc := range []struct {
		id         int64
		text, meta string
	}{{legacy, oldText, oldMeta}, {bare, "", ""}} {
		var text, meta, rText, rMeta, skills string
		if err := s.db.QueryRow(`SELECT instructions, instructions_meta, reviewer_instructions, reviewer_instructions_meta, skills
			FROM tasks WHERE id = ?`, tc.id).Scan(&text, &meta, &rText, &rMeta, &skills); err != nil {
			t.Fatalf("read task %d: %v", tc.id, err)
		}
		if text != tc.text || meta != tc.meta {
			t.Errorf("task %d: worker snapshot = %q %s, want the old %q %s kept", tc.id, text, meta, tc.text, tc.meta)
		}
		if rText != "" || rMeta != `{"source":"legacy","bytes":0}` {
			t.Errorf("task %d: reviewer snapshot = %q %s, want explicitly empty", tc.id, rText, rMeta)
		}
		if skills != "" {
			t.Errorf("task %d: skills = %q, want none", tc.id, skills)
		}
	}
	// A task created after the migration is not marked legacy.
	fresh := insertTask(t, s.db, plan)
	var rMeta string
	if err := s.db.QueryRow("SELECT reviewer_instructions_meta FROM tasks WHERE id = ?", fresh).Scan(&rMeta); err != nil || rMeta != "" {
		t.Errorf("new task reviewer meta = %q, %v, want unresolved ''", rMeta, err)
	}
}

// TestJobsPromptMetaMigrationKeepsOldJobs: opening a pre-0066 store keeps
// every job's prompt as it was and records no prompt meta for it — the
// prompt stays its own record.
func TestJobsPromptMetaMigrationKeepsOldJobs(t *testing.T) {
	path, db := openBeforeMigration(t, "jobs_prompt_meta")
	project := insertProject(t, db)
	task := insertTask(t, db, insertPlan(t, db, project))
	job := insertJob(t, db, project, task, "task:old", "queued")
	if err := db.Close(); err != nil {
		t.Fatalf("close raw store: %v", err)
	}

	s := openStore(t, path)
	var prompt, meta string
	if err := s.db.QueryRow("SELECT prompt, prompt_meta FROM jobs WHERE id = ?", job).Scan(&prompt, &meta); err != nil {
		t.Fatalf("read job: %v", err)
	}
	if prompt != "the prompt" || meta != "" {
		t.Errorf("old job = prompt %q meta %q, want its prompt kept and no meta", prompt, meta)
	}
}

// TestOutcomesSkillsMigrationKeepsCounts: opening a pre-0067 store keeps
// every outcome's attempts and successes, recorded as a task with no
// skills, and the new key splits by skill count from then on.
func TestOutcomesSkillsMigrationKeepsCounts(t *testing.T) {
	path, db := openBeforeMigration(t, "outcomes_skills")
	project := insertProject(t, db)
	agent := insertAgent(t, db, insertRunner(t, db))
	exec(t, db, `INSERT INTO outcomes (project_id, agent_id, class, attempts, successes, updated_at)
		VALUES (?, ?, 'implement', 5, 3, ?)`, project, agent, ts)
	if err := db.Close(); err != nil {
		t.Fatalf("close raw store: %v", err)
	}

	s := openStore(t, path)
	var skills, attempts, successes int
	if err := s.db.QueryRow("SELECT skills, attempts, successes FROM outcomes WHERE project_id = ? AND agent_id = ? AND class = 'implement'",
		project, agent).Scan(&skills, &attempts, &successes); err != nil {
		t.Fatalf("read outcome: %v", err)
	}
	if skills != 0 || attempts != 5 || successes != 3 {
		t.Errorf("old outcome = skills %d %d/%d, want 0 skills and 3/5 kept", skills, successes, attempts)
	}
	exec(t, s.db, `INSERT INTO outcomes (project_id, agent_id, class, skills, attempts, successes, updated_at)
		VALUES (?, ?, 'implement', 1, 1, 1, ?)`, project, agent, ts)
	if _, err := s.db.Exec(`INSERT INTO outcomes (project_id, agent_id, class, skills, attempts, successes, updated_at)
		VALUES (?, ?, 'implement', 0, 1, 1, ?)`, project, agent, ts); err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Errorf("second no-skill row: err = %v, want a UNIQUE failure", err)
	}
}

// TestSkillsTableRules: one row per project and name, for_role admits
// worker, reviewer and both (both by default), and the project
// reference is enforced.
func TestSkillsTableRules(t *testing.T) {
	f := newFixture(t)
	exec(t, f.db, `INSERT INTO skills (project_id, name, description, text, digest) VALUES (?, 'bugfix', 'd', 't', 'x')`, f.project)
	if got := queryStrings(t, f.db, "SELECT for_role FROM skills WHERE name = 'bugfix'"); len(got) != 1 || got[0] != "both" {
		t.Errorf("default for_role = %v, want both", got)
	}
	if _, err := f.db.Exec(`INSERT INTO skills (project_id, name, text, digest) VALUES (?, 'bugfix', 't', 'x')`, f.project); err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Errorf("second bugfix row: err = %v, want a UNIQUE failure", err)
	}
	if _, err := f.db.Exec(`INSERT INTO skills (project_id, name, for_role, text, digest) VALUES (?, 'x', 'foreman', 't', 'x')`, f.project); err == nil || !strings.Contains(err.Error(), "CHECK") {
		t.Errorf("for_role foreman: err = %v, want a CHECK failure", err)
	}
	if _, err := f.db.Exec(`INSERT INTO skills (project_id, name, text, digest) VALUES (?, 'y', 't', 'x')`, f.project+1000); err == nil || !strings.Contains(err.Error(), "FOREIGN KEY") {
		t.Errorf("row without its project: err = %v, want a FOREIGN KEY failure", err)
	}
}
