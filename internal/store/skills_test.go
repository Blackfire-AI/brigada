package store

import (
	"database/sql"
	"strconv"
	"strings"
	"testing"
)

// openBeforeTaskSkills builds, at a path of its own, the store a binary
// that predates 0065 would leave behind: every earlier migration
// applied, user_version set to the one before it. Old data is the
// caller's to insert before the returned handle is closed and reopened.
func openBeforeTaskSkills(t *testing.T) (string, *sql.DB) {
	t.Helper()
	migs, err := allMigrations()
	if err != nil {
		t.Fatalf("list migrations: %v", err)
	}
	var before int
	for _, m := range migs {
		if m.name == "task_skills" {
			before = m.version - 1
			break
		}
	}
	if before == 0 {
		t.Fatal("task_skills migration not found in allMigrations")
	}
	path := t.TempDir() + "/store.db"
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatalf("open raw store: %v", err)
	}
	for _, m := range migs[:before] {
		exec(t, db, m.sql)
	}
	exec(t, db, "PRAGMA user_version = "+strconv.Itoa(before))
	return path, db
}

// TestTaskSkillsMigrationKeepsLegacySnapshot: opening a pre-0065 store
// keeps every task's instructions and their provenance exactly as they
// were — the worker snapshot the task's jobs read — and gives the task
// an explicitly empty reviewer snapshot and no skills.
func TestTaskSkillsMigrationKeepsLegacySnapshot(t *testing.T) {
	path, db := openBeforeTaskSkills(t)
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
