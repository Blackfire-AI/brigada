package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/Blackfire-AI/brigada/internal/adapter"
	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/mcp"
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
	dir, err := ForemanScratchDir(r.cfg.StateDir, r.grant.ProjectID)
	if err != nil {
		r.finishWithError(ctx, "make the foreman's scratch directory: "+err.Error())
		return
	}
	r.cfg.Log.InfoContext(ctx, "foreman turn", "job_id", r.grant.JobID, "key", r.grant.Key, "dir", dir)
	// The MCP config is decided before the worker starts, where the
	// bridge's file would reach the adapter's command. A turn whose
	// config cannot be written is a turn without brigada's tools — it
	// cannot do its work, and the engine hears why; the hook leaves no
	// debris of its own behind.
	r.mcpConfig, err = r.foremanMCPConfig()
	if err != nil {
		r.finishWithError(ctx, "write the foreman's MCP config: "+err.Error())
		return
	}
	// Whatever ends the turn from here — an early return added tomorrow,
	// a panic in the worker's reading, the runner's own shutdown — the
	// token's file dies with it: the deferred cleanup always runs, and
	// runs idempotently beside the early one below.
	defer r.cleanupMCPConfig()

	kill := make(chan struct{})
	r.killWorker = sync.OnceFunc(func() { close(kill) })
	runCtx, abort := context.WithCancel(ctx)
	beatsDone := make(chan struct{})
	go r.heartbeats(runCtx, abort, beatsDone)
	ad, model, err := r.cfg.agentFor(r.grant)
	if err != nil {
		r.finishWithError(ctx, err.Error())
		return
	}
	exitErr := r.runWorker(runCtx, ad, model, dir, kill)
	abort()
	<-beatsDone
	// The turn is over for the worker's side whatever comes next —
	// dropped, stopped or completed — so the file goes now, before any
	// completion can land: it must not outlive the turn. The deferred
	// cleanup above finds nothing left to do.
	r.cleanupMCPConfig()

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

// ForemanScratchDir is the foreman's working directory for a project
// (§4): a scratch dir under the runner's state, created on demand and
// kept between turns. The foreman never edits code, so there is no
// worktree to make — but its harness still wants a corner of its own,
// stable across a long-lived session's turns.
//
// The rule lives here because two processes must agree on it (§4.2 item
// 1): the runner's headless turns and the client's attached chat are
// the same session, and a harness keys its sessions by working
// directory — an attach that ran anywhere else could not resume what
// the turns left, so the attach calls this same function.
func ForemanScratchDir(stateDir string, project int64) (string, error) {
	dir := filepath.Join(stateDir, "foreman", strconv.FormatInt(project, 10))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	return dir, nil
}

// mcpConfigsDir is the root of the foreman turns' MCP configs under the
// runner's state (§12.2): one private directory per turn, swept whole
// at the runner's startup (sweepMCPConfigs).
func mcpConfigsDir(stateDir string) string {
	return filepath.Join(stateDir, "mcp")
}

// WriteMCPConfig writes the harness's MCP config for one foreman
// session (§12.2) — a runner's headless turn or a client's attached
// chat, which share the machinery (§4.2): one server, `brigada` — the
// caller's own binary (os.Executable) run as the mcp bridge over the
// engine's socket — with the session's foreman token (a turn's is
// lease-scoped, a hold's is hold-scoped, §12.1) in the bridge's
// environment alone. The file is mode 0600 in a private 0700 directory
// under the runner's state, a fresh unguessable one per session — never
// a path another session could share, never the foreman's scratch
// directory (that is the working directory the harness reads at will),
// and never a command line, which anyone listing processes could read.
// The token reaches this file and the bridge it names; the caller
// deletes both with RemoveMCPConfig when the session ends.
//
// Every failure after the directory is made removes what it made — a
// partial file included — so the caller's error path has no debris to
// clean: it returns "" and nothing else.
func WriteMCPConfig(stateDir, socket, token string) (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("find the caller's own binary: %w", err)
	}
	root := mcpConfigsDir(stateDir)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", root, err)
	}
	dir, err := os.MkdirTemp(root, "turn-")
	if err != nil {
		return "", fmt.Errorf("create a turn directory under %s: %w", root, err)
	}
	// From here the directory is this session's alone, and any failure
	// takes it back whole — file, partial or whole, and all.
	cleanup := func() {
		os.Remove(filepath.Join(dir, "config.json"))
		os.Remove(dir)
	}
	data, err := mcp.HarnessConfig(self, socket, token)
	if err != nil {
		cleanup()
		return "", fmt.Errorf("render the MCP config: %w", err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		cleanup()
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	// The directory is fresh, so the file is new — the chmod only pins
	// the mode against a future that writes into a directory it did not
	// make (§12.2: the token's file is 0600, whatever it replaced).
	if err := os.Chmod(path, 0o600); err != nil {
		cleanup()
		return "", fmt.Errorf("chmod %s: %w", path, err)
	}
	return path, nil
}

// RemoveMCPConfig removes one MCP config WriteMCPConfig wrote — the
// file, then the private directory it lived in — whatever ended the
// session. The token inside is dead with its lease or hold; the file
// must not outlive it. A path already gone is not an error, so the
// removal is idempotent; any other failure is the caller's to report.
func RemoveMCPConfig(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Remove(filepath.Dir(path)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// foremanMCPConfig writes the turn's own MCP config (§12.2): the
// shared write under the runner's state, carrying the turn's
// lease-scoped foreman token.
func (r *jobRun) foremanMCPConfig() (string, error) {
	return WriteMCPConfig(r.cfg.StateDir, r.cfg.Socket, r.grant.ForemanToken)
}

// cleanupMCPConfig removes the turn's MCP config, whatever ended the
// turn. The token inside is dead with the turn's lease; the file must
// not outlive it. It is idempotent: the path is taken from r.mcpConfig
// and the field cleared, so the call before the completion and the
// deferred one at the turn's end are one cleanup, never a race over a
// file already gone. A removal that fails is warned about and left:
// the file is 0600 in a 0700 directory, and the token it holds is
// revoked already.
func (r *jobRun) cleanupMCPConfig() {
	path := r.mcpConfig
	if path == "" {
		return
	}
	r.mcpConfig = ""
	if err := RemoveMCPConfig(path); err != nil {
		r.cfg.Log.Warn("remove the foreman's MCP config", "job_id", r.grant.JobID, "path", path, "err", err)
	}
}

// sweepMCPConfigs removes the whole MCP config tree at the runner's
// startup (§12.2): each turn's config is deleted when its turn ends,
// but a runner killed mid-turn never ran its cleanup, and the files it
// left — each holding a token, revoked with its lease but a secret on
// disk all the same — must not wait there. The tree is rebuilt by the
// next turn that needs one.
func sweepMCPConfigs(ctx context.Context, stateDir string, log *slog.Logger) {
	root := mcpConfigsDir(stateDir)
	if err := os.RemoveAll(root); err != nil {
		log.WarnContext(ctx, "sweep the foreman turns' MCP configs", "dir", root, "err", err)
	}
}

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
