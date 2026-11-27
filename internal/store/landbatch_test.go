package store

import (
	"strings"
	"testing"
)

// TestLandBatchMigration pins 0089: tasks gain land_batch_job (NULL for
// every existing task, naming a job when set) and a land job may go
// without a task, while the pairing rule still refuses a task-less work
// job.
func TestLandBatchMigration(t *testing.T) {
	f := newFixture(t)
	if got := queryInt(t, f.db, "SELECT COUNT(*) FROM tasks WHERE land_batch_job IS NULL"); got != 1 {
		t.Errorf("tasks with no reservation = %d, want the fixture's 1 kept", got)
	}
	exec(t, f.db, `INSERT INTO jobs (project_id, task_id, kind, key, mode, prompt, status, created_at, updated_at)
		VALUES (?, NULL, 'land', 'land-batch:1', 'fresh', '', 'queued', ?, ?)`, f.project, ts, ts)
	job := queryInt(t, f.db, "SELECT id FROM jobs WHERE key = 'land-batch:1'")
	exec(t, f.db, "UPDATE tasks SET land_batch_job = ? WHERE id = ?", job, f.task)
	if got := queryInt(t, f.db, "SELECT land_batch_job FROM tasks WHERE id = ?", f.task); got != job {
		t.Errorf("land_batch_job = %d, want %d", got, job)
	}
	if _, err := f.db.Exec(`INSERT INTO jobs (project_id, task_id, kind, key, mode, prompt, status, created_at, updated_at)
		VALUES (?, NULL, 'work', 'work-without-task', 'fresh', '', 'queued', ?, ?)`, f.project, ts, ts); err == nil || !strings.Contains(err.Error(), "CHECK") {
		t.Errorf("work job without a task: err = %v; want the pairing CHECK to stand", err)
	}
}
