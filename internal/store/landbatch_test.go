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

// TestLandSkipReadyMigration pins 0091: tasks gain land_skip_ready, 0 for
// every existing task — none carries a released land_ready gate — and
// settable to 1.
func TestLandSkipReadyMigration(t *testing.T) {
	f := newFixture(t)
	if got := queryInt(t, f.db, "SELECT land_skip_ready FROM tasks WHERE id = ?", f.task); got != 0 {
		t.Errorf("land_skip_ready of an existing task = %d, want 0", got)
	}
	exec(t, f.db, "UPDATE tasks SET land_skip_ready = 1 WHERE id = ?", f.task)
	if got := queryInt(t, f.db, "SELECT land_skip_ready FROM tasks WHERE id = ?", f.task); got != 1 {
		t.Errorf("land_skip_ready = %d, want 1", got)
	}
}

// TestLandPublicationsMigration pins 0092: a publication records its
// links and members, and a baseline names one publication, one per
// project and target.
func TestLandPublicationsMigration(t *testing.T) {
	f := newFixture(t)
	exec(t, f.db, `INSERT INTO land_publications (project_id, target_branch, predecessor_head, final_head, job_id, members, created_at)
		VALUES (?, 'main', 'a', 'b', NULL, '[{"task_id":1,"commit":"b"}]', ?)`, f.project, ts)
	pub := queryInt(t, f.db, "SELECT id FROM land_publications WHERE final_head = 'b'")
	exec(t, f.db, `INSERT INTO land_baselines (project_id, target_branch, remote, command_path, command_digest, head, publication_id, verified_at)
		VALUES (?, 'main', 'r', 'scripts/land-ready.sh', 'd', 'b', ?, ?)`, f.project, pub, ts)
	if _, err := f.db.Exec(`INSERT INTO land_baselines (project_id, target_branch, remote, command_path, command_digest, head, publication_id, verified_at)
		VALUES (?, 'main', 'r', 'x', 'd', 'b', ?, ?)`, f.project, pub, ts); err == nil {
		t.Error("a second baseline for the same project and target was accepted")
	}
}
