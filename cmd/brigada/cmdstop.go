package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/lifecycle"
	"github.com/Blackfire-AI/brigada/internal/runnerstate"
)

// stopWait is how long stop waits for a signalled process to let go of its
// lock before it reports failure.
var stopWait = 10 * time.Second

// drainPollInterval is how often `stop --engine --drain` re-reads the
// engine's pauses while it waits for the last running job to finish
// (§8.3). A var so a test could tighten it.
var drainPollInterval = 250 * time.Millisecond

// Tests can exercise the drain over an in-process API without a
// listener.
var connectStop = connect

// runStop stops the detached runner recorded in the state directory, and
// with --engine the engine too. With --engine --drain it first pauses
// the engine with origin `drain` and waits until no job of it is leased
// or running, so nothing is stopped mid-run (§8.3). A pid is signalled
// only while its process still holds the lock the runner or engine
// took: a pid left behind in a stale record may belong to anything by
// now.
func runStop(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("stop",
		"brigada stop [--engine] [--drain]",
		"Stop the detached runner of this state directory: SIGTERM, then wait up to 10 seconds.")
	stderr = commandStderr(stderr, fs.Name())
	common := addCommon(fs)
	withEngine := fs.Bool("engine", false, "also stop the engine, if it is running")
	drain := fs.Bool("drain", false, "with --engine: pause the engine first and wait out its running jobs, naming them as it waits")
	if code, stop := common.parse(stdout, stderr, args); stop {
		return code
	}
	if args := fs.Args(); len(args) > 0 {
		return usagef(stderr, "stop takes no arguments, got %q", args)
	}
	if *drain && !*withEngine {
		return usagef(stderr, "--drain pauses the engine and waits out its jobs; it goes with --engine: brigada stop --engine --drain")
	}
	rt, code := resolveRuntime(common, stderr)
	if code != 0 {
		return code
	}
	if *drain {
		if code := drainEngine(ctx, rt, stdout, stderr); code != 0 {
			return code
		}
	}
	dir := runnerStateDir(rt)
	rec, running, err := runnerstate.Running(dir)
	if err != nil {
		return fail(stderr, err)
	}
	switch {
	case running:
		stopped, err := terminateRunner(ctx, dir, rec)
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

// terminateRunner checks both the lock and its owner before signalling.
func terminateRunner(ctx context.Context, dir string,
	rec runnerstate.Record) (bool, error) {
	if rec.PID <= 0 {
		return false, fmt.Errorf(
			"the runner holds %s but records no pid; not signalling anything",
			filepath.Join(dir, "runner.lock"))
	}
	return terminate(ctx, rec.PID, func() bool {
		current, live, err := runnerstate.Running(dir)
		return err == nil && live && current.PID == rec.PID
	})
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

// drainEngine is `stop --engine --drain`'s first half (§8.3): pause the
// engine with origin `drain`, then wait until it is drained — no job of
// it leased or running — naming the jobs it still waits on as the set
// changes. Over a user's pause the origin stays the user's; the
// stopping that follows is `stop --engine`'s own. Nothing is drained
// when no engine answers: nothing runs, and the stop says so.
func drainEngine(ctx context.Context, rt *runtime, stdout, stderr io.Writer) int {
	if !lifecycle.EngineRunning(ctx, rt.paths.Socket) {
		return 0
	}
	client, code := connectStop(ctx, rt, "client", true, stderr)
	if code != 0 {
		return code
	}
	res, err := client.Pause(ctx, "", engine.PauseOriginDrain)
	if err != nil {
		return fail(stderr, err)
	}
	if res.Started {
		fmt.Fprintln(stdout, "engine paused for the drain")
	} else {
		// A user's origin outranks a drain's (§8.3): an existing
		// user pause stands as it is.
		fmt.Fprintf(stdout, "engine already paused (origin %s); waiting out its jobs\n", res.Origin)
	}
	if code := waitDrained(ctx, client, res.Origin, stdout, stderr); code != 0 {
		return code
	}
	fmt.Fprintln(stdout, "engine drained")
	return 0
}

// waitDrained waits until none of the engine's jobs is leased or
// running (§8.3), naming the jobs it still waits on as it waits. An
// interrupt stops nothing and leaves the pause in place, and says so.
func waitDrained(ctx context.Context, client *api.Client, origin string, stdout, stderr io.Writer) int {
	interrupted := func() int {
		fmt.Fprintf(stdout, "drain interrupted: nothing stopped; the engine stays paused (origin %s); `brigada start` lifts it\n", origin)
		return 1
	}
	last := ""
	for {
		st, err := client.Status(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return interrupted()
			}
			return fail(stderr, err)
		}
		var draining []engine.PauseJob
		for _, p := range st.Pauses {
			if p.ProjectID == 0 {
				draining = p.Draining
				break
			}
		}
		if len(draining) == 0 {
			return 0
		}
		names := make([]string, len(draining))
		for i, j := range draining {
			names[i] = fmt.Sprintf("job %d (%s, %s)", j.ID, j.Kind, j.Status)
		}
		if line := strings.Join(names, ", "); line != last {
			fmt.Fprintf(stdout, "draining: %s\n", line)
			last = line
		}
		select {
		case <-ctx.Done():
			return interrupted()
		case <-time.After(drainPollInterval):
		}
	}
}
