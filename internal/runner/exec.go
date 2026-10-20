package runner

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// processGroup makes a command start in its own process group, so the
// runner can kill the whole tree — a worker CLI's children die with it,
// not just its own process — and so a check's timeout cannot leave the
// checked repo's grandchildren running.
func processGroup() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// killGroup kills the command's process group outright: negative pid
// means every process in it. The error names a group that is already
// gone, which a finished worker is, so callers report it only when the
// kill mattered.
func killGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

// terminateGroup kills the command's whole process tree the polite way
// (§5.6): SIGTERM to the group first, so a worker or child that cleans
// up on the term may, then SIGKILL to the group after grace whatever
// they did — a child that traps the term and outlives it is still dead
// on schedule, even once the leader itself has died and been reaped.
// It returns once the term is out; the kill rides its own timer, so
// the run moves on without waiting the grace out — the timer landing
// after the run has ended is intended, for the tree's stragglers die
// on schedule whatever the run did.
//
// The kill probes the group, not the leader: while any member of the
// tree lives, the kernel keeps the group id ours, so -pgid can only
// ever name this tree's processes. The probe only spares a group that
// is wholly gone, whose id the kernel may since have handed to someone
// else's process group.
func terminateGroup(cmd *exec.Cmd, grace time.Duration) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pgid := cmd.Process.Pid
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	time.AfterFunc(grace, func() {
		if err := syscall.Kill(-pgid, 0); err == nil || errors.Is(err, syscall.EPERM) {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		}
	})
}

// withPWD sets PWD to dir (made absolute) in env, replacing any PWD it
// holds and dropping OLDPWD. A nil env means "inherit the process's",
// so it is materialized first.
func withPWD(env []string, dir string) []string {
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	if env == nil {
		env = os.Environ()
	}
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if strings.HasPrefix(kv, "PWD=") || strings.HasPrefix(kv, "OLDPWD=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "PWD="+dir)
}

// scrubEnv removes every variable named in drop from env. A nil env
// means "inherit the process's", so it is materialized first — the
// scrub applies either way. This is how the runner's git credentials
// (Config.GitEnv) are kept out of the worker's environment (§5.1):
// adapters may build on the ambient environment for the CLI's own login
// state, but the runner strips what is only its.
func scrubEnv(env []string, drop []string) []string {
	if len(drop) == 0 {
		if env == nil {
			return nil
		}
		return env
	}
	dropKeys := make(map[string]bool, len(drop))
	for _, kv := range drop {
		if k, _, ok := strings.Cut(kv, "="); ok && k != "" {
			dropKeys[k] = true
		}
	}
	if env == nil {
		env = os.Environ()
	}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if k, _, ok := strings.Cut(kv, "="); ok && dropKeys[k] {
			continue
		}
		out = append(out, kv)
	}
	return out
}
