package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/prompt"
)

// TestRunnerLandOntoExistingTarget: a land_target the remote already holds
// with an earlier landing on it is the task's base (§6.1): the worktree is
// built on it, and the landing squashes onto its head with a patch that is
// the task's own change alone — not carrying, or reverting, what the
// target holds beyond the default branch.
func TestRunnerLandOntoExistingTarget(t *testing.T) {
	t.Parallel()
	markerDir(t)
	remote := newRemote(t)
	clone := t.TempDir()
	git(t, clone, "clone", remote, ".")
	git(t, clone, "checkout", "-b", "staging")
	if err := os.WriteFile(filepath.Join(clone, "prior.txt"), []byte("earlier landing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, clone, "add", "-A")
	git(t, clone, "-c", "user.name=s", "-c", "user.email=s@brigada.invalid", "commit", "-m", "earlier landing")
	git(t, clone, "push", "origin", "staging")
	targetHead := remoteBranchCommit(t, remote, "staging")
	mainHead := remoteBranchCommit(t, remote, "main")

	e := newTestEnv(t, engine.Config{})
	e.seedProject(remote)
	e.setProjectSettings(`{"gate_push":"auto","land_target":"staging"}`)
	task := e.seedTask("", `test "$(cat work.txt)" = hello && test -f prior.txt`)
	e.mustExec("UPDATE tasks SET base_ref = 'staging' WHERE id = ?", task)
	work := e.submitWork(task, prompt.ComposeWorkerPrompt("Add work.txt with hello in it.", "", ""))
	setTestVar(t, "BRIGADA_TEST_BRANCH", taskBranch(task, 1))
	e.startLandRunner(t, reviewScript(reviewApproveResult))

	e.waitJob(work, func(j *engine.JobInfo) bool { return j.Status == "done" })
	e.waitTaskState(task, "done")
	commit := e.waitLanding(task, "pushed")
	if got := remoteBranchCommit(t, remote, "staging"); got != commit {
		t.Errorf("remote staging = %s, want the landing commit %s", got, commit)
	}
	if got := parentOf(t, remote, commit); got != targetHead {
		t.Errorf("the landing's parent = %s, want the target's head %s", got, targetHead)
	}
	bare := strings.TrimPrefix(remote, "file://")
	files := strings.Fields(git(t, "", "--git-dir", bare, "diff-tree", "--no-commit-id", "--name-status", "-r", commit))
	if strings.Join(files, " ") != "A work.txt" {
		t.Errorf("the landing's patch = %v, want only the added work.txt", files)
	}
	if got := remoteBranchCommit(t, remote, "main"); got != mainHead {
		t.Errorf("main moved: %s, want %s", got, mainHead)
	}
}

// TestWorkspaceBasesOnLandTarget covers a project with land_target set
// (§6.1): the engine hands the target as the base ref, and the worktree
// starts at the target's fresh head, not the default branch's — also when
// the mirror was cached before the target moved. A target the remote does
// not hold yet (the runner creates it at land time, §7.1) falls back to
// the default branch, in the brigada/ namespace and outside it.
func TestWorkspaceBasesOnLandTarget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	remote := newRemote(t)
	cfg := &Config{StateDir: t.TempDir()}
	mainHead := strings.Fields(git(t, t.TempDir(), "ls-remote", remote, "refs/heads/main"))[0]

	// The default case: a base of main is unchanged.
	ws, err := prepareWorkspace(ctx, cfg, &api.JobGrant{JobID: 41, TaskID: 20, Attempt: 1, RepoURL: remote, BaseRef: "main"})
	if err != nil {
		t.Fatalf("prepare on main: %v", err)
	}
	if ws.base != mainHead {
		t.Errorf("base on main = %s, want %s", ws.base, mainHead)
	}
	ws.remove(ctx)

	commit := []string{"-c", "user.name=s", "-c", "user.email=s@brigada.invalid", "commit", "--allow-empty", "-m"}
	for i, target := range []string{"brigada/landed", "staging"} {
		seed := t.TempDir()
		git(t, seed, "clone", remote, ".")
		git(t, seed, "checkout", "-b", target)
		git(t, seed, append(commit, "landed work")...)
		git(t, seed, "push", "origin", target)
		targetHead := strings.TrimSpace(git(t, seed, "rev-parse", "HEAD"))

		ws, err := prepareWorkspace(ctx, cfg, &api.JobGrant{JobID: int64(42 + i), TaskID: int64(21 + i), Attempt: 1, RepoURL: remote, BaseRef: target})
		if err != nil {
			t.Fatalf("prepare on %s: %v", target, err)
		}
		if ws.base != targetHead {
			t.Errorf("base on %s = %s, want its head %s", target, ws.base, targetHead)
		}
		ws.remove(ctx)

		// The target moves; the cached mirror must see the new head.
		git(t, seed, append(commit, "more landed work")...)
		git(t, seed, "push", "origin", target)
		moved := strings.TrimSpace(git(t, seed, "rev-parse", "HEAD"))
		ws, err = prepareWorkspace(ctx, cfg, &api.JobGrant{JobID: int64(44 + i), TaskID: int64(23 + i), Attempt: 1, RepoURL: remote, BaseRef: target})
		if err != nil {
			t.Fatalf("prepare on moved %s: %v", target, err)
		}
		if ws.base != moved {
			t.Errorf("base on moved %s = %s, want %s", target, ws.base, moved)
		}
		ws.remove(ctx)
	}

	for i, missing := range []string{"brigada/unlanded", "unlanded"} {
		ws, err := prepareWorkspace(ctx, cfg, &api.JobGrant{JobID: int64(50 + i), TaskID: int64(30 + i), Attempt: 1, RepoURL: remote, BaseRef: missing})
		if err != nil {
			t.Fatalf("prepare on missing %s: %v", missing, err)
		}
		if ws.base != mainHead {
			t.Errorf("base on missing %s = %s, want the default branch %s", missing, ws.base, mainHead)
		}
		ws.remove(ctx)
	}
}
