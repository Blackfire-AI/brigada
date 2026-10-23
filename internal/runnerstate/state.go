// Package runnerstate holds the local single-runner protocol shared by the
// runner and its detached starter. Pids alone are never liveness evidence.
package runnerstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/Blackfire-AI/brigada/internal/api"
)

type Record struct {
	MaxParallel int         `json:"max_parallel,omitempty"`
	PID         int         `json:"pid"`
	Agents      []api.Agent `json:"agents"`
	Specs       []string    `json:"specs,omitempty"`
}

// The restart request modes: RestartDrain has the runner take no new
// lease and exit once its running jobs finish; RestartNow has it exit
// at once, cutting them.
const (
	RestartDrain = "drain"
	RestartNow   = "now"
)

// RequestRestart leaves a restart request for the runner in dir: a
// small JSON file it honours between jobs — drain finishes its running
// jobs first, now stops at once. A runner already draining ignores a
// second request; the fresh starter clears what it finds.
func RequestRestart(dir, mode string) error {
	switch mode {
	case RestartDrain, RestartNow:
	default:
		return fmt.Errorf("restart request: unknown mode %q; want %s or %s", mode, RestartDrain, RestartNow)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	data, err := json.Marshal(struct {
		Mode string `json:"mode"`
	}{mode})
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".restart-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(dir, "restart.json"))
}

// RestartRequested reads the pending restart request: its mode, and
// whether one is pending at all. A file that cannot be read or parsed
// counts as none — the runner never exits on a request it cannot
// understand.
func RestartRequested(dir string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(dir, "restart.json"))
	if err != nil {
		return "", false
	}
	var req struct {
		Mode string `json:"mode"`
	}
	if json.Unmarshal(data, &req) != nil {
		return "", false
	}
	switch req.Mode {
	case RestartDrain, RestartNow:
		return req.Mode, true
	default:
		return "", false
	}
}

// ClearRestart removes the restart request: the runner that honoured
// it, and the starter of the next one, leave nothing pending behind.
func ClearRestart(dir string) error {
	if err := os.Remove(filepath.Join(dir, "restart.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func Acquire(dir string) (func(), error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "runner.lock")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another runner holds %s (pid %d); refusing to start a second: %w", path, readPID(path), err)
	}
	// Clear a previous owner's metadata before publishing this pid. Even pid
	// reuse must not make a stale advertisement look like startup readiness.
	// The restart request goes with it: this runner starts because the last
	// one made way, so it must not honour the request that ended it.
	if err := clearRunnerFiles(dir); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Truncate(0); err != nil {
		f.Close()
		return nil, err
	}
	if _, err := f.WriteString(strconv.Itoa(os.Getpid()) + "\n"); err != nil {
		f.Close()
		return nil, err
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

// clearRunnerFiles removes a previous owner's runner.json and any
// restart request left for it, under the lock Acquire holds.
func clearRunnerFiles(dir string) error {
	for _, name := range []string{"runner.json", "restart.json"} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// ClearStale clears a previous runner's pid and metadata only while holding
// the runner lock. A busy lock returns the live pid and an error wrapping
// EWOULDBLOCK/EAGAIN: a runner is starting or running, so nothing is cleared.
// The lock file stays in place; removing it could split starters across two files.
func ClearStale(dir string) (pid int, cleared bool, err error) {
	path := filepath.Join(dir, "runner.lock")
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			pid := readPID(path)
			return pid, false, fmt.Errorf("a runner is starting or running (pid %d): %w", pid, err)
		}
		return 0, false, err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	data, err := io.ReadAll(f)
	if err != nil {
		return 0, false, err
	}
	pid, _ = strconv.Atoi(strings.TrimSpace(string(data)))
	if err := os.Remove(filepath.Join(dir, "runner.json")); err == nil {
		cleared = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return pid, false, err
	}
	if err := f.Truncate(0); err != nil {
		return pid, cleared, err
	}
	return pid, cleared || len(data) > 0, nil
}

func Publish(dir string, agents []api.Agent, specs ...string) error {
	return PublishParallel(dir, agents, 1, specs...)
}

// PublishParallel records the live runner capacity beside its agents.
func PublishParallel(dir string, agents []api.Agent, parallel int, specs ...string) error {
	data, err := json.Marshal(Record{PID: os.Getpid(), Agents: agents, Specs: specs, MaxParallel: parallel})
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".runner-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(dir, "runner.json"))
}

// Running opens an existing lock only; even a read-only probe creates nothing.
// A stale pid/record is ignored if the kernel lock is free. Metadata is trusted
// only when it names the lock holder (older runners may have no metadata).
func Running(dir string) (Record, bool, error) {
	path := filepath.Join(dir, "runner.lock")
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if errors.Is(err, os.ErrNotExist) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, err
	}
	defer f.Close()
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return Record{}, false, nil
	}
	if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
		return Record{}, false, err
	}
	pid := readPID(path)
	var rec Record
	data, err := os.ReadFile(filepath.Join(dir, "runner.json"))
	if err == nil && json.Unmarshal(data, &rec) == nil && rec.PID == pid {
		return rec, true, nil
	}
	return Record{PID: pid}, true, nil
}

// StalePIDs returns the pids a previous owner left in dir: the one in
// runner.lock and the one in runner.json. They are stale only while no
// fresh runner has cleared them, so a starter reads them before its own
// child can.
func StalePIDs(dir string) []int {
	var pids []int
	if pid := readPID(filepath.Join(dir, "runner.lock")); pid > 0 {
		pids = append(pids, pid)
	}
	var rec Record
	if data, err := os.ReadFile(filepath.Join(dir, "runner.json")); err == nil && json.Unmarshal(data, &rec) == nil && rec.PID > 0 {
		pids = append(pids, rec.PID)
	}
	return pids
}

func readPID(path string) int {
	data, _ := os.ReadFile(path)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	return pid
}
