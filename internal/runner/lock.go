package runner

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// One runner process per state directory (§5.1). The `runner` role's
// token is one per state dir, so two runner processes on it register
// under the same name and the second's advertisement replaces the
// first's — each process then believes it offers what it was started
// with, while the engine believes whatever registered last. A lock
// file in the state directory fails the second process at once,
// before it can register or touch the shared worktrees.
//
// Why a lock file and not an engine-side check at register: the
// engine may be down or restarting exactly when a second runner
// starts — register is retried with a backoff, so an engine-side
// refusal would come late, after the second process already holds the
// token and shares the state directory's worktrees and mirrors. The
// lock needs no engine at all, and the kernel releases it when the
// process dies however it dies, so a killed runner never wedges its
// state directory. The pid written into the file is for the message a
// second runner prints, not for the decision itself.

// lockStateDir takes the exclusive runner lock inside stateDir and
// returns its release. A lock another runner process holds is refused
// with its pid named.
func lockStateDir(stateDir string) (func(), error) {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, fmt.Errorf("runner: create the state directory: %w", err)
	}
	path := filepath.Join(stateDir, "runner.lock")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("runner: open the runner lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		pid := lockPid(f)
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			if pid > 0 {
				return nil, fmt.Errorf("runner: another runner (pid %d) is already running on this state directory (lock %s); a second would replace its agents at register, so refusing to start", pid, path)
			}
			return nil, fmt.Errorf("runner: another runner holds the lock %s; refusing to start a second", path)
		}
		return nil, fmt.Errorf("runner: lock %s: %w", path, err)
	}
	if err := f.Truncate(0); err == nil {
		f.Seek(0, 0)
		f.WriteString(strconv.Itoa(os.Getpid()) + "\n")
	}
	unlock := func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}
	return unlock, nil
}

// lockPid reads the holder's pid from a lock file this process could
// not lock: whatever the holder last wrote. 0 when it holds nothing
// readable — the message then names the lock alone, never a guess.
func lockPid(f *os.File) int {
	data, err := os.ReadFile(f.Name())
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}
