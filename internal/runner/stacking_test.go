package runner

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// stackingWorker adds first.txt on a worktree that lacks it and
// second.txt on one that has it: the file a worktree starts with says
// which base the runner built it on.
const stackingWorker = `
cat > /dev/null
if [ -f first.txt ]; then echo second > second.txt; else echo first > first.txt; fi
git add -A
git commit -qm 'fake work'
br=$(git rev-parse --abbrev-ref HEAD)
printf '{"type":"result","text":"done: {\\"status\\":\\"done\\",\\"summary\\":\\"stacked\\",\\"branch\\":\\"%s\\"}"}\n' "$br"
`

// TestRunnerStacksADependentOnItsBase is the stacked plan end to end
// (§6.1): the dependent's worktree is built on the dependency's result
// branch, so it holds the dependency's file, and the branch it pushes
// differs from the base by its own change alone.
func TestRunnerStacksADependentOnItsBase(t *testing.T) {
	remote := newRemote(t)
	e := newTestEnv(t, engine.Config{})
	e.seedProject(remote)
	res, err := e.eng.CreatePlan(e.ctx, e.project, "stacked", []engine.PlanTask{
		{Name: "first", Spec: "add first.txt", Class: "answer"},
		{Name: "second", Spec: "add second.txt", Class: "answer", Deps: []string{"first"}},
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	first, second := res.Tasks[0].TaskID, res.Tasks[1].TaskID
	markerDir(t)
	p := e.startRunner(writeWorkerScript(t, stackingWorker), 100*time.Millisecond)
	defer p.stop(t)

	e.waitJob(res.Tasks[0].JobID, func(j *engine.JobInfo) bool { return j.Status == "done" })
	e.waitUntil("the dependent's work job to finish", func() bool {
		var n int64
		err := e.st.View(e.ctx, func(tx *sql.Tx) error {
			return tx.QueryRow("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND status = 'done'", second).Scan(&n)
		})
		return err == nil && n == 1
	})

	bare := strings.TrimPrefix(remote, "file://")
	branch := func(task int64) string { return fmt.Sprintf("brigada/task-%d", task) }
	files := func(ref string) string {
		return strings.Join(strings.Fields(git(t, bare, "ls-tree", "-r", "--name-only", ref)), " ")
	}
	if got := files(branch(first)); got != "README.md first.txt" {
		t.Errorf("first's branch holds %q, want README.md first.txt", got)
	}
	if got := files(branch(second)); got != "README.md first.txt second.txt" {
		t.Errorf("second's branch holds %q, want the base's first.txt under its own second.txt", got)
	}
	// The review diff base…branch: only the dependent's own change.
	diff := strings.Fields(git(t, bare, "diff", "--name-only", branch(first)+"..."+branch(second)))
	if len(diff) != 1 || diff[0] != "second.txt" {
		t.Errorf("diff base...branch = %v, want only second.txt", diff)
	}
	e.assertNoWorktrees()
}
