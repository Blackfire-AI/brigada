package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/prompt"
)

// guardWorkspace prepares a fresh work job's workspace for the branch
// guard's tests.
func guardWorkspace(t *testing.T) *workspace {
	t.Helper()
	ctx := context.Background()
	ws, err := prepareWorkspace(ctx, &Config{StateDir: t.TempDir()}, &api.JobGrant{
		JobID: 31, TaskID: 13, Kind: "work", Attempt: 1, RepoURL: newRemote(t), BaseRef: "main",
	})
	if err != nil {
		t.Fatalf("prepare the workspace: %v", err)
	}
	t.Cleanup(func() { ws.remove(context.Background()) })
	return ws
}

// commitAs commits in dir with a throwaway identity.
func commitAs(t *testing.T, dir string, args ...string) {
	t.Helper()
	git(t, dir, append([]string{"-c", "user.name=w", "-c", "user.email=w@brigada.invalid", "commit", "-q"}, args...)...)
}

// sideBranch puts one commit adding file on a new branch side off the
// job's base, and returns to the task branch; it returns the commit.
func sideBranch(t *testing.T, ws *workspace, file string) string {
	t.Helper()
	git(t, ws.dir, "checkout", "-q", "-b", "side", ws.baseCommit)
	if err := os.WriteFile(filepath.Join(ws.dir, file), []byte(file+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws.dir, "add", file)
	commitAs(t, ws.dir, "-m", "side adds "+file)
	sha := strings.TrimSpace(git(t, ws.dir, "rev-parse", "HEAD"))
	git(t, ws.dir, "checkout", "-q", ws.branch)
	return sha
}

// wantGuardRefusal checks err is the branch guard's refusal with every
// one of the words given.
func wantGuardRefusal(t *testing.T, err error, words ...string) {
	t.Helper()
	var bg *branchGuardError
	if !errors.As(err, &bg) {
		t.Fatalf("guard = %v, want a branch guard refusal", err)
	}
	for _, w := range words {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("guard refusal %q does not name %q", err, w)
		}
	}
}

// TestBranchGuardRefusesASwitchedBranch: a worker that left the task
// branch for another is refused, naming the branch it is on.
func TestBranchGuardRefusesASwitchedBranch(t *testing.T) {
	t.Parallel()
	ws := guardWorkspace(t)
	git(t, ws.dir, "checkout", "-q", "-b", "elsewhere")
	wantGuardRefusal(t, ws.guardBranch(context.Background(), false), "on branch elsewhere", ws.branch)
}

// TestBranchGuardRefusesADetachedHead: a detached HEAD is refused.
func TestBranchGuardRefusesADetachedHead(t *testing.T) {
	t.Parallel()
	ws := guardWorkspace(t)
	git(t, ws.dir, "checkout", "-q", "--detach")
	wantGuardRefusal(t, ws.guardBranch(context.Background(), false), "detached", ws.branch)
}

// TestBranchGuardRefusesAHeadOffTheBase: a task branch reset onto a
// history that shares nothing with the job's base is refused, naming the
// base.
func TestBranchGuardRefusesAHeadOffTheBase(t *testing.T) {
	t.Parallel()
	ws := guardWorkspace(t)
	tree := strings.TrimSpace(git(t, ws.dir, "rev-parse", "HEAD^{tree}"))
	orphan := strings.TrimSpace(git(t, ws.dir, "-c", "user.name=w", "-c", "user.email=w@brigada.invalid", "commit-tree", tree, "-m", "orphan"))
	git(t, ws.dir, "reset", "-q", "--hard", orphan)
	wantGuardRefusal(t, ws.guardBranch(context.Background(), false), "no longer descends", "main", ws.baseCommit)
}

// TestBranchGuardRefusesAGrantedWorkersMerge: on a task with tool
// grants, a worker whose git merge committed is refused, naming the
// commits; without grants the same history passes.
func TestBranchGuardRefusesAGrantedWorkersMerge(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ws := guardWorkspace(t)
	side := sideBranch(t, ws, "side.txt")
	commitAs(t, ws.dir, "--allow-empty", "-m", "the worker's own commit")
	git(t, ws.dir, "-c", "user.name=w", "-c", "user.email=w@brigada.invalid", "merge", "-q", "--no-ff", "-m", "merge side", "side")
	merge := strings.TrimSpace(git(t, ws.dir, "rev-parse", "HEAD"))
	err := ws.guardBranch(ctx, true)
	wantGuardRefusal(t, err, "commits of its own", merge, side)
	if err := ws.guardBranch(ctx, false); err != nil {
		t.Errorf("guard without grants = %v, want the worker's commits allowed", err)
	}
}

// TestBranchGuardPassesGrantedUncommittedResults: on a task with tool
// grants, a path checkout, a `git merge --no-commit` and a
// `git cherry-pick -n` whose results are left uncommitted pass the guard,
// and the runner commits and publishes them.
func TestBranchGuardPassesGrantedUncommittedResults(t *testing.T) {
	t.Parallel()
	for name, use := range map[string]func(t *testing.T, ws *workspace, side string){
		"path checkout": func(t *testing.T, ws *workspace, side string) {
			git(t, ws.dir, "checkout", side, "--", "side.txt")
		},
		"merge --no-commit": func(t *testing.T, ws *workspace, side string) {
			gitMerge(t, ws.dir, "merge", "--no-commit", "--no-ff", "side")
		},
		"cherry-pick -n": func(t *testing.T, ws *workspace, side string) {
			git(t, ws.dir, "cherry-pick", "-n", side)
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			ws := guardWorkspace(t)
			side := sideBranch(t, ws, "side.txt")
			use(t, ws, side)
			if err := ws.guardBranch(ctx, true); err != nil {
				t.Fatalf("guard = %v, want the uncommitted result passed", err)
			}
			if err := ws.ensurePushed(ctx); err != nil {
				t.Fatalf("commit and publish: %v", err)
			}
			head := strings.TrimSpace(git(t, ws.dir, "rev-parse", "HEAD"))
			if head == ws.base {
				t.Fatal("nothing was committed")
			}
			if got := remoteHead(t, ws.repoURL, ws.branch); got != head {
				t.Errorf("remote branch = %s, want the runner's commit %s", got, head)
			}
			if out := git(t, ws.dir, "diff", "--name-only", ws.base, head); !strings.Contains(out, "side.txt") {
				t.Errorf("the runner's commit touched %q, want side.txt", out)
			}
		})
	}
}

// TestBranchGuardPassesAnUngrantedAmendedFixRound: a fix round with no
// tool grants that amended its branch passes the guard and publishes
// (§5.4, issue #275); with grants, the amend is the worker's commit.
func TestBranchGuardPassesAnUngrantedAmendedFixRound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	remote := newRemote(t)
	fix, branch := publishFixRound(t, remote, 14)
	commitAs(t, fix.dir, "--amend", "--allow-empty", "-m", "the work, amended")
	amended := strings.TrimSpace(git(t, fix.dir, "rev-parse", "HEAD"))
	wantGuardRefusal(t, fix.guardBranch(ctx, true), amended)
	if err := fix.guardBranch(ctx, false); err != nil {
		t.Fatalf("guard = %v, want the amended round passed", err)
	}
	if err := fix.ensurePushed(ctx); err != nil {
		t.Fatalf("publish the amended branch: %v", err)
	}
	if got := remoteHead(t, remote, branch); got != amended {
		t.Errorf("remote branch = %s, want the amended head %s", got, amended)
	}
}

// branchSwitchingWorker leaves the task branch and reports done.
const branchSwitchingWorker = `
cat > /dev/null
git checkout -q -b elsewhere
echo hello > work.txt
` + "echo '{\"type\":\"result\",\"text\":\"{\\\"status\\\":\\\"done\\\",\\\"summary\\\":\\\"did it\\\"}\"}'\n"

// TestRunnerBranchGuardFailsASwitchedBranch: a worker that switches
// branch fails the job with the branch_guard kind naming the branch,
// nothing is pushed, the task parks for the foreman and the agent is not
// marked broken.
func TestRunnerBranchGuardFailsASwitchedBranch(t *testing.T) {
	t.Parallel()
	remote := newRemote(t)
	e := newTestEnv(t, engine.Config{})
	e.seedProject(remote)
	task := e.seedTask("", "true")
	e.mustExec(`UPDATE tasks SET tool_grants = '["Bash(git checkout:*)"]' WHERE id = ?`, task)
	markerDir(t)

	p := e.startRunner(writeWorkerScript(t, branchSwitchingWorker), 100*time.Millisecond)
	defer p.stop(t)
	job := e.submitWork(task, prompt.ComposeWorkerPrompt("Add work.txt.", "", ""))
	failed := e.waitJob(job, func(j *engine.JobInfo) bool { return j.Status == "failed" || j.Status == "done" })

	if msg := e.errorResult(failed); !strings.Contains(msg, "branch guard") || !strings.Contains(msg, "on branch elsewhere") {
		t.Errorf("error = %q, want the branch guard naming the branch elsewhere", msg)
	}
	if got := e.strOf("SELECT COALESCE(error_kind, '') FROM jobs WHERE id = ?", job); got != engine.ErrorKindBranchGuard {
		t.Errorf("error kind = %q, want %q", got, engine.ErrorKindBranchGuard)
	}
	if h := e.agentHealth(); h != "ready" {
		t.Errorf("agent health = %q, want ready", h)
	}
	if state := e.strOf("SELECT state FROM tasks WHERE id = ?", task); state != "needs_foreman" {
		t.Errorf("task state = %q, want needs_foreman", state)
	}
	if n := e.int64Of("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'work' AND status = 'queued'", task); n != 0 {
		t.Errorf("queued work jobs = %d, want no retry", n)
	}
}
