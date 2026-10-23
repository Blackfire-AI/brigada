package lifecycle

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/runnerstate"
)

// Re-exec the test binary as a lock-owning child. It does not need an engine,
// vendor CLI or network; it exercises exactly the detached startup protocol.
func TestDetachedRunnerHelper(t *testing.T) {
	if os.Getenv("BRIGADA_RUNNER_HELPER") != "1" {
		return
	}
	var state string
	for i, arg := range os.Args {
		if arg == "--state-dir" && i+1 < len(os.Args) {
			state = os.Args[i+1]
		}
	}
	dir := filepath.Join(state, "runner")
	release, err := runnerstate.Acquire(dir)
	if err != nil {
		os.Exit(11)
	}
	defer release()
	fmt.Println("helper runner up")
	if err := runnerstate.Publish(dir, []api.Agent{{Harness: "claude", Model: "sonnet"}}); err != nil {
		os.Exit(12)
	}
	// A restart request is honoured the way the real runner honours one:
	// `now` exits at once, a drain only once its running job ended —
	// here, when the parent writes the release marker. Parent test
	// terminates us otherwise; the OS releases the lock.
	for {
		mode, ok := runnerstate.RestartRequested(dir)
		if !ok {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if mode == runnerstate.RestartNow {
			os.Exit(0)
		}
		for {
			if _, err := os.Stat(filepath.Join(dir, "release")); err == nil {
				os.Exit(0)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func helperRunnerBinary(t *testing.T) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "runner-helper")
	// The extra flags belong to the CLI being simulated, so pass them after --
	// and let the helper read them itself instead of Go's testing flag parser.
	script := "#!/bin/sh\nexec " + shellTestQuote(self) + " -test.run=^TestDetachedRunnerHelper$ -- \"$@\"\n"
	if err := os.WriteFile(binary, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BRIGADA_RUNNER_HELPER", "1")
	return binary
}
func shellTestQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

func TestEnsureRunnerStaleAndRacingStarts(t *testing.T) {
	binary := helperRunnerBinary(t)
	state := t.TempDir()
	dir := filepath.Join(state, "runner")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	stale, _ := json.Marshal(runnerstate.Record{PID: os.Getpid(), Agents: []api.Agent{{Model: "stale"}}})
	for name, data := range map[string][]byte{"runner.lock": []byte(strconv.Itoa(os.Getpid())), "runner.json": stale} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(state, "runner.log"), []byte("stale log\n"), 0600); err != nil {
		t.Fatal(err)
	}
	results := make(chan RunnerStartResult, 4)
	failures := make(chan error, 4)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			r, err := EnsureRunner(context.Background(), RunnerStartOptions{StateDir: state, Binary: binary})
			if err != nil {
				failures <- err
			} else {
				results <- r
			}
		})
	}
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		data, _ := os.ReadFile(filepath.Join(state, "runner.log"))
		t.Errorf("%v; log: %s", err, data)
	}
	started, pid := 0, 0
	for r := range results {
		if !r.Existing {
			started++
		}
		if pid != 0 && pid != r.Record.PID {
			t.Error("concurrent starters returned different pids")
		}
		pid = r.Record.PID
		if len(r.Record.Agents) != 1 || r.Record.Agents[0].Model != "sonnet" {
			t.Errorf("stale agents: %+v", r.Record)
		}
	}
	if pid > 0 && pid != os.Getpid() {
		defer syscall.Kill(pid, syscall.SIGTERM)
	}
	if started != 1 {
		t.Fatalf("started %d runners; want exactly one", started)
	}
	if pid == os.Getpid() {
		t.Fatal("trusted stale pid instead of kernel lock")
	}
	data, err := os.ReadFile(filepath.Join(state, "runner.log"))
	if err != nil || !strings.Contains(string(data), "helper runner up") {
		t.Fatal("no runner log")
	}
}

func TestEnsureRunnerStartupFailure(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "fails")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 23\n"), 0755); err != nil {
		t.Fatal(err)
	}
	_, err := EnsureRunner(context.Background(), RunnerStartOptions{StateDir: t.TempDir(), Binary: binary, Wait: time.Second})
	if err == nil {
		t.Fatal("reported a failed runner as started")
	}
}

// TestEnsureRunnerRestartHandoff covers the restart modes a starter can
// ask for: a drain waits out the runner's running job before the fresh
// start, and `now` makes way at once.
func TestEnsureRunnerRestartHandoff(t *testing.T) {
	binary := helperRunnerBinary(t)
	state := t.TempDir()
	dir := filepath.Join(state, "runner")
	ctx := context.Background()
	first, err := EnsureRunner(ctx, RunnerStartOptions{StateDir: state, Binary: binary})
	if err != nil {
		t.Fatalf("start the first runner: %v", err)
	}
	defer syscall.Kill(first.Record.PID, syscall.SIGTERM)

	// The drain blocks while the runner's running job has not ended.
	type start struct {
		result RunnerStartResult
		err    error
	}
	drained := make(chan start, 1)
	go func() {
		r, err := EnsureRunner(ctx, RunnerStartOptions{StateDir: state, Binary: binary, Restart: runnerstate.RestartDrain})
		drained <- start{r, err}
	}()
	select {
	case r := <-drained:
		t.Fatalf("the drain restarted the runner before its job ended: %+v %v", r.result, r.err)
	case <-time.After(400 * time.Millisecond):
	}
	if _, running, _ := runnerstate.Running(dir); !running {
		t.Fatal("the drain replaced a runner without waiting for its exit")
	}
	// The job ends; the drain completes into a fresh runner.
	if err := os.WriteFile(filepath.Join(dir, "release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-drained:
		if r.err != nil {
			t.Fatalf("the drain restart: %v", r.err)
		}
		if r.result.Existing || r.result.Record.PID == first.Record.PID {
			t.Fatalf("the drain restart = %+v, want a fresh runner", r.result)
		}
		defer syscall.Kill(r.result.Record.PID, syscall.SIGTERM)
	case <-time.After(10 * time.Second):
		t.Fatal("the drain restart never finished")
	}
	if _, ok := runnerstate.RestartRequested(dir); ok {
		t.Error("a consumed restart request was left behind")
	}

	// `now` makes way at once, with no job to wait out.
	now := make(chan start, 1)
	go func() {
		r, err := EnsureRunner(ctx, RunnerStartOptions{StateDir: state, Binary: binary, Restart: runnerstate.RestartNow})
		now <- start{r, err}
	}()
	select {
	case r := <-now:
		if r.err != nil {
			t.Fatalf("the --now restart: %v", r.err)
		}
		if r.result.Existing {
			t.Fatalf("the --now restart = %+v, want a fresh runner", r.result)
		}
		syscall.Kill(r.result.Record.PID, syscall.SIGTERM)
	case <-time.After(10 * time.Second):
		t.Fatal("the --now restart never finished")
	}
}
