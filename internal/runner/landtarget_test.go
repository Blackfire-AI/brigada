package runner

import (
	"context"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/api"
)

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
