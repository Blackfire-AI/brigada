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

// TestTaskBranch pins the result branch names (§5.1): brigada, with an
// attempt suffix once a job was re-leased. The exact strings matter —
// the fetch exclusion lives in the same namespace.
func TestTaskBranch(t *testing.T) {
	cases := []struct {
		taskID, attempt int64
		want            string
	}{
		{7, 1, "brigada/task-7"},
		{7, 2, "brigada/task-7-a2"},
		{7, 5, "brigada/task-7-a5"},
	}
	for _, tc := range cases {
		if got := taskBranch(tc.taskID, tc.attempt); got != tc.want {
			t.Errorf("taskBranch(%d, %d) = %q, want %q", tc.taskID, tc.attempt, got, tc.want)
		}
	}
}

func TestPrepareWorkspaceRejectsMissingURL(t *testing.T) {
	cfg := &Config{StateDir: t.TempDir()}
	grant := &api.JobGrant{TaskID: 3, BaseRef: "main", RepoURL: ""}
	_, err := prepareWorkspace(context.Background(), cfg, grant)
	if err == nil || !strings.Contains(err.Error(), "no repo URL") {
		t.Fatalf("a repo without a URL must be rejected, got %v", err)
	}
	grant = &api.JobGrant{TaskID: 3, RepoURL: newRemote(t), BaseRef: ""}
	_, err = prepareWorkspace(context.Background(), cfg, grant)
	if err == nil || !strings.Contains(err.Error(), "base ref") {
		t.Fatalf("a task without a base ref must be rejected, got %v", err)
	}
}

// TestWorkspaceLifecycle drives the whole workspace story (§5.1) against
// a real bare repo: the mirror is cloned, the worktree is built on the
// branch from the base ref, a commit lands on it, the runner pushes it
// when the worker did not, the remote ends up with the branch, and the
// worktree comes down while the mirror stays cached.
func TestWorkspaceLifecycle(t *testing.T) {
	ctx := context.Background()
	cfg := &Config{StateDir: t.TempDir()}
	remote := newRemote(t)

	ws, err := prepareWorkspace(ctx, cfg, &api.JobGrant{
		JobID: 11, TaskID: 4, Attempt: 1, RepoURL: remote, BaseRef: "main",
	})
	if err != nil {
		t.Fatalf("prepare the workspace: %v", err)
	}
	defer ws.remove(ctx)
	if ws.branch != "brigada/task-4" {
		t.Errorf("branch = %q, want brigada/task-4", ws.branch)
	}
	if _, err := os.Stat(filepath.Join(ws.dir, "README.md")); err != nil {
		t.Errorf("the worktree has no checkout of the base ref: %v", err)
	}
	if _, ok := ws.remoteBranchSHA(ctx); ok {
		t.Error("the remote already has the branch before any push")
	}

	// "Worker" edits and commits, but does not push.
	git(t, ws.dir, "commit", "--allow-empty", "-m", "work")
	if err := ws.ensurePushed(ctx); err != nil {
		t.Fatalf("the runner pushes the branch the worker left: %v", err)
	}

	// The remote's main moves on between the two jobs, so the second
	// workspace proves the cached mirror was fetched, not re-cloned —
	// and that the checked-out result branch does not block the fetch.
	upd := filepath.Join(t.TempDir(), "upd")
	git(t, "", "clone", "-q", remote, upd)
	git(t, upd, "-c", "user.name=seed", "-c", "user.email=seed@brigada.invalid",
		"commit", "--allow-empty", "-m", "main moved on")
	git(t, upd, "push", "-q", "origin", "main")

	ws2, err := prepareWorkspace(ctx, cfg, &api.JobGrant{
		JobID: 12, TaskID: 4, Attempt: 2, RepoURL: remote, BaseRef: "main",
	})
	if err != nil {
		t.Fatalf("prepare the second workspace: %v", err)
	}
	defer ws2.remove(ctx)
	if ws2.branch != "brigada/task-4-a2" {
		t.Errorf("second attempt branch = %q, want the attempt suffix", ws2.branch)
	}
	if out := git(t, ws2.dir, "log", "--oneline", "-1"); !strings.Contains(out, "main moved on") {
		t.Errorf("the mirror was not fetched: latest commit is %q", strings.TrimSpace(out))
	}

	ws.remove(ctx)
	ws2.remove(ctx)
	entries, err := os.ReadDir(filepath.Join(cfg.StateDir, "worktrees"))
	if err == nil && len(entries) != 0 {
		t.Errorf("worktrees left behind: %v", entries)
	}
	mirrors, err := os.ReadDir(filepath.Join(cfg.StateDir, "mirrors"))
	if err != nil || len(mirrors) != 1 {
		t.Errorf("the mirror cache did not survive: %v (%v)", mirrors, err)
	}
}

// TestEnsurePushedRejectsStaleRemoteBranch covers the worker that
// pushed an early commit and then kept working: the branch exists on
// the remote, but it does not hold the worktree's commit, and the
// runner must push the real one instead of trusting the name.
func TestEnsurePushedRejectsStaleRemoteBranch(t *testing.T) {
	ctx := context.Background()
	remote := newRemote(t)
	ws, err := prepareWorkspace(ctx, &Config{StateDir: t.TempDir()}, &api.JobGrant{
		JobID: 21, TaskID: 6, Attempt: 1, RepoURL: remote, BaseRef: "main",
	})
	if err != nil {
		t.Fatalf("prepare the workspace: %v", err)
	}
	defer ws.remove(ctx)

	// The worker pushes an early, empty commit, then keeps working.
	git(t, ws.dir, "commit", "--allow-empty", "-m", "early")
	git(t, ws.dir, "push", "-q", "origin", "HEAD")
	git(t, ws.dir, "commit", "--allow-empty", "-m", "the real work")

	if err := ws.ensurePushed(ctx); err != nil {
		t.Fatalf("the runner must replace the stale remote branch: %v", err)
	}
	head, err := ws.worktreeHead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := ws.remoteBranchSHA(ctx); got != head {
		t.Errorf("remote holds %q, want the worktree's %q (%q)", got, head, "the real work")
	}
	// And what shipped is the real work, not the early commit.
	clone := t.TempDir()
	git(t, "", "clone", "-q", "--branch", ws.branch, remote, clone)
	if out := git(t, clone, "log", "--oneline", "-1"); !strings.Contains(out, "the real work") {
		t.Errorf("the remote branch's tip is %q, want the worktree's commit", strings.TrimSpace(out))
	}
}

// TestEnsurePushedCommitsLeftovers covers the worker that reported done
// with uncommitted edits: without a commit, pushing HEAD would ship the
// base commit and the run's branch would be empty of the work.
func TestEnsurePushedCommitsLeftovers(t *testing.T) {
	ctx := context.Background()
	remote := newRemote(t)
	ws, err := prepareWorkspace(ctx, &Config{StateDir: t.TempDir()}, &api.JobGrant{
		JobID: 22, TaskID: 7, Attempt: 1, RepoURL: remote, BaseRef: "main",
	})
	if err != nil {
		t.Fatalf("prepare the workspace: %v", err)
	}
	defer ws.remove(ctx)

	// The worker edits but neither commits nor pushes.
	if err := os.WriteFile(filepath.Join(ws.dir, "work.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ws.ensurePushed(ctx); err != nil {
		t.Fatalf("the runner must commit and push the leftovers: %v", err)
	}
	clone := t.TempDir()
	git(t, "", "clone", "-q", "--branch", ws.branch, remote, clone)
	if data, err := os.ReadFile(filepath.Join(clone, "work.txt")); err != nil || string(data) != "hello\n" {
		t.Errorf("the pushed branch does not hold the worker's edit: %q (%v)", data, err)
	}
	// A second pass over the now-clean, now-current tree is a no-op.
	if err := ws.ensurePushed(ctx); err != nil {
		t.Errorf("re-verifying a shipped branch: %v", err)
	}
}

// TestRedactURL checks that credentials riding a repo URL never reach
// an error message.
func TestRedactURL(t *testing.T) {
	cases := map[string]string{
		"https://user:secret@example.invalid/repo.git": "https://***@example.invalid/repo.git",
		"https://token@example.invalid/repo.git":       "https://token@example.invalid/repo.git", // no password: nothing to redact
		"file:///tmp/remote.git":                       "file:///tmp/remote.git",
		"git@github.com:org/repo.git":                  "git@github.com:org/repo.git",
	}
	for in, want := range cases {
		if got := redactURL(in); got != want {
			t.Errorf("redactURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// newRemote makes a local bare repo as the task's git remote, seeded
// with one commit on main, and returns its path (a file URL for git).
func newRemote(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	remote := filepath.Join(dir, "remote.git")
	seed := filepath.Join(dir, "seed")
	git(t, dir, "init", "--bare", "-b", "main", remote)
	git(t, dir, "init", "-b", "main", seed)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("# seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, seed, "add", "-A")
	git(t, seed, "-c", "user.name=seed", "-c", "user.email=seed@brigada.invalid", "commit", "-m", "seed")
	git(t, seed, "push", "file://"+remote, "main")
	return "file://" + remote
}

// git runs one git command in dir and returns its output, failing the
// test on error.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// TestWorkspaceBaseOnResultBranch covers the follow-up job that builds
// on an earlier attempt's result branch (§5.3): the mirror's fetch
// skips the brigada/ namespace, so the base branch is fetched by name —
// and a runner that never ran the earlier attempt still gets its ref.
func TestWorkspaceBaseOnResultBranch(t *testing.T) {
	ctx := context.Background()
	remote := newRemote(t)
	cfg := &Config{StateDir: t.TempDir()}

	// A first job builds the mirror cache while the remote has no
	// result branches at all.
	ws1, err := prepareWorkspace(ctx, cfg, &api.JobGrant{
		JobID: 31, TaskID: 8, Attempt: 1, RepoURL: remote, BaseRef: "main",
	})
	if err != nil {
		t.Fatalf("prepare the first workspace: %v", err)
	}
	git(t, ws1.dir, "commit", "--allow-empty", "-m", "earlier attempt")
	if err := ws1.ensurePushed(ctx); err != nil {
		t.Fatalf("ship the earlier attempt: %v", err)
	}
	ws1.remove(ctx)

	// The follow-up names the earlier attempt's branch as its base, on
	// a mirror that has never seen it: the explicit fetch of the base
	// ref must bring it.
	ws2, err := prepareWorkspace(ctx, cfg, &api.JobGrant{
		JobID: 32, TaskID: 9, Attempt: 1, RepoURL: remote, BaseRef: "brigada/task-8",
	})
	if err != nil {
		t.Fatalf("prepare the follow-up workspace on the result branch: %v", err)
	}
	defer ws2.remove(ctx)
	if out := git(t, ws2.dir, "log", "--oneline", "-1"); !strings.Contains(out, "earlier attempt") {
		t.Errorf("the follow-up's base = %q, want the earlier attempt's commit", strings.TrimSpace(out))
	}
	// And its own branch is still its own.
	if ws2.branch != "brigada/task-9" {
		t.Errorf("follow-up branch = %q, want brigada/task-9", ws2.branch)
	}
}
