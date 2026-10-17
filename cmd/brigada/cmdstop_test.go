package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/lifecycle"
	"github.com/Blackfire-AI/brigada/internal/runnerstate"
	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

// TestStopHelperProcess re-execs the test binary as a lock-owning child: a
// runner (BRIGADA_STOP_HELPER=runner) or an engine (=engine) that holds its
// lock until SIGTERM, without needing a real engine or vendor CLI.
func TestStopHelperProcess(t *testing.T) {
	role := os.Getenv("BRIGADA_STOP_HELPER")
	if role == "" {
		return
	}
	state := os.Getenv("BRIGADA_STOP_STATE")
	switch role {
	case "runner":
		release, err := runnerstate.Acquire(filepath.Join(state, "runner"))
		if err != nil {
			os.Exit(11)
		}
		defer release()
		if err := runnerstate.Publish(filepath.Join(state, "runner"), nil); err != nil {
			os.Exit(12)
		}
	case "engine":
		lock, err := lifecycle.AcquireLock(lifecycle.PathsOf(state).Lock)
		if err != nil {
			os.Exit(13)
		}
		defer lock.Release()
	}
	for {
		time.Sleep(time.Hour)
	}
}

// startStopHelper runs a helper holding the lock of role and returns its pid
// once the lock is observable. The child is reaped in the background.
func startStopHelper(t *testing.T, role, state string) int {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, "-test.run=^TestStopHelperProcess$")
	cmd.Env = append(os.Environ(), "BRIGADA_STOP_HELPER="+role, "BRIGADA_STOP_STATE="+state)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { cmd.Wait(); close(exited) }()
	t.Cleanup(func() {
		cmd.Process.Kill()
		<-exited
	})
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		switch role {
		case "runner":
			if rec, running, _ := runnerstate.Running(filepath.Join(state, "runner")); running && rec.PID == cmd.Process.Pid && fileExists(filepath.Join(state, "runner", "runner.json")) {
				return cmd.Process.Pid
			}
		case "engine":
			if lifecycle.EnginePid(lifecycle.PathsOf(state).Lock) == cmd.Process.Pid {
				return cmd.Process.Pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s helper did not take its lock", role)
	return 0
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func stopDo(t *testing.T, state string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(context.Background(), append([]string{"stop", "--state-dir", state}, args...), &out, &errb)
	return code, out.String(), errb.String()
}

func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

func TestStopRunningRunner(t *testing.T) {
	state := testsupport.ShortStateDir(t)
	pid := startStopHelper(t, "runner", state)
	code, out, errs := stopDo(t, state)
	if code != 0 || strings.TrimSpace(out) != "runner stopped (pid "+strconv.Itoa(pid)+")" || errs != "" {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
	if _, running, _ := runnerstate.Running(filepath.Join(state, "runner")); running {
		t.Fatal("runner still holds its lock")
	}
	code, out, _ = stopDo(t, state)
	if code != 0 || !strings.Contains(out, "no runner running") {
		t.Fatalf("second stop: code=%d out=%q", code, out)
	}
}

func TestStopNoRunner(t *testing.T) {
	state := testsupport.ShortStateDir(t)
	code, out, errs := stopDo(t, state)
	if code != 0 || strings.TrimSpace(out) != "no runner running" || errs != "" {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
	code, out, _ = stopDo(t, state, "--engine")
	if code != 0 || !strings.Contains(out, "no engine running") {
		t.Fatalf("--engine with none: code=%d out=%q", code, out)
	}
}

// A record left by a dead runner names a pid that may now belong to any
// process: it is cleared and reported, and that process is left alone.
func TestStopStaleRecordSignalsNobody(t *testing.T) {
	state := testsupport.ShortStateDir(t)
	other := exec.Command("sleep", "60")
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { other.Wait(); close(exited) }()
	t.Cleanup(func() {
		other.Process.Kill()
		<-exited
	})
	dir := filepath.Join(state, "runner")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	pid := strconv.Itoa(other.Process.Pid)
	if err := os.WriteFile(filepath.Join(dir, "runner.lock"), []byte(pid+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "runner.json"), []byte(`{"pid":`+pid+`,"agents":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	code, out, errs := stopDo(t, state)
	if code != 0 || !strings.Contains(out, "stale runner record (pid "+pid+")") || !strings.Contains(out, "no runner running") || errs != "" {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
	if fileExists(filepath.Join(dir, "runner.json")) {
		t.Error("stale runner.json kept")
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "runner.lock")); strings.TrimSpace(string(data)) != "" {
		t.Errorf("stale pid kept in runner.lock: %q", data)
	}
	time.Sleep(100 * time.Millisecond)
	if !processAlive(other.Process.Pid) {
		t.Fatal("the unrelated process was signalled")
	}
	select {
	case <-exited:
		t.Fatal("the unrelated process exited")
	default:
	}
	if code, out, _ = stopDo(t, state); code != 0 || strings.TrimSpace(out) != "no runner running" {
		t.Fatalf("after cleanup: code=%d out=%q", code, out)
	}
}

func TestStopEngine(t *testing.T) {
	state := testsupport.ShortStateDir(t)
	runnerPid := startStopHelper(t, "runner", state)
	enginePid := startStopHelper(t, "engine", state)

	// Without --engine the engine is left alone.
	code, out, errs := stopDo(t, state)
	if code != 0 || !strings.Contains(out, "runner stopped (pid "+strconv.Itoa(runnerPid)+")") || strings.Contains(out, "engine") || errs != "" {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
	if lifecycle.EnginePid(lifecycle.PathsOf(state).Lock) != enginePid {
		t.Fatal("engine stopped without --engine")
	}
	code, out, errs = stopDo(t, state, "--engine")
	if code != 0 || !strings.Contains(out, "no runner running") || !strings.Contains(out, "engine stopped (pid "+strconv.Itoa(enginePid)+")") || errs != "" {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
	if pid := lifecycle.EnginePid(lifecycle.PathsOf(state).Lock); pid != 0 {
		t.Fatalf("engine pid %d still holds the lock", pid)
	}
}

func TestStopRejectsArguments(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"stop", "now"}, &out, &errb); code != 2 {
		t.Fatalf("code=%d err=%q", code, errb.String())
	}
}
