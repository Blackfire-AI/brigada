package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// TestRunnerSyncMergesDefaultIntoTarget: a land target the default branch
// moved past — a hotfix pushed by hand — is brought current by the sync
// (§7.1): the runner merges the default branch into the target as a true
// merge commit and pushes it under auto, and the engine's standing record
// reads current. The merge carries the Brigada-Sync trailer naming both
// branches.
func TestRunnerSyncMergesDefaultIntoTarget(t *testing.T) {
	t.Parallel()
	markerDir(t)
	remote := newRemote(t)
	clone := t.TempDir()
	git(t, clone, "clone", remote, ".")
	git(t, clone, "checkout", "-b", "staging")
	git(t, clone, "push", "origin", "staging")
	stagingBefore := remoteBranchCommit(t, remote, "staging")
	// The default branch advances outside brigada: staging falls behind.
	git(t, clone, "checkout", "main")
	if err := os.WriteFile(filepath.Join(clone, "hotfix.txt"), []byte("hand merge\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, clone, "add", "-A")
	git(t, clone, "-c", "user.name=s", "-c", "user.email=s@brigada.invalid", "commit", "-m", "hotfix")
	git(t, clone, "push", "origin", "main")
	mainHead := remoteBranchCommit(t, remote, "main")

	e := newTestEnv(t, engine.Config{})
	e.seedProject(remote)
	e.setProjectSettings(`{"gate_push":"auto","land_target":"staging"}`)
	sr, err := e.eng.Submit(e.ctx, engine.SubmitParams{
		Key:       fmt.Sprintf("sync:%d", e.project),
		ProjectID: e.project,
		Kind:      engine.KindSync,
		Mode:      engine.ModeFresh,
		Labels:    []string{"git"},
	})
	if err != nil {
		t.Fatalf("submit the sync: %v", err)
	}
	e.startLandRunner(t, reviewScript(reviewApproveResult))
	e.waitJob(sr.JobID, func(j *engine.JobInfo) bool { return j.Status == "done" })

	stagingHead := remoteBranchCommit(t, remote, "staging")
	if stagingHead == stagingBefore {
		t.Fatalf("staging did not move: the sync merged nothing onto a behind target")
	}
	bare := strings.TrimPrefix(remote, "file://")
	if mb := strings.TrimSpace(git(t, "", "--git-dir", bare, "merge-base", mainHead, stagingHead)); mb != mainHead {
		t.Errorf("staging does not hold the default branch's head %s (merge-base %s)", mainHead, mb)
	}
	msg := git(t, "", "--git-dir", bare, "log", "-1", "--format=%B", stagingHead)
	if !strings.Contains(msg, "Brigada-Sync: main@"+mainHead) {
		t.Errorf("the sync merge message = %q, want the Brigada-Sync trailer naming main@%s", msg, mainHead)
	}
	if state := e.strOf("SELECT state FROM land_syncs WHERE project_id = ?", e.project); state != "current" {
		t.Errorf("sync state = %q, want current", state)
	}
	if behind := e.int64Of("SELECT behind FROM land_syncs WHERE project_id = ?", e.project); behind != 0 {
		t.Errorf("sync behind = %d, want 0 once current", behind)
	}
}

// TestRunnerSyncCurrentTargetPushesNothing: a target that already holds
// every default commit is current (§7.1) — the sync merges nothing and
// pushes nothing, and the target's head is untouched.
func TestRunnerSyncCurrentTargetPushesNothing(t *testing.T) {
	t.Parallel()
	markerDir(t)
	remote := newRemote(t)
	clone := t.TempDir()
	git(t, clone, "clone", remote, ".")
	git(t, clone, "checkout", "-b", "staging")
	git(t, clone, "push", "origin", "staging")
	stagingBefore := remoteBranchCommit(t, remote, "staging")

	e := newTestEnv(t, engine.Config{})
	e.seedProject(remote)
	e.setProjectSettings(`{"gate_push":"auto","land_target":"staging"}`)
	sr, err := e.eng.Submit(e.ctx, engine.SubmitParams{
		Key:       fmt.Sprintf("sync:%d", e.project),
		ProjectID: e.project,
		Kind:      engine.KindSync,
		Mode:      engine.ModeFresh,
		Labels:    []string{"git"},
	})
	if err != nil {
		t.Fatalf("submit the sync: %v", err)
	}
	e.startLandRunner(t, reviewScript(reviewApproveResult))
	e.waitJob(sr.JobID, func(j *engine.JobInfo) bool { return j.Status == "done" })
	if got := remoteBranchCommit(t, remote, "staging"); got != stagingBefore {
		t.Errorf("a current target moved: %s, want %s", got, stagingBefore)
	}
	if state := e.strOf("SELECT state FROM land_syncs WHERE project_id = ?", e.project); state != "current" {
		t.Errorf("sync state = %q, want current", state)
	}
}

// submitSync queues the project's sync job and runs it to completion.
func (e *testEnv) submitSync(t *testing.T) {
	t.Helper()
	sr, err := e.eng.Submit(e.ctx, engine.SubmitParams{
		Key:       fmt.Sprintf("sync:%d", e.project),
		ProjectID: e.project,
		Kind:      engine.KindSync,
		Mode:      engine.ModeFresh,
		Labels:    []string{"git"},
	})
	if err != nil {
		t.Fatalf("submit the sync: %v", err)
	}
	e.startLandRunner(t, reviewScript(reviewApproveResult))
	e.waitJob(sr.JobID, func(j *engine.JobInfo) bool { return j.Status == "done" })
}

// TestRunnerSyncAbsentTargetIsCurrent: a target the remote does not hold
// yet is current (§7.1) — the first landing cuts it from the default
// branch — so the sync merges and pushes nothing, and creates no branch.
func TestRunnerSyncAbsentTargetIsCurrent(t *testing.T) {
	t.Parallel()
	markerDir(t)
	remote := newRemote(t)
	e := newTestEnv(t, engine.Config{})
	e.seedProject(remote)
	e.setProjectSettings(`{"gate_push":"auto","land_target":"staging"}`)
	e.submitSync(t)
	bare := strings.TrimPrefix(remote, "file://")
	if out := git(t, "", "--git-dir", bare, "branch", "--list", "staging"); strings.TrimSpace(out) != "" {
		t.Errorf("the sync created the absent target: %q", out)
	}
	if state := e.strOf("SELECT state FROM land_syncs WHERE project_id = ?", e.project); state != "current" {
		t.Errorf("sync state = %q, want current", state)
	}
}

// TestRunnerSyncMergeRunsNoTaskGate: a behind target gets a true two-parent
// merge, pushed as a fast-forward of the old head under auto, and none of
// a landing's gates run — no check, no after_land, no land_ready (§7.1).
func TestRunnerSyncMergeRunsNoTaskGate(t *testing.T) {
	t.Parallel()
	markers := markerDir(t)
	remote := landReadyRemote(t, landReadyBody(markers))
	clone := t.TempDir()
	git(t, clone, "clone", remote, ".")
	git(t, clone, "checkout", "-b", "staging")
	git(t, clone, "push", "origin", "staging")
	stagingBefore := remoteBranchCommit(t, remote, "staging")
	git(t, clone, "checkout", "main")
	if err := os.WriteFile(filepath.Join(clone, "hotfix.txt"), []byte("hand merge\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, clone, "add", "-A")
	git(t, clone, "-c", "user.name=s", "-c", "user.email=s@brigada.invalid", "commit", "-m", "hotfix")
	git(t, clone, "push", "origin", "main")
	mainHead := remoteBranchCommit(t, remote, "main")

	e := newTestEnv(t, engine.Config{})
	e.seedProject(remote)
	e.setProjectSettings(`{"gate_push":"auto","land_target":"staging","land_ready":".brigada/land-ready.sh",` +
		`"default_check":"touch '` + markers + `/check'","after_land":"touch '` + markers + `/after-land'"}`)
	e.submitSync(t)

	bare := strings.TrimPrefix(remote, "file://")
	stagingHead := remoteBranchCommit(t, remote, "staging")
	parents := strings.Fields(git(t, "", "--git-dir", bare, "log", "-1", "--format=%P", stagingHead))
	if len(parents) != 2 || parents[0] != stagingBefore || parents[1] != mainHead {
		t.Errorf("merge parents = %v, want [%s %s]", parents, stagingBefore, mainHead)
	}
	if out := git(t, "", "--git-dir", bare, "merge-base", "--is-ancestor", stagingBefore, stagingHead); out != "" {
		t.Errorf("the push was not a fast-forward: %q", out)
	}
	for _, name := range []string{"ran", "check", "after-land"} {
		if _, err := os.Stat(filepath.Join(markers, name)); err == nil {
			t.Errorf("%s ran during a sync; a sync runs no task gate", name)
		}
	}
}
