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
	if running {
		return RunnerStartResult{Existing: true, Record: rec}, nil
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
