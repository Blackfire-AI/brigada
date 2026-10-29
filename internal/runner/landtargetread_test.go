package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
)

// pushLanded puts a commit on the remote's branch, made from main, and
// returns its sha: what a landing leaves on a land target (issue #122).
func pushLanded(t *testing.T, remote, branch string) string {
	t.Helper()
	seed := t.TempDir()
	git(t, "", "clone", "--branch", "main", remote, seed)
	git(t, seed, "checkout", "-b", branch)
	if err := os.WriteFile(filepath.Join(seed, "landed.txt"), []byte("landed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, seed, "add", ".")
	git(t, seed, "-c", "user.name=test", "-c", "user.email=test@brigada.invalid", "commit", "-m", "landed")
	git(t, seed, "push", "origin", branch)
	return strings.TrimSpace(git(t, seed, "rev-parse", "HEAD"))
}

// TestPrepareForemanCheckoutLandTarget: the reading checkout is the land
// target when one is set and the remote holds it; the default branch when
// none is set; and, when the remote does not hold it yet, the default
// branch again, with the checkout saying so.
func TestPrepareForemanCheckoutLandTarget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	remote := newRemote(t)
	mainHead := strings.TrimSpace(git(t, "", "ls-remote", remote, "refs/heads/main"))
	mainHead = strings.Fields(mainHead)[0]

	unset, err := PrepareForemanCheckout(ctx, t.TempDir(), remote, "", "main", nil)
	if err != nil {
		t.Fatal(err)
	}
	if unset.Branch != "main" || unset.Commit != mainHead || unset.LandTarget != "" || unset.DefaultBranch != "main" {
		t.Errorf("unset land target: checkout = %+v", unset)
	}

	scratch := t.TempDir()
	missing, err := PrepareForemanCheckout(ctx, scratch, remote, "brigada/landed", "main", nil)
	if err != nil {
		t.Fatal(err)
	}
	if missing.Branch != "main" || missing.Commit != mainHead || missing.LandTarget != "brigada/landed" {
		t.Errorf("missing land target: checkout = %+v, want the default branch with the target recorded", missing)
	}

	landed := pushLanded(t, remote, "brigada/landed")
	set, err := PrepareForemanCheckout(ctx, scratch, remote, "brigada/landed", "main", nil)
	if err != nil {
		t.Fatal(err)
	}
	if set.Branch != "brigada/landed" || set.Commit != landed || set.DefaultBranch != "main" {
		t.Errorf("land target set: checkout = %+v, want brigada/landed at %s", set, landed)
	}
	if _, err := os.Stat(filepath.Join(set.Path, "landed.txt")); err != nil {
		t.Errorf("the land target's file is not in the checkout: %v", err)
	}

	// The target is deleted again: the checkout falls back to main.
	git(t, "", "--git-dir", strings.TrimPrefix(remote, "file://"), "branch", "-D", "brigada/landed")
	back, err := PrepareForemanCheckout(ctx, scratch, remote, "brigada/landed", "main", nil)
	if err != nil {
		t.Fatal(err)
	}
	if back.Branch != "main" || back.Commit != mainHead {
		t.Errorf("land target gone: checkout = %+v", back)
	}
	if _, err := os.Stat(filepath.Join(back.Path, "landed.txt")); !os.IsNotExist(err) {
		t.Errorf("the land target's file survived the fallback: %v", err)
	}
}

// TestRunnerForemanCheckoutLandTarget: a headless turn reads the land
// target, and its prompt names both refs; a land target the remote lacks
// reads the default branch and the prompt says so.
func TestRunnerForemanCheckoutLandTarget(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t, engine.Config{})
	remote := newRemote(t)
	e.seedProject(remote)
	e.setForeman("fake", "fake-1")
	e.setProjectSettings(`{"land_target":"brigada/landed"}`)
	markers := markerDir(t)
	if err := os.WriteFile(filepath.Join(markers, "release"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	first := e.submitForemanTurn("first read")
	p := e.startRunner(writeWorkerScript(t, turnWorker), 100*time.Millisecond)
	defer p.stop(t)
	e.waitJob(first.JobID, func(j *engine.JobInfo) bool { return j.Status == "done" })
	if got := e.scratchFile("prompt.txt"); !strings.Contains(got, `land target "brigada/landed" does not exist on the remote yet`) || !strings.Contains(got, `default branch "main"`) {
		t.Errorf("fallback prompt does not name both refs:\n%s", got)
	}
	landed := pushLanded(t, remote, "brigada/landed")
	second := e.submitForemanTurn("read again")
	e.waitJob(second.JobID, func(j *engine.JobInfo) bool { return j.Status == "done" })
	repo := filepath.Join(e.foremanScratch(), "repo")
	if got := strings.TrimSpace(git(t, repo, "rev-parse", "HEAD")); got != landed {
		t.Errorf("checkout = %s, want the land target's head %s", got, landed)
	}
	if got := e.scratchFile("prompt.txt"); !strings.Contains(got, `Branch: "brigada/landed"`) || !strings.Contains(got, `default branch is "main" and may lag`) {
		t.Errorf("prompt does not name the land target and the default branch:\n%s", got)
	}
}

// TestPreparePlanReviewWorkspaceLandTarget: the plan reviewer's worktree
// is at the land target; at the default branch when none is set or the
// remote does not hold it yet, and the prompt names the ref read.
func TestPreparePlanReviewWorkspaceLandTarget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	remote := newRemote(t)
	mainHead := strings.Fields(git(t, "", "ls-remote", remote, "refs/heads/main"))[0]
	cfg := &Config{StateDir: t.TempDir()}
	grant := func(id int64, base string) *api.JobGrant {
		return &api.JobGrant{JobID: id, Attempt: 1, Kind: string(engine.KindPlanReview), RepoURL: remote,
			BaseRef: base, DefaultBranch: "main", Prompt: "plan"}
	}
	read := func(g *api.JobGrant) string {
		t.Helper()
		w, err := preparePlanReviewWorkspace(ctx, cfg, g)
		if err != nil {
			t.Fatal(err)
		}
		defer w.remove(ctx)
		return w.base
	}

	g := grant(1, "main")
	if got := read(g); got != mainHead || !strings.Contains(g.Prompt, `head of "main", the project's default branch`) {
		t.Errorf("unset: base %s prompt %q", got, g.Prompt)
	}
	g = grant(2, "brigada/landed")
	if got := read(g); got != mainHead || !strings.Contains(g.Prompt, `land target "brigada/landed" does not exist on the remote yet`) {
		t.Errorf("missing: base %s prompt %q", got, g.Prompt)
	}
	landed := pushLanded(t, remote, "brigada/landed")
	g = grant(3, "brigada/landed")
	if got := read(g); got != landed || !strings.Contains(g.Prompt, `head of "brigada/landed", the project's land target`) || !strings.Contains(g.Prompt, `default branch "main" may lag`) {
		t.Errorf("set: base %s (want %s) prompt %q", got, landed, g.Prompt)
	}
}
