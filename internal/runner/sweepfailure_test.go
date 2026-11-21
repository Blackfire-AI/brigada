package runner

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// TestRunnerTaskBranchSweepFailuresAreBestEffort: one refused deletion leaves
// its ref for the next start, but neither fails the job nor holds back another
// doomed ref in the same enumeration.
func TestRunnerTaskBranchSweepFailuresAreBestEffort(t *testing.T) {
	t.Parallel()
	markerDir(t)
	remote := newRemote(t)
	e := newTestEnv(t, engine.Config{})
	e.seedProject(remote)
	e.setProjectSettings(`{"branch_retention_days":0}`)
	refused := e.seedTask("", "")
	deleted := e.seedTask("", "")
	err := e.update(func(tx *sql.Tx) error {
		_, err := tx.Exec("UPDATE tasks SET state = 'cancelled', settled_at = ? WHERE id IN (?, ?)",
			time.Now().UTC().Format(engine.TimeLayout), refused, deleted)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	scratch := t.TempDir()
	git(t, "", "clone", "-q", remote, scratch)
	for _, id := range []int64{refused, deleted} {
		git(t, scratch, "push", "-q", "origin", "main:refs/heads/"+taskBranch(id, 1))
	}
	hook := filepath.Join(strings.TrimPrefix(remote, "file://"), "hooks", "pre-receive")
	script := fmt.Sprintf("#!/bin/sh\nwhile read old new ref; do\n  [ \"$ref\" = refs/heads/%s ] && exit 1\ndone\nexit 0\n", taskBranch(refused, 1))
	if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := e.eng.QueueTaskBranchSweeps(e.ctx); err != nil {
		t.Fatal(err)
	}
	job := e.int64Of("SELECT id FROM jobs WHERE kind = 'sweep'")
	runner := e.startRunner("", 100*time.Millisecond)
	defer runner.stop(t)
	e.waitJob(job, func(j *engine.JobInfo) bool { return j.Status == "done" })
	if got := remoteBranchCommit(t, remote, taskBranch(refused, 1)); got == "" {
		t.Error("the refused ref is gone; the failure fixture did not hold")
	}
	if got := remoteBranchCommit(t, remote, taskBranch(deleted, 1)); got != "" {
		t.Errorf("the other doomed ref = %s, want it deleted despite its neighbour's refusal", got)
	}
}
