package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Blackfire-AI/brigada/internal/runnerstate"
)

// RunnerStartOptions uses the same detach/log convention as EnsureEngine.
// Args are already validated runner flags, including the chosen agents.
type RunnerStartOptions struct {
	StateDir   string
	ConfigPath string
	Binary     string
	Args       []string
	Wait       time.Duration
	// Restart, when "drain" or "now", makes a running runner make way
	// for the start: "drain" hands it a request it honours between
	// jobs — it takes no new lease and exits once its running jobs
	// finish — while "now" has it exit at once, cutting them. Empty
	// keeps a running runner running and only reports it.
	Restart string
}

type RunnerStartResult struct {
	Existing bool
	Record   runnerstate.Record
}

// EnsureRunner serializes starters until the child owns runner.lock. The
// runner itself holds that lock throughout its life, including manual starts.
// Stale logs and pid records never count as a running process.
func EnsureRunner(ctx context.Context, o RunnerStartOptions) (RunnerStartResult, error) {
	var result RunnerStartResult
	if err := ctx.Err(); err != nil {
		return result, err
	}
	dir := filepath.Join(o.StateDir, "runner")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return result, err
	}
	gate, err := os.OpenFile(filepath.Join(dir, "start.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return result, err
	}
	defer gate.Close()
	wait := o.Wait
	if wait <= 0 {
		wait = startWait
	}
	deadline := time.Now().Add(wait)
	for {
		err := syscall.Flock(int(gate.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return result, err
		}
		if err := runnerStartPause(ctx, deadline); err != nil {
			return result, err
		}
	}
	defer syscall.Flock(int(gate.Fd()), syscall.LOCK_UN)
	rec, running, err := runnerstate.Running(dir)
	if err != nil {
		return result, err
	}
	if running && o.Restart == "" {
		return RunnerStartResult{Existing: true, Record: rec}, nil
	}
	if running {
		// The handoff runs under the same start gate: the request is
		// written, the runner honoured it, and the fresh start follows
		// with no second starter in between. The readiness wait starts
		// over, wider: the handoff itself may have waited out the
		// window taken at entry, and the machine may still be busy
		// with the old runner's teardown.
		if err := handoffRunner(ctx, dir, o.Restart); err != nil {
			return result, err
		}
		wait = max(wait, restartStartWait)
		deadline = time.Now().Add(wait)
	}
	binary := o.Binary
	if binary == "" {
		binary, err = os.Executable()
		if err != nil {
			return result, err
		}
	}
	args := []string{"runner", "--state-dir", o.StateDir}
	if o.ConfigPath != "" {
		args = append(args, "--config", o.ConfigPath)
	}
	args = append(args, o.Args...)
	logPath := filepath.Join(o.StateDir, "runner.log")
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return result, err
	}
	defer log.Close()
	null, err := os.Open(os.DevNull)
	if err != nil {
		return result, err
	}
	defer null.Close()
	cmd := exec.Command(binary, args...)
	cmd.Dir = o.StateDir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = null, log, log
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := cmd.Start(); err != nil {
		return result, fmt.Errorf("start runner: %w", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	acknowledged := false
	defer func() {
		if !acknowledged {
			_ = cmd.Process.Kill()
		}
	}()
	for {
		rec, running, err := runnerstate.Running(dir)
		if err != nil {
			return result, err
		}
		if running && rec.Agents != nil {
			// A manual runner won the race. Stop our child before it can
			// acquire the lock later and silently replace the reported agents.
			acknowledged = rec.PID == cmd.Process.Pid
			return RunnerStartResult{Existing: rec.PID != cmd.Process.Pid, Record: rec}, nil
		}
		select {
		case err := <-exited:
			// A manual runner may have won the lock during startup. Report it.
			rec, running, probeErr := runnerstate.Running(dir)
			if probeErr == nil && running {
				acknowledged = true
				return RunnerStartResult{Existing: true, Record: rec}, nil
			}
			return result, fmt.Errorf("runner exited during startup (%v); see %s", err, logPath)
		default:
		}
		if err := runnerStartPause(ctx, deadline); err != nil {
			// Do not leave an unacknowledged child that could win the lock later.
			_ = cmd.Process.Kill()
			<-exited
			return result, fmt.Errorf("runner startup: %w; see %s", err, logPath)
		}
	}
}

func runnerStartPause(ctx context.Context, deadline time.Time) error {
	if !time.Now().Before(deadline) {
		return fmt.Errorf("timed out waiting for the runner")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(startPoll):
		return nil
	}
}

// restartNowWait is how long a --now handoff waits for the runner to
// honour the request before it falls back to the signal, and
// restartDrainWait how long a drain may take: a drain never falls back
// — no running job is cut by a restart that did not ask for --now — so
// the wait spans a job's whole run and the caller's ctx is the only
// earlier way out. restartStartWait is the readiness window the start
// after a handoff gets, wider than a cold start's.
var (
	restartNowWait   = 30 * time.Second
	restartDrainWait = time.Hour
	restartStartWait = 30 * time.Second
)

// handoffRunner hands a running runner the restart request and waits
// for it to exit (§5.1): `drain` waits out its running jobs, `now`
// stops it at once. A `now` the runner does not honour in time — an
// older binary, a stuck process — falls back to SIGTERM, like stop; a
// drain never signals, it reports and leaves the runner running.
func handoffRunner(ctx context.Context, dir, mode string) error {
	switch mode {
	case runnerstate.RestartDrain, runnerstate.RestartNow:
	default:
		return fmt.Errorf("restart the runner: unknown mode %q; want %s or %s", mode, runnerstate.RestartDrain, runnerstate.RestartNow)
	}
	if err := runnerstate.RequestRestart(dir, mode); err != nil {
		return fmt.Errorf("restart the runner: %w", err)
	}
	wait := restartNowWait
	if mode == runnerstate.RestartDrain {
		wait = restartDrainWait
	}
	deadline := time.Now().Add(wait)
	signalled := false
	for {
		rec, running, err := runnerstate.Running(dir)
		if err != nil {
			return err
		}
		if !running {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if mode == runnerstate.RestartNow && !signalled && !time.Now().Before(deadline) {
			// The runner did not make way in time: the request could be
			// one its binary predates. Stop asks the same of it here.
			if err := syscall.Kill(rec.PID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
				return fmt.Errorf("signal the runner (pid %d): %w", rec.PID, err)
			}
			signalled = true
			deadline = time.Now().Add(10 * time.Second)
			continue
		}
		if time.Now().After(deadline) {
			if mode == runnerstate.RestartDrain {
				return fmt.Errorf("the runner (pid %d) still holds running jobs after %s; no restart was forced; rerun with --now to cut them",
					rec.PID, wait)
			}
			return fmt.Errorf("the runner (pid %d) still runs %s after SIGTERM", rec.PID, 10*time.Second)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(startPoll):
		}
	}
}
