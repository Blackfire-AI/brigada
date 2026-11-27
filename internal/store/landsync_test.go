package store

import (
	"database/sql"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestLandSyncMigration pins 0084's two in-place CHECK edits: the sync
// kind may go without a task, while every other pairing rule and the
// existing job kinds stay intact, and the land_syncs record table holds
// one row per project.
func TestLandSyncMigration(t *testing.T) {
	f := newFixture(t)
	exec(t, f.db, `INSERT INTO jobs (project_id, task_id, kind, key, mode, prompt, status, created_at, updated_at)
		VALUES (?, NULL, 'sync', 'sync:1', 'fresh', '', 'queued', ?, ?)`, f.project, ts, ts)
	if got := queryString(t, f.db, "SELECT kind FROM jobs WHERE key = 'sync:1'"); got != "sync" {
		t.Errorf("sync job kind = %q, want sync", got)
	}
	if _, err := f.db.Exec(`INSERT INTO jobs (project_id, task_id, kind, key, mode, prompt, status, created_at, updated_at)
		VALUES (?, NULL, 'work', 'work-without-task', 'fresh', '', 'queued', ?, ?)`, f.project, ts, ts); err == nil || !strings.Contains(err.Error(), "CHECK") {
		t.Errorf("work job without a task: err = %v; want the old CHECK to stand", err)
	}
	exec(t, f.db, `INSERT INTO land_syncs (project_id, state, behind, default_branch, target_branch, updated_at)
		VALUES (?, 'waiting', 3, 'main', 'brigada/landed', ?)`, f.project, ts)
	if got := queryInt(t, f.db, "SELECT behind FROM land_syncs WHERE project_id = ?", f.project); got != 3 {
		t.Errorf("land_syncs behind = %d, want 3", got)
	}
	if _, err := f.db.Exec(`INSERT INTO land_syncs (project_id, state, behind, default_branch, target_branch, updated_at)
		VALUES (?, 'bogus', 0, 'main', 'brigada/landed', ?)`, f.project, ts); err == nil || !strings.Contains(err.Error(), "CHECK") {
		t.Errorf("land_syncs bogus state: err = %v; want the state CHECK", err)
	}
}

// TestLandSyncMigrationFromSQLOnlyStore upgrades a store whose earlier
// migrations ran without their Go schema edits. Its jobs table still
// carries 0001's CHECKs, which 0084 must replace as readily as the fully
// edited forms.
func TestLandSyncMigrationFromSQLOnlyStore(t *testing.T) {
	migs, err := allMigrations()
	if err != nil {
		t.Fatal(err)
	}
	before := 0
	for _, m := range migs {
		if m.name == "land_sync" {
			before = m.version - 1
			break
		}
	}
	if before == 0 {
		t.Fatal("land_sync migration not found")
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
		VALUES (?, NULL, 'sync', 'sync:legacy', 'fresh', '', 'queued', ?, ?)`, project, ts, ts)
	if got := queryInt(t, s.db, "SELECT COUNT(*) FROM jobs WHERE kind = 'sync' AND task_id IS NULL"); got != 1 {
		t.Errorf("task-less sync jobs after the legacy upgrade = %d, want 1", got)
	}
	exec(t, s.db, `INSERT INTO land_syncs (project_id, state, behind, default_branch, target_branch, updated_at)
		VALUES (?, 'current', 0, 'main', 'brigada/landed', ?)`, project, ts)
	if got := queryString(t, s.db, "SELECT state FROM land_syncs WHERE project_id = ?", project); got != "current" {
		t.Errorf("land_syncs state after the legacy upgrade = %q, want current", got)
	}
}
