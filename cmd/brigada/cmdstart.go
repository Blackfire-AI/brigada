package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strconv"

	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/lifecycle"
	"github.com/Blackfire-AI/brigada/internal/runnerstate"
)

// Tests can exercise the drain lift over an in-process API without a
// listener.
var connectStart = connect

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
	// An interrupted `stop --engine --drain` leaves its pause standing
	// (§8.3): an engine that still answers has it lifted now, for the
	// already-running path below starts none. The normal path lifts it
	// once its own engine is up.
	drainLifted := false
	if lifecycle.EngineRunning(ctx, rt.paths.Socket) {
		liftDrainPause(ctx, rt, stdout, stderr)
		drainLifted = true
	}
	dir := runnerStateDir(rt)
	rec, running, err := runnerstate.Running(dir)
	if err != nil {
		return fail(stderr, err)
	}
	if running {
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
	if !drainLifted {
		liftDrainPause(ctx, rt, stdout, stderr)
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

// liftDrainPause is start's half of §8.3's stopping story: a pause of
// origin `drain` is lifted here through the engine's drain-only lift —
// a resume, with its evaluation of the held wakes — while a user's
// pause is left alone and said to stay. The engine must already answer.
func liftDrainPause(ctx context.Context, rt *runtime, stdout, stderr io.Writer) {
	client, code := connectStart(ctx, rt, "client", true, stderr)
	if code != 0 {
		return // the pause survives; the next start lifts it
	}
	res, err := client.ResumeDrain(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "brigada: lift the drain pause: %v\n", err)
		return
	}
	if res.Resumed {
		fmt.Fprintln(stdout, "engine resumed: the drain pause is lifted")
		return
	}
	st, err := client.Status(ctx)
	if err != nil {
		return
	}
	for _, p := range st.Pauses {
		if p.ProjectID == 0 && p.Origin == engine.PauseOriginUser {
			fmt.Fprintln(stdout, "the engine stays paused (a user's pause); `brigada resume` lifts it")
		}
	}
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
