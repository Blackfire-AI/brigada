package runnerstate_test

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/runnerstate"
)

func TestClearStaleRefusesHeldLock(t *testing.T) {
	dir := t.TempDir()
	// Model a runner taking the lock between stop's probe and cleanup.
	if _, running, err := runnerstate.Running(dir); err != nil || running {
		t.Fatalf("Running: running=%v err=%v", running, err)
	}
	release, err := runnerstate.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := runnerstate.Publish(dir, nil, "test-spec"); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(dir, "runner.lock")
	metadata := filepath.Join(dir, "runner.json")
	wantRecord, err := os.ReadFile(metadata)
	if err != nil {
		t.Fatal(err)
	}
	pid, cleared, err := runnerstate.ClearStale(dir)
	if pid != os.Getpid() || cleared || (!errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN)) {
		t.Fatalf("ClearStale: pid=%d cleared=%v err=%v", pid, cleared, err)
	}
	if !strings.Contains(err.Error(), "pid "+strconv.Itoa(os.Getpid())) {
		t.Errorf("error does not name live pid: %v", err)
	}
	if data, err := os.ReadFile(lock); err != nil || string(data) != strconv.Itoa(os.Getpid())+"\n" {
		t.Errorf("live pid changed: data=%q err=%v", data, err)
	}
	if data, err := os.ReadFile(metadata); err != nil || string(data) != string(wantRecord) {
		t.Errorf("live metadata changed: data=%q err=%v", data, err)
	}
	if rec, running, err := runnerstate.Running(dir); err != nil || !running || rec.PID != os.Getpid() {
		t.Errorf("runner lock lost: record=%+v running=%v err=%v", rec, running, err)
	}
}

func TestClearStaleFreeLock(t *testing.T) {
	for _, tc := range []struct {
		name     string
		pid      int
		metadata bool
		cleared  bool
	}{
		{name: "pid and metadata", pid: 12345, metadata: true, cleared: true},
		{name: "pid only", pid: 12345, cleared: true},
		{name: "metadata only", metadata: true, cleared: true},
		{name: "empty lock"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			lock := filepath.Join(dir, "runner.lock")
			var data []byte
			if tc.pid != 0 {
				data = []byte(strconv.Itoa(tc.pid) + "\n")
			}
			if err := os.WriteFile(lock, data, 0600); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(lock)
			if err != nil {
				t.Fatal(err)
			}
			metadata := filepath.Join(dir, "runner.json")
			if tc.metadata {
				if err := os.WriteFile(metadata, []byte(`{"pid":12345}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			pid, cleared, err := runnerstate.ClearStale(dir)
			if err != nil || pid != tc.pid || cleared != tc.cleared {
				t.Fatalf("ClearStale: pid=%d cleared=%v err=%v", pid, cleared, err)
			}
			if _, err := os.Stat(metadata); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("metadata remains: %v", err)
			}
			if data, err := os.ReadFile(lock); err != nil || len(data) != 0 {
				t.Errorf("lock not emptied: data=%q err=%v", data, err)
			}
			after, err := os.Stat(lock)
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(before, after) {
				t.Error("lock file replaced")
			}
			// Cleanup must release its lock so a new runner can start.
			release, err := runnerstate.Acquire(dir)
			if err != nil {
				t.Fatal(err)
			}
			release()
		})
	}
}

func TestClearStaleNoLock(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")
	pid, cleared, err := runnerstate.ClearStale(dir)
	if err != nil || pid != 0 || cleared {
		t.Fatalf("ClearStale: pid=%d cleared=%v err=%v", pid, cleared, err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ClearStale created state directory: %v", err)
	}
}
