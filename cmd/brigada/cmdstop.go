package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Blackfire-AI/brigada/internal/lifecycle"
	"github.com/Blackfire-AI/brigada/internal/runnerstate"
)

// stopWait is how long stop waits for a signalled process to let go of its
// lock before it reports failure.
var stopWait = 10 * time.Second

// runStop stops the detached runner recorded in the state directory, and
// with --engine the engine too. A pid is signalled only while its process
// still holds the lock the runner or engine took: a pid left behind in a
// stale record may belong to anything by now.
func runStop(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("stop",
		"brigada stop [--engine]",
		"Stop the detached runner of this state directory: SIGTERM, then wait up to 10 seconds.")
	stderr = commandStderr(stderr, fs.Name())
	common := addCommon(fs)
	withEngine := fs.Bool("engine", false, "also stop the engine, if it is running")
	if code, stop := common.parse(stdout, stderr, args); stop {
		return code
	}
	if args := fs.Args(); len(args) > 0 {
		return usagef(stderr, "stop takes no arguments, got %q", args)
	}
	rt, code := resolveRuntime(common, stderr)
	if code != 0 {
		return code
	}
	dir := runnerStateDir(rt)
	rec, running, err := runnerstate.Running(dir)
	if err != nil {
		return fail(stderr, err)
	}
	switch {
	case running:
		if rec.PID <= 0 {
			return fail(stderr, fmt.Errorf("the runner holds %s but records no pid; not signalling anything", filepath.Join(dir, "runner.lock")))
		}
		stopped, err := terminate(ctx, rec.PID, func() bool {
			_, live, err := runnerstate.Running(dir)
			return err == nil && live
		})
		if err != nil {
			return fail(stderr, fmt.Errorf("runner (pid %d): %w", rec.PID, err))
		}
		if stopped {
			fmt.Fprintf(stdout, "runner stopped (pid %d)\n", rec.PID)
		} else {
			fmt.Fprintln(stdout, "no runner running")
		}
	default:
		pid, cleared, err := runnerstate.ClearStale(dir)
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			fmt.Fprintf(stdout, "a runner is starting (pid %d); run brigada stop again\n", pid)
			return 0
		}
		if err != nil {
			return fail(stderr, err)
		}
		if cleared {
			fmt.Fprintf(stdout, "removed the stale runner record (pid %d); no runner running\n", pid)
		} else {
			fmt.Fprintln(stdout, "no runner running")
		}
	}
	if !*withEngine {
		return 0
	}
	pid := lifecycle.EnginePid(rt.paths.Lock)
	if pid <= 0 {
		fmt.Fprintln(stdout, "no engine running")
		return 0
	}
	stopped, err := terminate(ctx, pid, func() bool { return lifecycle.EnginePid(rt.paths.Lock) == pid })
	if err != nil {
		return fail(stderr, fmt.Errorf("engine (pid %d): %w", pid, err))
	}
	if stopped {
		fmt.Fprintf(stdout, "engine stopped (pid %d)\n", pid)
	} else {
		fmt.Fprintln(stdout, "no engine running")
	}
	return 0
}

// terminate sends SIGTERM to pid and waits until holds reports the lock
// free. holds is what ties pid to the recorded process: it is checked
// again right before the signal, so a process that exited meanwhile is
// never signalled by a recycled pid. It reports whether a signal was sent.
func terminate(ctx context.Context, pid int, holds func() bool) (bool, error) {
	if !holds() {
		return false, nil
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return false, nil
		}
		return false, err
	}
	deadline := time.Now().Add(stopWait)
	for holds() {
		if !time.Now().Before(deadline) {
			return true, fmt.Errorf("still running %s after SIGTERM", stopWait)
		}
		select {
		case <-ctx.Done():
			return true, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
	return true, nil
}
