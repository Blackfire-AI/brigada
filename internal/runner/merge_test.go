package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/api"
)

// gitMerge runs a git command that may exit non-zero — a merge conflict is
// this test's own setup, not a failure.
func gitMerge(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, _ := cmd.CombinedOutput()
	return string(out)
}

// makeMergeConflict puts the task branch and a side branch on conflicting
// edits of README.md and runs the worker's `git merge --no-commit side`,
// leaving the conflict (MERGE_HEAD set, README.md unmerged with markers)
// in the worktree.
func makeMergeConflict(t *testing.T, ws *workspace) {
	t.Helper()
	git(t, ws.dir, "checkout", "-b", "side")
	if err := os.WriteFile(filepath.Join(ws.dir, "README.md"), []byte("# side\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws.dir, "add", "-A")
	git(t, ws.dir, "-c", "user.name=side", "-c", "user.email=side@brigada.invalid", "commit", "-m", "side change")
	git(t, ws.dir, "checkout", ws.branch)
	if err := os.WriteFile(filepath.Join(ws.dir, "README.md"), []byte("# task\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws.dir, "add", "-A")
	git(t, ws.dir, "-c", "user.name=task", "-c", "user.email=task@brigada.invalid", "commit", "-m", "task change")
	gitMerge(t, ws.dir, "merge", "--no-commit", "side")
}

// TestEnsurePushedCommitsFinishedMerge: a worker's merge the worker
// resolved — MERGE_HEAD present, nothing left unmerged — is committed by
// the runner as the two-parent merge git records, and pushed.
func TestEnsurePushedCommitsFinishedMerge(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	remote := newRemote(t)
	ws, err := prepareWorkspace(ctx, &Config{StateDir: t.TempDir()}, &api.JobGrant{
		JobID: 24, TaskID: 12, Attempt: 1, RepoURL: remote, BaseRef: "main",
	})
	if err != nil {
		t.Fatalf("prepare the workspace: %v", err)
	}
	defer ws.remove(ctx)

	makeMergeConflict(t, ws)
	if err := os.WriteFile(filepath.Join(ws.dir, "README.md"), []byte("# merged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws.dir, "add", "README.md") // the worker resolved and staged it

	if err := ws.ensurePushed(ctx); err != nil {
		t.Fatalf("commit and push the finished merge: %v", err)
	}
	sha := remoteBranchCommit(t, remote, ws.branch)
	bare := strings.TrimPrefix(remote, "file://")
	parents := strings.Fields(git(t, "", "--git-dir", bare, "show", "-s", "--format=%P", sha))
	if len(parents) != 2 {
		t.Errorf("merge commit %s has %d parents, want a two-parent merge", sha, len(parents))
	}
}

// TestEnsurePushedRefusesUnfinishedMerge: a worker's merge still holding
// unmerged, marker-carrying files fails the done job, naming the files,
// and nothing is pushed.
func TestEnsurePushedRefusesUnfinishedMerge(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	remote := newRemote(t)
	ws, err := prepareWorkspace(ctx, &Config{StateDir: t.TempDir()}, &api.JobGrant{
		JobID: 25, TaskID: 13, Attempt: 1, RepoURL: remote, BaseRef: "main",
	})
	if err != nil {
		t.Fatalf("prepare the workspace: %v", err)
	}
	defer ws.remove(ctx)

	makeMergeConflict(t, ws) // left unresolved

	err = ws.ensurePushed(ctx)
	if err == nil || !strings.Contains(err.Error(), "unfinished merge") || !strings.Contains(err.Error(), "README.md") {
		t.Fatalf("ensurePushed = %v, want an unfinished-merge refusal naming README.md", err)
	}
	if got := remoteBranchCommit(t, remote, ws.branch); got != "" {
		t.Errorf("the unfinished merge was pushed (%s)", got)
	}
}

// TestSalvageAbortsUnfinishedMerge: a failed job's tree holding an
// unfinished merge does not mask the original failure — salvage aborts the
// merge and commits the worker's other work, recording the abort.
func TestSalvageAbortsUnfinishedMerge(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	remote := newRemote(t)
	ws, err := prepareWorkspace(ctx, &Config{StateDir: t.TempDir()}, &api.JobGrant{
		JobID: 26, TaskID: 14, Attempt: 1, RepoURL: remote, BaseRef: "main",
	})
	if err != nil {
		t.Fatalf("prepare the workspace: %v", err)
	}
	defer ws.remove(ctx)

	makeMergeConflict(t, ws)
	if err := os.WriteFile(filepath.Join(ws.dir, "work.txt"), []byte("saved\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	branch := ws.branch + "-salvage-26"
	saved, err := ws.salvage(ctx, branch)
	if err != nil || !saved {
		t.Fatalf("salvage = %v, %v; want the other work saved", saved, err)
	}
	bare := strings.TrimPrefix(remote, "file://")
	if got := git(t, "", "--git-dir", bare, "show", branch+":work.txt"); got != "saved\n" {
		t.Errorf("salvage holds %q, want the uncommitted edit", got)
	}
	// The aborted merge leaves the task's own README.md, not the markers.
	if got := git(t, "", "--git-dir", bare, "show", branch+":README.md"); got != "# task\n" {
		t.Errorf("salvage README.md = %q, want the task's own, the merge aborted", got)
	}
	msg := git(t, "", "--git-dir", bare, "show", "-s", "--format=%s", branch)
	if !strings.Contains(msg, "merge was aborted") {
		t.Errorf("salvage commit message = %q, want it to record the abort", msg)
	}
}
