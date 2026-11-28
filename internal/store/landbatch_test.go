package store

import (
	"context"
	"database/sql"
	"path/filepath"
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

// TestLandBatchProgressMigration builds the store of a binary that
// predates 0093_land_batch_progress — the store carrying 0092 — with a
// batch job already live, and opens it with this binary: the table exists,
// the old job has no progress, and a row takes the settled and intent
// values while the vocabulary CHECKs refuse others.
func TestLandBatchProgressMigration(t *testing.T) {
	migs, err := allMigrations()
	if err != nil {
		t.Fatal(err)
	}
	before := 0
	for _, m := range migs {
		if m.name == "land_batch_progress" {
			before = m.version - 1
			break
		}
	}
	if before != 92 {
		t.Fatalf("land_batch_progress applies after version %d, want 92 (0092_land_publications)", before)
	}
	path := filepath.Join(t.TempDir(), "store.db")
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	// The old binary's migrations, schema edits included, each in its
	// own transaction as Open applies them.
	old := &Store{db: db}
	for _, m := range migs[:before] {
		if err := old.apply(context.Background(), m); err != nil {
			t.Fatal(err)
		}
	}
	if v := queryInt(t, db, "PRAGMA user_version"); v != before {
		t.Fatalf("old store at version %d, want %d", v, before)
	}
	project := insertProject(t, db)
	task := insertTask(t, db, insertPlan(t, db, project))
	exec(t, db, `INSERT INTO jobs (project_id, task_id, kind, key, mode, prompt, status, created_at, updated_at)
		VALUES (?, ?, 'land', 'land-batch:1', 'fresh', '', 'leased', ?, ?)`, project, task, ts, ts)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s := openStore(t, path)
	job := queryInt(t, s.db, "SELECT id FROM jobs WHERE key = 'land-batch:1'")
	if got := queryInt(t, s.db, "SELECT COUNT(*) FROM land_batch_progress"); got != 0 {
		t.Errorf("progress rows after upgrade = %d, want none: no old job has progress", got)
	}
	exec(t, s.db, `INSERT INTO land_batch_progress (job_id, task_id, outcome, result, intent, position, updated_at)
		VALUES (?, ?, 'parked', '{}', 'target', 1, ?)`, job, task, ts)
	for name, q := range map[string]string{
		"outcome": `UPDATE land_batch_progress SET outcome = 'landed'`,
		"intent":  `UPDATE land_batch_progress SET intent = 'elsewhere'`,
	} {
		if _, err := s.db.Exec(q); err == nil || !strings.Contains(err.Error(), "CHECK") {
			t.Errorf("%s outside the vocabulary: err = %v, want the CHECK", name, err)
		}
	}
}
