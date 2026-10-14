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
	if line := out.String(); !strings.Contains(line, "(harness kimi, model default)") {
		t.Errorf("the startup line = %q, want the model to read default", line)
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
