package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/lifecycle"
	"github.com/Blackfire-AI/brigada/internal/runnerstate"
	"github.com/Blackfire-AI/brigada/internal/store"
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

// TestStopDrainNeedsEngine: --drain without --engine is refused with a
// clear error (§8.3).
func TestStopDrainNeedsEngine(t *testing.T) {
	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"stop", "--drain"}, &out, &errb)
	if code != 2 || !strings.Contains(errb.String(), "--drain pauses the engine and waits out its jobs") {
		t.Fatalf("code=%d err=%q, want the --engine requirement", code, errb.String())
	}
}

// drainTestEnv is the `stop --engine --drain` fixture: an in-process
// API served on the state directory's own socket over a store holding
// one project and one running foreman job, a runner helper holding the
// runner lock and an engine helper holding the engine lock. The API
// server stands in for the engine's client surface; the helpers stand
// in for its processes.
func drainTestEnv(t *testing.T) (state string, st *store.Store, jobID int64) {
	t.Helper()
	ctx := context.Background()
	state = testsupport.ShortStateDir(t)
	paths := lifecycle.PathsOf(state)
	st = openFreshStore(t, paths.Store)
	t.Cleanup(func() { st.Close() })
	if err := lifecycle.EnsureTokenFile(paths.Tokens); err != nil {
		t.Fatal(err)
	}
	err := st.Update(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO projects (repo_url, default_branch, settings, foreman_harness, foreman_model, created_at, updated_at)
			VALUES ('https://example.invalid/one.git', 'main', '{}', 'claude', 'sonnet', '2026-11-09T00:00:00Z', '2026-11-09T00:00:00Z')`); err != nil {
			return err
		}
		res, err := tx.Exec(`INSERT INTO jobs (project_id, kind, key, mode, prompt, status, created_at, updated_at)
			VALUES (1, 'foreman', 'foreman:1', 'session', 'wake up', 'running', '2026-11-09T00:00:00Z', '2026-11-09T00:00:00Z')`)
		if err != nil {
			return err
		}
		jobID, err = res.LastInsertId()
		return err
	})
	if err != nil {
		t.Fatalf("seed the store: %v", err)
	}
	srv, err := api.New(ctx, st, engine.New(st, engine.Config{}), nil, paths.Tokens)
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", paths.Socket)
	if err != nil {
		t.Fatalf("listen on the test socket: %v", err)
	}
	server := httptest.NewUnstartedServer(srv.Handler())
	server.Listener = l
	server.Start()
	t.Cleanup(server.Close)
	startStopHelper(t, "runner", state)
	startStopHelper(t, "engine", state)
	return state, st, jobID
}

// drainPause reads the engine's standing pause, if one stands: its
// origin, else the empty string.
func drainPause(t *testing.T, st *store.Store) string {
	t.Helper()
	var origin string
	err := st.View(context.Background(), func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT origin FROM pauses WHERE ended_at IS NULL AND project_id IS NULL`).Scan(&origin)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return ""
	}
	if err != nil {
		t.Fatalf("read the engine's pause: %v", err)
	}
	return origin
}

// TestStopEngineDrain: the drain pauses the engine, waits while the
// running job holds it — naming the job — and stops the runner and the
// engine once the job ends (§8.3).
func TestStopEngineDrain(t *testing.T) {
	state, st, job := drainTestEnv(t)
	finish := time.AfterFunc(3*drainPollInterval, func() {
		if err := st.Update(context.Background(), func(tx *sql.Tx) error {
			_, err := tx.Exec(`UPDATE jobs SET status = 'done', updated_at = '2026-11-09T00:01:00Z' WHERE id = ?`, job)
			return err
		}); err != nil {
			t.Errorf("finish the job: %v", err)
		}
	})
	defer finish.Stop()
	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"stop", "--engine", "--drain", "--state-dir", state}, &out, &errb)
	if code != 0 || errb.String() != "" {
		t.Fatalf("code=%d out=%q err=%q", code, out.String(), errb.String())
	}
	for _, want := range []string{
		"engine paused for the drain",
		fmt.Sprintf("draining: job %d (foreman, running)", job),
		"engine drained",
		"runner stopped",
		"engine stopped",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout %q lacks %q", out.String(), want)
		}
	}
	// The drain's pause outlives the stop: stored, it waits for
	// `brigada start` to lift it (§8.3).
	if origin := drainPause(t, st); origin != engine.PauseOriginDrain {
		t.Fatalf("the standing engine pause has origin %q, want drain", origin)
	}
}

// TestStopEngineDrainOverUserPause: over a user's pause the origin
// stays the user's (§8.3), and the drain still waits the jobs out.
func TestStopEngineDrainOverUserPause(t *testing.T) {
	state, st, job := drainTestEnv(t)
	client := api.NewClient(lifecycle.PathsOf(state).Socket, clientToken(t, state))
	if _, err := client.Pause(context.Background(), "", engine.PauseOriginUser); err != nil {
		t.Fatalf("pause the engine: %v", err)
	}
	finish := time.AfterFunc(2*drainPollInterval, func() {
		if err := st.Update(context.Background(), func(tx *sql.Tx) error {
			_, err := tx.Exec(`UPDATE jobs SET status = 'done', updated_at = '2026-11-09T00:01:00Z' WHERE id = ?`, job)
			return err
		}); err != nil {
			t.Errorf("finish the job: %v", err)
		}
	})
	defer finish.Stop()
	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"stop", "--engine", "--drain", "--state-dir", state}, &out, &errb)
	if code != 0 || errb.String() != "" {
		t.Fatalf("code=%d out=%q err=%q", code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "engine already paused (origin user)") || !strings.Contains(out.String(), "engine drained") {
		t.Fatalf("stdout=%q, want the user's origin named and the drain finished", out.String())
	}
	if origin := drainPause(t, st); origin != engine.PauseOriginUser {
		t.Fatalf("the standing engine pause has origin %q, want the user's", origin)
	}
}

// TestStopEngineDrainInterrupted: interrupting the wait stops nothing
// and leaves the pause in place, and says so (§8.3); `brigada start`
// then lifts the drain pause, the engine still up.
func TestStopEngineDrainInterrupted(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	state, st, _ := drainTestEnv(t)
	runnerDir := filepath.Join(state, "runner")
	before, running, err := runnerstate.Running(runnerDir)
	if err != nil || !running {
		t.Fatalf("the runner helper is not running: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(3*drainPollInterval, cancel)
	var out, errb bytes.Buffer
	code := run(ctx, []string{"stop", "--engine", "--drain", "--state-dir", state}, &out, &errb)
	if code != 1 {
		t.Fatalf("code=%d out=%q err=%q, want the interrupt's exit 1", code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "drain interrupted: nothing stopped; the engine stays paused (origin drain); `brigada start` lifts it") {
		t.Fatalf("stdout=%q, want the interruption said", out.String())
	}
	if origin := drainPause(t, st); origin != engine.PauseOriginDrain {
		t.Fatalf("the standing engine pause has origin %q, want drain", origin)
	}
	if rec, running, _ := runnerstate.Running(runnerDir); !running || rec.PID != before.PID {
		t.Fatal("the drain's interrupt stopped the runner")
	}
	if pid := lifecycle.EnginePid(lifecycle.PathsOf(state).Lock); pid == 0 {
		t.Fatal("the drain's interrupt stopped the engine")
	}
	// The engine still up after the interrupted drain: `brigada start`
	// lifts the drain pause (§8.3).
	startCode, startOut, startErrs := startDo("--state-dir", state)
	if startCode != 0 || !strings.Contains(startOut, "engine resumed: the drain pause is lifted") || startErrs != "" {
		t.Fatalf("start after the interrupt: code=%d out=%q err=%q", startCode, startOut, startErrs)
	}
	if origin := drainPause(t, st); origin != "" {
		t.Fatalf("the engine pause still stands with origin %q, want it lifted", origin)
	}
}

// clientToken reads the state directory's client token secret, for the
// fixtures that drive the in-process API directly.
func clientToken(t *testing.T, state string) string {
	t.Helper()
	_, secret, err := lifecycle.TokenForRole(lifecycle.PathsOf(state).Tokens, "client")
	if err != nil {
		t.Fatal(err)
	}
	return secret
}
