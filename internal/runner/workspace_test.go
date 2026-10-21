package runner

import (
	"context"
	"errors"
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
	// The grant's own branch wins verbatim, whatever the attempt; only a
	// grant without one — an older engine — falls back to the naming
	// above.
	if got := grantBranch(&api.JobGrant{TaskID: 7, Attempt: 2, Branch: "brigada/task-7-r1"}); got != "brigada/task-7-r1" {
		t.Errorf("grantBranch with a branch = %q, want the grant's own", got)
	}
	if got := grantBranch(&api.JobGrant{TaskID: 7, Attempt: 2}); got != "brigada/task-7-a2" {
		t.Errorf("grantBranch without a branch = %q, want the fallback", got)
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

// TestWorktreeDirNamesTheSessionStablePath pins the worktree naming
// (§5.1): a task job's path is the task's, its role's and its session
// generation's, so the jobs that share a session share a directory and a
// restart's attempt does not. A job without a task keeps the per-job
// path: nothing it runs shares the directory.
func TestWorktreeDirNamesTheSessionStablePath(t *testing.T) {
	dir := func(grant *api.JobGrant) string { return filepath.Base(worktreeDir("/state", grant)) }
	work := &api.JobGrant{JobID: 1, TaskID: 7, Kind: "work", Attempt: 1}
	fix := &api.JobGrant{JobID: 2, TaskID: 7, Kind: "fix", Attempt: 1, ResumeSessionID: "sess-1"}
	if got, want := dir(work), "task-7-author-s0"; got != want {
		t.Errorf("work path = %q, want %q", got, want)
	}
	if dir(fix) != dir(work) {
		t.Errorf("the fix round runs at %q, want the work job's %q: a resumed session needs its directory", dir(fix), dir(work))
	}
	// A lost lease re-taken keeps the path: the job, its attempt counter
	// and its path are not the same thing (§5.1).
	refix := &api.JobGrant{JobID: 2, TaskID: 7, Kind: "fix", Attempt: 3, ResumeSessionID: "sess-1"}
	if dir(refix) != dir(work) {
		t.Errorf("the re-leased fix runs at %q, want the session's %q", dir(refix), dir(work))
	}
	// The reviewer's role is the reviewer's own: a re-review resuming the
	// review's session runs where the review did.
	review := &api.JobGrant{JobID: 3, TaskID: 7, Kind: "review", Attempt: 1}
	rereview := &api.JobGrant{JobID: 4, TaskID: 7, Kind: "review", Attempt: 1, ResumeSessionID: "sess-rev"}
	if got, want := dir(review), "task-7-review-s0"; got != want {
		t.Errorf("review path = %q, want %q", got, want)
	}
	if dir(rereview) != dir(review) {
		t.Errorf("the resumed re-review runs at %q, want the review's %q", dir(rereview), dir(review))
	}
	// A restart begins a new session generation and never reuses the
	// directory of the session it does not resume.
	restart := &api.JobGrant{JobID: 5, TaskID: 7, Kind: "work", Attempt: 1, SessionGen: 1}
	if dir(restart) == dir(work) {
		t.Errorf("the restart reuses %q, want its own generation's path", dir(restart))
	}
	if got, want := dir(restart), "task-7-author-s1"; got != want {
		t.Errorf("restart path = %q, want %q", got, want)
	}
	// A fresh job without a task — a standalone review — keeps the
	// per-job path of job id and attempt.
	standalone := &api.JobGrant{JobID: 9, Kind: "review", Attempt: 1}
	if got, want := dir(standalone), "job-9-a1"; got != want {
		t.Errorf("standalone review path = %q, want %q", got, want)
	}
	if dir(&api.JobGrant{JobID: 9, Kind: "review", Attempt: 2}) == dir(standalone) {
		t.Error("two attempts of a standalone review share a directory")
	}
}

// TestSessionWorktreeRunsWhereItsSessionStarted covers the shared path
// against a real repo: the work job's worktree and the fix round's sit
// at one directory, one after the other, and the second finds a clean
// checkout of the first's work — the state a harness session expects
// when it resumes where it started.
func TestSessionWorktreeRunsWhereItsSessionStarted(t *testing.T) {
	ctx := context.Background()
	remote := newRemote(t)
	cfg := &Config{StateDir: t.TempDir()}

	work, err := prepareWorkspace(ctx, cfg, &api.JobGrant{
		JobID: 41, TaskID: 5, Kind: "work", Attempt: 1,
		RepoURL: remote, BaseRef: "main", Branch: "brigada/task-5",
	})
	if err != nil {
		t.Fatalf("prepare the work workspace: %v", err)
	}
	git(t, work.dir, "commit", "--allow-empty", "-m", "the work")
	if err := work.ensurePushed(ctx); err != nil {
		t.Fatalf("ship the work: %v", err)
	}
	path := work.dir
	work.remove(ctx)

	fix, err := prepareWorkspace(ctx, cfg, &api.JobGrant{
		JobID: 42, TaskID: 5, Kind: "fix", Attempt: 1, ResumeSessionID: "sess-1",
		RepoURL: remote, BaseRef: "main", Branch: "brigada/task-5",
	})
	if err != nil {
		t.Fatalf("prepare the fix workspace: %v", err)
	}
	defer fix.remove(ctx)
	if fix.dir != path {
		t.Errorf("the fix round runs at %s, want the work job's %s", fix.dir, path)
	}
	if out := git(t, fix.dir, "log", "--oneline", "-1"); !strings.Contains(out, "the work") {
		t.Errorf("the fix worktree starts at %q, want the pushed work", strings.TrimSpace(out))
	}
	entries, err := os.ReadDir(filepath.Join(cfg.StateDir, "worktrees"))
	if err != nil || len(entries) != 1 {
		t.Errorf("the task holds %d worktree directories (err %v), want the one shared path", len(entries), err)
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
	// It is another task's job: a task's worktree path is its own
	// (§5.1), and the first task's is still live.
	upd := filepath.Join(t.TempDir(), "upd")
	git(t, "", "clone", "-q", remote, upd)
	git(t, upd, "-c", "user.name=seed", "-c", "user.email=seed@brigada.invalid",
		"commit", "--allow-empty", "-m", "main moved on")
	git(t, upd, "push", "-q", "origin", "main")

	ws2, err := prepareWorkspace(ctx, cfg, &api.JobGrant{
		JobID: 12, TaskID: 5, Attempt: 2, RepoURL: remote, BaseRef: "main",
	})
	if err != nil {
		t.Fatalf("prepare the second workspace: %v", err)
	}
	defer ws2.remove(ctx)
	if ws2.branch != "brigada/task-5-a2" {
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

// A different runner may have already pushed the work and more. Only
// work missing from that remote belongs on a salvage branch, even when
// the checkout and the task's remote branch have diverged.
func TestSalvageComparesAgainstRemote(t *testing.T) {
	ctx := context.Background()
	remote := newRemote(t)
	ws, err := prepareWorkspace(ctx, &Config{StateDir: t.TempDir()}, &api.JobGrant{
		JobID: 23, TaskID: 5, Attempt: 1, RepoURL: remote, BaseRef: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.remove(ctx)
	git(t, ws.dir, "commit", "--allow-empty", "-m", "already saved")
	if err := ws.ensurePushed(ctx); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	git(t, "", "clone", "-q", "--branch", ws.branch, remote, other)
	git(t, other, "-c", "user.name=other", "-c", "user.email=other@brigada.invalid",
		"commit", "--allow-empty", "-m", "later work")
	git(t, other, "push", "-q", "origin", "HEAD")
	before := remoteBranchCommit(t, remote, ws.branch)
	branch := ws.branch + "-salvage-23"
	if saved, err := ws.salvage(ctx, branch); err != nil || saved {
		t.Fatalf("salvage already saved work = %v, %v; want no salvage", saved, err)
	}
	if got := remoteBranchCommit(t, remote, branch); got != "" {
		t.Errorf("salvage branch holds %q, want no branch", got)
	}
	if got := remoteBranchCommit(t, remote, ws.branch); got != before {
		t.Errorf("salvage moved the task's remote branch from %q to %q", before, got)
	}
	// New edits still need saving, on their own branch. Committing and
	// pushing them must preserve the other runner's later work as well.
	if err := os.WriteFile(filepath.Join(ws.dir, "work.txt"), []byte("leftovers\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if saved, err := ws.salvage(ctx, branch); err != nil || !saved {
		t.Fatalf("salvage new edits = %v, %v; want saved", saved, err)
	}
	bare := strings.TrimPrefix(remote, "file://")
	if got := git(t, "", "--git-dir", bare, "show", branch+":work.txt"); got != "leftovers\n" {
		t.Errorf("salvage holds %q, want the uncommitted edit", got)
	}
	if got := remoteBranchCommit(t, remote, ws.branch); got != before {
		t.Errorf("saving edits moved the task's remote branch from %q to %q", before, got)
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
		// A scp-like password holding digits and a '/' is no host and port.
		"fatal: 'u.n:1234/s3cr3t/x@example.invalid:t/r' failed": "fatal: '***@example.invalid:t/r' failed",
	}
	for in, want := range cases {
		if got := redactURL(in); got != want {
			t.Errorf("redactURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestFetchFailuresAreInfrastructureErrors pins issue #46's fix: a
// clone or fetch of the remote that fails is the runner's plumbing —
// the shape of a network that is not up in the seconds after a wake —
// and completes as an infrastructure error (engine.ErrorKindInfra) the
// engine retries, capped. A grant that cannot be run at all stays the
// task's own failure.
func TestFetchFailuresAreInfrastructureErrors(t *testing.T) {
	ctx := context.Background()
	remote := newRemote(t)
	cfg := &Config{StateDir: t.TempDir()}
	grant := &api.JobGrant{
		JobID: 51, TaskID: 11, Kind: "work", Attempt: 1,
		RepoURL: remote, BaseRef: "main", Branch: "brigada/task-11",
	}
	ws, err := prepareWorkspace(ctx, cfg, grant)
	if err != nil {
		t.Fatalf("prepare the workspace: %v", err)
	}
	ws.remove(ctx)

	// The remote goes away, and the mirror's fetch fails as
	// infrastructure.
	if err := os.Rename(strings.TrimPrefix(remote, "file://"), strings.TrimPrefix(remote, "file://")+"-gone"); err != nil {
		t.Fatal(err)
	}
	var infra *infraError
	if _, err := prepareWorkspace(ctx, cfg, grant); err == nil || !errors.As(err, &infra) {
		t.Fatalf("a failed fetch of the mirror = %v, want an infrastructure error", err)
	} else if !strings.Contains(err.Error(), "fetch the mirror") {
		t.Errorf("fetch failure = %q, want the mirror's fetch named", err)
	}
	// The branches the worktree builds on are fetched by name the same
	// way, and fail the same way.
	if err := ws.fetchBranch(ctx, "main"); err == nil || !errors.As(err, &infra) {
		t.Fatalf("a failed fetch of a branch = %v, want an infrastructure error", err)
	}
	// The first job of a repo clones instead of fetching: the same
	// fault, the same class.
	if _, err := prepareWorkspace(ctx, &Config{StateDir: t.TempDir()}, grant); err == nil || !errors.As(err, &infra) {
		t.Fatalf("a failed clone = %v, want an infrastructure error", err)
	} else if !strings.Contains(err.Error(), "clone") {
		t.Errorf("clone failure = %q, want the clone named", err)
	}

	// A grant that cannot be run by anyone — no repo to fetch at all —
	// is not the runner's plumbing and stays the task's failure.
	_, err = prepareWorkspace(ctx, cfg, &api.JobGrant{TaskID: 12, BaseRef: "main", RepoURL: ""})
	if err == nil || errors.As(err, &infra) {
		t.Fatalf("a task without a repo URL = %v, want the task's own failure", err)
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

// TestFixJobContinuesTheBranch covers the fix round's workspace (§3,
// §5.1): the grant names the task's branch — the engine's choice, not
// the runner's — and the worktree starts at that branch's head as the
// remote holds it, so the round's work builds on the work before it and
// its push is a fast-forward of the branch, never a force.
func TestFixJobContinuesTheBranch(t *testing.T) {
	ctx := context.Background()
	remote := newRemote(t)
	cfg := &Config{StateDir: t.TempDir()}

	// The work round: the branch the grant names, pushed with its work.
	ws, err := prepareWorkspace(ctx, cfg, &api.JobGrant{
		JobID: 41, TaskID: 5, Kind: "work", Attempt: 1,
		RepoURL: remote, BaseRef: "main", Branch: "brigada/task-5",
	})
	if err != nil {
		t.Fatalf("prepare the work workspace: %v", err)
	}
	if ws.branch != "brigada/task-5" {
		t.Errorf("work branch = %q, want the grant's brigada/task-5", ws.branch)
	}
	git(t, ws.dir, "commit", "--allow-empty", "-m", "the work")
	if err := ws.ensurePushed(ctx); err != nil {
		t.Fatalf("ship the work: %v", err)
	}
	ws.remove(ctx)

	// The fix round continues the branch: its worktree starts at the
	// pushed head, and its own work lands on top of it.
	fix, err := prepareWorkspace(ctx, cfg, &api.JobGrant{
		JobID: 42, TaskID: 5, Kind: "fix", Attempt: 1,
		RepoURL: remote, BaseRef: "main", Branch: "brigada/task-5",
	})
	if err != nil {
		t.Fatalf("prepare the fix workspace: %v", err)
	}
	defer fix.remove(ctx)
	if fix.branch != "brigada/task-5" {
		t.Errorf("fix branch = %q, want the task's brigada/task-5", fix.branch)
	}
	if out := git(t, fix.dir, "log", "--oneline", "-1"); !strings.Contains(out, "the work") {
		t.Errorf("the fix worktree starts at %q, want the pushed work", strings.TrimSpace(out))
	}
	git(t, fix.dir, "commit", "--allow-empty", "-m", "the fix")
	if err := fix.ensurePushed(ctx); err != nil {
		t.Fatalf("ship the fix: %v", err)
	}
	clone := t.TempDir()
	git(t, "", "clone", "-q", "--branch", "brigada/task-5", remote, clone)
	if out := git(t, clone, "log", "--oneline"); !strings.Contains(out, "the fix") || !strings.Contains(out, "the work") {
		t.Errorf("the branch's history = %q, want the fix on top of the work", out)
	}

	// A remote branch that moved beyond the worktree's start is never
	// force-pushed over: the push is refused instead.
	mover := t.TempDir()
	git(t, "", "clone", "-q", "--branch", "brigada/task-5", remote, mover)
	git(t, mover, "-c", "user.name=seed", "-c", "user.email=seed@brigada.invalid",
		"commit", "--allow-empty", "-m", "moved on")
	git(t, mover, "push", "-q", "origin", "brigada/task-5")
	git(t, fix.dir, "commit", "--allow-empty", "-m", "behind the move")
	if err := fix.ensurePushed(ctx); err == nil {
		t.Error("a non-fast-forward push succeeded; want the refusal, never a force push")
	}
}

// TestFixJobWithoutAPushedBranch covers the fix round whose branch the
// remote does not hold — the work never pushed anything: the worktree
// starts at the base ref like a work job's, and the round's push brings
// the branch its start.
func TestFixJobWithoutAPushedBranch(t *testing.T) {
	ctx := context.Background()
	remote := newRemote(t)
	fix, err := prepareWorkspace(ctx, &Config{StateDir: t.TempDir()}, &api.JobGrant{
		JobID: 43, TaskID: 6, Kind: "fix", Attempt: 1,
		RepoURL: remote, BaseRef: "main", Branch: "brigada/task-6",
	})
	if err != nil {
		t.Fatalf("prepare the fix workspace: %v", err)
	}
	defer fix.remove(ctx)
	if out := git(t, fix.dir, "log", "--oneline", "-1"); !strings.Contains(out, "seed") {
		t.Errorf("the fix worktree starts at %q, want the base ref's seed commit", strings.TrimSpace(out))
	}
	if err := fix.ensurePushed(ctx); err != nil {
		t.Fatalf("push the branch the remote lacked: %v", err)
	}
}

// TestRemoteBranchSeparatesAbsentFromUnreachable pins the two ways an
// ls-remote answer can be no (§3): a remote that answers cleanly that
// it holds no such branch, and a remote that cannot be asked at all.
// The fix round's workspace builds on the difference — the first falls
// back to the base ref, the second fails the job rather than silently
// rebuild from the base — so the two must not fold into one boolean.
func TestRemoteBranchSeparatesAbsentFromUnreachable(t *testing.T) {
	ctx := context.Background()
	w := &workspace{repoURL: newRemote(t)}

	if sha, ok, err := w.remoteBranch(ctx, "brigada/task-99"); err != nil || ok || sha != "" {
		t.Errorf("an absent branch = %q/%t/%v, want a clean no", sha, ok, err)
	}

	// A branch the remote does hold answers with its commit.
	seed := t.TempDir()
	git(t, "", "clone", "-q", w.repoURL, seed)
	git(t, seed, "-c", "user.name=seed", "-c", "user.email=seed@brigada.invalid",
		"commit", "--allow-empty", "-m", "work")
	git(t, seed, "branch", "brigada/task-99")
	git(t, seed, "push", "-q", "origin", "brigada/task-99")
	if sha, ok, err := w.remoteBranch(ctx, "brigada/task-99"); err != nil || !ok || sha == "" {
		t.Errorf("the pushed branch = %q/%t/%v, want its commit", sha, ok, err)
	}

	bad := &workspace{repoURL: "file://" + filepath.Join(t.TempDir(), "missing.git")}
	if _, ok, err := bad.remoteBranch(ctx, "brigada/task-1"); err == nil || ok {
		t.Errorf("an unreachable remote = %t/%v, want the question's failure, not absence", ok, err)
	}
}
