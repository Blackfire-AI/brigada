package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/prompt"
)

// The land-time pruning of the task-branch family (§13.2): once a
// task's landing commit is on the target, the land job deletes the
// task's whole family from the remote — the first attempt's branch, the
// re-lease and restart branches, the salvages — for the work they
// carried is contained in the target. The set is the remote's own
// branches filtered through the family's matcher, so nothing else —
// the land target, the plans branch, the default branch, a user's
// branch, a task id that merely shares the numeric prefix — is ever
// deleted, and a deletion that fails is logged, never the landing's.

// landedBranchHead reads the tip a landed task's branch had from the
// squash commit's Brigada-Branch trailer on the remote's main: the
// branch itself was pruned at land time (§13.2).
func landedBranchHead(t *testing.T, remote string, task int64) string {
	t.Helper()
	out := git(t, "", "--git-dir", strings.TrimPrefix(remote, "file://"), "log", "-n1",
		"--format=%(trailers:key=Brigada-Branch,valueonly)", "-E",
		fmt.Sprintf("--grep=^Brigada-Task: %d$", task), "main")
	trailer := strings.TrimSpace(out)
	at := strings.LastIndexByte(trailer, '@')
	if at < 0 || at+1 == len(trailer) {
		t.Fatalf("no Brigada-Branch trailer for task %d on main", task)
	}
	return trailer[at+1:]
}

// TestRunnerLandPrunesTheTaskBranchFamily: a landed task's whole ref
// family — the first attempt's branch, a re-lease's -a<n>, a restart's
// -r<n> and a salvage — goes from the remote with the landing's push,
// while a task id that shares the numeric prefix and a user's branch
// stay.
func TestRunnerLandPrunesTheTaskBranchFamily(t *testing.T) {
	t.Parallel()
	markerDir(t)
	remote := newRemote(t)
	e := newTestEnv(t, engine.Config{})
	e.seedProject(remote)
	e.setProjectSettings(`{"gate_push":"auto"}`)
	task := e.seedTask("", `test "$(cat work.txt)" = hello`)
	e.submitWork(task, prompt.ComposeWorkerPrompt("Add work.txt with hello in it.", "", ""))
	setTestVar(t, "BRIGADA_TEST_BRANCH", taskBranch(task, 1))

	// The rest of the family, pushed before the runner starts so the
	// prune's enumeration sees it, beside the refs the prune must never
	// touch: a task id that shares the numeric prefix, a user's branch.
	scratch := t.TempDir()
	git(t, "", "clone", "-q", remote, scratch)
	family := []string{
		fmt.Sprintf("brigada/task-%d-a2", task),
		fmt.Sprintf("brigada/task-%d-r1", task),
		fmt.Sprintf("brigada/task-%d-r1-salvage-9", task),
	}
	kept := []string{
		fmt.Sprintf("brigada/task-%d0", task),
		"feature-x",
	}
	for _, b := range append(append([]string(nil), family...), kept...) {
		git(t, scratch, "push", "-q", "origin", "main:refs/heads/"+b)
	}

	e.startLandRunner(t, reviewScript(reviewApproveResult))
	e.waitTaskState(task, "done")
	e.waitLanding(task, "pushed")

	for _, b := range append([]string{taskBranch(task, 1)}, family...) {
		if got := remoteBranchCommit(t, remote, b); got != "" {
			t.Errorf("%s = %s on the remote after the landing, want it pruned", b, got)
		}
	}
	for _, b := range append([]string{"main"}, kept...) {
		if got := remoteBranchCommit(t, remote, b); got == "" {
			t.Errorf("%s is gone from the remote; the prune must never touch it", b)
		}
	}
}

// TestRunnerLandPruneFailureDoesNotFailTheLand: a remote that refuses
// the family's deletion keeps the landing — the commit is pushed and
// the record says pushed — and the family stays for the reconcile
// sweep (§13.2).
func TestRunnerLandPruneFailureDoesNotFailTheLand(t *testing.T) {
	t.Parallel()
	markerDir(t)
	remote := newRemote(t)
	// The bare repo's pre-receive refuses deletes of the family's refs;
	// creates and the target's push pass.
	hook := filepath.Join(strings.TrimPrefix(remote, "file://"), "hooks", "pre-receive")
	script := "#!/bin/sh\n" +
		"while read old new ref; do\n" +
		"  case \"$ref\" in refs/heads/brigada/*)\n" +
		"    [ \"$new\" = 0000000000000000000000000000000000000000 ] && exit 1;;\n" +
		"  esac\n" +
		"done\n" +
		"exit 0\n"
	if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	e := newTestEnv(t, engine.Config{})
	task, _ := e.landTest(t, remote, `{"gate_push":"auto"}`,
		"Add work.txt with hello in it.", `test "$(cat work.txt)" = hello`, reviewScript(reviewApproveResult))

	commit := e.waitLanding(task, "pushed")
	if got := remoteBranchCommit(t, remote, "main"); got != commit {
		t.Errorf("remote main = %s, want the landing commit %s", got, commit)
	}
	if got := remoteBranchCommit(t, remote, taskBranch(task, 1)); got == "" {
		t.Error("the task's branch is gone though the remote refused the deletion")
	}
}
