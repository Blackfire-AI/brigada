package runner

import (
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// processGroup makes a command start in its own process group, so the
// runner can kill the whole tree — a worker CLI's children die with it,
// not just its own process — and so a check's timeout cannot leave the
// checked repo's grandchildren running.
func processGroup() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// killGroup kills the command's process group: negative pid means every
// process in it. The error names a group that is already gone, which a
// finished worker is, so callers report it only when the kill mattered.
func killGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
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
