package runner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
)

// A sweep job's execution (§13.2): no worker and no checkout. The runner
// enumerates the remote with its own credentials, keeps only refs the shared
// matcher maps to the engine's deletable ids, and deletes each one. Every
// remote failure is logged and the job still completes: the next engine start
// queues the ids whose refs remain.
func isSweepJob(grant *api.JobGrant) bool {
	return engine.Kind(grant.Kind) == engine.KindSweep
}

func (r *jobRun) runSweepJob(ctx context.Context) {
	runCtx, abort := context.WithCancel(ctx)
	beatsDone := make(chan struct{})
	go r.heartbeats(runCtx, abort, beatsDone)
	defer func() {
		abort()
		<-beatsDone
	}()
	// Even a deletion by explicit remote URL needs a local git directory.
	// A short-lived bare one supplies it without fetching or checking out
	// anything; the remote remains the only source of the ref list.
	gitDir, err := os.MkdirTemp(r.cfg.StateDir, "sweep-")
	if err == nil {
		defer os.RemoveAll(gitDir)
		_, err = r.sweepGit(runCtx, "", "init", "--bare", "--quiet", gitDir)
	}
	if err != nil {
		r.cfg.Log.WarnContext(runCtx, "prepare the task-branch sweep's git directory; leaving its refs for the next engine start",
			"job_id", r.grant.JobID, "err", err)
	} else {
		r.sweepTaskBranches(runCtx, gitDir)
	}
	r.deliver(runCtx, api.Completion{Result: "{}"})
}

func (r *jobRun) sweepTaskBranches(ctx context.Context, gitDir string) {
	grant := r.grant
	if grant.RepoURL == "" || len(grant.SweepTaskIDs) == 0 {
		r.cfg.Log.WarnContext(ctx, "the task-branch sweep is incomplete; leaving its refs for the next engine start",
			"job_id", grant.JobID, "repo", redactURL(grant.RepoURL), "tasks", len(grant.SweepTaskIDs))
		return
	}
	out, err := r.sweepGit(ctx, gitDir, "ls-remote", "--heads", grant.RepoURL)
	if err != nil {
		r.cfg.Log.WarnContext(ctx, "list the remote's branches for the reconcile sweep; leaving them for the next engine start",
			"job_id", grant.JobID, "repo", redactURL(grant.RepoURL), "err", trimCmdError(out, err))
		return
	}
	deletable := make(map[int64]bool, len(grant.SweepTaskIDs))
	for _, id := range grant.SweepTaskIDs {
		deletable[id] = true
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || !strings.HasPrefix(fields[1], "refs/heads/") {
			continue
		}
		id, _, ok := engine.ParseTaskBranchRef(strings.TrimPrefix(fields[1], "refs/heads/"))
		if !ok || !deletable[id] {
			continue
		}
		if out, err := r.sweepGit(ctx, gitDir, "push", "--quiet", "--delete", grant.RepoURL, fields[1]); err != nil {
			r.cfg.Log.WarnContext(ctx, "delete a dead task branch from the remote; leaving it for the next engine start",
				"job_id", grant.JobID, "task_id", id, "ref", fields[1], "err", trimCmdError(out, err))
		}
	}
}

// sweepGit runs git with the same runner-only credential environment as the
// landing and workspace commands. gitDir is the sweep's bare repository, empty
// for the init command that creates it.
func (r *jobRun) sweepGit(ctx context.Context, gitDir string, args ...string) ([]byte, error) {
	if gitDir != "" {
		args = append([]string{"--git-dir", gitDir}, args...)
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = r.cfg.StateDir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	cmd.Env = append(cmd.Env, r.cfg.GitEnv...)
	var combined strings.Builder
	cmd.Stdout = &combined
	cmd.Stderr = &combined
	if err := cmd.Run(); err != nil {
		return []byte(combined.String()), fmt.Errorf("git %s: %w", args[0], err)
	}
	return []byte(combined.String()), nil
}
