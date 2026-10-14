package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/lifecycle"
	"github.com/Blackfire-AI/brigada/internal/store"
	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

// syncBuffer is a bytes.Buffer a running goroutine writes to while the
// test reads: the runner's startup line is read while the runner runs.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestRunnerKimiRegisters runs the runner command against a real engine
// started in-process (§13), the same lifecycle a user's engine sees,
// with --harness kimi and no --model: the agent that registers carries
// the kimi adapter and the kimi family, its model is the name for the
// CLI's own default, and the startup line says "default" rather than an
// empty string. The agent file resolves at startup, so a good one is
// through before the first job.
func TestRunnerKimiRegisters(t *testing.T) {
	state := testsupport.ShortStateDir(t)
	agentFile := filepath.Join(t.TempDir(), "worker.md")
	if err := os.WriteFile(agentFile, []byte("# the agent\n"), 0o644); err != nil {
		t.Fatalf("write the agent file: %v", err)
	}

	// The store handle the test polls: opened before the engine starts,
	// read while it runs — one reader beside the engine's writer.
	st, err := store.Open(context.Background(), lifecycle.PathsOf(state).Store)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	defer st.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- lifecycle.Run(ctx, lifecycle.Config{StateDir: state, Idle: time.Minute}) }()
	socket := lifecycle.PathsOf(state).Socket
	deadline := time.Now().Add(10 * time.Second)
	for !lifecycle.EngineRunning(ctx, socket) {
		if time.Now().After(deadline) {
			t.Fatal("the engine did not answer in time")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The runner runs until stopped; registering is the first thing it
	// does, so the row it leaves is the test's signal to stop it.
	rctx, stop := context.WithCancel(context.Background())
	defer stop()
	out := &syncBuffer{}
	runDone := make(chan int, 1)
	go func() {
		runDone <- run(rctx, []string{"runner", "--harness", "kimi", "--agent-file", agentFile,
			"--state-dir", state, "--no-start"}, out, io.Discard)
	}()
	var adapterName, family, model string
	deadline = time.Now().Add(10 * time.Second)
	for {
		err := st.View(ctx, func(tx *sql.Tx) error {
			return tx.QueryRow("SELECT adapter, family, model FROM agents WHERE harness = 'kimi'").
				Scan(&adapterName, &family, &model)
		})
		if err == nil {
			break
		}
		if !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("read the registered agent: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("the runner did not register its agent in time")
		}
		time.Sleep(20 * time.Millisecond)
	}
	stop()
	select {
	case <-runDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the runner did not stop")
	}

	if adapterName != "kimi" || family != "kimi" {
		t.Errorf("the registered agent = adapter %s, family %s; want kimi, kimi", adapterName, family)
	}
	if model != "default" {
		t.Errorf("the registered model = %q, want \"default\": the name for the CLI's own", model)
	}
	if line := out.String(); !strings.Contains(line, "kimi (model default, classes implement,review,answer)") {
		t.Errorf("the startup line = %q, want the model to read default with the classes", line)
	}

	// The engine stops cleanly once the runner is gone.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("the engine stopped with an error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Error("the engine did not stop")
	}
}

// TestRunnerHarnessFlags covers the harness flag's rules at startup:
// --agent-file belongs to kimi alone and is a usage error under every
// other harness, an agent file that does not exist fails at once rather
// than at the first job, and an unknown harness names all three.
func TestRunnerHarnessFlags(t *testing.T) {
	state := testsupport.ShortStateDir(t)
	dir := t.TempDir()
	agentFile := filepath.Join(dir, "worker.md")
	if err := os.WriteFile(agentFile, []byte("# the agent\n"), 0o644); err != nil {
		t.Fatalf("write the agent file: %v", err)
	}
	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantErr  string
	}{
		{
			name:     "an unknown harness names all three",
			args:     []string{"runner", "--harness", "codex", "--state-dir", state},
			wantCode: 2,
			wantErr:  `unknown harness "codex"; want claude, kimi or fake`,
		},
		{
			name:     "agent file with the claude harness is a usage error",
			args:     []string{"runner", "--harness", "claude", "--agent-file", agentFile, "--state-dir", state},
			wantCode: 2,
			wantErr:  "--agent-file is a --harness kimi flag",
		},
		{
			name: "agent file with the fake harness is a usage error",
			args: []string{"runner", "--harness", "fake", "--worker-script", "worker.sh",
				"--agent-file", agentFile, "--state-dir", state},
			wantCode: 2,
			wantErr:  "--agent-file is a --harness kimi flag",
		},
		{
			name: "a missing agent file fails at startup",
			args: []string{"runner", "--harness", "kimi", "--agent-file", filepath.Join(dir, "nope.md"),
				"--state-dir", state},
			wantCode: 1,
			wantErr:  "nope.md",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), tt.args, &stdout, &stderr)
			if code != tt.wantCode {
				t.Errorf("run(%q) exit code = %d, want %d", tt.args, code, tt.wantCode)
			}
			if !strings.Contains(stderr.String(), tt.wantErr) {
				t.Errorf("run(%q) stderr = %q, want it to contain %q", tt.args, stderr.String(), tt.wantErr)
			}
		})
	}
}

// TestRunnerAgentFlag covers the --agent flag's rules at startup: two
// agents register as two rows with their own models and classes, a
// duplicate harness is a usage error, mixing --agent with the
// single-agent shorthand flags is a usage error, and a missing agent
// file or an empty classes list fails at once rather than at the first
// job.
func TestRunnerAgentFlag(t *testing.T) {
	state := testsupport.ShortStateDir(t)
	dir := t.TempDir()
	agentFile := filepath.Join(dir, "worker.md")
	if err := os.WriteFile(agentFile, []byte("# the agent\n"), 0o644); err != nil {
		t.Fatalf("write the agent file: %v", err)
	}
	worker := filepath.Join(dir, "worker.sh")
	if err := os.WriteFile(worker, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write the worker script: %v", err)
	}

	// Two --agent values, no engine: the usage errors all fire before
	// the runner would start one.
	bad := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name:    "a duplicate harness is refused",
			args:    []string{"runner", "--agent", "harness=claude", "--agent", "harness=claude,model=opus", "--state-dir", state},
			wantErr: `harness "claude" is offered twice`,
		},
		{
			name:    "mixing --agent with --harness is refused",
			args:    []string{"runner", "--agent", "harness=claude", "--harness", "kimi", "--state-dir", state},
			wantErr: "--agent and --harness cannot be mixed",
		},
		{
			name:    "mixing --agent with --model is refused",
			args:    []string{"runner", "--agent", "harness=claude", "--model", "opus", "--state-dir", state},
			wantErr: "--agent and --model cannot be mixed",
		},
		{
			name:    "mixing --agent with --classes is refused",
			args:    []string{"runner", "--agent", "harness=claude", "--classes", "implement", "--state-dir", state},
			wantErr: "--agent and --classes cannot be mixed",
		},
		{
			name:    "a missing harness is refused",
			args:    []string{"runner", "--agent", "model=opus", "--state-dir", state},
			wantErr: "harness=<h> is required",
		},
		{
			name:    "an unknown field is refused",
			args:    []string{"runner", "--agent", "harness=claude,budget=5", "--state-dir", state},
			wantErr: `unknown field "budget"`,
		},
		{
			name:    "an unknown harness names all three",
			args:    []string{"runner", "--agent", "harness=codex", "--state-dir", state},
			wantErr: `unknown harness "codex"; want claude, kimi or fake`,
		},
		{
			name:    "an agent file under the claude harness is refused",
			args:    []string{"runner", "--agent", "harness=claude,agent-file=" + agentFile, "--state-dir", state},
			wantErr: "--agent-file is a --harness kimi flag",
		},
		{
			name:    "a missing agent file fails at startup",
			args:    []string{"runner", "--agent", "harness=kimi,agent-file=" + filepath.Join(dir, "nope.md"), "--state-dir", state},
			wantErr: "nope.md",
		},
		{
			name:    "an empty classes list is refused",
			args:    []string{"runner", "--agent", "harness=claude,classes=", "--state-dir", state},
			wantErr: `classes is empty`,
		},
	}
	for _, tt := range bad {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), tt.args, &stdout, &stderr)
			if code == 0 {
				t.Errorf("run(%q) exit code = 0, want a refusal", tt.args)
			}
			if !strings.Contains(stderr.String(), tt.wantErr) {
				t.Errorf("run(%q) stderr = %q, want it to contain %q", tt.args, stderr.String(), tt.wantErr)
			}
		})
	}

	// Two agents against a real engine: both register, each with its
	// own model and classes, and the startup line lists them both.
	st, err := store.Open(context.Background(), lifecycle.PathsOf(state).Store)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	defer st.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- lifecycle.Run(ctx, lifecycle.Config{StateDir: state, Idle: time.Minute}) }()
	socket := lifecycle.PathsOf(state).Socket
	deadline := time.Now().Add(10 * time.Second)
	for !lifecycle.EngineRunning(ctx, socket) {
		if time.Now().After(deadline) {
			t.Fatal("the engine did not answer in time")
		}
		time.Sleep(20 * time.Millisecond)
	}

	rctx, stop := context.WithCancel(context.Background())
	defer stop()
	out := &syncBuffer{}
	runDone := make(chan int, 1)
	go func() {
		runDone <- run(rctx, []string{"runner",
			"--agent", "harness=claude,model=sonnet,classes=implement+review+answer",
			"--agent", "harness=fake,classes=review",
			"--worker-script", worker,
			"--state-dir", state, "--no-start"}, out, io.Discard)
	}()
	deadline = time.Now().Add(10 * time.Second)
	for {
		var n int
		err := st.View(ctx, func(tx *sql.Tx) error {
			return tx.QueryRow("SELECT COUNT(*) FROM agents WHERE harness IN ('claude', 'fake')").Scan(&n)
		})
		if err != nil {
			t.Fatalf("count the registered agents: %v", err)
		}
		if n == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the runner did not register both agents in time")
		}
		time.Sleep(20 * time.Millisecond)
	}
	var fakeClasses string
	if err := st.View(ctx, func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT classes FROM agents WHERE harness = 'fake'").Scan(&fakeClasses)
	}); err != nil {
		t.Fatalf("read the fake agent's classes: %v", err)
	}
	// A second runner process on the same state directory is refused at
	// the lock — after the engine answered and the token was read, but
	// before it can announce itself up: the startup line belongs to a
	// runner that is running.
	secondArgs := []string{"runner",
		"--agent", "harness=claude,model=sonnet,classes=implement+review+answer",
		"--agent", "harness=fake,classes=review",
		"--worker-script", worker,
		"--state-dir", state, "--no-start"}
	var secondOut, secondErr bytes.Buffer
	if code := run(context.Background(), secondArgs, &secondOut, &secondErr); code == 0 {
		t.Error("the second runner process on one state dir exited 0, want a refusal")
	}
	if err := secondErr.String(); !strings.Contains(err, "another runner") {
		t.Errorf("the second runner's stderr = %q, want it to name another runner", err)
	}
	if up := secondOut.String(); strings.Contains(up, "up on") {
		t.Errorf("the second runner printed a startup line before failing: %q", up)
	}
	stop()
	select {
	case <-runDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the runner did not stop")
	}
	if !strings.Contains(fakeClasses, `"review"`) {
		t.Errorf("the fake agent's classes = %s, want only review", fakeClasses)
	}
	line := out.String()
	for _, want := range []string{"claude (model sonnet, classes implement,review,answer)", "fake (model fake-1, classes review)"} {
		if !strings.Contains(line, want) {
			t.Errorf("the startup line = %q, want it to list %q", line, want)
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("the engine stopped with an error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Error("the engine did not stop")
	}
}
