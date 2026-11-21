package store

import (
	"database/sql"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestTaskBranchSweepMigration pins 0079's two in-place CHECK edits: the
// internal sweep kind may go without a task, while every other pairing rule
// and the existing job kinds stay intact.
func TestTaskBranchSweepMigration(t *testing.T) {
	f := newFixture(t)
	exec(t, f.db, `INSERT INTO jobs (project_id, task_id, kind, key, mode, prompt, status, created_at, updated_at)
		VALUES (?, NULL, 'sweep', 'sweep:1', 'fresh', '{\"task_ids\":[1]}', 'queued', ?, ?)`, f.project, ts, ts)
	if got := queryString(t, f.db, "SELECT kind FROM jobs WHERE key = 'sweep:1'"); got != "sweep" {
		t.Errorf("sweep job kind = %q, want sweep", got)
	}
	if _, err := f.db.Exec(`INSERT INTO jobs (project_id, task_id, kind, key, mode, prompt, status, created_at, updated_at)
		VALUES (?, NULL, 'work', 'work-without-task', 'fresh', '', 'queued', ?, ?)`, f.project, ts, ts); err == nil || !strings.Contains(err.Error(), "CHECK") {
		t.Errorf("work job without a task: err = %v; want the old CHECK to stand", err)
	}
}

// TestTaskBranchSweepMigrationFromSQLOnlyStore upgrades a store whose earlier
// migrations ran without their Go schema edits. Its jobs table still carries
// 0001's CHECKs, which 0079 must replace as readily as the fully edited forms.
func TestTaskBranchSweepMigrationFromSQLOnlyStore(t *testing.T) {
	migs, err := allMigrations()
	if err != nil {
		t.Fatal(err)
	}
	before := 0
	for _, m := range migs {
		if m.name == "task_branch_sweep" {
			before = m.version - 1
			break
		}
	}
	if before == 0 {
		t.Fatal("task_branch_sweep migration not found")
	}
	path := filepath.Join(t.TempDir(), "store.db")
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migs[:before] {
		exec(t, db, m.sql)
	}
	exec(t, db, "PRAGMA user_version = "+strconv.Itoa(before))
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s := openStore(t, path)
	project := insertProject(t, s.db)
	exec(t, s.db, `INSERT INTO jobs (project_id, task_id, kind, key, mode, prompt, status, created_at, updated_at)
		VALUES (?, NULL, 'sweep', 'sweep:legacy', 'fresh', '{"task_ids":[1]}', 'queued', ?, ?)`, project, ts, ts)
	if got := queryInt(t, s.db, "SELECT COUNT(*) FROM jobs WHERE kind = 'sweep' AND task_id IS NULL"); got != 1 {
		t.Errorf("task-less sweep jobs after the legacy upgrade = %d, want 1", got)
	}
}
