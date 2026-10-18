package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"

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
	if err := lifecycle.EnsureEngine(ctx, lifecycle.StartOptions{
		Socket: rt.paths.Socket, StateDir: rt.stateDir,
		ConfigPath: rt.configForStart(), Binary: initBinary,
	}); err != nil {
		return fail(stderr, err)
	}
	result, err := startDetachedRunner(ctx, rt, saved)
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

// startDetachedRunner is shared by init and start, including init's restart
// after changing agents. EnsureRunner serializes starts and waits for readiness.
func startDetachedRunner(ctx context.Context, rt *runtime, specs []string) (lifecycle.RunnerStartResult, error) {
	args := make([]string, 0, 2*len(specs))
	for _, spec := range specs {
		args = append(args, "--agent", spec)
	}
	return lifecycle.EnsureRunner(ctx, lifecycle.RunnerStartOptions{
		StateDir: rt.stateDir, ConfigPath: rt.configForStart(),
		Binary: initBinary, Args: args,
	})
}
