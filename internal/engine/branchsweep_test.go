package engine

import (
	"slices"
	"testing"
	"time"
)

func TestQueueTaskBranchSweepsSelectsOnlyDeletableTasks(t *testing.T) {
	e := newEnv(t)
	cancelled := e.addTask("{}")
	retired := e.addTask("{}")
	within := e.addTask("{}")
	live := e.addTask("{}")
	landed := e.addTask("{}")
	doneUnlanded := e.addTask("{}")
	unsettled := e.addTask("{}")
	old := fmtTime(e.clock.Now().Add(-8 * 24 * time.Hour))
	recent := fmtTime(e.clock.Now().Add(-24 * time.Hour))
	for _, id := range []int64{cancelled, retired} {
		e.exec("UPDATE tasks SET state = ?, settled_at = ? WHERE id = ?", map[int64]string{cancelled: taskCancelled, retired: taskRetired}[id], old, id)
	}
	e.exec("UPDATE tasks SET state = 'cancelled', settled_at = ? WHERE id = ?", recent, within)
	e.exec("UPDATE tasks SET state = 'running' WHERE id = ?", live)
	e.exec("UPDATE tasks SET state = 'done' WHERE id IN (?, ?)", landed, doneUnlanded)
	e.exec("UPDATE tasks SET state = 'cancelled', settled_at = NULL WHERE id = ?", unsettled)
	e.exec(`INSERT INTO landings (task_id, project_id, target_branch, commit_sha, pushed, created_at)
		VALUES (?, ?, 'main', 'abc123', 'waiting', ?)`, landed, e.project, seedTS)

	queued, err := e.eng.QueueTaskBranchSweeps(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if queued != 1 || e.int("SELECT COUNT(*) FROM jobs WHERE kind = 'sweep'") != 1 {
		t.Fatalf("queued = %d, sweep jobs = %d; want one", queued, e.int("SELECT COUNT(*) FROM jobs WHERE kind = 'sweep'"))
	}
	g := e.lease(e.runner1, nil, []string{"git"})
	if g == nil || g.Kind != KindSweep || g.TaskID != 0 || g.Agent != "" {
		t.Fatalf("sweep grant = %+v, want an agent-less, task-less sweep", g)
	}
	want := []int64{cancelled, retired, landed}
	if !slices.Equal(g.SweepTaskIDs, want) {
		t.Errorf("deletable ids = %v, want %v", g.SweepTaskIDs, want)
	}
	if g.RepoURL != "https://example.invalid/brigada.git" {
		t.Errorf("sweep repo = %q, want the project's repo", g.RepoURL)
	}

	// A second start coalesces behind the live job instead of duplicating it.
	queued, err = e.eng.QueueTaskBranchSweeps(e.ctx)
	if err != nil || queued != 0 {
		t.Fatalf("repeat queue = %d, %v; want no new job", queued, err)
	}
	if got := e.int("SELECT COUNT(*) FROM jobs WHERE kind = 'sweep'"); got != 1 {
		t.Errorf("sweep jobs after the repeat = %d, want 1", got)
	}
}

func TestQueueTaskBranchSweepsZeroRetentionDeletesAtOnce(t *testing.T) {
	e := newEnv(t)
	e.exec("UPDATE projects SET settings = '{\"branch_retention_days\":0}' WHERE id = ?", e.project)
	// RFC3339 without fractional seconds is valid in old fixtures and rows;
	// the window comparison must accept it as well as the store's own layout.
	settled := e.clock.Now().Format(time.RFC3339)
	e.exec("UPDATE tasks SET state = 'cancelled', settled_at = ? WHERE id = ?", settled, e.task)
	queued, err := e.eng.QueueTaskBranchSweeps(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if queued != 1 {
		t.Fatalf("queued = %d, want the immediately deletable task's sweep", queued)
	}
	g := e.lease(e.runner1, nil, []string{"git"})
	if g == nil || !slices.Equal(g.SweepTaskIDs, []int64{e.task}) {
		t.Errorf("zero-retention grant = %+v, want task %d", g, e.task)
	}
}
