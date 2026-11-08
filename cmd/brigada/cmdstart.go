package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strconv"

	"github.com/Blackfire-AI/brigada/internal/lifecycle"
	"github.com/Blackfire-AI/brigada/internal/runnerstate"
)

func runStart(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("start",
		"brigada start [--state-dir DIR] [--config PATH]",
		"Start the engine and detached runner with the agents saved by brigada init.")
	stderr = commandStderr(stderr, fs.Name())
	common := addCommon(fs)
	if code, stop := common.parse(stdout, stderr, args); stop {
		return code
	}
	if extra := fs.Args(); len(extra) > 0 {
		return usagef(stderr, "start takes no arguments, got %q", extra)
	}
	rt, code := resolveRuntime(common, stderr)
	if code != 0 {
		return code
	}
	// The detached runner changes directory; pass absolute paths so relative
	// state and config flags still name the files the caller selected.
	var err error
	rt.stateDir, err = filepath.Abs(rt.stateDir)
	if err != nil {
		return fail(stderr, err)
	}
	rt.paths = lifecycle.PathsOf(rt.stateDir)
	if rt.explicitCfg {
		rt.configPath, err = filepath.Abs(rt.configPath)
		if err != nil {
			return fail(stderr, err)
		}
	}
	dir := runnerStateDir(rt)
	rec, running, err := runnerstate.Running(dir)
	if err != nil {
		return fail(stderr, err)
	}
	if running {
		// An interrupted drain leaves the runner and the engine up under the
		// drain's pause (§8.3): start is what lifts it. An engine that is not
		// up holds nothing to lift, and start does not start one here.
		if lifecycle.EngineRunning(ctx, rt.paths.Socket) {
			if code := liftDrainPause(ctx, rt, stdout, stderr); code != 0 {
				return code
			}
		}
		fmt.Fprintf(stdout, "runner already running (pid %d)\n", rec.PID)
		return 0
	}
	saved, err := runnerstate.LoadAgents(dir)
	if err != nil {
		return fail(stderr, err)
	}
	if len(saved) == 0 {
		return fail(stderr, fmt.Errorf("no saved agents; run brigada init in a repository first"))
	}
	var agents agentSpecs
	for _, spec := range saved {
		if err := agents.Set(spec); err != nil {
			return fail(stderr, fmt.Errorf("saved agents: %w", err))
		}
	}
	if _, _, err := resolveRunnerAgents(rt, agents.specs, ""); err != nil {
		return fail(stderr, fmt.Errorf("saved agents: %w", err))
	}
	parallel, err := runnerstate.LoadParallel(dir)
	if err != nil {
		return fail(stderr, err)
	}
	if err := lifecycle.EnsureEngine(ctx, lifecycle.StartOptions{
		Socket: rt.paths.Socket, StateDir: rt.stateDir,
		ConfigPath: rt.configForStart(), Binary: initBinary,
	}); err != nil {
		return fail(stderr, err)
	}
	// The engine may have been stopped by a drain, whose pause is stored
	// (§8.3): it comes back paused, and start lifts that pause.
	if code := liftDrainPause(ctx, rt, stdout, stderr); code != 0 {
		return code
	}
	result, err := startDetachedRunner(ctx, rt, saved, parallel, "")
	if err != nil {
		return fail(stderr, err)
	}
	if result.Existing {
		fmt.Fprintf(stdout, "runner already running (pid %d)\n", result.Record.PID)
	} else {
		fmt.Fprintf(stdout, "runner started (pid %d); log: %s\n",
			result.Record.PID, filepath.Join(rt.stateDir, "runner.log"))
	}
	return 0
}

// liftDrainPause is the lift `brigada start` makes (§8.3): the engine's
// pause is lifted when a drain set it — a resume, with its evaluation of
// the held wakes — and a user's pause is left alone, with a line saying
// the engine stays paused. It talks to the engine that answers and
// starts none.
func liftDrainPause(ctx context.Context, rt *runtime, stdout, stderr io.Writer) int {
	client, code := connect(ctx, rt, "client", true, stderr)
	if code != 0 {
		return code
	}
	res, err := client.ResumeDrain(ctx)
	if err != nil {
		return fail(stderr, fmt.Errorf("lift the drain pause: %w", err))
	}
	if res.Resumed {
		fmt.Fprintln(stdout, "lifted the drain pause: the engine dispatches again")
		return 0
	}
	st, err := client.Status(ctx)
	if err != nil {
		return fail(stderr, fmt.Errorf("read the pauses: %w", err))
	}
	for _, p := range st.Pauses {
		if p.ProjectID == 0 {
			fmt.Fprintln(stdout, "the engine stays paused: the pause is the user's; brigada resume lifts it")
			break
		}
	}
	return 0
}

// startDetachedRunner is shared by init, start and agents add, including
// init's restart after changing agents. A restart mode of "drain" or
// "now" hands a running runner the request first. EnsureRunner serializes
// starts and waits for readiness.
func startDetachedRunner(ctx context.Context, rt *runtime, specs []string, parallel int, restart string) (lifecycle.RunnerStartResult, error) {
	args := []string{"--max-parallel", strconv.Itoa(parallel)}
	for _, spec := range specs {
		args = append(args, "--agent", spec)
	}
	return lifecycle.EnsureRunner(ctx, lifecycle.RunnerStartOptions{
		StateDir: rt.stateDir, ConfigPath: rt.configForStart(),
		Binary: initBinary, Args: args, Restart: restart,
	})
}
