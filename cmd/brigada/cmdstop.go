package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
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

// runStop stops the detached runner recorded in the state directory, and
// with --engine the engine too. A pid is signalled only while its process
// still holds the lock the runner or engine took: a pid left behind in a
// stale record may belong to anything by now.
func runStop(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("stop",
		"brigada stop [--engine [--drain]]",
		"Stop the detached runner of this state directory: SIGTERM, then wait up to 10 seconds.")
	stderr = commandStderr(stderr, fs.Name())
	common := addCommon(fs)
	withEngine := fs.Bool("engine", false, "also stop the engine, if it is running")
	drain := fs.Bool("drain", false, "with --engine: pause the engine and wait for its running jobs to end before stopping")
	if code, stop := common.parse(stdout, stderr, args); stop {
		return code
	}
	if args := fs.Args(); len(args) > 0 {
		return usagef(stderr, "stop takes no arguments, got %q", args)
	}
	if *drain && !*withEngine {
		return usagef(stderr, "--drain needs --engine: it pauses the engine and waits for its jobs before stopping it")
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

// drainPoll is how often a drain looks at the jobs it waits on.
var drainPoll = time.Second

// drainEngine is the first half of `stop --engine --drain` (§8.3): pause
// the engine with origin drain and wait until no job is leased or
// running, naming the jobs it still waits on. With no engine running
// there is nothing to drain. Interrupting the wait stops nothing and
// leaves the pause in place, and says so.
func drainEngine(ctx context.Context, rt *runtime, stdout, stderr io.Writer) int {
	if lifecycle.EnginePid(rt.paths.Lock) <= 0 {
		return 0
	}
	client, code := connect(ctx, rt, "client", true, stderr)
	if code != 0 {
		return code
	}
	res, err := client.Pause(ctx, "", engine.PauseOriginDrain)
	if err != nil {
		return fail(stderr, err)
	}
	switch {
	case res.Started:
		fmt.Fprintln(stdout, "engine paused to drain; running jobs finish, queued jobs wait")
	case res.Origin == engine.PauseOriginUser:
		fmt.Fprintln(stdout, "engine already paused by you (origin user); draining under it")
	default:
		fmt.Fprintln(stdout, "engine already paused to drain; waiting for its running jobs")
	}
	err = waitDrained(ctx, stdout, func(ctx context.Context) ([]engine.PauseJob, bool, error) {
		st, err := client.Status(ctx)
		if err != nil {
			return nil, false, err
		}
		jobs, held := enginePauseJobs(st)
		return jobs, held, nil
	})
	switch {
	case err == nil:
		fmt.Fprintln(stdout, "engine drained")
		return 0
	case ctx.Err() != nil:
		fmt.Fprintln(stderr, "brigada: interrupted while draining; nothing was stopped and the engine stays paused (brigada start lifts a drain's pause, brigada resume any)")
		return 130
	}
	return fail(stderr, err)
}

// enginePauseJobs reads the jobs the engine's pause still drains from a
// status; held is false when no pause of the engine stands.
func enginePauseJobs(st api.Status) (jobs []engine.PauseJob, held bool) {
	for _, p := range st.Pauses {
		if p.ProjectID == 0 {
			return p.Draining, true
		}
	}
	return nil, false
}

// waitDrained polls until the engine's pause has no job left to drain,
// printing the jobs it waits on whenever that set changes. poll reports
// the draining jobs and whether the engine's pause still stands.
func waitDrained(ctx context.Context, stdout io.Writer, poll func(context.Context) ([]engine.PauseJob, bool, error)) error {
	last := ""
	for {
		jobs, held, err := poll(ctx)
		if err != nil {
			return err
		}
		if !held {
			return errors.New("the engine's pause was lifted while waiting; nothing was stopped")
		}
		if len(jobs) == 0 {
			return nil
		}
		line := "waiting on"
		for i, j := range jobs {
			if i > 0 {
				line += ","
			}
			line += fmt.Sprintf(" job %d (%s, %s)", j.ID, j.Kind, j.Status)
		}
		if line != last {
			fmt.Fprintln(stdout, line)
			last = line
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(drainPoll):
		}
	}
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
