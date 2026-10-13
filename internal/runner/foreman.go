package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/Blackfire-AI/brigada/internal/adapter"
	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
)

// runForemanTurn runs one headless foreman turn (§4.2 item 2, §4): the
// foreman is a worker on the same machinery — the same adapters, the
// same session resume on the key, the same stall watch and class caps,
// the same transcript — without the task worker's material side. No
// workspace is made: no repo to clone, no worktree, no branch to push,
// no check to run (§4: the foreman job is excluded from the runner's
// checks). The turn runs in the project's scratch directory and is
// exempt from the result contract (§5.4): a clean exit completes the
// job as done, with the turn's final message as its summary.
func (r *jobRun) runForemanTurn(ctx context.Context) {
	dir, err := foremanScratchDir(r.cfg.StateDir, r.grant.ProjectID)
	if err != nil {
		r.finishWithError(ctx, "make the foreman's scratch directory: "+err.Error())
		return
	}
	r.cfg.Log.InfoContext(ctx, "foreman turn", "job_id", r.grant.JobID, "key", r.grant.Key, "dir", dir)
	// The MCP config is decided before the worker starts, where the
	// bridge's file would reach the adapter's command.
	r.mcpConfig = r.foremanMCPConfig()

	kill := make(chan struct{})
	r.killWorker = sync.OnceFunc(func() { close(kill) })
	runCtx, abort := context.WithCancel(ctx)
	beatsDone := make(chan struct{})
	go r.heartbeats(runCtx, abort, beatsDone)
	ad, model := r.cfg.adapterFor(r.grant)
	exitErr := r.runWorker(runCtx, ad, model, dir, kill)
	abort()
	<-beatsDone

	if r.takeDropped() {
		r.cfg.Log.WarnContext(context.WithoutCancel(ctx),
			"foreman turn no longer ours; worker killed and result dropped",
			"job_id", r.grant.JobID)
		return
	}
	if ctx.Err() != nil {
		// As with a task job: no completion on a dead context — the
		// lease lapses and the turn runs again.
		r.cfg.Log.WarnContext(context.WithoutCancel(ctx),
			"runner shutting down mid-turn; the lease will lapse",
			"job_id", r.grant.JobID)
		return
	}
	if why := r.takeStopped(); why != nil {
		r.finishForemanStopped(ctx, why)
		return
	}
	r.finishForemanTurn(ctx, ad, exitErr)
}

// foremanScratchDir is the foreman's working directory for a project
// (§4): a scratch dir under the runner's state, created on demand and
// kept between turns. The foreman never edits code, so there is no
// worktree to make — but its harness still wants a corner of its own,
// stable across a long-lived session's turns.
func foremanScratchDir(stateDir string, project int64) (string, error) {
	dir := filepath.Join(stateDir, "foreman", strconv.FormatInt(project, 10))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	return dir, nil
}

// foremanMCPConfig is the hook for the `brigada mcp` bridge (§12.2),
// the item after this one: where the config that offers the foreman's
// harness brigada's tools — the file naming the bridge's stdio server
// — reaches the adapter (adapter.Job.MCPConfig). Nothing is passed
// yet, so a turn runs with the harness's own tools and none of
// brigada's; the bridge will return its config path here.
func (r *jobRun) foremanMCPConfig() string { return "" }

// finishForemanTurn turns the dead foreman CLI's record into the
// engine's completion (§5.4's exemption): an adapter error or a
// non-zero exit fails the turn as a worker's would, and a clean exit
// completes the job as done with the turn's final message as its
// summary — no result block to parse, no branch to verify, no check to
// run. The transcript is uploaded as for any job (§9).
func (r *jobRun) finishForemanTurn(ctx context.Context, ad adapter.Adapter, exitErr error) {
	var cliErr *adapter.Event
	r.mu.Lock()
	switch {
	case r.cliError != nil:
		e := *r.cliError
		cliErr = &e
	case r.stderrError != nil && exitErr != nil:
		e := *r.stderrError
		cliErr = &e
	}
	r.mu.Unlock()
	if cliErr != nil {
		r.finishWithError(ctx, fmt.Sprintf("foreman CLI failed (%s): %s", cliErr.ErrorKind, cliErr.Text))
		return
	}
	if nw := (&noWorkerError{}); errors.As(exitErr, &nw) {
		r.finishWithError(ctx, "start the foreman CLI: "+nw.err.Error())
		return
	}
	if exitErr != nil {
		// A non-zero exit failed the turn (§5.4 offers no result block
		// to outweigh it); what the CLI said is in the transcript.
		r.finishWithError(ctx, fmt.Sprintf("foreman CLI exited with %v", exitErr))
		return
	}
	res := WorkerResult{Status: statusDone, Summary: ad.ResultText(r.snapshotEvents())}
	encoded, err := json.Marshal(res)
	if err != nil {
		r.finishWithError(ctx, "encode the turn's result: "+err.Error())
		return
	}
	comp := api.Completion{Result: string(encoded), SessionID: r.sessionIDOf()}
	if err := r.saveTranscript(ctx); err != nil {
		r.cfg.Log.WarnContext(ctx, "save transcript", "job_id", r.grant.JobID, "err", err)
	}
	r.deliver(ctx, comp)
}

// finishForemanStopped completes a foreman turn the runner's own policy
// cut short (§5.6): the outcome ends the job as stalled with the last
// progress events attached, as for any stopped run — with no branch to
// save, the transcript is all a stopped turn leaves.
func (r *jobRun) finishForemanStopped(ctx context.Context, why *stop) {
	res := WorkerResult{
		Status:   why.Outcome,
		Reason:   why.Reason,
		Progress: r.recentProgress(stoppedRunEventsKept),
	}
	encoded, err := json.Marshal(res)
	if err != nil {
		r.finishWithError(ctx, "encode the result block: "+err.Error())
		return
	}
	comp := api.Completion{SessionID: r.sessionIDOf(), Outcome: why.Outcome, Result: string(encoded)}
	if err := r.saveTranscript(ctx); err != nil {
		r.cfg.Log.WarnContext(ctx, "save transcript", "job_id", r.grant.JobID, "err", err)
	}
	r.deliver(ctx, comp)
}

// isForemanTurn reports whether the grant is a foreman turn (§11): the
// one job kind that belongs to a project instead of a task.
func isForemanTurn(grant *api.JobGrant) bool {
	return engine.Kind(grant.Kind) == engine.KindForeman
}
