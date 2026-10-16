// Package runnerstate holds the local single-runner protocol shared by the
// runner and its detached starter. Pids alone are never liveness evidence.
package runnerstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/Blackfire-AI/brigada/internal/api"
)

type Record struct {
	PID    int         `json:"pid"`
	Agents []api.Agent `json:"agents"`
	Specs  []string    `json:"specs,omitempty"`
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
	if err := os.Remove(filepath.Join(dir, "runner.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
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

func Publish(dir string, agents []api.Agent, specs ...string) error {
	data, err := json.Marshal(Record{PID: os.Getpid(), Agents: agents, Specs: specs})
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
func readPID(path string) int {
	data, _ := os.ReadFile(path)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	return pid
}
