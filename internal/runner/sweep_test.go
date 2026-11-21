package runner

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// TestRunnerTaskBranchReconcileSweep runs the engine-queued job against a real
// remote: stragglers and a landed task's family go, while live work, the
// retention window, a numeric-prefix neighbour and every non-family branch
// stay. The landed task has no settled_at: its landings row alone is enough.
func TestRunnerTaskBranchReconcileSweep(t *testing.T) {
	t.Parallel()
	markerDir(t)
	remote := newRemote(t)
	e := newTestEnv(t, engine.Config{})
	e.seedProject(remote)
	e.setProjectSettings(`{"branch_retention_days":7,"land_target":"release","plan_file_branch":"brigada/plans"}`)
	old := e.seedTask("", "")
	landed := e.seedTask("", "")
	within := e.seedTask("", "")
	active := e.seedTask("", "")
	doneUnlanded := e.seedTask("", "")
	now := time.Now().UTC()
	err := e.update(func(tx *sql.Tx) error {
		if _, err := tx.Exec("UPDATE tasks SET state = 'cancelled', settled_at = ? WHERE id = ?", now.Add(-8*24*time.Hour).Format(engine.TimeLayout), old); err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE tasks SET state = 'cancelled', settled_at = ? WHERE id = ?", now.Add(-24*time.Hour).Format(engine.TimeLayout), within); err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE tasks SET state = 'done' WHERE id IN (?, ?)", landed, doneUnlanded); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO landings (task_id, project_id, target_branch, commit_sha, pushed, created_at)
			VALUES (?, ?, 'main', 'abc123', 'pushed', ?)`, landed, e.project, e2eSeedTS)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	scratch := t.TempDir()
	git(t, "", "clone", "-q", remote, scratch)
	doomed := []string{
		taskBranch(old, 1),
		fmt.Sprintf("brigada/task-%d-r2-salvage-9", old),
		taskBranch(landed, 1),
		fmt.Sprintf("brigada/task-%d-a2", landed),
	}
	kept := []string{
		taskBranch(within, 1), taskBranch(active, 1), taskBranch(doneUnlanded, 1),
		fmt.Sprintf("brigada/task-%d0", landed),
		"main", "release", "brigada/plans", "feature-x",
	}
	for _, branch := range append(append([]string(nil), doomed...), kept...) {
		git(t, scratch, "push", "-q", "origin", "main:refs/heads/"+branch)
	}
	queued, err := e.eng.QueueTaskBranchSweeps(e.ctx)
	if err != nil || queued != 1 {
		t.Fatalf("queue the sweep = %d, %v; want one job", queued, err)
	}
	job := e.int64Of("SELECT id FROM jobs WHERE kind = 'sweep'")
	runner := e.startRunner("", 100*time.Millisecond)
	defer runner.stop(t)
	e.waitJob(job, func(j *engine.JobInfo) bool { return j.Status == "done" })

	for _, branch := range doomed {
		if got := remoteBranchCommit(t, remote, branch); got != "" {
			t.Errorf("%s = %s after the sweep, want it deleted", branch, got)
		}
	}
	for _, branch := range kept {
		if got := remoteBranchCommit(t, remote, branch); got == "" {
			t.Errorf("%s is gone after the sweep; it was not deletable", branch)
		}
	}
}
