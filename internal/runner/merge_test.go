package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/landing"
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

// makeModifyDeleteConflict deletes README.md on a side branch and edits
// it on the task branch, then runs the worker's `git merge --no-commit
// side`: README.md is left unmerged (a modify/delete conflict) with no
// conflict marker in its content.
func makeModifyDeleteConflict(t *testing.T, ws *workspace) {
	t.Helper()
	git(t, ws.dir, "checkout", "-b", "side")
	git(t, ws.dir, "rm", "-q", "README.md")
	git(t, ws.dir, "-c", "user.name=side", "-c", "user.email=side@brigada.invalid", "commit", "-m", "side deletes")
	git(t, ws.dir, "checkout", ws.branch)
	if err := os.WriteFile(filepath.Join(ws.dir, "README.md"), []byte("# task\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws.dir, "add", "-A")
	git(t, ws.dir, "-c", "user.name=task", "-c", "user.email=task@brigada.invalid", "commit", "-m", "task change")
	gitMerge(t, ws.dir, "merge", "--no-commit", "side")
	if got := git(t, ws.dir, "diff", "--name-only", "--diff-filter=U"); strings.TrimSpace(got) != "README.md" {
		t.Fatalf("setup: unmerged = %q, want README.md", got)
	}
	data, _ := os.ReadFile(filepath.Join(ws.dir, "README.md"))
	if hasConflictMarkers(data) {
		t.Fatalf("setup: README.md carries markers, want a marker-free conflict")
	}
}

// TestEnsurePushedCommitsFinishedMerge: a worker's merge the worker
// resolved — MERGE_HEAD present, nothing left unmerged — is committed by
// the runner as the two-parent merge git records, pushed, and lands: the
// landing's patch-based squash (TreeDiff, Squash) carries the merge's
// tree onto the target as one commit.
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

	// Land the branch the way the land job does: a tree diff against the
	// target, squashed onto it as one commit.
	land := filepath.Join(t.TempDir(), "land")
	git(t, "", "clone", "-q", "-b", "main", remote, land)
	git(t, land, "config", "user.name", "lander")
	git(t, land, "config", "user.email", "lander@brigada.invalid")
	git(t, land, "fetch", "-q", "origin", ws.branch)
	patch, err := landing.TreeDiff(ctx, land, "main", "FETCH_HEAD", nil)
	if err != nil {
		t.Fatalf("tree diff of the merged branch: %v", err)
	}
	res, err := landing.Squash(ctx, land, 12, "land the merged task\n", patch, nil)
	if err != nil || res.Empty {
		t.Fatalf("squash the merged branch = %+v, %v; want one landed commit", res, err)
	}
	if got, _ := os.ReadFile(filepath.Join(land, "README.md")); string(got) != "# merged\n" {
		t.Errorf("landed README.md = %q, want the merge's resolution", got)
	}
	if got := strings.Fields(git(t, land, "show", "-s", "--format=%P", "HEAD")); len(got) != 1 {
		t.Errorf("the landed commit has %d parents, want the one squash commit", len(got))
	}
}

// TestEnsurePushedRefusesMarkerFreeUnmerged: an unmerged entry is the
// gate, not its content — a modify/delete conflict carries no marker, and
// still fails the done job, naming the file, with nothing pushed.
func TestEnsurePushedRefusesMarkerFreeUnmerged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	remote := newRemote(t)
	ws, err := prepareWorkspace(ctx, &Config{StateDir: t.TempDir()}, &api.JobGrant{
		JobID: 27, TaskID: 15, Attempt: 1, RepoURL: remote, BaseRef: "main",
	})
	if err != nil {
		t.Fatalf("prepare the workspace: %v", err)
	}
	defer ws.remove(ctx)

	makeModifyDeleteConflict(t, ws)

	err = ws.ensurePushed(ctx)
	if err == nil || !strings.Contains(err.Error(), "unfinished merge") || !strings.Contains(err.Error(), "README.md") {
		t.Fatalf("ensurePushed = %v, want an unfinished-merge refusal naming README.md", err)
	}
	if got := remoteBranchCommit(t, remote, ws.branch); got != "" {
		t.Errorf("the unfinished merge was pushed (%s)", got)
	}
}

// TestEnsurePushedRefusesStagedMarkers: a worker that staged a conflicted
// file without resolving it clears the unmerged entry; the runner reads
// the merge's own conflicted paths for markers and refuses, naming the
// file, while a marker in a file the merge did not conflict on is left
// alone.
func TestEnsurePushedRefusesStagedMarkers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	remote := newRemote(t)
	ws, err := prepareWorkspace(ctx, &Config{StateDir: t.TempDir()}, &api.JobGrant{
		JobID: 28, TaskID: 16, Attempt: 1, RepoURL: remote, BaseRef: "main",
	})
	if err != nil {
		t.Fatalf("prepare the workspace: %v", err)
	}
	defer ws.remove(ctx)

	makeMergeConflict(t, ws)
	if err := os.WriteFile(filepath.Join(ws.dir, "doc.md"), []byte("<<<<<<< a legitimate example\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws.dir, "add", "README.md") // staged with its markers

	err = ws.ensurePushed(ctx)
	if err == nil || !strings.Contains(err.Error(), "README.md") || strings.Contains(err.Error(), "doc.md") {
		t.Fatalf("ensurePushed = %v, want a refusal naming README.md alone", err)
	}
	if got := remoteBranchCommit(t, remote, ws.branch); got != "" {
		t.Errorf("the unresolved merge was pushed (%s)", got)
	}
}

// TestSalvageAbortsMarkerFreeUnmerged: salvage keys on the unmerged list,
// not on markers — a modify/delete conflict is aborted too, and the
// worker's other work is saved.
func TestSalvageAbortsMarkerFreeUnmerged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	remote := newRemote(t)
	ws, err := prepareWorkspace(ctx, &Config{StateDir: t.TempDir()}, &api.JobGrant{
		JobID: 29, TaskID: 17, Attempt: 1, RepoURL: remote, BaseRef: "main",
	})
	if err != nil {
		t.Fatalf("prepare the workspace: %v", err)
	}
	defer ws.remove(ctx)

	makeModifyDeleteConflict(t, ws)
	if err := os.WriteFile(filepath.Join(ws.dir, "work.txt"), []byte("saved\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	branch := ws.branch + "-salvage-29"
	saved, err := ws.salvage(ctx, branch)
	if err != nil || !saved {
		t.Fatalf("salvage = %v, %v; want the other work saved", saved, err)
	}
	bare := strings.TrimPrefix(remote, "file://")
	if got := git(t, "", "--git-dir", bare, "show", branch+":work.txt"); got != "saved\n" {
		t.Errorf("salvage holds %q, want the uncommitted edit", got)
	}
	if parents := strings.Fields(git(t, "", "--git-dir", bare, "show", "-s", "--format=%P", branch)); len(parents) != 1 {
		t.Errorf("salvage commit has %d parents, want the merge aborted", len(parents))
	}
	msg := git(t, "", "--git-dir", bare, "show", "-s", "--format=%s", branch)
	if !strings.Contains(msg, "merge was aborted") {
		t.Errorf("salvage commit message = %q, want it to record the abort", msg)
	}
}

// TestEnsurePushedRefusesUnfinishedMerge: a worker's merge still holding
// unmerged files fails the done job, naming the files,
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
