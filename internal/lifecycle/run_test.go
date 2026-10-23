// Package lifecycle_test runs the engine role as the real binary (§13):
// the single-instance lock, start on demand, --no-start, the idle exit,
// and the crash-and-recover story of the BP8a exit test. Everything uses
// short state directories and the shared test binary.
package lifecycle_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/lifecycle"
	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

// runBrigada runs the binary to completion and returns its output and
// exit code.
func runBrigada(t *testing.T, bin string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("run brigada %s: %v", args, err)
		}
		code = ee.ExitCode()
	}
	return out.String(), errb.String(), code
}

// engineProc is one engine started as a child of the test. The child's
// output lands in a locked buffer — exec's stdout and stderr copiers
// keep writing while a test may quote the log — and its exit, waited on
// by a goroutine of our own, is remembered under a mutex: nothing but
// the Wait call ever touches cmd.ProcessState.
type engineProc struct {
	cmd  *exec.Cmd
	log  lockedBuffer
	done chan error

	mu     sync.Mutex
	exited bool
}

// lockedBuffer is a bytes.Buffer the engine's two copy goroutines write
// and the test reads, so every access rides one mutex.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// startEngine runs `brigada engine` on stateDir, registering a cleanup
// that stops it.
func startEngine(t *testing.T, bin, stateDir string, extra ...string) *engineProc {
	t.Helper()
	args := append([]string{"engine", "--state-dir", stateDir}, extra...)
	cmd := exec.Command(bin, args...)
	p := &engineProc{cmd: cmd, done: make(chan error, 1)}
	cmd.Stdout = &p.log
	cmd.Stderr = &p.log
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the engine: %v", err)
	}
	go func() {
		err := cmd.Wait()
		p.mu.Lock()
		p.exited = true
		p.mu.Unlock()
		p.done <- err
	}()
	t.Cleanup(func() { p.stop(t) })
	return p
}

// hasExited reports whether the engine's Wait returned.
func (p *engineProc) hasExited() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exited
}

// stop terminates the engine politely, then kills it. Whether the
// engine already left comes from the wait bookkeeping, never from
// cmd.ProcessState, which only the Wait goroutine may read.
func (p *engineProc) stop(t *testing.T) {
	t.Helper()
	if p.hasExited() {
		return
	}
	p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		p.cmd.Process.Kill()
		<-p.done
	}
}

// waitExit waits for the engine to leave on its own (the idle exit) and
// reports its exit code.
func (p *engineProc) waitExit(t *testing.T, within time.Duration) int {
	t.Helper()
	select {
	case err := <-p.done:
		if err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				return ee.ExitCode()
			}
			t.Fatalf("the engine failed: %v (log: %s)", err, p.log.String())
		}
		return 0
	case <-time.After(within):
		t.Fatalf("the engine did not exit within %v (log: %s)", within, p.log.String())
		return -1
	}
}

// killEngineAt kills whatever engine holds the lock of stateDir — the
// detached engines a client started on demand, which are no one's
// children here — and waits for the lock to come free and for the
// engine's process group to be gone. A detached engine leads its own
// session, so its group holds the git push of its plan-file mirror: a
// push still in flight when only the engine dies would go on writing
// into the remote after the test returns, racing the TempDir removal.
func killEngineAt(t *testing.T, stateDir string) {
	t.Helper()
	lock := lifecycle.PathsOf(stateDir).Lock
	pid := lifecycle.EnginePid(lock)
	if pid <= 0 {
		return
	}
	if err := syscall.Kill(int(pid), syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("kill the engine pid %d: %v", pid, err)
	}
	// The group id is the engine's pid only while the engine leads one; a
	// pid that leads no group answers ESRCH, and darwin answers EPERM for
	// a group holding only zombies (see waitGroupGone).
	if err := syscall.Kill(-int(pid), syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) && !errors.Is(err, syscall.EPERM) {
		t.Fatalf("kill the engine's process group %d: %v", pid, err)
	}
	waitFor(t, "the killed engine's lock to come free", func() bool {
		return lifecycle.EnginePid(lock) == 0
	})
	waitGroupGone(t, int(pid), "engine")
}

// killEngineOnCleanup detaches a killer for the engine that will hold
// stateDir's lock, for the tests whose clients start one on demand.
func killEngineOnCleanup(t *testing.T, stateDir string) {
	t.Cleanup(func() { killEngineAt(t, stateDir) })
}

// waitFor polls cond until it holds, failing with what was being waited
// for. On timeout it runs the diagnosis, if any, so a failure explains
// itself: the engine's log, the runner's log, whatever the waiter knows.
func waitFor(t *testing.T, what string, cond func() bool, diagnose ...func()) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			for _, d := range diagnose {
				d()
			}
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// dumpEngineLog quotes the state dir's engine log into the test output.
func dumpEngineLog(t *testing.T, stateDir string) func() {
	return func() {
		t.Helper()
		if data, err := os.ReadFile(filepath.Join(stateDir, "engine.log")); err == nil {
			t.Logf("engine log:\n%s", data)
		}
	}
}

// waitHealthy waits until an engine answers on the state dir's socket.
func waitHealthy(t *testing.T, stateDir string) {
	t.Helper()
	socket := lifecycle.PathsOf(stateDir).Socket
	waitFor(t, "the engine to answer on "+socket, func() bool {
		return lifecycle.EngineRunning(context.Background(), socket)
	})
}

func TestSingleInstanceRefusal(t *testing.T) {
	bin := testsupport.BuildBrigada(t)
	state := testsupport.ShortStateDir(t)
	first := startEngine(t, bin, state)
	waitHealthy(t, state)
	pid := lifecycle.EnginePid(lifecycle.PathsOf(state).Lock)
	if pid <= 0 {
		t.Fatalf("no engine pid found in the lock of %s", state)
	}

	stdout, stderr, code := runBrigada(t, bin, "engine", "--state-dir", state)
	if code != 1 {
		t.Errorf("second engine exit code = %d (stdout %q), want 1", code, stdout)
	}
	for _, want := range []string{"another engine", "pid", strconv.Itoa(pid), "refusing"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("second engine stderr = %q, want it to contain %q", stderr, want)
		}
	}
	// The first engine is untouched.
	if !lifecycle.EngineRunning(context.Background(), lifecycle.PathsOf(state).Socket) {
		t.Error("the first engine stopped on the second's refusal")
	}
	first.stop(t)
}

func TestStartOnDemand(t *testing.T) {
	bin := testsupport.BuildBrigada(t)
	state := testsupport.ShortStateDir(t)
	killEngineOnCleanup(t, state)

	if lifecycle.EnginePid(lifecycle.PathsOf(state).Lock) != 0 {
		t.Fatal("an engine already holds a fresh state dir's lock")
	}
	stdout, stderr, code := runBrigada(t, bin, "status", "--state-dir", state)
	if code != 0 {
		t.Fatalf("status exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "engine pid ") || !strings.Contains(stdout, "jobs:") {
		t.Errorf("status output = %q, want the engine line and the jobs line", stdout)
	}
	// The client started an engine and left it running detached.
	if pid := lifecycle.EnginePid(lifecycle.PathsOf(state).Lock); pid <= 0 {
		t.Error("no engine was left running by the client")
	}
	waitHealthy(t, state)
	// A log file carries the detached engine's output.
	if _, err := os.Stat(filepath.Join(state, "engine.log")); err != nil {
		t.Errorf("the engine's log: %v", err)
	}
}

func TestNoStartRefusesWithoutAnEngine(t *testing.T) {
	bin := testsupport.BuildBrigada(t)
	state := testsupport.ShortStateDir(t)

	stdout, stderr, code := runBrigada(t, bin, "status", "--state-dir", state, "--no-start")
	if code != 1 {
		t.Errorf("status --no-start exit = %d (stdout %q), want 1", code, stdout)
	}
	if !strings.Contains(stderr, "no engine is answering") || !strings.Contains(stderr, "--no-start") {
		t.Errorf("stderr = %q, want the refusal naming --no-start", stderr)
	}
	if lifecycle.EnginePid(lifecycle.PathsOf(state).Lock) != 0 {
		t.Error("--no-start started an engine anyway")
	}
	if _, err := os.Stat(lifecycle.PathsOf(state).Socket); !os.IsNotExist(err) {
		t.Error("a socket was created without an engine")
	}
}

func TestIdleShutdown(t *testing.T) {
	bin := testsupport.BuildBrigada(t)
	state := testsupport.ShortStateDir(t)
	// 0.03 minutes is 1.8 s: long enough to start, short enough to test.
	p := startEngine(t, bin, state, "--idle-minutes", "0.03")
	waitHealthy(t, state)

	if code := p.waitExit(t, 15*time.Second); code != 0 {
		t.Errorf("the idle engine exited %d, want 0", code)
	}
	socket := lifecycle.PathsOf(state).Socket
	if _, err := os.Stat(socket); !os.IsNotExist(err) {
		t.Errorf("the idle engine left its socket behind: %v", err)
	}
	if pid := lifecycle.EnginePid(lifecycle.PathsOf(state).Lock); pid != 0 {
		t.Errorf("the idle engine left its lock held by pid %d", pid)
	}

	// The next engine starts cleanly on the same state directory.
	next := startEngine(t, bin, state)
	waitHealthy(t, state)
	next.stop(t)
}
