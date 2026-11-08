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

	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/lifecycle"
	"github.com/Blackfire-AI/brigada/internal/runnerstate"
)

// stopWait is how long stop waits for a signalled process to let go of its
// lock before it reports failure.
var stopWait = 10 * time.Second

// drainPoll is how often a drain asks the engine whether it is drained.
var drainPoll = 500 * time.Millisecond

// terminateEngine signals the engine to stop. It is a variable so a test
// whose engine runs in its own process never signals itself.
var terminateEngine = terminate

// runStop stops the detached runner recorded in the state directory, and
// with --engine the engine too; with --drain it first pauses the engine
// and waits until no job is leased or running (§8.3). A pid is signalled
// only while its process still holds the lock the runner or engine took:
// a pid left behind in a stale record may belong to anything by now.
func runStop(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("stop",
		"brigada stop [--engine [--drain]]",
		"Stop the detached runner of this state directory: SIGTERM, then wait up to 10 seconds.")
	stderr = commandStderr(stderr, fs.Name())
	common := addCommon(fs)
	withEngine := fs.Bool("engine", false, "also stop the engine, if it is running")
	drain := fs.Bool("drain", false, "with --engine: first pause the engine and wait until no job is leased or running")
	if code, stop := common.parse(stdout, stderr, args); stop {
		return code
	}
	if args := fs.Args(); len(args) > 0 {
		return usagef(stderr, "stop takes no arguments, got %q", args)
	}
	if *drain && !*withEngine {
		return usagef(stderr, "--drain needs --engine: it drains the engine before stopping it")
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
	stopped, err := terminateEngine(ctx, pid, func() bool { return lifecycle.EnginePid(rt.paths.Lock) == pid })
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

// drainEngine is the first half of `stop --engine --drain` (§8.3): it
// pauses the engine with origin drain — a user's pause already standing
// keeps its origin — and waits until the engine is drained, naming the
// jobs it still waits on as the set changes. The runner keeps running
// meanwhile, for the jobs it holds end only through it. Interrupting the
// wait stops nothing and leaves the pause in place; it returns 1 so the
// caller stops nothing then.
func drainEngine(ctx context.Context, rt *runtime, stdout, stderr io.Writer) int {
	if !lifecycle.EngineRunning(ctx, rt.paths.Socket) {
		fmt.Fprintln(stdout, "no engine running; nothing to drain")
		return 0
	}
	client, code := connect(ctx, rt, "client", true, stderr)
	if code != 0 {
		return code
	}
	res, err := client.Pause(ctx, "", engine.PauseOriginDrain)
	if err != nil {
		return fail(stderr, fmt.Errorf("pause the engine for the drain: %w", err))
	}
	switch {
	case res.Started:
		fmt.Fprintln(stdout, "engine paused to drain")
	case res.Origin == engine.PauseOriginUser:
		fmt.Fprintln(stdout, "the engine is already paused by the user; the pause stays the user's")
	default:
		fmt.Fprintln(stdout, "the engine is already paused by a drain")
	}
	interrupted := func() int {
		fmt.Fprintln(stdout, "interrupted: nothing was stopped and the engine stays paused; "+
			"brigada start lifts a drain pause, brigada resume lifts any")
		return 1
	}
	last := ""
	for {
		st, err := client.Status(ctx)
		if ctx.Err() != nil {
			return interrupted()
		}
		if err != nil {
			return fail(stderr, fmt.Errorf("read the drain (nothing was stopped, the engine stays paused): %w", err))
		}
		var pause *engine.LivePause
		for i := range st.Pauses {
			if st.Pauses[i].ProjectID == 0 {
				pause = &st.Pauses[i]
			}
		}
		if pause == nil {
			return fail(stderr, fmt.Errorf("the engine's pause was lifted while the drain waited; nothing was stopped"))
		}
		if pause.Drained {
			fmt.Fprintln(stdout, "engine drained: no job leased or running")
			return 0
		}
		if line := drainingLine(pause.Draining); line != last {
			fmt.Fprintln(stdout, line)
			last = line
		}
		select {
		case <-ctx.Done():
			return interrupted()
		case <-time.After(drainPoll):
		}
	}
}

// drainingLine names the jobs a drain still waits on.
func drainingLine(jobs []engine.PauseJob) string {
	names := make([]string, len(jobs))
	for i, j := range jobs {
		if j.TaskID != 0 {
			names[i] = fmt.Sprintf("job %d (task %d, %s, %s)", j.ID, j.TaskID, j.Kind, j.Status)
		} else {
			names[i] = fmt.Sprintf("job %d (%s, %s)", j.ID, j.Kind, j.Status)
		}
	}
	noun := "jobs"
	if len(jobs) == 1 {
		noun = "job"
	}
	return fmt.Sprintf("waiting on %d %s: %s", len(jobs), noun, strings.Join(names, ", "))
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
